package ocistore

import "context"

// PlatformMatch 是一次条件挑选命中的平台。
type PlatformMatch struct {
	Entry PlatformEntry
}

// PlatformSelector 是平台筛选条件。
//
// 条件语义（关键）：
//
//   - 只有被显式设置过的维度才参与相等判断；未设置（nil）的维度完全不参与
//     比较。例如只调用 WithOS("linux") 时，架构/变体/版本/特性都不构成约束，
//     linux/amd64 与 linux/arm64/v8 都会进入候选。
//   - 候选在未被约束的维度上多写信息不会被排除：条件没给 variant 时，
//     带 variant=v8 的候选依然算候选；条件给了特性集合时，候选携带额外特性
//     也不会被排除（子集合包含语义），但条件要求的每一个特性都必须出现。
//   - 显式设置为空串（如 WithVariant("")）与未设置不同：它要求候选该维度
//     恰好为空。需要这种精确语义时才使用链式 setter；零值 PlatformSelector
//     不构成任何约束（在全部平台中择优）。
type PlatformSelector struct {
	os           *string
	architecture *string
	variant      *string
	osVersion    *string
	osFeatures   []string
	features     []string
}

// NewPlatformSelector 创建一个不含任何约束的空条件（在全部平台中择优）。
func NewPlatformSelector() *PlatformSelector { return &PlatformSelector{} }

func strp(v string) *string { return &v }

// WithOS 约束操作系统维度。
func (q *PlatformSelector) WithOS(v string) *PlatformSelector { q.os = strp(v); return q }

// WithArchitecture 约束架构维度。
func (q *PlatformSelector) WithArchitecture(v string) *PlatformSelector {
	q.architecture = strp(v)
	return q
}

// WithVariant 约束变体维度。
func (q *PlatformSelector) WithVariant(v string) *PlatformSelector { q.variant = strp(v); return q }

// WithOSVersion 约束操作系统版本维度。
func (q *PlatformSelector) WithOSVersion(v string) *PlatformSelector { q.osVersion = strp(v); return q }

// WithOSFeatures 约束系统附加特性：候选必须包含给定全部特性（携带额外特性不排除）。
func (q *PlatformSelector) WithOSFeatures(feats ...string) *PlatformSelector {
	q.osFeatures = append(q.osFeatures, feats...)
	return q
}

// WithFeatures 约束附加特性：候选必须包含给定全部特性（携带额外特性不排除）。
func (q *PlatformSelector) WithFeatures(feats ...string) *PlatformSelector {
	q.features = append(q.features, feats...)
	return q
}

// empty 报告条件是否不含任何约束。
func (q *PlatformSelector) empty() bool {
	return q == nil || (q.os == nil && q.architecture == nil && q.variant == nil &&
		q.osVersion == nil && len(q.osFeatures) == 0 && len(q.features) == 0)
}

// match 判断单个候选是否满足条件：只比较被设置的维度，其余维度不参与判断。
func (q *PlatformSelector) match(p Platform) bool {
	if q == nil {
		return true
	}
	if q.os != nil && p.OS != *q.os {
		return false
	}
	if q.architecture != nil && p.Architecture != *q.architecture {
		return false
	}
	if q.variant != nil && p.Variant != *q.variant {
		return false
	}
	if q.osVersion != nil && p.OSVersion != *q.osVersion {
		return false
	}
	if !containsAll(p.OSFeatures, q.osFeatures) || !containsAll(p.Features, q.features) {
		return false
	}
	return true
}

// containsAll 判断 have 是否包含 want 的每一个元素（have 可以有额外元素）。
func containsAll(have, want []string) bool {
	if len(want) == 0 {
		return true
	}
	seen := make(map[string]struct{}, len(have))
	for _, h := range have {
		seen[h] = struct{}{}
	}
	for _, w := range want {
		if _, ok := seen[w]; !ok {
			return false
		}
	}
	return true
}

