package ocistore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
)

// richArtifacts 聚合一个三平台富索引的构件。
type richArtifacts struct {
	indexD Digest
	plats  map[string]testArtifacts
	order  []string // 索引中平台的内容定义顺序
}

// buildRichIndex 构造 linux/amd64、linux/arm64/v8（带 osVersion/osFeatures/features）、
// linux/arm64/v7 三个平台的索引并打标签。
func buildRichIndex(t *testing.T, s *Store, tag string) richArtifacts {
	t.Helper()
	amd := buildManifest(t, s, "amd64")
	armv8 := buildManifest(t, s, "arm64-v8")
	armv7 := buildManifest(t, s, "arm64-v7")
	descs := []Descriptor{
		{MediaType: MediaTypeOCIManifest, Digest: amd.manifestD, Size: int64(len(amd.manifest)),
			Platform: &Platform{Architecture: "amd64", OS: "linux"}},
		{MediaType: MediaTypeOCIManifest, Digest: armv8.manifestD, Size: int64(len(armv8.manifest)),
			Platform: &Platform{Architecture: "arm64", OS: "linux", Variant: "v8",
				OSVersion: "8.0", OSFeatures: []string{"foo", "bar"}, Features: []string{"custom"}}},
		{MediaType: MediaTypeOCIManifest, Digest: armv7.manifestD, Size: int64(len(armv7.manifest)),
			Platform: &Platform{Architecture: "arm64", OS: "linux", Variant: "v7"}},
	}
	idx := &ImageIndex{SchemaVersion: 2, MediaType: MediaTypeOCIIndex, Manifests: descs}
	raw, err := json.Marshal(idx)
	if err != nil {
		t.Fatal(err)
	}
	idxD := putBytes(t, s, raw)
	if tag != "" {
		if err := s.PutTag(context.Background(), tag, idxD); err != nil {
			t.Fatal(err)
		}
	}
	return richArtifacts{indexD: idxD, order: []string{"amd64", "v8", "v7"},
		plats: map[string]testArtifacts{"amd64": amd, "v8": armv8, "v7": armv7}}
}

// TestDiscoverIndex：发现多架构索引，字段完整、顺序与索引内容定义一致、可复现。
func TestDiscoverIndex(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	rich := buildRichIndex(t, s, "multi:v1")
	t.Logf("input: tag multi:v1 -> index %s with %d entries in order %v",
		rich.indexD, len(rich.order), rich.order)

	disc, err := s.DiscoverPlatforms(context.Background(), "multi:v1")
	if err != nil {
		t.Fatal(err)
	}
	if disc.Kind != KindIndex || disc.Root != rich.indexD || len(disc.Entries) != 3 {
		t.Fatalf("judgement basis: want 3 index entries, got kind=%s root=%s n=%d",
			disc.Kind, disc.Root, len(disc.Entries))
	}
	// 顺序必须是内容定义顺序，且稳定可复现
	gotOrder := []string{}
	for _, e := range disc.Entries {
		switch {
		case e.Platform.Architecture == "amd64":
			gotOrder = append(gotOrder, "amd64")
		case e.Platform.Variant == "v8":
			gotOrder = append(gotOrder, "v8")
		case e.Platform.Variant == "v7":
			gotOrder = append(gotOrder, "v7")
		}
	}
	if fmt.Sprint(gotOrder) != fmt.Sprint(rich.order) {
		t.Fatalf("entry order = %v, want %v", gotOrder, rich.order)
	}
	disc2, _ := s.DiscoverPlatforms(context.Background(), rich.indexD.String())
	if fmt.Sprintf("%+v", disc.Entries) != fmt.Sprintf("%+v", disc2.Entries) {
		t.Fatalf("discovery not reproducible between tag and digest refs")
	}
	v8 := disc.Entries[1]
	if v8.Platform.OSVersion != "8.0" || len(v8.Platform.OSFeatures) != 2 ||
		v8.Platform.OSFeatures[0] != "foo" || v8.Platform.Features[0] != "custom" {
		t.Fatalf("optional fields lost: %+v", v8.Platform)
	}
	if v8.Descriptor.Digest != rich.plats["v8"].manifestD {
		t.Fatalf("entry descriptor digest mismatch")
	}
	t.Logf("final state: discovered %d platforms in stable order %v; optional fields on v8 preserved",
		len(disc.Entries), gotOrder)
}

