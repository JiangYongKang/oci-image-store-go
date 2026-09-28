package ocistore

import (
	"context"
	"errors"
	"testing"
)

// TestResolveAllPlatformsOK：一次解析全部平台，逐平台层数/总大小可见，顺序稳定。
func TestResolveAllPlatformsOK(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	idxD, idx := buildRichIndex(t, s, "rich")
	t.Logf("input: index=%s entries=%d; resolve ALL platforms", idxD, len(idx.Manifests))

	res, err := s.ResolveAllPlatforms(context.Background(), "rich")
	if err != nil {
		t.Fatalf("ResolveAllPlatforms: %v", err)
	}
	if !res.IsIndex || res.Root != idxD || len(res.Platforms) != 6 {
		t.Fatalf("unexpected batch header: %+v", res)
	}
	var names []string
	for _, p := range res.Platforms {
		if p.Err != nil {
			t.Fatalf("platform %s unexpectedly failed: %v", p.Name, p.Err)
		}
		if p.LayerCount != 1 || p.Image == nil || len(p.Image.Manifest.Layers) != 1 {
			t.Fatalf("platform %s layers not prepared: count=%d", p.Name, p.LayerCount)
		}
		// SizeBytes 与描述符声明大小一致；层内容可读且通过校验
		var wantSize int64
		for _, l := range p.Image.Manifest.Layers {
			wantSize += l.Size
		}
		if p.SizeBytes != wantSize {
			t.Fatalf("platform %s size=%d want %d", p.Name, p.SizeBytes, wantSize)
		}
		if ok, _ := s.Has(p.Layers[0].Digest); !ok {
			t.Fatalf("platform %s layer %s not verified-present", p.Name, p.Layers[0].Digest)
		}
		names = append(names, p.Name)
	}
	wantNames := []string{
		"linux/386", "linux/amd64", "linux/arm",
		"linux/arm64/v7", "linux/arm64/v8", "windows/amd64",
	}
	for i := range wantNames {
		if names[i] != wantNames[i] {
			t.Fatalf("order=%v want %v", names, wantNames)
		}
	}
	if failed := res.Failed(); len(failed) != 0 {
		t.Fatalf("Failed() on all-success batch: %d", len(failed))
	}
	t.Logf("judgement basis: 6/6 platforms resolved; each LayerCount=1; order=%v", names)
	t.Logf("final state: %s total-size=%d layers prepared and verified", names[0], res.Platforms[0].SizeBytes)
}

// TestResolveAllSingleManifest：单平台镜像也能批量解析，结果恰有一个条目。
func TestResolveAllSingleManifest(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	a := buildManifest(t, s, "amd64")
	_ = s.PutTag(context.Background(), "img", a.manifestD)
	t.Logf("input: single manifest %s tagged 'img'", a.manifestD)

	res, err := s.ResolveAllPlatforms(context.Background(), "img")
	if err != nil {
		t.Fatal(err)
	}
	if res.IsIndex || len(res.Platforms) != 1 {
		t.Fatalf("want single entry, got %+v", res)
	}
	p := res.Platforms[0]
	if p.Err != nil || p.LayerCount != 1 || p.SizeBytes != int64(len("layer-data-for-amd64")) {
		t.Fatalf("single entry wrong: %+v size=%d", p, p.SizeBytes)
	}
	if p.Name != a.manifestD.Hex()[:12] {
		t.Fatalf("single-image name=%s want digest prefix", p.Name)
	}
	t.Logf("judgement basis: one entry name=%s layers=%d size=%d (digest-prefix naming)",
		p.Name, p.LayerCount, p.SizeBytes)
}

