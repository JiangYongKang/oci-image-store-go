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

// AttachedEntry 是索引中未声明 platform 的附属条目（如签名、证明材料等）。
// 它不是任何平台：不进入 Entries，不参与条件筛选、批量准备与平台/层数/
// 大小统计；这里仅按索引内容定义顺序原样列出，供调用方审计索引构成。
type AttachedEntry struct {
	Index      int        // 在索引 manifests 中的序号
	Descriptor Descriptor // 附属条目描述符（原样呈现，不解析其内容）
}

// DiscoveryResult 是一次平台发现的结果。
//
// Kind 为 KindIndex 表示引用指向多架构索引；为 KindManifest 表示引用本身
// 就是单个平台镜像清单——此时 Entries 恒有且仅有一条（平台为零值、描述符
// 指向根自身），以统一结果结构表达，而不是用错误让调用方猜测。
//
// 条目分类（多架构索引）：
//   - 声明了合法 platform（os/architecture 均非空）的条目 -> Entries，即平台；
//   - 完全没有 platform 的条目（签名、attestation 等附属物）-> Attached，
//     不计为平台，也不参与后续批量准备与统计；
//   - 声明了 platform 但 os/architecture 残缺 -> 发现直接返回 ErrPlatformBad，
//     这是清单本身有问题，与"条件没筛中"严格区分。
//
// 顺序语义：Entries 与 Attached 都严格按清单 manifests 数组的内容定义顺序；
// 由于索引本身是内容寻址的不可变字节，同一摘要永远得到同一结果，稳定可复现。
type DiscoveryResult struct {
	Kind     string
	Root     Digest
	Entries  []PlatformEntry
	Attached []AttachedEntry
}

// DiscoverPlatforms 解析标签或摘要引用（ref 为 "sha256:..." 按摘要，否则按标签），
// 列出其包含的全部"平台"描述：
//
//   - 多架构索引：只有声明了合法 platform（os/architecture 均非空）的 manifests
//     条目才进入 Entries，按数组顺序，基础信息（os/arch）与可选信息（variant、
//     os.version、os.features、features）原样呈现；没有 platform 的附属条目
//     （签名、证明材料等）进入 Attached 而不是平台列表；
//   - 单平台镜像清单：返回恰好一项，Platform 为零值、Descriptor 指向根自身，
//     Kind 为 KindManifest，调用方无需靠错误类型猜测；
//   - 索引中存在摘要非法的条目，或条目声明了 platform 但 os/architecture
//     残缺：返回 ErrPlatformBad（清单本身有问题），与"条件没筛中"明确区分；
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
		res.Attached = make([]AttachedEntry, 0)
		for i := range idx.Manifests {
			d := idx.Manifests[i]
			// 坏条目不静默跳过：清单本身有问题要与"没筛中"区分开。
			if _, err := ParseDigest(string(d.Digest)); err != nil {
				return nil, fmt.Errorf("%w: manifests[%d] has invalid digest %q",
					ErrPlatformBad, i, d.Digest)
			}
			if d.Platform == nil {
				// 无平台信息的附属条目（签名、证明材料等）：不是平台，
				// 单列审计，不参与筛选/批量/统计。
				res.Attached = append(res.Attached, AttachedEntry{Index: i, Descriptor: d})
				continue
			}
			// 声明了平台却缺 os/architecture：清单内容残缺，按坏清单报出。
			if d.Platform.OS == "" || d.Platform.Architecture == "" {
				return nil, fmt.Errorf("%w: manifests[%d] declares incomplete platform %q",
					ErrPlatformBad, i, d.Platform)
			}
			res.Entries = append(res.Entries, PlatformEntry{
				Index:      i,
				Platform:   *d.Platform,
				Descriptor: d,
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
	res.Attached = []AttachedEntry{}
	return res, nil
}
