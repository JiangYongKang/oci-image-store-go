package ocistore

import "context"

// PlatformMatch 是一次条件筛选命中的候选平台。
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

// SelectPlatforms 按部分条件在引用中挑选平台。
//
// 返回值约定（两种"没结果"严格分开）：
//
//   - 发现/解析失败（引用不存在、根内容损坏、JSON 非法、媒体类型不支持、
//     索引条目摘要非法）：返回错误，调用方可用 errors.Is 判定具体原因；
//   - 清单本身合法但条件一个都没命中：返回 err == nil 且 matches 为空切片，
//     这不是错误，调用方据此区分"清单有问题"与"条件没匹配上"。
//
// 命中多个时的择优规则：本方法不做随机二选一，而是按发现顺序（即索引
// manifests 的内容定义顺序）返回全部命中，由调用方按 Index 自行取舍；
// 批量解析时取这些命中的并集，顺序因此确定且可解释。
func (s *Store) SelectPlatforms(ctx context.Context, ref string, q *PlatformSelector) (*DiscoveryResult, []PlatformMatch, error) {
	disc, err := s.DiscoverPlatforms(ctx, ref)
	if err != nil {
		return nil, nil, err
	}
	matches := make([]PlatformMatch, 0)
	if q.empty() {
		for _, e := range disc.Entries {
			matches = append(matches, PlatformMatch{Entry: e})
		}
		return disc, matches, nil
	}
	for _, e := range disc.Entries {
		if q.match(e.Platform) {
			matches = append(matches, PlatformMatch{Entry: e})
		}
	}
	return disc, matches, nil
}
