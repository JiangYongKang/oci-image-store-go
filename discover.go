package ocistore

import (
	"context"
	"fmt"
)

// PlatformEntry 描述一个候选平台（发现阶段不读取子清单内容）。
type PlatformEntry struct {
	Index      int        // 在索引 manifests 中的序号；单镜像清单来源恒为 0
	Platform   Platform   // 候选声明的平台；单镜像清单来源为零值 Platform{}
	Descriptor Descriptor // 索引条目描述符；单镜像清单来源为指向根自身的描述符
}

// DiscoveryResult 是一次平台发现的结果。
//
// Kind 为 KindIndex 表示引用指向多架构索引；为 KindManifest 表示引用本身
// 就是单个平台镜像清单——此时 Entries 恒有且仅有一条（平台为零值、描述符
// 指向根自身），以统一结果结构表达，而不是用错误让调用方猜测。
//
// 顺序语义：索引来源严格按清单 manifests 数组的内容定义顺序返回；由于索引
// 本身是内容寻址的不可变字节，同一摘要永远得到同一顺序，结果稳定可复现。
type DiscoveryResult struct {
	Kind    string
	Root    Digest
	Entries []PlatformEntry
}

// DiscoverPlatforms 解析标签或摘要引用（ref 为 "sha256:..." 按摘要，否则按标签），
// 列出其包含的全部平台描述：
//
//   - 多架构索引：每个 manifests 条目一项，按数组顺序，基础信息（os/arch）
//     与可选信息（variant、os.version、os.features、features）原样呈现；
//   - 单平台镜像清单：返回恰好一项，Platform 为零值、Descriptor 指向根自身，
//     Kind 为 KindManifest，调用方无需靠错误类型猜测；
//   - 索引中存在摘要非法的条目：返回 ErrPlatformBad（清单本身有问题），
//     与"条件没筛中"明确区分；
//   - 根内容截断/非法 JSON、媒体类型不支持、损坏/缺失：返回对应既有错误。
func (s *Store) DiscoverPlatforms(ctx context.Context, ref string) (*DiscoveryResult, error) {
	root, err := s.resolveRef(ref)
	if err != nil {
		return nil, err
	}
	data, err := s.blobData(root)
	if err != nil {
		return nil, err
	}
	isIndex, idx, mani, err := parseManifest("", data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", root, err)
	}

	res := &DiscoveryResult{Root: root}
	if isIndex {
		res.Kind = KindIndex
		res.Entries = make([]PlatformEntry, 0, len(idx.Manifests))
		for i := range idx.Manifests {
			d := idx.Manifests[i]
			// 坏条目不静默跳过：清单本身有问题要与"没筛中"区分开。
			if _, err := ParseDigest(string(d.Digest)); err != nil {
				return nil, fmt.Errorf("%w: manifests[%d] has invalid digest %q",
					ErrPlatformBad, i, d.Digest)
			}
			entry := PlatformEntry{Index: i, Descriptor: d}
			if d.Platform != nil {
				entry.Platform = *d.Platform
			}
			res.Entries = append(res.Entries, entry)
		}
		return res, nil
	}

	// 单个平台镜像清单：统一为"恰好一个候选"的结果。
	res.Kind = KindManifest
	res.Entries = []PlatformEntry{{
		Index:      0,
		Platform:   Platform{},
		Descriptor: Descriptor{Digest: root, MediaType: mani.MediaType, Size: int64(len(data))},
	}}
	return res, nil
}
