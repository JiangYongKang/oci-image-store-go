package ocistore

import (
	"context"
	"errors"
	"testing"
)

// multiArchBuild 构造 amd64 与 arm64 两个平台镜像及其索引。
type multiArch struct {
	indexData []byte
	amdD      Digest
	armD      Digest
}

func buildMultiArch(t *testing.T, s *Store, indexMedia, amdMedia, armMedia string) multiArch {
	t.Helper()
	// amd64
	amdCfg := mustPut(t, s, MediaTypeOCIConfig, []byte(`{"os":"linux","architecture":"amd64"}`))
	amdLayer := mustPut(t, s, MediaTypeOCILayer, []byte("amd-layer"))
	amdM := ImageManifest{SchemaVersion: 2, MediaType: amdMedia, Config: amdCfg, Layers: []Descriptor{amdLayer}}
	amdMB, _ := jsonMarshal(amdM)
	amdDesc := mustPut(t, s, amdMedia, amdMB)
	amdDesc.Platform = &Platform{Architecture: "amd64", OS: "linux"}

	// arm64
	armCfg := mustPut(t, s, MediaTypeOCIConfig, []byte(`{"os":"linux","architecture":"arm64"}`))
	armLayer := mustPut(t, s, MediaTypeOCILayer, []byte("arm-layer"))
	armM := ImageManifest{SchemaVersion: 2, MediaType: armMedia, Config: armCfg, Layers: []Descriptor{armLayer}}
	armMB, _ := jsonMarshal(armM)
	armDesc := mustPut(t, s, armMedia, armMB)
	armDesc.Platform = &Platform{Architecture: "arm64", OS: "linux", Variant: "v8"}

	idx := Index{SchemaVersion: 2, MediaType: indexMedia, Manifests: []Descriptor{amdDesc, armDesc}}
	idxB, _ := jsonMarshal(idx)
	_ = mustPut(t, s, indexMedia, idxB)
	return multiArch{indexData: idxB, amdD: amdDesc.Digest, armD: armDesc.Digest}
}

// TestResolvePlatformSelectsExact：精确选中请求平台，不回退。
func TestResolvePlatformSelectsExact(t *testing.T) {
	s, _ := newTestStore(t, 0)
	ctx := context.Background()
	ma := buildMultiArch(t, s, MediaTypeOCIIndex, MediaTypeOCIManifest, MediaTypeOCIManifest)
	if _, err := s.PutManifest(ctx, "multi", MediaTypeOCIIndex, ma.indexData); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		plat Platform
		want Digest
	}{
		{"amd64", Platform{OS: "linux", Architecture: "amd64"}, ma.amdD},
		{"arm64", Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}, ma.armD},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := s.ResolvePlatform(ctx, "multi", tc.plat)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Release()
			tLog(t, "resolve",
				"请求平台", tc.plat,
				"选中", r.Manifest.Digest,
				"判定依据", "platform 精确匹配",
				"来源索引", r.Index != nil)
			if r.Manifest.Digest != tc.want {
				t.Fatalf("选中=%s, 期望 %s", r.Manifest.Digest, tc.want)
			}
			if r.Index == nil {
				t.Fatal("应当记录来源索引描述符")
			}
			if len(r.Layers) != 1 {
				t.Fatalf("应解析出 1 个层, got %d", len(r.Layers))
			}
		})
	}
}

// TestResolvePlatformMissing：缺少匹配平台必须返回 ErrPlatformNotFound，
// 且错误可 As 为 *PlatformNotFoundError，绝不静默回退。
func TestResolvePlatformMissing(t *testing.T) {
	s, _ := newTestStore(t, 0)
	ctx := context.Background()
	ma := buildMultiArch(t, s, MediaTypeOCIIndex, MediaTypeOCIManifest, MediaTypeOCIManifest)
	if _, err := s.PutManifest(ctx, "multi", MediaTypeOCIIndex, ma.indexData); err != nil {
		t.Fatal(err)
	}

	want := Platform{OS: "linux", Architecture: "riscv64"}
	_, err := s.ResolvePlatform(ctx, "multi", want)
	tLog(t, "platform-missing", "请求", want, "错误", err)
	if !errors.Is(err, ErrPlatformNotFound) {
		t.Fatalf("必须返回 ErrPlatformNotFound, got %v", err)
	}
	var pn *PlatformNotFoundError
	if !errors.As(err, &pn) {
		t.Fatal("应可 As 为 *PlatformNotFoundError")
	}
	if len(pn.Available) != 2 {
		t.Fatalf("错误应携带 2 个候选平台, got %d", len(pn.Available))
	}
	// 明确不得回退：amd64 不应被悄悄返回。
	for _, a := range pn.Available {
		if a.Architecture == "riscv64" {
			t.Fatal("候选中不应出现请求平台")
		}
	}
}

