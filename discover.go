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
// 覆盖范围：只包含真正声明了平台（platform 非空且 os/architecture 齐全）的
// 条目；没有平台信息的附属条目（签名、证明材料等）不出现在 Entries 中。
//
// 顺序语义：平台条目严格按清单 manifests 数组的内容定义顺序返回，Entry.Index
// 是该条目在原 manifests 数组中的下标（附属条目被跳过后下标可能不连续）；
// 由于索引本身是内容寻址的不可变字节，同一摘要永远得到同一顺序，结果稳定
// 可复现。
type DiscoveryResult struct {
	Kind    string
	Root    Digest
	Entries []PlatformEntry
}

// DiscoverPlatforms 解析标签或摘要引用（ref 为 "sha256:..." 按摘要，否则按标签），
// 列出其包含的全部平台描述：
//
//   - 多架构索引：只收录真正声明了平台（platform 非空且 os/architecture 齐全）
//     的条目，按 manifests 数组顺序，基础信息（os/arch）与可选信息
//     （variant、os.version、os.features、features）原样呈现；
//   - 没有 platform 信息的附属条目（签名、证明材料等）不是平台：不会出现在
//     结果里，也不参与后续"全部平台"的批量准备与统计；
//   - 条目不合法（摘要格式非法，或声明了 platform 但 os/architecture 残缺）：
//     返回 ErrPlatformBad（清单本身有问题），与"条件没筛中"明确区分；
//   - 单平台镜像清单：返回恰好一项，Platform 为零值、Descriptor 指向根自身，
//     Kind 为 KindManifest，调用方无需靠错误类型猜测；
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
			if d.Platform == nil {
				// 附属条目（签名/证明材料等）不声明平台：不是平台镜像，
				// 既不进平台列表，也不参与批量准备与统计，直接跳过。
				continue
			}
			// 真正的平台条目必须合法：坏条目不静默跳过，清单本身有问题要与
			// "条件没筛中"区分开。
			if _, err := ParseDigest(string(d.Digest)); err != nil {
				return nil, fmt.Errorf("%w: manifests[%d] has invalid digest %q",
					ErrPlatformBad, i, d.Digest)
			}
			if d.Platform.OS == "" || d.Platform.Architecture == "" {
				return nil, fmt.Errorf("%w: manifests[%d] declares platform without os/architecture: %+v",
					ErrPlatformBad, i, d.Platform)
			}
			res.Entries = append(res.Entries, PlatformEntry{
				Index: i, Platform: *d.Platform, Descriptor: d,
			})
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
