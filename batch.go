package ocistore

import (
	"context"
	"fmt"
	"sort"
)

// 发现结果种类。
const (
	KindIndex    = "index"
	KindManifest = "manifest"
)

// PlatformImage 是批量解析中单个平台的准备结果。
//
// Err 为 nil 表示成功：镜像清单、它引用的 config 与全部层都已通过完整性校验，
// LayerCount/TotalSize 可直接用于汇报。Err 非 nil 表示该平台未能准备好
// （子清单缺失/损坏、嵌套索引、config 或层缺失/损坏等），错误只属于该平台，
// 不影响批量中其它平台，也不产生半成品（批量过程只读，不发布任何临时内容）。
type PlatformImage struct {
	Entry              PlatformEntry
	Manifest           *ImageManifest
	ManifestDescriptor Descriptor
	Layers             []Descriptor
	LayerCount         int
	TotalSize          int64 // 各层 Descriptor.Size 之和
	Err                error
}

// BatchResult 是一次批量解析的整体结果。
//
//   - Images：去重后实际准备的平台，顺序与发现顺序一致（即索引 manifests 的
//     内容定义顺序）；多个条件命中同一平台清单时只准备一次。
//   - Unmatched：提交的条件中没有命中任何平台的条件下标（升序）。它与
//     Images[i].Err 表达两类不同问题：前者是"条件没筛中"，后者是"筛中了但
//     内容准备失败"。
type BatchResult struct {
	Kind      string
	Root      Digest
	Images    []PlatformImage
	Unmatched []int
}

// BatchResolve 一次提交多组部分条件：
//
//  1. 先发现引用下的全部平台，再按每个条件挑选命中平台，取并集并按清单摘要
//     去重（同一份镜像清单不重复准备）；没有命中任何平台的条件下标记入
//     Unmatched，不构成错误；
//  2. 对每个命中平台独立解析：读取子清单、拒绝嵌套索引、校验 config 与全部层，
//     汇总层数与总大小；单个平台失败只标记该 PlatformImage.Err，不连累其它平台；
//  3. 整次批量基于一个被 Pin 保护的摘要快照：标签迁移/删除只影响名字指针，
//     GC 因根 Pin 及其引用边不会回收解析链上的任何内容，因此批量不会读到
//     写了一半的内容，也不会出现"清单在、层被回收"的悬空引用。
func (s *Store) BatchResolve(ctx context.Context, ref string, selectors []*PlatformSelector) (*BatchResult, error) {
	// 阶段 1：发现。得到一个由摘要固定的内容定义快照；之后标签如何迁移都与本批无关。
	disc, err := s.DiscoverPlatforms(ctx, ref)
	if err != nil {
		return nil, err
	}

	// 阶段 2：在与 GC/发布互斥的窗口内确认根仍完好并登记 Pin。
	// 锁顺序与 GC 一致（gcmu -> pinmu），避免死锁。
	s.gcmu.Lock()
	ok, _ := s.hasLocked(disc.Root)
	if !ok {
		s.gcmu.Unlock()
		return nil, fmt.Errorf("%w: %s vanished before batch resolve", ErrNotExist, disc.Root)
	}
	s.pinmu.Lock()
	s.pins[disc.Root]++
	s.pinmu.Unlock()
	s.gcmu.Unlock()
	defer func() { _ = s.Unpin(disc.Root) }()

	// 阶段 3：按条件挑选，取并集并去重（保持发现顺序）。
	want := map[Digest]bool{}
	var order []Digest
	unmatched := []int{}
	if len(selectors) == 0 {
		for _, e := range disc.Entries {
			if !want[e.Descriptor.Digest] {
				want[e.Descriptor.Digest] = true
				order = append(order, e.Descriptor.Digest)
			}
		}
	} else {
		for ci, q := range selectors {
			hits := 0
			for _, e := range disc.Entries {
				if !q.empty() && !q.match(e.Platform) {
					continue
				}
				hits++
				if !want[e.Descriptor.Digest] {
					want[e.Descriptor.Digest] = true
					order = append(order, e.Descriptor.Digest)
				}
			}
			if hits == 0 {
				unmatched = append(unmatched, ci)
			}
		}
	}
	sort.Ints(unmatched)
	entryByDigest := map[Digest]PlatformEntry{}
	for _, e := range disc.Entries {
		if _, exists := entryByDigest[e.Descriptor.Digest]; !exists {
			entryByDigest[e.Descriptor.Digest] = e
		}
	}

	// 阶段 4：逐平台独立准备；任何一个失败都只记入该项，不影响其它平台。
	res := &BatchResult{Kind: disc.Kind, Root: disc.Root, Unmatched: unmatched}
	res.Images = make([]PlatformImage, 0, len(order))
	for _, d := range order {
		res.Images = append(res.Images, s.preparePlatform(entryByDigest[d]))
	}
	return res, nil
}

// preparePlatform 准备单个平台：校验子清单与其全部依赖，汇总层数与总大小。
// 所有失败都封装进返回值的 Err，由调用方按平台隔离，不向上中断整批。
func (s *Store) preparePlatform(entry PlatformEntry) PlatformImage {
	img := PlatformImage{Entry: entry, ManifestDescriptor: entry.Descriptor}

	data, err := s.blobData(entry.Descriptor.Digest)
	if err != nil {
		img.Err = fmt.Errorf("platform %s: read manifest: %w", entry.Platform, err)
		return img
	}
	isIndex, _, mani, err := parseManifest(entry.Descriptor.MediaType, data)
	if err != nil {
		img.Err = fmt.Errorf("platform %s: %w", entry.Platform, err)
		return img
	}
	if isIndex {
		img.Err = fmt.Errorf("platform %s: %w nested index at %s",
			entry.Platform, ErrMediaType, entry.Descriptor.Digest)
		return img
	}

	// 校验 config 与全部层存在且完好；缺一即该平台失败（不返回半成品）。
	for _, rd := range mani.referencedDigests() {
		if _, herr := s.blobData(rd); herr != nil {
			img.Err = fmt.Errorf("platform %s: referenced content %s: %w",
				entry.Platform, rd, herr)
			return img
		}
	}

	img.Manifest = mani
	img.Layers = mani.Layers
	img.LayerCount = len(mani.Layers)
	for _, l := range mani.Layers {
		img.TotalSize += l.Size
	}
	return img
}

// ResolveAllPlatforms 准备引用下的全部平台，等价于 BatchResolve 不传任何条件。
func (s *Store) ResolveAllPlatforms(ctx context.Context, ref string) (*BatchResult, error) {
	return s.BatchResolve(ctx, ref, nil)
}