// TestResolveVariantNoFallback：请求 arm v7 而索引只有 v8，必须拒绝，不回退 v8。
func TestResolveVariantNoFallback(t *testing.T) {
	s, _ := newTestStore(t, 0)
	ctx := context.Background()
	ma := buildMultiArch(t, s, MediaTypeOCIIndex, MediaTypeOCIManifest, MediaTypeOCIManifest)
	if _, err := s.PutManifest(ctx, "multi", MediaTypeOCIIndex, ma.indexData); err != nil {
		t.Fatal(err)
	}
	_, err := s.ResolvePlatform(ctx, "multi", Platform{OS: "linux", Architecture: "arm64", Variant: "v7"})
	if !errors.Is(err, ErrPlatformNotFound) {
		t.Fatalf("arm v7 请求不得回退到 v8, got %v", err)
	}
	tLog(t, "variant-no-fallback", "请求", "arm64/v7", "错误", err)
}

// TestResolveChildDigestMismatch：索引条目的声明摘要与实际内容不符，必须拒绝。
func TestResolveChildDigestMismatch(t *testing.T) {
	s, _ := newTestStore(t, 0)
	ctx := context.Background()

	// 直接构造一个“声明了错误摘要”的索引。
	cfg := mustPut(t, s, MediaTypeOCIConfig, []byte(`{"os":"linux","architecture":"amd64"}`))
	layer := mustPut(t, s, MediaTypeOCILayer, []byte("L"))
	m := ImageManifest{SchemaVersion: 2, MediaType: MediaTypeOCIManifest, Config: cfg, Layers: []Descriptor{layer}}
	mb, _ := jsonMarshal(m)
	badDesc := Descriptor{
		MediaType: MediaTypeOCIManifest,
		Digest:    Digest("sha256:" + repeat('f', 64)), // 故意错误
		Size:      int64(len(mb)),
		Platform:  &Platform{Architecture: "amd64", OS: "linux"},
	}
	// 真实清单也写入存储，但索引指向的是错误摘要。
	_ = mustPut(t, s, MediaTypeOCIManifest, mb)
	idx := Index{
		SchemaVersion: 2,
		MediaType:     MediaTypeOCIIndex,
		Manifests:     []Descriptor{badDesc},
	}
	idxB, _ := jsonMarshal(idx)
	if _, err := s.PutManifest(ctx, "badidx", MediaTypeOCIIndex, idxB); err != nil {
		t.Fatal(err)
	}

	_, err := s.ResolvePlatform(ctx, "badidx", Platform{OS: "linux", Architecture: "amd64"})
	tLog(t, "child-digest-mismatch", "声明", badDesc.Digest, "错误", err)
	if !errors.Is(err, ErrDigestMismatch) && !errors.Is(err, ErrNotFound) {
		t.Fatalf("错误的子摘要必须以 ErrDigestMismatch/ErrNotFound 拒绝, got %v", err)
	}
}

// TestResolveTruncatedManifest：平台清单 JSON 被截断必须返回 ErrManifestTruncated。
func TestResolveTruncatedManifest(t *testing.T) {
	s, _ := newTestStore(t, 0)
	ctx := context.Background()

	// 构造一个内容被截断的“清单” blob，并让索引声明其（被截断内容的）摘要。
	trunc := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{`)
	td := mustPut(t, s, MediaTypeOCIManifest, trunc) // 按截断内容本身寻址
	td.Platform = &Platform{Architecture: "amd64", OS: "linux"}
	idx := Index{SchemaVersion: 2, MediaType: MediaTypeOCIIndex, Manifests: []Descriptor{td}}
	idxB, _ := jsonMarshal(idx)
	if _, err := s.PutManifest(ctx, "trunc", MediaTypeOCIIndex, idxB); err != nil {
		t.Fatal(err)
	}

	_, err := s.ResolvePlatform(ctx, "trunc", Platform{OS: "linux", Architecture: "amd64"})
	tLog(t, "truncated-manifest", "截断长度", len(trunc), "错误", err)
	if !errors.Is(err, ErrManifestTruncated) {
		t.Fatalf("截断清单必须返回 ErrManifestTruncated, got %v", err)
	}
}
