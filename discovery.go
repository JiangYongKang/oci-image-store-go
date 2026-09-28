package ocistore

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// PlatformSelector 是“部分平台条件”：只有被显式设置的维度才参与筛选，
// 未设置（nil）的维度不参与相等判断。
//
// 切片维度（OSFeatures/Features）为 nil 表示不约束；为非 nil 时要求候选
// “包含全部给定特性”（子集语义），候选可以额外携带未被要求的特性。
// 注意 nil 与非 nil 的空切片语义相同（都表示“不要求任何具体特性”），
// 要区分请只依赖 With* 构造器：不调用即不约束。
type PlatformSelector struct {
	OS           *string
	Architecture *string
	OSVersion    *string
	Variant      *string
	OSFeatures   []string
	Features     []string
}

func strPtr(v string) *string { return &v }

// WithOS 仅以“系统”作为筛选条件（其余维度不参与比较）。
func (sel PlatformSelector) WithOS(v string) PlatformSelector {
	sel.OS = strPtr(v)
	return sel
}

// WithArchitecture 仅以“架构”作为筛选条件（其余维度不参与比较）。
func (sel PlatformSelector) WithArchitecture(v string) PlatformSelector {
	sel.Architecture = strPtr(v)
	return sel
}

// WithOSVersion 仅以“系统版本”作为筛选条件。
func (sel PlatformSelector) WithOSVersion(v string) PlatformSelector {
	sel.OSVersion = strPtr(v)
	return sel
}

// WithVariant 仅以“变体”作为筛选条件。
func (sel PlatformSelector) WithVariant(v string) PlatformSelector {
	sel.Variant = strPtr(v)
	return sel
}

// WithOSFeatures 要求候选携带给出的全部系统特性；候选可额外携带其它特性。
// 不传参数等同于不在该维度设置约束。
func (sel PlatformSelector) WithOSFeatures(v ...string) PlatformSelector {
	if len(v) == 0 {
		sel.OSFeatures = nil
		return sel
	}
	sel.OSFeatures = append([]string(nil), v...)
	return sel
}

// WithFeatures 要求候选携带给出的全部（架构相关）特性；候选可额外携带其它特性。
// 不传参数等同于不在该维度设置约束。
func (sel PlatformSelector) WithFeatures(v ...string) PlatformSelector {
	if len(v) == 0 {
		sel.Features = nil
		return sel
	}
	sel.Features = append([]string(nil), v...)
	return sel
}

// PlatformInfo 是发现结果中的一个平台条目。
type PlatformInfo struct {
	Platform   Platform
	Descriptor Descriptor
	// IndexPos 是该条目在原始索引 manifests 数组中的位置；
	// 单平台镜像（非索引）的占位条目固定为 0。
	IndexPos int
}

// DiscoverResult 是一次发现的结果：
//   - 引用指向索引时 IsIndex=true，Platforms 为全部平台条目（确定性排序）；
//   - 引用指向单平台镜像清单时 IsIndex=false，Platforms 恰有一个占位条目：
//     Platform 为零值（单清单本身不带独立平台描述）、Descriptor 指向该清单本身。
//     这样“单平台镜像”也是一个明确一致的结果，而不是让调用方猜的错误。
type DiscoverResult struct {
	Root      Digest
	IsIndex   bool
	Index     *ImageIndex
	Platforms []PlatformInfo
}

// ---- 匹配 ----

// containsAll 报告 have 是否包含 want 的全部元素（顺序无关、子集语义）。
func containsAll(have, want []string) bool {
	if len(want) == 0 {
		return true
	}
	set := make(map[string]struct{}, len(have))
	for _, h := range have {
		set[h] = struct{}{}
	}
	for _, w := range want {
		if _, ok := set[w]; !ok {
			return false
		}
	}
	return true
}

