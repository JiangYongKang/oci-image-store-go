package ocistore

import "context"

// PlatformMatch 是一次条件挑选选中的平台。
type PlatformMatch struct {
	Entry PlatformEntry
}

// PlatformSelector 是平台筛选条件。
//
// 条件语义（关键）：
//
//   - 只有被显式设置过的维度才参与相等判断；未设置（nil）的维度完全不参与
//     比较。例如只调用 WithOS("linux") 时，架构/变体/版本/特性都不构成约束，
//     linux/amd64 与 linux/arm64/v8 都会命中。
//   - 候选在未被约束的维度上多写信息不会被排除：条件没给 variant 时，
//     带 variant=v8 的候选依然算命中；条件给了特性集合时，候选携带额外特性
//     也不会被排除（子集合包含语义），但条件要求的每一个特性都必须出现。
//   - 显式设置为空串（如 WithVariant("")）与未设置不同：它要求候选该维度
//     恰好为空。需要这种精确语义时才使用链式 setter；零值 PlatformSelector
//     不构成任何约束。
type PlatformSelector struct {
	os           *string
	architecture *string
	variant      *string
	osVersion    *string
	osFeatures   []string
	features     []string
}

// NewPlatformSelector 创建一个不含任何约束的空条件（匹配全部平台）。
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

// SelectPlatform 按部分条件在引用中挑选唯一一个最合适的平台。
//
// 返回值约定（两种"没结果"严格分开）：
//
//   - 发现/解析失败（引用不存在、根内容损坏、JSON 非法、媒体类型不支持、
//     索引条目摘要非法或平台声明残缺）：返回 error，调用方可用 errors.Is
//     判定具体原因；
//   - 清单本身合法但条件一个都没命中：返回 err == nil 且 match == nil，
//     这不是错误，调用方据此区分"清单有问题"与"条件没匹配上"；
//   - 命中一个或多个：返回 err == nil 且 match 非 nil，永远是同一个结果，
//     调用方不需要再做二次选择。
//
// 择优规则（确定、可解释，同一份索引同一组条件结果恒定）：
//
//  1. 先按条件相等判断筛出全部命中（未设置的维度不参与比较，见 match）；
//  2. 在命中集合中，优先选择"额外特化更少"的候选：统计候选在条件未约束的
//     维度上多写的非默认信息数（variant、os.version、os.features、features
//     各按一项计），计数最小者胜——即条件只给 arch=arm64 时，裸 arm64 比
//     arm64/v7 更合适；条件没约束任何维度时，该计数对所有候选都生效；
//  3. 计数相同则按发现顺序（即索引 manifests 内容定义顺序）取最靠前者。
//
// 单平台镜像清单（Kind=KindManifest）：空条件选中唯一条目；非空条件因其
// 平台为零值而零命中（match == nil），与索引上的规则完全一致。
func (s *Store) SelectPlatform(ctx context.Context, ref string, q *PlatformSelector) (*DiscoveryResult, *PlatformMatch, error) {
	disc, err := s.DiscoverPlatforms(ctx, ref)
	if err != nil {
		return nil, nil, err
	}
	best := selectBest(disc.Entries, q)
	if best == nil {
		return disc, nil, nil
	}
	return disc, &PlatformMatch{Entry: *best}, nil
}

// selectBest 按 SelectPlatform 的择优规则从候选中选出唯一一个；零命中返回 nil。
func selectBest(entries []PlatformEntry, q *PlatformSelector) *PlatformEntry {
	var best *PlatformEntry
	bestExtra := 0
	for i := range entries {
		e := &entries[i]
		if !q.match(e.Platform) {
			continue
		}
		extra := extraSpecializations(e.Platform, q)
		if best == nil || extra < bestExtra {
			best = e
			bestExtra = extra
			// 计数相同不替换：entries 为发现顺序，天然保留"最靠前"。
		}
	}
	return best
}

// extraSpecializations 统计候选在条件未约束维度上多写的非默认特化信息数：
// variant / os.version / 特性列表四类各按一项计（列表非空即一项，不按元素数，
// 避免长特性列表在择优中获得不成比例的权重）。
func extraSpecializations(p Platform, q *PlatformSelector) int {
	if q == nil {
		q = NewPlatformSelector() // nil 条件 = 无约束：所有维度都计为额外特化
	}
	n := 0
	if q.variant == nil && p.Variant != "" {
		n++
	}
	if q.osVersion == nil && p.OSVersion != "" {
		n++
	}
	if len(q.osFeatures) == 0 && len(p.OSFeatures) > 0 {
		n++
	}
	if len(q.features) == 0 && len(p.Features) > 0 {
		n++
	}
	return n
}
