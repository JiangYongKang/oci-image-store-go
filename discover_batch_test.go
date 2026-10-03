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
	sigD   Digest // 无 platform 的签名附属条目（不是平台）
	plats  map[string]testArtifacts
	order  []string // 索引中平台的内容定义顺序
}

// buildRichIndex 构造 linux/amd64、linux/arm64/v8（带 osVersion/osFeatures/features）、
// linux/arm64/v7 三个平台的索引，并在末尾挂一个没有 platform 的签名附属条目后打标签。
func buildRichIndex(t *testing.T, s *Store, tag string) richArtifacts {
	t.Helper()
	amd := buildManifest(t, s, "amd64")
	armv8 := buildManifest(t, s, "arm64-v8")
	armv7 := buildManifest(t, s, "arm64-v7")
	// 附属条目：签名/证明材料一类内容，没有 platform，不应被当成平台。
	sig := []byte(`{"payload":"fake-signature-payload-for-attached-entry"}`)
	sigD := putBytes(t, s, sig)
	descs := []Descriptor{
		{MediaType: MediaTypeOCIManifest, Digest: amd.manifestD, Size: int64(len(amd.manifest)),
			Platform: &Platform{Architecture: "amd64", OS: "linux"}},
		{MediaType: MediaTypeOCIManifest, Digest: armv8.manifestD, Size: int64(len(armv8.manifest)),
			Platform: &Platform{Architecture: "arm64", OS: "linux", Variant: "v8",
				OSVersion: "8.0", OSFeatures: []string{"foo", "bar"}, Features: []string{"custom"}}},
		{MediaType: MediaTypeOCIManifest, Digest: armv7.manifestD, Size: int64(len(armv7.manifest)),
			Platform: &Platform{Architecture: "arm64", OS: "linux", Variant: "v7"}},
		// 无 platform：附属条目，必须出现在 Attached 而不是平台列表。
		{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: sigD, Size: int64(len(sig)),
			Annotations: map[string]string{"vnd.docker.reference.type": "attestation-manifest"}},
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
	return richArtifacts{indexD: idxD, sigD: sigD, order: []string{"amd64", "v8", "v7"},
		plats: map[string]testArtifacts{"amd64": amd, "v8": armv8, "v7": armv7}}
}

// TestDiscoverIndex：发现多架构索引，字段完整、顺序与索引内容定义一致、可复现。
func TestDiscoverIndex(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	rich := buildRichIndex(t, s, "multi:v1")
	t.Logf("input: tag multi:v1 -> index %s with %d platform entries in order %v plus 1 platform-less signature %s",
		rich.indexD, len(rich.order), rich.order, rich.sigD)

	disc, err := s.DiscoverPlatforms(context.Background(), "multi:v1")
	if err != nil {
		t.Fatal(err)
	}
	if disc.Kind != KindIndex || disc.Root != rich.indexD || len(disc.Entries) != 3 {
		t.Fatalf("judgement basis: want 3 platform entries, got kind=%s root=%s n=%d",
			disc.Kind, disc.Root, len(disc.Entries))
	}
	// 无 platform 的签名条目不得混进平台列表，而是单列在 Attached
	if len(disc.Attached) != 1 {
		t.Fatalf("judgement basis: want exactly 1 attached entry, got %d: %+v",
			len(disc.Attached), disc.Attached)
	}
	if disc.Attached[0].Descriptor.Digest != rich.sigD || disc.Attached[0].Index != 3 {
		t.Fatalf("attached entry misreported: %+v", disc.Attached[0])
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
	t.Logf("final state: discovered %d platforms in stable order %v; attached=%v excluded from platforms; optional fields on v8 preserved",
		len(disc.Entries), gotOrder, rich.sigD)
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

// TestSelectPartialConditions：只给部分条件时，未给维度不参与比较（硬过滤），
// 多候选由接口按固定规则直接择优选出唯一一个；没筛中是 nil+nil 而不是错误。
func TestSelectPartialConditions(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	rich := buildRichIndex(t, s, "multi:v1")
	ctx := context.Background()
	t.Logf("input: index %s platforms=[linux/amd64, linux/arm64/v8{osVer=8.0 os.features=[foo,bar] features=[custom]}, linux/arm64/v7], plus 1 platform-less signature",
		rich.indexD)

	selectOne := func(q *PlatformSelector) *PlatformMatch {
		t.Helper()
		_, m, err := s.SelectPlatform(ctx, "multi:v1", q)
		if err != nil {
			t.Fatalf("select with %#v: unexpected error %v", q, err)
		}
		return m
	}
	keyOf := func(m *PlatformMatch) string {
		if m == nil {
			return "<none>"
		}
		return m.Entry.Platform.Architecture + "/" + m.Entry.Platform.Variant
	}

	// 只给架构：v8 与 v7 都过硬过滤；arm64 的惯例默认变体是 v8 -> 选 v8
	if got := keyOf(selectOne(NewPlatformSelector().WithArchitecture("arm64"))); got != "arm64/v8" {
		t.Fatalf("judgement basis: arch-only pick = %s, want arm64/v8 (default variant)", got)
	}
	// 只给系统：linux 三平台都过；amd64 无默认变体且最精简 -> 选 amd64
	if got := keyOf(selectOne(NewPlatformSelector().WithOS("linux"))); got != "amd64/" {
		t.Fatalf("judgement basis: os-only pick = %s, want amd64/ (least specific)", got)
	}
	// 系统+架构+变体：精确命中 v7（显式变体约束是硬条件，不做默认值替换）
	if got := keyOf(selectOne(NewPlatformSelector().WithOS("linux").WithArchitecture("arm64").WithVariant("v7"))); got != "arm64/v7" {
		t.Fatalf("full-condition filter should pick v7, got %s", got)
	}
	// 显式要求 variant 为空：只命中 amd64，v8 不回退
	if got := keyOf(selectOne(NewPlatformSelector().WithArchitecture("amd64").WithVariant(""))); got != "amd64/" {
		t.Fatalf("empty-variant constraint should pick amd64, got %s", got)
	}
	// 系统版本约束：唯一过过滤的是 v8
	if got := keyOf(selectOne(NewPlatformSelector().WithOSVersion("8.0"))); got != "arm64/v8" {
		t.Fatalf("osVersion filter should pick v8, got %s", got)
	}
	// 特性子集合：要求 foo 时，携带 [foo,bar] 的 v8 命中（额外特性不排除）
	if got := keyOf(selectOne(NewPlatformSelector().WithOSFeatures("foo"))); got != "arm64/v8" {
		t.Fatalf("os.features subset filter should pick v8, got %s", got)
	}
	// 附加特性 features 维度同理
	if got := keyOf(selectOne(NewPlatformSelector().WithFeatures("custom"))); got != "arm64/v8" {
		t.Fatalf("features subset filter should pick v8, got %s", got)
	}
	// 要求候选没有的特性：零命中但不是错误
	_, m, err := s.SelectPlatform(ctx, "multi:v1", NewPlatformSelector().WithOSFeatures("missing"))
	if err != nil || m != nil {
		t.Fatalf("missing feature: want nil match + nil err, got m=%v err=%v", m, err)
	}
	// 完全没有匹配的系统/架构：同样 nil+nil
	_, m, err = s.SelectPlatform(ctx, "multi:v1", NewPlatformSelector().WithOS("windows"))
	if err != nil || m != nil {
		t.Fatalf("no-hit OS: want nil match + nil err, got m=%+v err=%v", m, err)
	}
	t.Logf("final state: partial conditions filter only on set dimensions; one deterministic winner; no-hit is nil+nil")
}

// TestSelectMultiHitDeterministic：部分条件命中多个时，接口直接给出唯一且
// 稳定的结果；调整索引条目顺序也不改变选择，证明择优不依赖条目排列。
func TestSelectMultiHitDeterministic(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	rich := buildRichIndex(t, s, "multi:v1")
	ctx := context.Background()

	// 空条件：在全部平台中择优；amd64 无默认变体概念、声明最精简 -> amd64
	_, m, err := s.SelectPlatform(ctx, "multi:v1", NewPlatformSelector())
	if err != nil || m == nil {
		t.Fatalf("empty selector must pick one, m=%v err=%v", m, err)
	}
	if m.Entry.Descriptor.Digest != rich.plats["amd64"].manifestD {
		t.Fatalf("judgement basis: empty-selector pick = %s, want amd64 %s",
			m.Entry.Descriptor.Digest, rich.plats["amd64"].manifestD)
	}

	// 只给架构：arm64 默认变体 v8，与条目排列无关
	q := NewPlatformSelector().WithArchitecture("arm64")
	picks := map[Digest]bool{}
	for round := 0; round < 20; round++ {
		_, mm, e := s.SelectPlatform(ctx, "multi:v1", q)
		if e != nil || mm == nil {
			t.Fatalf("round %d: m=%v err=%v", round, mm, e)
		}
		picks[mm.Entry.Descriptor.Digest] = true
	}
	if len(picks) != 1 || !picks[rich.plats["v8"].manifestD] {
		t.Fatalf("judgement basis: repeated picks not a single stable winner: %v", picks)
	}

	// 再构造一个平台相同、但 manifests 顺序被打乱的索引：选择必须保持一致。
	descs := []Descriptor{
		{MediaType: MediaTypeOCIManifest, Digest: rich.plats["v7"].manifestD,
			Size: int64(len(blobBytes(s, rich.plats["v7"].manifestD))),
			Platform: &Platform{Architecture: "arm64", OS: "linux", Variant: "v7"}},
		{MediaType: MediaTypeOCIManifest, Digest: rich.plats["v8"].manifestD,
			Size: int64(len(blobBytes(s, rich.plats["v8"].manifestD))),
			Platform: &Platform{Architecture: "arm64", OS: "linux", Variant: "v8",
				OSVersion: "8.0", OSFeatures: []string{"foo", "bar"}, Features: []string{"custom"}}},
	}
	raw, _ := json.Marshal(&ImageIndex{SchemaVersion: 2, MediaType: MediaTypeOCIIndex, Manifests: descs})
	shuffledD := putBytes(t, s, raw)
	_, m2, err := s.SelectPlatform(ctx, shuffledD.String(), NewPlatformSelector().WithArchitecture("arm64"))
	if err != nil || m2 == nil || m2.Entry.Descriptor.Digest != rich.plats["v8"].manifestD {
		t.Fatalf("shuffled-order index must still pick v8, got %+v err=%v", m2, err)
	}
	t.Logf("final state: arch=arm64 deterministically picks %s across 20 repeats and shuffled index order",
		rich.plats["v8"].manifestD)
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
		t.Fatalf("judgement basis: want 3 images (signature attached entry excluded), got %+v", res)
	}
	sizeByVariant := map[string]int64{}
	for _, img := range res.Images {
		if img.Err != nil {
			t.Fatalf("platform %s unexpectedly failed: %v", img.Entry.Platform, img.Err)
		}
		if img.LayerCount != 1 {
			t.Fatalf("platform %s layer count = %d, want 1", img.Entry.Platform, img.LayerCount)
		}
		if img.ManifestDescriptor.Digest == rich.sigD {
			t.Fatalf("platform-less signature must never be prepared as an image")
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

// TestBatchSelectorsDedupAndUnmatched：每组条件只择优一个平台；多个条件择中
// 同一清单去重；没命中的条件下标与平台失败分开报告；附属条目不参与。
func TestBatchSelectorsDedupAndUnmatched(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	rich := buildRichIndex(t, s, "multi:v1")
	t.Logf("input: selectors [linux->amd64, linux->amd64(dup), linux/arm64/v7, windows/amd64(no hit)] against %s",
		rich.indexD)

	conds := []*PlatformSelector{
		NewPlatformSelector().WithOS("linux"), // 择优：amd64（最精简）
		NewPlatformSelector().WithOS("linux"), // 重复择优：同一平台只准备一次
		NewPlatformSelector().WithOS("linux").WithArchitecture("arm64").WithVariant("v7"),
		NewPlatformSelector().WithOS("windows"), // 零命中：进入 Unmatched
	}
	res, err := s.BatchResolve(context.Background(), "multi:v1", conds)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Images) != 2 {
		t.Fatalf("union should be 2 distinct picked platforms (amd64,v7), got %d: %+v",
			len(res.Images), res.Images)
	}
	got := map[Digest]bool{}
	for _, img := range res.Images {
		got[img.ManifestDescriptor.Digest] = true
		if img.ManifestDescriptor.Digest == rich.sigD {
			t.Fatalf("signature attached entry must not be batch-prepared")
		}
	}
	if !got[rich.plats["amd64"].manifestD] || !got[rich.plats["v7"].manifestD] {
		t.Fatalf("judgement basis: prepared set = %v, want {amd64,v7}", got)
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
	t.Logf("final state: %d distinct picked platforms prepared once each; unmatched=%v; signature excluded; pins released",
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
		if len(res.Images) != 1 { // 择优：arm64 的默认变体 v8
			bad <- fmt.Errorf("want exactly 1 picked arm64 platform (v8), got %d", len(res.Images))
			return
		}
		if res.Images[0].Entry.Platform.Variant != "v8" {
			bad <- fmt.Errorf("arm64 pick should be v8, got %+v", res.Images[0].Entry.Platform)
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

// buildIndexWithAttached 造一个平台条目与无 platform 附属条目交错的索引：
// 顺序为 amd64(平台)、sig1(附属)、arm64/v8(平台)、attestation(附属)。
func buildIndexWithAttached(t *testing.T, s *Store, tag string) (Digest, testArtifacts, testArtifacts, Digest, Digest) {
	t.Helper()
	amd := buildManifest(t, s, "amd64")
	armv8 := buildManifest(t, s, "arm64-v8")
	sig1 := []byte(`{"artifact":"signature-1"}`)
	sig1D := putBytes(t, s, sig1)
	att := []byte(`{"artifact":"attestation-1"}`)
	attD := putBytes(t, s, att)
	descs := []Descriptor{
		{MediaType: MediaTypeOCIManifest, Digest: amd.manifestD, Size: int64(len(amd.manifest)),
			Platform: &Platform{Architecture: "amd64", OS: "linux"}},
		{MediaType: "application/vnd.oci.empty.v1+json", Digest: sig1D, Size: int64(len(sig1))},
		{MediaType: MediaTypeOCIManifest, Digest: armv8.manifestD, Size: int64(len(armv8.manifest)),
			Platform: &Platform{Architecture: "arm64", OS: "linux", Variant: "v8"}},
		{MediaType: "application/vnd.oci.empty.v1+json", Digest: attD, Size: int64(len(att))},
	}
	raw, err := json.Marshal(&ImageIndex{SchemaVersion: 2, MediaType: MediaTypeOCIIndex, Manifests: descs})
	if err != nil {
		t.Fatal(err)
	}
	idxD := putBytes(t, s, raw)
	if tag != "" {
		if err := s.PutTag(context.Background(), tag, idxD); err != nil {
			t.Fatal(err)
		}
	}
	return idxD, amd, armv8, sig1D, attD
}

// TestDiscoverPlatformLessEntriesExcluded：无 platform 的附属条目（签名/证明）
// 与平台条目交错出现时，只发现真正声明平台的条目；附属条目单列且保留原始序号。
func TestDiscoverPlatformLessEntriesExcluded(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	idxD, amd, armv8, sig1D, attD := buildIndexWithAttached(t, s, "mix:v1")
	t.Logf("input: index=%s manifests[0]=linux/amd64 manifests[1]=sig(no platform) manifests[2]=linux/arm64/v8 manifests[3]=attestation(no platform)",
		idxD)

	disc, err := s.DiscoverPlatforms(context.Background(), "mix:v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(disc.Entries) != 2 {
		t.Fatalf("judgement basis: want 2 platforms, got %d: %+v", len(disc.Entries), disc.Entries)
	}
	if disc.Entries[0].Index != 0 || disc.Entries[0].Descriptor.Digest != amd.manifestD ||
		disc.Entries[1].Index != 2 || disc.Entries[1].Descriptor.Digest != armv8.manifestD {
		t.Fatalf("platform entries must keep original index/order: %+v", disc.Entries)
	}
	if len(disc.Attached) != 2 {
		t.Fatalf("want 2 attached entries, got %+v", disc.Attached)
	}
	if disc.Attached[0].Index != 1 || disc.Attached[0].Descriptor.Digest != sig1D ||
		disc.Attached[1].Index != 3 || disc.Attached[1].Descriptor.Digest != attD {
		t.Fatalf("attached entries misreported: %+v", disc.Attached)
	}
	t.Logf("final state: platforms=[%s@0,%s@2], attached=[%s@1,%s@3] — never the twain shall mix",
		amd.manifestD, armv8.manifestD, sig1D, attD)
}

// TestDiscoverIncompletePlatformIsBad：声明了 platform 但内容残缺（缺 os 或缺
// architecture）属于清单本身有问题，必须 ErrPlatformBad，与"没筛中"严格区分。
func TestDiscoverIncompletePlatformIsBad(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()

	cases := []struct {
		name     string
		platform string
	}{
		{"missing os", `{"architecture":"amd64"}`},
		{"missing architecture", `{"os":"linux"}`},
		{"both empty", `{"os":"","architecture":""}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(`{"schemaVersion":2,"mediaType":"` + MediaTypeOCIIndex + `","manifests":[` +
				`{"mediaType":"` + MediaTypeOCIManifest + `","digest":"sha256:` + repeat64("a") +
				`","size":1,"platform":` + tc.platform + `}]}`)
			idxD := putBytes(t, s, raw)
			t.Logf("input: index=%s with a declared-but-incomplete platform %s", idxD, tc.platform)
			if _, err := s.DiscoverPlatforms(context.Background(), idxD.String()); !errors.Is(err, ErrPlatformBad) {
				t.Fatalf("judgement basis: want ErrPlatformBad for %s, got %v", tc.name, err)
			}
			t.Logf("final state: incomplete declared platform rejected with ErrPlatformBad (not a no-match)")
		})
	}

	// 对照：完全没有 platform 字段不是坏清单，而是正常的附属条目
	ok := []byte(`{"schemaVersion":2,"mediaType":"` + MediaTypeOCIIndex + `","manifests":[` +
		`{"mediaType":"application/vnd.oci.empty.v1+json","digest":"sha256:` + repeat64("b") +
		`","size":0}]}`)
	okD := putBytes(t, s, ok)
	disc, err := s.DiscoverPlatforms(context.Background(), okD.String())
	if err != nil || len(disc.Entries) != 0 || len(disc.Attached) != 1 {
		t.Fatalf("platform-less entry must be attached, not bad: err=%v disc=%+v", err, disc)
	}
	t.Logf("final state: control case — platform-less entry is Attached (0 platforms, 0 errors)")
}

// TestAttachedNeverSelectable：空条件（匹配全部）也不会选中无平台附属条目；
// 全部平台批量准备同样跳过它们，不污染平台数/层数/总大小。
func TestAttachedNeverSelectable(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	idxD, amd, _, sig1D, attD := buildIndexWithAttached(t, s, "mix:v1")
	ctx := context.Background()
	t.Logf("input: index=%s has 2 platforms and 2 attached; select with empty selector then resolve all", idxD)

	_, m, err := s.SelectPlatform(ctx, "mix:v1", NewPlatformSelector())
	if err != nil || m == nil {
		t.Fatalf("empty selector must still pick a real platform: m=%v err=%v", m, err)
	}
	if m.Entry.Descriptor.Digest != amd.manifestD {
		t.Fatalf("judgement basis: empty selector picked %s, want amd64 %s",
			m.Entry.Descriptor.Digest, amd.manifestD)
	}
	res, err := s.ResolveAllPlatforms(ctx, "mix:v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Images) != 2 {
		t.Fatalf("resolve-all must cover exactly the 2 platforms, got %d images", len(res.Images))
	}
	var totalSize int64
	for _, img := range res.Images {
		if img.Err != nil {
			t.Fatalf("platform %s failed: %v", img.Entry.Platform, img.Err)
		}
		if img.ManifestDescriptor.Digest == sig1D || img.ManifestDescriptor.Digest == attD {
			t.Fatalf("attached descriptor leaked into images: %s", img.ManifestDescriptor.Digest)
		}
		totalSize += img.TotalSize
	}
	t.Logf("final state: select/resolve-all saw 2 platforms only, totalSize=%d (attached bytes excluded)", totalSize)
}

// TestSelectSingleManifest：引用本身是单平台镜像时，空条件选出唯一结果，
// 表现与发现一致，不因择优改造而含糊；非空条件则正常空结果。
func TestSelectSingleManifest(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	a := buildManifest(t, s, "amd64")
	_ = s.PutTag(context.Background(), "img", a.manifestD)
	ctx := context.Background()
	t.Logf("input: tag img -> single manifest %s", a.manifestD)

	disc, m, err := s.SelectPlatform(ctx, "img", NewPlatformSelector())
	if err != nil || m == nil {
		t.Fatalf("single manifest + empty selector: m=%v err=%v", m, err)
	}
	if disc.Kind != KindManifest || m.Entry.Descriptor.Digest != a.manifestD {
		t.Fatalf("judgement basis: want the one self-descriptor, got %+v", m)
	}
	// 非空条件与零值平台不相等：正常空结果而非错误
	_, m2, err := s.SelectPlatform(ctx, "img", NewPlatformSelector().WithOS("linux"))
	if err != nil || m2 != nil {
		t.Fatalf("single manifest + OS condition must be nil+nil, got m=%+v err=%v", m2, err)
	}
	t.Logf("final state: single manifest keeps one unambiguous result; non-empty condition is clean empty")
}

// TestSelectConcurrentTagMigrate：择优与标签在两个索引间迁移并发时，
// 每次返回必须对当时解析到的根摘要自洽（择中的条目属于该索引且稳定）。
func TestSelectConcurrentTagMigrate(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	ctx := context.Background()

	first := buildRichIndex(t, s, "moving:latest") // amd64, v8, v7 + signature
	secondA := buildManifest(t, s, "alt-amd64")
	secondArm := buildManifest(t, s, "alt-arm64-v8")
	descs := []Descriptor{
		{MediaType: MediaTypeOCIManifest, Digest: secondA.manifestD, Size: int64(len(secondA.manifest)),
			Platform: &Platform{Architecture: "amd64", OS: "linux"}},
		{MediaType: MediaTypeOCIManifest, Digest: secondArm.manifestD, Size: int64(len(secondArm.manifest)),
			Platform: &Platform{Architecture: "arm64", OS: "linux", Variant: "v8"}},
	}
	raw, _ := json.Marshal(&ImageIndex{SchemaVersion: 2, MediaType: MediaTypeOCIIndex, Manifests: descs})
	secondD := putBytes(t, s, raw)
	t.Logf("input: tag moving:latest flips between index %s (3 platforms+sig) and %s (2 platforms)",
		first.indexD, secondD)

	const rounds = 60
	var wg, selWg sync.WaitGroup
	bad := make(chan error, rounds*2)
	stop := make(chan struct{})

	wg.Add(1)
	go func() { // 迁移者：在两个索引间反复切换，收到 stop 才退出
		defer wg.Done()
		targets := []Digest{first.indexD, secondD}
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = s.PutTag(ctx, "moving:latest", targets[i%2])
			runtime.Gosched()
		}
	}()

	for r := 0; r < rounds; r++ {
		selWg.Add(1)
		go func() {
			defer selWg.Done()
			disc, m, err := s.SelectPlatform(ctx, "moving:latest",
				NewPlatformSelector().WithArchitecture("arm64"))
			if err != nil {
				if errors.Is(err, ErrLabelNotExist) {
					return
				}
				bad <- fmt.Errorf("select error not classifiable: %w", err)
				return
			}
			if m == nil {
				bad <- fmt.Errorf("arm64 must match in both index versions")
				return
			}
			// 自洽性：返回的 DiscoveryResult 与择中条目必须属于同一个根，
			// 且该条目确实在根的平台集合里。
			found := false
			for _, e := range disc.Entries {
				if e.Descriptor.Digest == m.Entry.Descriptor.Digest {
					found = true
				}
			}
			if !found {
				bad <- fmt.Errorf("picked %s not member of resolved root %s platforms",
					m.Entry.Descriptor.Digest, disc.Root)
				return
			}
			switch disc.Root {
			case first.indexD:
				if m.Entry.Descriptor.Digest != first.plats["v8"].manifestD {
					bad <- fmt.Errorf("root-v1 pick = %s, want v8", m.Entry.Descriptor.Digest)
				}
			case secondD:
				if m.Entry.Descriptor.Digest != secondArm.manifestD {
					bad <- fmt.Errorf("root-v2 pick = %s, want alt arm64/v8", m.Entry.Descriptor.Digest)
				}
			default:
				bad <- fmt.Errorf("resolved unexpected root %s", disc.Root)
			}
		}()
	}
	wg.Wait()
	close(stop)
	close(bad)
	for e := range bad {
		t.Fatalf("race violation: %v", e)
	}
	t.Logf("final state: %d concurrent selects each returned a root-self-consistent deterministic winner", rounds)
}
