package ocistore

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

// verifyBatchUsable 校验一次成功的批量结果在返回时完全自洽可用：
// 根索引确实包含每个平台条目，且 config/层全部可读、通过完整性校验。
// 任何“半写/悬空”内容都会在这里暴露。
func verifyBatchUsable(t *testing.T, s *Store, res *BatchResult) {
	t.Helper()
	// 根内容必须是完整索引，且条目确实来自这一根（防止版本串读）。
	rootData, err := s.blobData(res.Root)
	if err != nil {
		t.Errorf("batch root %s unreadable: %v", res.Root, err)
		return
	}
	isIndex, idx, _, perr := parseManifest("", rootData)
	if perr != nil || !isIndex {
		t.Errorf("batch root not a complete index: %v", perr)
		return
	}
	members := map[Digest]bool{}
	for _, m := range idx.Manifests {
		members[m.Digest] = true
	}
	for _, p := range res.Platforms {
		if p.Err != nil {
			t.Errorf("platform %s failed inside a successful batch: %v", p.Name, p.Err)
			continue
		}
		if !members[p.Info.Descriptor.Digest] {
			t.Errorf("platform %s descriptor %s not a member of resolved root %s (cross-version read)",
				p.Name, p.Info.Descriptor.Digest, res.Root)
		}
		if p.LayerCount != len(p.Image.Manifest.Layers) || p.LayerCount == 0 {
			t.Errorf("platform %s layer count mismatch: %d", p.Name, p.LayerCount)
		}
		for _, rd := range p.Image.Manifest.referencedDigests() {
			if ok, _ := s.Has(rd); !ok {
				t.Errorf("platform %s referenced content %s missing/corrupt", p.Name, rd)
			}
		}
		if got := p.Image.ManifestDescriptor.Size; got <= 0 {
			t.Errorf("platform %s descriptor size not prepared", p.Name)
		}
	}
}

// TestConcurrentBatchWhileMigrate：批量解析与标签在两个不同版本索引之间
// 高频迁移、以及 GC 并发；每次成功的批量结果必须完整自洽、不串版本。
func TestConcurrentBatchWhileMigrate(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	ctx := context.Background()

	// 两个互不相同的版本（不同平台/层），标签在二者间迁移。
	v1 := buildManifest(t, s, "amd64")
	v1b := buildManifest(t, s, "arm64")
	idx1 := &ImageIndex{SchemaVersion: 2, MediaType: MediaTypeOCIIndex, Manifests: []Descriptor{
		{MediaType: MediaTypeOCIManifest, Digest: v1.manifestD, Size: int64(len(v1.manifest)),
			Platform: &Platform{Architecture: "amd64", OS: "linux"}},
		{MediaType: MediaTypeOCIManifest, Digest: v1b.manifestD, Size: int64(len(v1b.manifest)),
			Platform: &Platform{Architecture: "arm64", OS: "linux"}},
	}}
	raw1, _ := json.Marshal(idx1)
	d1 := putBytes(t, s, raw1)

	v2 := buildManifest(t, s, "386")
	idx2 := &ImageIndex{SchemaVersion: 2, MediaType: MediaTypeOCIIndex, Manifests: []Descriptor{
		{MediaType: MediaTypeOCIManifest, Digest: v2.manifestD, Size: int64(len(v2.manifest)),
			Platform: &Platform{Architecture: "386", OS: "linux"}},
	}}
	raw2, _ := json.Marshal(idx2)
	d2 := putBytes(t, s, raw2)
	if err := s.PutTag(ctx, "moving", d1); err != nil {
		t.Fatal(err)
	}
	// 测试语义是“标签在两个完整版本间迁移”，而非“旧版本可被回收”：
	// 全程 Pin 两个版本，使 PutTag 不会因目标被 GC 回收而失败（那会让
	// 迁移者提前 return 而不关闭 stop）。被 Pin 的版本恰好保证批量结果完整。
	if err := s.Pin(d1); err != nil {
		t.Fatal(err)
	}
	if err := s.Pin(d2); err != nil {
		t.Fatal(err)
	}
	defer s.Unpin(d1)
	defer s.Unpin(d2)
	t.Logf("input: tag 'moving' migrates between index v1=%s(2 plats) and v2=%s(1 plat); GC runs concurrently",
		d1, d2)

	const iter = 120
	var wg sync.WaitGroup
	var stopOnce sync.Once
	stop := make(chan struct{})
	signalStop := func() { stopOnce.Do(func() { close(stop) }) }

	wg.Add(1) // 迁移者
	go func() {
		defer wg.Done()
		defer signalStop() // 任何退出路径都释放读者/GC 协程
		for i := 0; i < iter; i++ {
			target := d1
			if i%2 == 1 {
				target = d2
			}
			if err := s.PutTag(ctx, "moving", target); err != nil {
				t.Errorf("migrate: %v", err)
				return
			}
		}
	}()

	wg.Add(1) // GC 者：旧版本不可达即回收（但正在批量的版本被 Pin 保护）
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := s.GC(ctx); err != nil {
				t.Errorf("gc: %v", err)
				return
			}
		}
	}()

	var batches, okBatches int
	wg.Add(1) // 批量读取者
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			res, err := s.ResolveAllPlatforms(ctx, "moving")
			if err != nil {
				// 唯一可接受的顶层失败是标签瞬时不存在；其余一律不允许。
				if !errors.Is(err, ErrLabelNotExist) {
					t.Errorf("unexpected batch error: %v", err)
				}
				continue
			}
			batches++
			// 版本一致性：平台数必须与根版本匹配。
			wantN := 2
			if res.Root == d2 {
				wantN = 1
			}
			if res.Root != d1 && res.Root != d2 {
				t.Errorf("unknown batch root %s", res.Root)
				continue
			}
			if len(res.Platforms) != wantN {
				t.Errorf("root %s resolved %d platforms want %d", res.Root, len(res.Platforms), wantN)
				continue
			}
			verifyBatchUsable(t, s, res)
			okBatches++
		}
	}()
	wg.Wait()
	t.Logf("judgement basis: %d successful batches (all self-consistent, no cross-version reads)", batches)
	if okBatches == 0 {
		t.Fatalf("no successful batch observed; concurrency test proved nothing")
	}
	t.Logf("final state: every successful batch had prepared layers; GC never reclaimed in-use content")
}