// TestBatchPartialFailure：某平台的子清单损坏，只失败该平台，其它平台结果完好。
func TestBatchPartialFailure(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	ctx := context.Background()
	idxD, idx := buildRichIndex(t, s, "rich")
	var armV8 Digest
	for _, d := range idx.Manifests {
		if d.Platform != nil && d.Platform.String() == "linux/arm64/v8" {
			armV8 = d.Digest
		}
	}
	t.Logf("input: index=%s; tamper child manifest %s (linux/arm64/v8) on disk",
		idxD, armV8)
	corruptBlobOnDisk(t, dir, armV8)

	res, err := s.ResolveAllPlatforms(ctx, "rich")
	if err != nil {
		t.Fatalf("partial platform failure must stay per-entry, got top-level: %v", err)
	}
	failed := res.Failed()
	if len(failed) != 1 || failed[0].Name != "linux/arm64/v8" {
		t.Fatalf("want exactly one failure at v8, got %+v", failed)
	}
	if !errors.Is(failed[0].Err, ErrCorrupted) {
		t.Fatalf("failure must classify as ErrCorrupted, got %v", failed[0].Err)
	}
	ok := len(res.Platforms) - 1
	if ok != 5 {
		t.Fatalf("want 5 unaffected platforms, got %d", ok)
	}
	for _, p := range res.Platforms {
		if p.Name == "linux/arm64/v8" {
			continue
		}
		if p.Err != nil || p.Image == nil {
			t.Fatalf("unaffected platform %s tainted: %v", p.Name, p.Err)
		}
	}
	t.Logf("judgement basis: 1/6 failed with ErrCorrupted (isolated), 5/6 fully usable; index=%d entries",
		len(idx.Manifests))
	t.Logf("final state: failed=%v others-prepared=%d; no half-finished entries",
		[]string{failed[0].Name}, ok)
}

// TestResolvePlatformsPartialAndOrder：按一批条件解析；无命中只失败该位置；
// 结果顺序与请求一致；重复条件允许。
func TestResolvePlatformsPartialAndOrder(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	idxD, _ := buildRichIndex(t, s, "rich")
	sels := []PlatformSelector{
		PlatformSelector{}.WithOS("linux").WithArchitecture("amd64"),
		PlatformSelector{}.WithOS("plan9"), // 无命中
		PlatformSelector{}.WithArchitecture("386"),
		PlatformSelector{}.WithOS("linux").WithArchitecture("amd64"), // 重复
	}
	t.Logf("input: index=%s selectors=%v (one unmatched, one duplicate)", idxD, sels)

	res, err := s.ResolvePlatforms(context.Background(), "rich", sels)
	if err != nil {
		t.Fatalf("top-level must not fail for an unmatched selector: %v", err)
	}
	if len(res.Platforms) != 4 {
		t.Fatalf("want 4 entries in request order, got %d", len(res.Platforms))
	}
	if res.Platforms[0].Name != "linux/amd64" || res.Platforms[0].Err != nil {
		t.Fatalf("entry0: %+v", res.Platforms[0])
	}
	if !errors.Is(res.Platforms[1].Err, ErrPlatformNoMatch) {
		t.Fatalf("entry1 want ErrPlatformNoMatch, got %v", res.Platforms[1].Err)
	}
	if res.Platforms[2].Name != "linux/386" || res.Platforms[2].Err != nil {
		t.Fatalf("entry2: %+v", res.Platforms[2])
	}
	if res.Platforms[3].Name != "linux/amd64" || res.Platforms[3].Err != nil {
		t.Fatalf("entry3 duplicate: %+v", res.Platforms[3])
	}
	if n := len(res.Failed()); n != 1 {
		t.Fatalf("Failed()=%d want 1", n)
	}
	t.Logf("judgement basis: request order preserved; unmatched selector isolated at position 1; duplicate allowed")

	// 单平台镜像 + 条件：空条件匹配；带 OS 的条件因单清单无平台描述而无命中。
	a := buildManifest(t, s, "solo")
	_ = s.PutTag(context.Background(), "solo", a.manifestD)
	r2, err := s.ResolvePlatforms(context.Background(), "solo", []PlatformSelector{
		{}, // 无条件：占位条目通过
		PlatformSelector{}.WithOS("linux"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if r2.Platforms[0].Err != nil {
		t.Fatalf("empty selector on single image: %v", r2.Platforms[0].Err)
	}
	if !errors.Is(r2.Platforms[1].Err, ErrPlatformNoMatch) {
		t.Fatalf("constrained selector on platform-less single image, got %v", r2.Platforms[1].Err)
	}
	t.Logf("final state: single image matches only the empty selector; constraint -> ErrPlatformNoMatch")
}

// TestBatchTopLevelFailures：引用缺失/根损坏属于整批失败，逐平台错误不掩盖它。
func TestBatchTopLevelFailures(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()

	if _, err := s.ResolveAllPlatforms(context.Background(), "missing"); !errors.Is(err, ErrLabelNotExist) {
		t.Fatalf("missing tag: want ErrLabelNotExist, got %v", err)
	} else {
		t.Logf("judgement basis: missing ref -> top-level %v (distinct from per-platform errors)", err)
	}
}