// TestDiscoverSingleManifest：引用指向单平台镜像时给出统一结果，而不是模糊错误。
func TestDiscoverSingleManifest(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	a := buildManifest(t, s, "amd64")
	_ = s.PutTag(context.Background(), "img", a.manifestD)
	t.Logf("input: tag img -> single manifest %s (not an index)", a.manifestD)

	disc, err := s.DiscoverPlatforms(context.Background(), "img")
	if err != nil {
		t.Fatalf("single manifest must not be an error: %v", err)
	}
	if disc.Kind != KindManifest || len(disc.Entries) != 1 {
		t.Fatalf("judgement basis: want exactly 1 manifest-kind entry, got %+v", disc)
	}
	e := disc.Entries[0]
	if e.Index != 0 || e.Platform.Architecture != "" || e.Platform.OS != "" ||
		e.Platform.Variant != "" || e.Platform.OSVersion != "" ||
		len(e.Platform.OSFeatures) != 0 || len(e.Platform.Features) != 0 ||
		e.Descriptor.Digest != a.manifestD {
		t.Fatalf("single-manifest entry inconsistent: %+v", e)
	}
	t.Logf("final state: single manifest reported as KindManifest with one self-descriptor entry %s",
		e.Descriptor.Digest)
}

// TestDiscoverBadEntryDistinctFromNoMatch：索引条目摘要非法属于清单问题，
// 必须与"条件没筛中"分开。
func TestDiscoverBadEntryDistinctFromNoMatch(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()

	badIdx := []byte(`{"schemaVersion":2,"mediaType":"` + MediaTypeOCIIndex + `","manifests":[` +
		`{"mediaType":"` + MediaTypeOCIManifest + `","digest":"sha256:not-a-digest","size":1,` +
		`"platform":{"architecture":"amd64","os":"linux"}}]}`)
	idxD := putBytes(t, s, badIdx)
	t.Logf("input: index %s contains an entry with invalid digest", idxD)

	if _, err := s.DiscoverPlatforms(context.Background(), idxD.String()); !errors.Is(err, ErrPlatformBad) {
		t.Fatalf("want ErrPlatformBad for malformed index entry, got %v", err)
	}
	t.Logf("judgement basis: malformed index rejected with ErrPlatformBad (distinct from no-match)")
}

// TestSelectPartialConditions：只给部分条件时，未给维度不参与比较，候选多写信息不被排除。
func TestSelectPartialConditions(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	rich := buildRichIndex(t, s, "multi:v1")
	ctx := context.Background()
	t.Logf("input: index %s entries=[linux/amd64, linux/arm64/v8{osVer=8.0 os.features=[foo,bar] features=[custom]}, linux/arm64/v7]",
		rich.indexD)

	// 只给 OS：三个 linux 全部命中（v8 多写的 variant/osVersion/features 不构成排除理由）
	_, ms, err := s.SelectPlatforms(ctx, "multi:v1", NewPlatformSelector().WithOS("linux"))
	if err != nil || len(ms) != 3 {
		t.Fatalf("OS-only filter: n=%d err=%v, want 3", len(ms), err)
	}
	// 只给架构：v8 与 v7 都命中，顺序为内容定义顺序 v8 在前
	_, ms, _ = s.SelectPlatforms(ctx, "multi:v1", NewPlatformSelector().WithArchitecture("arm64"))
	if len(ms) != 2 || ms[0].Entry.Platform.Variant != "v8" || ms[1].Entry.Platform.Variant != "v7" {
		t.Fatalf("arch-only filter should hit v8,v7 in index order, got %+v", ms)
	}
	// 系统+架构+变体：精确命中 v7
	_, ms, _ = s.SelectPlatforms(ctx, "multi:v1",
		NewPlatformSelector().WithOS("linux").WithArchitecture("arm64").WithVariant("v7"))
	if len(ms) != 1 || ms[0].Entry.Platform.Variant != "v7" {
		t.Fatalf("full-condition filter should hit only v7, got %+v", ms)
	}
	// 显式要求 variant 为空：只命中 amd64，v8 不回退
	_, ms, _ = s.SelectPlatforms(ctx, "multi:v1",
		NewPlatformSelector().WithArchitecture("amd64").WithVariant(""))
	if len(ms) != 1 || ms[0].Entry.Platform.Architecture != "amd64" {
		t.Fatalf("empty-variant constraint should hit only amd64, got %+v", ms)
	}
	// 系统版本约束
	_, ms, _ = s.SelectPlatforms(ctx, "multi:v1", NewPlatformSelector().WithOSVersion("8.0"))
	if len(ms) != 1 || ms[0].Entry.Platform.Variant != "v8" {
		t.Fatalf("osVersion filter should hit only v8, got %+v", ms)
	}
	// 特性子集合：要求 foo 时，携带 [foo,bar] 的 v8 命中（额外特性不排除）
	_, ms, _ = s.SelectPlatforms(ctx, "multi:v1", NewPlatformSelector().WithOSFeatures("foo"))
	if len(ms) != 1 || ms[0].Entry.Platform.Variant != "v8" {
		t.Fatalf("os.features subset filter should hit v8, got %+v", ms)
	}
	// 要求候选没有的特性：零命中但不是错误
	_, ms, err = s.SelectPlatforms(ctx, "multi:v1", NewPlatformSelector().WithOSFeatures("missing"))
	if err != nil || len(ms) != 0 {
		t.Fatalf("missing feature: want empty matches + nil err, got n=%d err=%v", len(ms), err)
	}
	// 附加特性 features 维度同理
	_, ms, _ = s.SelectPlatforms(ctx, "multi:v1", NewPlatformSelector().WithFeatures("custom"))
	if len(ms) != 1 || ms[0].Entry.Platform.Variant != "v8" {
		t.Fatalf("features subset filter should hit v8, got %+v", ms)
	}
	t.Logf("final state: partial conditions judged only on set dimensions; extras never excluded; no-match is empty+nil")
}