// defaultVariant 返回架构在清单未声明 variant 时的惯例默认变体。
// 只收录镜像生态里事实通用的映射（arm64/aarch64 -> v8，arm -> v7），
// 其余架构没有公认默认值，返回空串。
func defaultVariant(arch string) string {
	switch arch {
	case "arm64", "aarch64":
		return "v8"
	case "arm":
		return "v7"
	}
	return ""
}

// variantRank 给候选的变体维度排优先级（仅在条件未约束 variant 时使用）：
// 命中该架构惯例默认变体、或省略 variant（惯例上等价于默认）的候选最通用，
// 更特异的显式变体排后；该架构无默认变体概念时全部同分。
func variantRank(p Platform) int {
	def := defaultVariant(p.Architecture)
	if def == "" {
		return 0
	}
	switch p.Variant {
	case "", def:
		return 0
	default:
		return 1
	}
}

// less 报告候选 a 是否应排在 b 前面（a 比 b "更合适"）。
//
// 排序只使用清单自身声明的内容，比较的都是条件未约束、因此不参与硬过滤的
// 维度；声明越接近"该架构的惯例通用形态、越精简"的候选越靠前：
//
//  1. 变体：默认变体/省略优于更特异的变体；
//  2. os.version：未声明优于额外声明；
//  3. os.features / features：未携带优于携带额外特性；
//  4. 以上完全并列时，按索引内容定义顺序（Index）兜底。
//
// 因而同一索引、同一组条件的结果永远一致，与调用时机、map 遍历无关。
func (q *PlatformSelector) less(a, b PlatformEntry) bool {
	av, bv := variantRank(a.Platform), variantRank(b.Platform)
	if av != bv {
		return av < bv
	}
	if (a.Platform.OSVersion == "") != (b.Platform.OSVersion == "") {
		return a.Platform.OSVersion == ""
	}
	if (len(a.Platform.OSFeatures) == 0) != (len(b.Platform.OSFeatures) == 0) {
		return len(a.Platform.OSFeatures) == 0
	}
	if (len(a.Platform.Features) == 0) != (len(b.Platform.Features) == 0) {
		return len(a.Platform.Features) == 0
	}
	return a.Index < b.Index
}

// pickBest 在已通过硬过滤的候选中择优：确定性排序，取第一名。
func (q *PlatformSelector) pickBest(cands []PlatformEntry) PlatformEntry {
	best := cands[0]
	for _, c := range cands[1:] {
		if q.less(c, best) {
			best = c
		}
	}
	return best
}

// SelectPlatform 按（可部分给出的）条件在引用中挑选唯一一个最合适的平台。
//
// 与"返回全部命中、让调用方二次选择"不同，本方法直接给出确定结果：
// 先按条件做硬过滤（未设置的维度不参与比较，见 PlatformSelector），再按
// PlatformSelector.less 的固定择优规则排序取第一名；同一份索引、同一组
// 条件，无论调用多少次结果都一致，调用方不需要再做二次选择。
//
// 返回值约定（两种"没结果"严格分开）：
//
//   - 发现/解析失败（引用不存在、根内容损坏、JSON 非法、媒体类型不支持、
//     索引条目摘要非法、平台声明残缺）：返回错误，可用 errors.Is 判定原因；
//   - 清单本身合法但条件一个都没命中：返回 err == nil 且 match == nil，
//     这不是错误；批量解析中对应条件下标会计入 BatchResult.Unmatched。
//
// 单平台镜像清单：其唯一 entry 平台为零值，空条件选中它，非空条件不命中。
func (s *Store) SelectPlatform(ctx context.Context, ref string, q *PlatformSelector) (*DiscoveryResult, *PlatformMatch, error) {
	disc, err := s.DiscoverPlatforms(ctx, ref)
	if err != nil {
		return nil, nil, err
	}
	cands := make([]PlatformEntry, 0, len(disc.Entries))
	for _, e := range disc.Entries {
		if q.match(e.Platform) {
			cands = append(cands, e)
		}
	}
	if len(cands) == 0 {
		return disc, nil, nil
	}
	best := q.pickBest(cands)
	return disc, &PlatformMatch{Entry: best}, nil
}