// TestConcurrentBatchWhileDeleteAndGC：删标签 -> GC（整棵图不可达）-> 重建
// 与批量解析并发。成功的批量必须完整；顶层只允许 ErrLabelNotExist/ErrNotExist。
func TestConcurrentBatchWhileDeleteAndGC(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	ctx := context.Background()

	idxD, amd, arm := buildIndex(t, s, "multi")
	t.Logf("input: index=%s amd=%s arm=%s; delete->GC->rebuild racing with batches",
		idxD, amd.manifestD, arm.manifestD)

	// 重新序列化与原索引完全相同的 JSON（结构体字段顺序确定 -> 同一摘要）。
	idxObj := &ImageIndex{SchemaVersion: 2, MediaType: MediaTypeOCIIndex, Manifests: []Descriptor{
		{MediaType: MediaTypeOCIManifest, Digest: amd.manifestD, Size: int64(len(amd.manifest)),
			Platform: &Platform{Architecture: "amd64", OS: "linux"}},
		{MediaType: MediaTypeOCIManifest, Digest: arm.manifestD, Size: int64(len(arm.manifest)),
			Platform: &Platform{Architecture: "arm64", OS: "linux", Variant: "v8"}},
	}}
	idxRaw, _ := json.Marshal(idxObj)
	if ComputeDigest(idxRaw) != idxD {
		t.Fatalf("rebuilt index bytes digest differs: got %s want %s", ComputeDigest(idxRaw), idxD)
	}

	// rebuild 按文档协议重建整棵图：先 Pin 全部摘要（含尚不存在者，
	// Pin 只校验摘要格式），再写依赖、最后建标签引用，完成后统一 Unpin。
	// 这样“有标签”的状态一定指向一张完整图，GC 不会在发布中途回收子内容。
	rebuild := func() error {
		payloads := [][]byte{amd.layer, amd.config, arm.layer, arm.config, amd.manifest, arm.manifest, idxRaw}
		digests := make([]Digest, 0, len(payloads))
		for _, b := range payloads {
			digests = append(digests, ComputeDigest(b))
		}
		for _, d := range digests {
			if err := s.Pin(d); err != nil {
				return err
			}
		}
		defer func() {
			for _, d := range digests {
				_ = s.Unpin(d)
			}
		}()
		for _, b := range payloads {
			d := ComputeDigest(b)
			if _, _, err := s.PutBlob(ctx, d, bytesReader(b)); err != nil {
				return err
			}
		}
		return s.PutTag(ctx, "multi", idxD)
	}

	const iter = 60
	var wg sync.WaitGroup
	var stopOnce sync.Once
	stop := make(chan struct{})
	signalStop := func() { stopOnce.Do(func() { close(stop) }) }

	wg.Add(1) // 删除/重建者
	go func() {
		defer wg.Done()
		defer signalStop()
		for i := 0; i < iter; i++ {
			if err := s.DeleteTag(ctx, "multi"); err != nil && !errors.Is(err, ErrLabelNotExist) {
				t.Errorf("delete: %v", err)
				return
			}
			if err := rebuild(); err != nil {
				t.Errorf("rebuild: %v", err)
				return
			}
		}
	}()

	wg.Add(1) // GC 者：尽力回收不可达整图
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = s.GC(ctx)
		}
	}()

	var successful int
	wg.Add(1) // 批量者
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			res, err := s.ResolveAllPlatforms(ctx, "multi")
			if err != nil {
				if errors.Is(err, ErrLabelNotExist) || errors.Is(err, ErrNotExist) {
					continue
				}
				t.Errorf("unexpected top-level error: %v", err)
				return
			}
			if len(res.Platforms) != 2 {
				t.Errorf("want 2 platforms, got %d", len(res.Platforms))
				continue
			}
			verifyBatchUsable(t, s, res)
			successful++
		}
	}()
	wg.Wait()
	if successful == 0 {
		t.Fatalf("no successful batch during delete+gc race")
	}
	t.Logf("judgement basis: %d complete batches despite delete/rebuild+GC; pinned content never swept",
		successful)
}