// TestSelectMultiHitDeterministic：多个命中时按内容定义顺序全部返回，不随机二选一。
func TestSelectMultiHitDeterministic(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	rich := buildRichIndex(t, s, "multi:v1")

	_, ms, err := s.SelectPlatforms(context.Background(), "multi:v1", NewPlatformSelector())
	if err != nil || len(ms) != 3 {
		t.Fatalf("empty selector must match all, n=%d err=%v", len(ms), err)
	}
	wantDigests := []Digest{
		rich.plats["amd64"].manifestD, rich.plats["v8"].manifestD, rich.plats["v7"].manifestD,
	}
	for i, m := range ms {
		if m.Entry.Index != i || m.Entry.Descriptor.Digest != wantDigests[i] {
			t.Fatalf("judgement basis: match order not deterministic at %d: %+v", i, m)
		}
	}
	t.Logf("final state: %d hits returned in index order %v", len(ms), wantDigests)
}

// TestResolveAllPlatformsBatch：批量准备全部平台，汇总层数与总大小。
func TestResolveAllPlatformsBatch(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	rich := buildRichIndex(t, s, "multi:v1")
	t.Logf("input: resolve all platforms of index %s", rich.indexD)

	res, err := s.ResolveAllPlatforms(context.Background(), "multi:v1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != KindIndex || len(res.Images) != 3 || len(res.Unmatched) != 0 {
		t.Fatalf("judgement basis: want 3 images, got %+v", res)
	}
	sizeByVariant := map[string]int64{}
	for _, img := range res.Images {
		if img.Err != nil {
			t.Fatalf("platform %s unexpectedly failed: %v", img.Entry.Platform, img.Err)
		}
		if img.LayerCount != 1 {
			t.Fatalf("platform %s layer count = %d, want 1", img.Entry.Platform, img.LayerCount)
		}
		key := img.Entry.Platform.Architecture + "/" + img.Entry.Platform.Variant
		sizeByVariant[key] = img.TotalSize
	}
	for k, art := range map[string]testArtifacts{
		"amd64/": rich.plats["amd64"], "arm64/v8": rich.plats["v8"], "arm64/v7": rich.plats["v7"]} {
		if sizeByVariant[k] != int64(len(art.layer)) {
			t.Fatalf("total size for %s = %d, want %d", k, sizeByVariant[k], len(art.layer))
		}
	}
	t.Logf("final state: 3 platforms ready, each layers=1, sizes=%v; none failed", sizeByVariant)
}

// TestBatchSelectorsDedupAndUnmatched：条件并集去重；没命中的条件下标与平台失败分开报告。
func TestBatchSelectorsDedupAndUnmatched(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	rich := buildRichIndex(t, s, "multi:v1")
	t.Logf("input: selectors [linux, linux, arm64-v7, windows/amd64] against %s", rich.indexD)

	conds := []*PlatformSelector{
		NewPlatformSelector().WithOS("linux"), // 命中全部
		NewPlatformSelector().WithOS("linux"), // 重复条件：同一平台只准备一次
		NewPlatformSelector().WithOS("linux").WithArchitecture("arm64").WithVariant("v7"),
		NewPlatformSelector().WithOS("windows"), // 零命中：进入 Unmatched
	}
	res, err := s.BatchResolve(context.Background(), "multi:v1", conds)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Images) != 3 {
		t.Fatalf("union should dedup to 3 platforms, got %d", len(res.Images))
	}
	if fmt.Sprint(res.Unmatched) != "[3]" {
		t.Fatalf("judgement basis: unmatched = %v, want [3]", res.Unmatched)
	}
	for _, img := range res.Images {
		if img.Err != nil {
			t.Fatalf("platform %s should be ready: %v", img.Entry.Platform, img.Err)
		}
	}
	// 批量结束后 Pin 必须释放，不泄漏保护计数
	s.pinmu.Lock()
	np := len(s.pins)
	s.pinmu.Unlock()
	if np != 0 {
		t.Fatalf("pins leaked after batch: %d remain", np)
	}
	t.Logf("final state: %d distinct platforms prepared once each; unmatched=%v; pins released",
		len(res.Images), res.Unmatched)
}