// platformMatches 按“只比较显式给出的维度”判断候选是否满足条件。
// 候选在未约束维度上多写的任何信息都不会导致排除。
func platformMatches(p Platform, sel PlatformSelector) bool {
	if sel.OS != nil && p.OS != *sel.OS {
		return false
	}
	if sel.Architecture != nil && p.Architecture != *sel.Architecture {
		return false
	}
	if sel.OSVersion != nil && p.OSVersion != *sel.OSVersion {
		return false
	}
	if sel.Variant != nil && p.Variant != *sel.Variant {
		return false
	}
	if sel.OSFeatures != nil && !containsAll(p.OSFeatures, sel.OSFeatures) {
		return false
	}
	if sel.Features != nil && !containsAll(p.Features, sel.Features) {
		return false
	}
	return true
}

// surplusCount 统计候选在“未被条件约束的可选维度”上携带的额外信息量：
// 变体/系统版本非空各计 1；未要求的系统特性/特性按个数计。
// OS/Architecture 属身份维度，不计入额外量。数值越小越“朴素”。
func surplusCount(p Platform, sel PlatformSelector) int {
	n := 0
	if sel.Variant == nil && p.Variant != "" {
		n++
	}
	if sel.OSVersion == nil && p.OSVersion != "" {
		n++
	}
	if sel.OSFeatures == nil {
		n += len(p.OSFeatures)
	} else {
		want := map[string]struct{}{}
		for _, f := range sel.OSFeatures {
			want[f] = struct{}{}
		}
		for _, f := range p.OSFeatures {
			if _, ok := want[f]; !ok {
				n++
			}
		}
	}
	if sel.Features == nil {
		n += len(p.Features)
	} else {
		want := map[string]struct{}{}
		for _, f := range sel.Features {
			want[f] = struct{}{}
		}
		for _, f := range p.Features {
			if _, ok := want[f]; !ok {
				n++
			}
		}
	}
	return n
}

// canonicalKey 生成平台条目的确定性排序键：
// os/架构/变体/系统版本，再到两组特性（排序后），最后以描述符摘要兜底。
func canonicalKey(p Platform, d Digest) string {
	osf := append([]string(nil), p.OSFeatures...)
	ft := append([]string(nil), p.Features...)
	sort.Strings(osf)
	sort.Strings(ft)
	parts := []string{
		p.OS, p.Architecture, p.Variant, p.OSVersion,
		strings.Join(osf, ","), strings.Join(ft, ","),
		d.Hex(),
	}
	return strings.Join(parts, "\x00")
}

// SelectPlatform 按部分条件在索引中择优选择一个平台条目。
//
// 语义：
//   - 只有选择器中非 nil 的维度参与相等判断；候选多出的信息不排除候选；
//   - 清单本身有问题（条目摘要非法）返回 ErrPlatformBad；
//   - 没有任何候选满足条件返回 ErrPlatformNoMatch；两种失败可明确区分；
//   - 多个候选同时满足时按以下确定性顺序择优，不随机挑选：
//     1. 额外信息量少者优先（surplusCount：未约束的变体/系统版本/特性更少）；
//     2. 规范键字典序（os/架构/变体/系统版本/特性/摘要）；
//     3. 原始索引中的位置靠前。
func SelectPlatform(idx *ImageIndex, sel PlatformSelector) (PlatformInfo, error) {
	if idx == nil {
		return PlatformInfo{}, fmt.Errorf("%w: not an index", ErrPlatformBad)
	}
	// 先整体校验清单，坏清单绝不进入匹配（与 ErrPlatformNoMatch 区分）。
	for i := range idx.Manifests {
		d := idx.Manifests[i]
		if _, err := ParseDigest(string(d.Digest)); err != nil {
			return PlatformInfo{},
				fmt.Errorf("%w: manifests[%d] has invalid digest %q", ErrPlatformBad, i, d.Digest)
		}
	}

	var hits []PlatformInfo
	for i := range idx.Manifests {
		d := idx.Manifests[i]
		if d.Platform == nil {
			continue // 无平台描述的条目（如证明材料）不参与平台选择
		}
		if platformMatches(*d.Platform, sel) {
			hits = append(hits, PlatformInfo{Platform: *d.Platform, Descriptor: d, IndexPos: i})
		}
	}
	if len(hits) == 0 {
		return PlatformInfo{}, fmt.Errorf("%w: selector %s in index with %d entries",
			ErrPlatformNoMatch, selectorString(sel), len(idx.Manifests))
	}
	sort.SliceStable(hits, func(a, b int) bool {
		sa, sb := surplusCount(hits[a].Platform, sel), surplusCount(hits[b].Platform, sel)
		if sa != sb {
			return sa < sb
		}
		ka := canonicalKey(hits[a].Platform, hits[a].Descriptor.Digest)
		kb := canonicalKey(hits[b].Platform, hits[b].Descriptor.Digest)
		if ka != kb {
			return ka < kb
		}
		return hits[a].IndexPos < hits[b].IndexPos
	})
	return hits[0], nil
}

