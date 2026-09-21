package ocistore

import (
	"context"
	"fmt"
	"io"
)

// ResolvedManifest 是索引解析到具体平台后的结果。
type ResolvedManifest struct {
	// Manifest 是平台清单描述符；Index 是来源索引（直接给清单时为 nil）。
	Manifest Descriptor
	Index    *Descriptor
	Platform Platform
	Config   Descriptor
	Layers   []Descriptor
	// data 是平台清单原文。
	data []byte
	// lease 固定解析链路上的内容，Release 后解除。
	lease *Lease
}

// Data 返回平台清单原文。
func (r *ResolvedManifest) Data() []byte { return r.data }

// Release 释放解析期间持有的 GC 保护租约。
// 调用方在完整拉取（config/layers）期间应继续持有，读完再释放。
func (r *ResolvedManifest) Release() {
	if r.lease != nil {
		r.lease.Release()
	}
}

// loadAndVerify 读取一个 blob 并在返回前完整校验：
//   - 实际摘要必须等于声明摘要（DigestMismatchError）；
//   - 实际长度必须等于声明长度（截断 -> DigestMismatchError，带 op）；
//   - JSON 截断/非法 -> ErrManifestTruncated；
//
// 校验通过的内容 Pin 到租约，防止后续步骤被 GC 回收。
func (s *Store) loadAndVerify(ctx context.Context, lease *Lease, op string, declared Descriptor) ([]byte, *rawManifest, error) {
	if err := declared.Digest.validate(); err != nil {
		return nil, nil, err
	}
	rc, stored, err := s.GetBlob(ctx, declared.Digest)
	if err != nil {
		return nil, nil, err
	}
	buf, rerr := io.ReadAll(rc)
	// 立即关闭并释放其 inflight 门闩：必须在随后的 lease.Pin（会再次
	// beginOp）之前完成，否则 GC 排空时会出现“本 goroutine 持有计数、
	// 又等待排空结束去 Pin”的自死锁。
	cerr := rc.Close()
	if rerr != nil {
		// verifyReader 在 EOF 上报的摘要不一致等错误。
		return nil, nil, rerr
	}
	if cerr != nil {
		return nil, nil, cerr
	}

	// 长度截断判定：声明长度与磁盘实际长度不符，显式拒绝。
	if declared.Size > 0 && int64(len(buf)) != declared.Size {
		s.quarantine(declared.Digest)
		return nil, nil, &DigestMismatchError{Op: op, Declared: declared.Digest, Actual: digestBytes(buf)}
	}
	// 即便声明 Size 为 0，也要复核磁盘摘要（GetBlob 的 EOF 校验已做一次）。
	if actual := digestBytes(buf); actual != declared.Digest {
		s.quarantine(declared.Digest)
		return nil, nil, &DigestMismatchError{Op: op, Declared: declared.Digest, Actual: actual}
	}

	if lease != nil {
		if err := lease.Pin(ctx, declared.Digest); err != nil {
			return nil, nil, err
		}
	}

	media := declared.MediaType
	if media == "" {
		media = stored.MediaType
	}
	rm, err := parseManifest(buf, media)
	if err != nil {
		return nil, nil, err
	}
	return buf, rm, nil
}

// digestBytes 计算字节内容的 sha256 摘要。
func digestBytes(b []byte) Digest {
	h, _ := newHash(defaultAlgorithm)
	h.Write(b)
	return digestFromHash(defaultAlgorithm, h)
}

// ResolvePlatform 从标签/摘要出发，把索引解析为 p 对应的具体平台清单。
//
// 失败原因（可 errors.Is 区分）：
//   - ErrPlatformNotFound：没有任何条目满足请求平台，绝不静默回退其它平台；
//   - ErrDigestMismatch：子清单声明摘要/长度与实际内容不符（截断）；
//   - ErrManifestTruncated：清单 JSON 截断或结构非法；
//   - ErrUnsupportedMediaType：索引/清单媒体类型不受支持。
func (s *Store) ResolvePlatform(ctx context.Context, ref string, p Platform) (*ResolvedManifest, error) {
	lease := s.NewLease()
	resolved, err := s.resolveWithLease(ctx, lease, ref, p)
	if err != nil {
		lease.Release()
		return nil, err
	}
	resolved.lease = lease
	return resolved, nil
}

func (s *Store) resolveWithLease(ctx context.Context, lease *Lease, ref string, want Platform) (*ResolvedManifest, error) {
	// 1) 解析起点：标签或摘要。
	var rootDesc Descriptor
	if isDigestRef(ref) {
		d := Digest(ref)
		desc, err := s.StatBlob(ctx, d)
		if err != nil {
			return nil, err
		}
		rootDesc = desc
		rootDesc.Digest = d
	} else {
		desc, err := s.ResolveTag(ctx, ref)
		if err != nil {
			return nil, err
		}
		rootDesc = desc
	}

	rootData, rootRM, err := s.loadAndVerify(ctx, lease, "resolve.root", rootDesc)
	if err != nil {
		return nil, err
	}
	_ = rootData

	// 2) 起点可能直接就是单平台清单。
	if rootRM.kind == kindImage {
		return &ResolvedManifest{
			Manifest: rootDesc,
			Platform: want,
			Config:   rootRM.image.Config,
			Layers:   append([]Descriptor(nil), rootRM.image.Layers...),
			data:     rootData,
		}, nil
	}

	// 3) 索引：按平台精确选择，收集可用平台用于错误信息。
	var available []Platform
	var chosen *Descriptor
	for i := range rootRM.index.Manifests {
		m := rootRM.index.Manifests[i]
		if m.Platform == nil {
			available = append(available, Platform{})
			continue
		}
		available = append(available, *m.Platform)
		if chosen == nil && matchPlatform(want, *m.Platform) {
			match := m
			chosen = &match
		}
	}
	if chosen == nil {
		return nil, &PlatformNotFoundError{Wanted: want, Available: available}
	}

	// 4) 拉取并严格校验选中的平台清单（不回退到其它候选）。
	childData, childRM, err := s.loadAndVerify(ctx, lease, "resolve.child", *chosen)
	if err != nil {
		return nil, err
	}
	if childRM.kind != kindImage {
		return nil, fmt.Errorf("%w: platform entry %s is not an image manifest",
			ErrManifestTruncated, chosen.Digest)
	}

	idxDesc := rootDesc
	return &ResolvedManifest{
		Manifest: *chosen,
		Index:    &idxDesc,
		Platform: *chosen.Platform,
		Config:   childRM.image.Config,
		Layers:   append([]Descriptor(nil), childRM.image.Layers...),
		data:     childData,
	}, nil
}