// TestBatchPartialPlatformFailure：某个平台内容损坏只失败该项，其它平台照常，无半成品。
func TestBatchPartialPlatformFailure(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	rich := buildRichIndex(t, s, "multi:v1")
	t.Logf("input: tamper v8 child manifest %s, then resolve all of %s",
		rich.plats["v8"].manifestD, rich.indexD)
	corruptBlobOnDisk(t, dir, rich.plats["v8"].manifestD)

	res, err := s.ResolveAllPlatforms(context.Background(), "multi:v1")
	if err != nil {
		t.Fatalf("batch itself must not abort for one bad platform: %v", err)
	}
	if len(res.Images) != 3 {
		t.Fatalf("want 3 per-platform results, got %d", len(res.Images))
	}
	status := map[string]error{}
	for _, img := range res.Images {
		key := img.Entry.Platform.Architecture + "/" + img.Entry.Platform.Variant
		status[key] = img.Err
		if img.Err != nil && img.Manifest != nil {
			t.Fatalf("failed platform %s must not expose a half-prepared manifest", key)
		}
	}
	if !errors.Is(status["arm64/v8"], ErrCorrupted) {
		t.Fatalf("v8 should fail with ErrCorrupted, got %v", status["arm64/v8"])
	}
	if status["amd64/"] != nil || status["arm64/v7"] != nil {
		t.Fatalf("other platforms must not be affected: %v", status)
	}
	t.Logf("final state: per-platform status amd64=ok v7=ok v8=%v; no partial manifests exposed",
		status["arm64/v8"])
}

// TestBatchSingleManifest：批量接口对单平台镜像同样给出一个完整结果。
func TestBatchSingleManifest(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	a := buildManifest(t, s, "amd64")
	_ = s.PutTag(context.Background(), "img", a.manifestD)

	res, err := s.ResolveAllPlatforms(context.Background(), "img")
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != KindManifest || len(res.Images) != 1 || res.Images[0].Err != nil {
		t.Fatalf("single-manifest batch inconsistent: %+v", res)
	}
	img := res.Images[0]
	if img.LayerCount != 1 || img.TotalSize != int64(len(a.layer)) ||
		img.ManifestDescriptor.Digest != a.manifestD {
		t.Fatalf("single-manifest image summary wrong: %+v", img)
	}
	t.Logf("final state: single manifest batch -> 1 ready platform layers=%d size=%d",
		img.LayerCount, img.TotalSize)
}