// selectorString 给出条件的可读描述，仅列出被显式设置的维度（用于错误/日志）。
func selectorString(sel PlatformSelector) string {
	var parts []string
	add := func(name, v string) { parts = append(parts, name+"="+v) }
	if sel.OS != nil {
		add("os", *sel.OS)
	}
	if sel.Architecture != nil {
		add("arch", *sel.Architecture)
	}
	if sel.OSVersion != nil {
		add("os.version", *sel.OSVersion)
	}
	if sel.Variant != nil {
		add("variant", *sel.Variant)
	}
	if sel.OSFeatures != nil {
		add("os.features", "["+strings.Join(sel.OSFeatures, ",")+"]")
	}
	if sel.Features != nil {
		add("features", "["+strings.Join(sel.Features, ",")+"]")
	}
	if len(parts) == 0 {
		return "{}"
	}
	return "{" + strings.Join(parts, " ") + "}"
}

// ---- 发现 ----

// discoverLocked 解析已读取出的根内容为发现结果（不涉及存储读取，便于复用）。
func discoverFromRoot(root Digest, data []byte) (*DiscoverResult, error) {
	isIndex, idx, mani, err := parseManifest("", data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", root, err)
	}
	res := &DiscoverResult{Root: root}
	if !isIndex {
		// 单平台镜像：统一成“恰好一个平台条目”的结果，不报错。
		res.Platforms = []PlatformInfo{{
			Descriptor: Descriptor{
				MediaType: mani.MediaType,
				Digest:    root,
				Size:      int64(len(data)),
			},
			IndexPos: 0,
		}}
		return res, nil
	}
	res.IsIndex = true
	res.Index = idx
	for i := range idx.Manifests {
		d := idx.Manifests[i]
		if d.Platform == nil {
			continue // 无平台描述的条目不列入平台清单
		}
		if _, err := ParseDigest(string(d.Digest)); err != nil {
			return nil, fmt.Errorf("%w: manifests[%d] has invalid digest %q",
				ErrPlatformBad, i, d.Digest)
		}
		res.Platforms = append(res.Platforms, PlatformInfo{
			Platform:   *d.Platform,
			Descriptor: d,
			IndexPos:   i,
		})
	}
	// 稳定可复现排序：规范键，相同键时保持原始位置顺序。
	sort.SliceStable(res.Platforms, func(a, b int) bool {
		ka := canonicalKey(res.Platforms[a].Platform, res.Platforms[a].Descriptor.Digest)
		kb := canonicalKey(res.Platforms[b].Platform, res.Platforms[b].Descriptor.Digest)
		if ka != kb {
			return ka < kb
		}
		return res.Platforms[a].IndexPos < res.Platforms[b].IndexPos
	})
	return res, nil
}

// Discover 解析标签或摘要引用（ref 为 sha256:... 时按摘要，否则按标签），
// 列出其包含的全部平台描述。顺序稳定可复现。
func (s *Store) Discover(ctx context.Context, ref string) (*DiscoverResult, error) {
	root, err := s.resolveRef(ref)
	if err != nil {
		return nil, err
	}
	data, err := s.blobData(root)
	if err != nil {
		return nil, err
	}
	return discoverFromRoot(root, data)
}