// TestBatchConcurrentTagMigrateAndGC：批量解析与标签迁移、删除、GC 真并发：
// 成功的批量必须每个成功平台都自洽完整；失败必须是可区分的根级错误；
// 不允许"清单在、层被回收"的悬空结果。
func TestBatchConcurrentTagMigrateAndGC(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	ctx := context.Background()

	const rounds = 40
	var wg sync.WaitGroup
	bad := make(chan error, rounds)

	for round := 0; round < rounds; round++ {
		tag := "batch-moving-" + itoa(round)
		rich := buildRichIndex(t, s, tag)
		other := buildRichIndex(t, s, "")

		// 迁移者：在两个索引之间反复迁移标签，同时删除 + GC
		wg.Add(1)
		go func() {
			defer wg.Done()
			targets := []Digest{rich.indexD, other.indexD}
			for i := 0; i < 20; i++ {
				_ = s.PutTag(ctx, tag, targets[i%2])
			}
			_ = s.DeleteTag(ctx, tag)
			if _, err := s.GC(ctx); err != nil {
				bad <- err
			}
		}()

		// 批量解析者
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.ResolveAllPlatforms(ctx, tag)
			if err != nil {
				// 根级失败必须是可区分的明确原因（标签已删/内容不存在/损坏）
				if errors.Is(err, ErrLabelNotExist) || errors.Is(err, ErrNotExist) ||
					errors.Is(err, ErrCorrupted) {
					return
				}
				bad <- fmt.Errorf("batch root error not classifiable: %w", err)
				return
			}
			for _, img := range res.Images {
				if img.Err != nil {
					// 平台级失败同样必须可区分；失败项不得带半成品
					if !(errors.Is(img.Err, ErrNotExist) || errors.Is(img.Err, ErrCorrupted) ||
						errors.Is(img.Err, ErrMediaType) || errors.Is(img.Err, ErrManifestBadJSON)) {
						bad <- fmt.Errorf("platform error not classifiable: %w", img.Err)
					}
					if img.Manifest != nil {
						bad <- fmt.Errorf("half-prepared platform exposed: %s", img.Entry.Platform)
					}
					continue
				}
				if img.LayerCount != len(img.Manifest.Layers) {
					bad <- fmt.Errorf("layer summary inconsistent for %s", img.Entry.Platform)
					continue
				}
				// 成功项的 config 与每一层必须仍然可读：悬空引用不允许
				if _, gerr := s.blobData(img.Manifest.Config.Digest); gerr != nil {
					bad <- fmt.Errorf("config of resolved %s gone: %w", img.Entry.Platform, gerr)
				}
				for _, l := range img.Layers {
					if _, gerr := s.blobData(l.Digest); gerr != nil {
						bad <- fmt.Errorf("layer of resolved %s gone: %w", img.Entry.Platform, gerr)
					}
				}
			}
		}()
		wg.Wait()
	}
	close(bad)
	for e := range bad {
		t.Fatalf("race violation: %v", e)
	}
	st, _ := s.Stats()
	t.Logf("final state: %d rounds batch-vs-migrate/delete/GC completed without dangling refs; blobs=%d",
		rounds, st.BlobCount)
}

// TestBatchPinnedContentNotCollected：标签已删、GC 与批量并发时，
// 正在解析（已 Pin）的内容不得被回收，批量必须完整成功。
func TestBatchPinnedContentNotCollected(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	ctx := context.Background()

	rich := buildRichIndex(t, s, "multi:v1")
	if err := s.DeleteTag(ctx, "multi:v1"); err != nil {
		t.Fatal(err)
	}
	// 模拟拉取协议：在启动 GC 之前先 Pin 住根，整段子图据此得到保护。
	if err := s.Pin(rich.indexD); err != nil {
		t.Fatal(err)
	}
	t.Logf("input: tag deleted, index %s kept alive solely by the in-flight root pin", rich.indexD)

	var wg sync.WaitGroup
	bad := make(chan error, 4)
	done := make(chan struct{})
	wg.Add(2)
	go func() { // 批量进行期间连续 GC：不得误删 Pin 保护中的解析链
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			if _, err := s.GC(ctx); err != nil {
				bad <- err
				return
			}
			runtime.Gosched() // 让出 CPU，给批量 goroutine 推进机会
		}
	}()
	go func() {
		defer wg.Done()
		defer close(done) // 批量一结束就停止 GC：本测试只保证"进行中"不被回收
		res, err := s.BatchResolve(ctx, rich.indexD.String(),
			[]*PlatformSelector{NewPlatformSelector().WithArchitecture("arm64")})
		if err != nil {
			bad <- err
			return
		}
		if len(res.Images) != 2 { // v8 + v7
			bad <- fmt.Errorf("want 2 arm64 platforms, got %d", len(res.Images))
			return
		}
		for _, img := range res.Images {
			if img.Err != nil {
				bad <- fmt.Errorf("pinned platform %s failed: %w", img.Entry.Platform, img.Err)
			}
		}
	}()
	wg.Wait()
	close(bad)
	for e := range bad {
		t.Fatalf("pin/GC guarantee violated: %v", e)
	}
	// 批量期间整图仅靠根 Pin 存活、GC 全程紧逼却完整成功，证明解析链在"进行中"
	// 未被回收；释放 Pin 后再 GC，整图应可被彻底回收，反证保护确实来自 Pin。
	for _, art := range rich.plats {
		if ok, _ := s.Has(art.manifestD); !ok {
			t.Fatalf("pinned graph must survive GC while batch in flight: %s missing", art.manifestD)
		}
	}
	if err := s.Unpin(rich.indexD); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GC(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Stats(); st.BlobCount != 0 {
		t.Fatalf("after unpin the tagless graph should be fully reclaimed, blobs=%d", st.BlobCount)
	}
	t.Logf("final state: pinned batch survived hammering GC; unpin+GC reclaimed all blobs")
}
