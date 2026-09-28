package ocistore

import (
	"context"
	"errors"
	"testing"
)

// buildRichIndex 造一个平台更丰富的索引（顺序刻意打乱，验证稳定排序）：
//   linux/amd64
//   linux/arm64/v7  （带 os.version 与 os.features）
//   linux/arm64/v8  （两个候选同 arch，用于择优）
//   linux/arm       （无 variant，额外信息最少）
//   linux/386
//   windows/amd64  （带 os.feature win32k）
func buildRichIndex(t *testing.T, s *Store, tag string) (Digest, *ImageIndex) {
	t.Helper()
	ctx := context.Background()
	mkEntry := func(arch, variant, osver string, osfeat []string) (Descriptor, Digest) {
		a := buildManifest(t, s, arch+variant)
		return Descriptor{
			MediaType: MediaTypeOCIManifest,
			Digest:    a.manifestD,
			Size:      int64(len(a.manifest)),
			Platform: &Platform{
				Architecture: arch, OS: "linux", Variant: variant,
				OSVersion: osver, OSFeatures: osfeat,
			},
		}, a.manifestD
	}
	eAmd, _ := mkEntry("amd64", "", "", nil)
	eArmV7, _ := mkEntry("arm64", "v7", "1.0", []string{"foo"})
	eArmV8, _ := mkEntry("arm64", "v8", "", nil)
	eArm, _ := mkEntry("arm", "", "", nil)
	e386, _ := mkEntry("386", "", "", nil)
	win := buildManifest(t, s, "win-amd64")
	eWin := Descriptor{
		MediaType: MediaTypeOCIManifest, Digest: win.manifestD, Size: int64(len(win.manifest)),
		Platform: &Platform{Architecture: "amd64", OS: "windows", OSFeatures: []string{"win32k"}},
	}
	idx := &ImageIndex{
		SchemaVersion: 2,
		MediaType:     MediaTypeOCIIndex,
		// 刻意不按序排列
		Manifests: []Descriptor{eArmV8, eWin, e386, eAmd, eArm, eArmV7},
	}
	raw, err := jsonMarshal(idx)
	if err != nil {
		t.Fatal(err)
	}
	idxD := putBytes(t, s, raw)
	if tag != "" {
		if err := s.PutTag(ctx, tag, idxD); err != nil {
			t.Fatal(err)
		}
	}
	return idxD, idx
}

// TestDiscoverStableOrder：发现结果顺序稳定可复现，且包含全部可选信息。
func TestDiscoverStableOrder(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	idxD, idx := buildRichIndex(t, s, "rich")
	t.Logf("input: index=%s entries=%d (shuffled on disk); discover by tag and by digest",
		idxD, len(idx.Manifests))

	for _, ref := range []string{"rich", idxD.String()} {
		disc, err := s.Discover(context.Background(), ref)
		if err != nil {
			t.Fatalf("Discover(%s): %v", ref, err)
		}
		if !disc.IsIndex || len(disc.Platforms) != len(idx.Manifests) {
			t.Fatalf("Discover(%s): IsIndex=%v platforms=%d want %d",
				ref, disc.IsIndex, len(disc.Platforms), len(idx.Manifests))
		}
		got := make([]string, len(disc.Platforms))
		for i, p := range disc.Platforms {
			got[i] = p.Platform.String()
		}
		// 期望按规范键排序：linux/386, linux/amd64, linux/arm, linux/arm64/v7,
		// linux/arm64/v8, windows/amd64
		want := []string{
			"linux/386", "linux/amd64", "linux/arm",
			"linux/arm64/v7", "linux/arm64/v8", "windows/amd64",
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("Discover(%s) order=%v want %v", ref, got, want)
			}
		}
		// 可选信息完整保留
		var v7 *PlatformInfo
		for i := range disc.Platforms {
			if disc.Platforms[i].Platform.String() == "linux/arm64/v7" {
				v7 = &disc.Platforms[i]
			}
		}
		if v7 == nil || v7.Platform.OSVersion != "1.0" ||
			len(v7.Platform.OSFeatures) != 1 || v7.Platform.OSFeatures[0] != "foo" {
			t.Fatalf("optional fields not retained: %+v", v7)
		}
		t.Logf("judgement basis (%s): canonical sort -> %v; os.version/os.features retained", ref, got)
	}
	t.Logf("final state: discovery by tag and by digest identical and reproducible")
}

// TestDiscoverSingleManifest：引用指向单平台镜像时得到一个明确的占位条目而非错误。
func TestDiscoverSingleManifest(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	a := buildManifest(t, s, "amd64")
	_ = s.PutTag(context.Background(), "img", a.manifestD)
	t.Logf("input: tag 'img' -> single manifest %s (not an index)", a.manifestD)

	disc, err := s.Discover(context.Background(), "img")
	if err != nil {
		t.Fatalf("single manifest discovery must not error: %v", err)
	}
	if disc.IsIndex || len(disc.Platforms) != 1 {
		t.Fatalf("want exactly one placeholder entry, got IsIndex=%v n=%d",
			disc.IsIndex, len(disc.Platforms))
	}
	pi := disc.Platforms[0]
	if pi.Descriptor.Digest != a.manifestD || pi.IndexPos != 0 {
		t.Fatalf("placeholder entry wrong: %+v", pi)
	}
	t.Logf("judgement basis: single manifest -> one entry descriptor=%s, platform left zero",
		pi.Descriptor.Digest)
	t.Logf("final state: callers see a uniform 1-entry result, no ambiguous error")
}

// TestSelectorPartialConditions：只给部分条件；未给维度不参与比较；
// 候选多写信息不被排除；没给 variant/features 的场景重点覆盖。
func TestSelectorPartialConditions(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	_, idx := buildRichIndex(t, s, "rich")

	cases := []struct {
		name string
		sel  PlatformSelector
		want string
	}{
		{"only OS", (PlatformSelector{}).WithOS("windows"), "windows/amd64"},
		{"only arch", (PlatformSelector{}).WithArchitecture("386"), "linux/386"},
		{"os+arch, no variant", (PlatformSelector{}).WithOS("linux").WithArchitecture("amd64"), "linux/amd64"},
		{
			"os+arch+variant picks v7",
			(PlatformSelector{}).WithOS("linux").WithArchitecture("arm64").WithVariant("v7"),
			"linux/arm64/v7",
		},
		{
			"arch arm64 without variant -> least-surplus v8",
			(PlatformSelector{}).WithArchitecture("arm64"),
			"linux/arm64/v8",
		},
		{
			"required os.feature does not exclude extras",
			(PlatformSelector{}).WithOS("windows").WithOSFeatures("win32k"),
			"windows/amd64",
		},
	}
	for _, c := range cases {
		got, err := SelectPlatform(idx, c.sel)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		t.Logf("input: %s selector=%s -> %s", c.name, selectorString(c.sel), got.Platform)
		if got.Platform.String() != c.want {
			t.Fatalf("%s: got %s want %s", c.name, got.Platform, c.want)
		}
	}

	// 关键语义：只给 arch=arm64、不给 variant 时，v7/v8 都满足；
	// v7 额外带了 os.version+os.feature（surplus=2），v8 无额外信息（surplus=0），
	// 故择优必须是 v8，而不是按索引位置或随机命中 v7。
	got, err := SelectPlatform(idx, PlatformSelector{}.WithArchitecture("arm64"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Platform.String() != "linux/arm64/v8" {
		t.Fatalf("surplus tie-break: got %s, want least-surplus linux/arm64/v8", got.Platform)
	}
	t.Logf("judgement basis: arm64-only matches v7/v8; surplus(v7)=2 surplus(v8)=0 -> v8")

	// 候选多写了未被要求的信息，不因此被排除
	got, err = SelectPlatform(idx, PlatformSelector{}.WithOS("linux").WithArchitecture("arm64").WithVariant("v7"))
	if err != nil || got.Platform.OSVersion != "1.0" {
		t.Fatalf("extra os.version must not exclude candidate: %v", err)
	}
	t.Logf("final state: partial conditions honored; extras never exclude; tie-break deterministic")
}

// TestSelectorFeaturesSubset：特性条件为子集语义，候选可携带额外特性。
func TestSelectorFeaturesSubset(t *testing.T) {
	idx := &ImageIndex{SchemaVersion: 2, Manifests: []Descriptor{
		descWithPlatform("sha256:"+repeat64("1"), Platform{OS: "linux", Architecture: "amd64", Features: []string{"sse4", "avx"}}),
		descWithPlatform("sha256:"+repeat64("2"), Platform{OS: "linux", Architecture: "amd64", Features: []string{"sse4"}}),
	}}
	// 要求 sse4：两个都满足；额外特性多的排后 -> 选中只有 sse4 的
	got, err := SelectPlatform(idx, PlatformSelector{}.WithFeatures("sse4"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Descriptor.Digest.Hex() != repeat64("2") {
		t.Fatalf("subset + surplus: got %s", got.Descriptor.Digest)
	}
	// 同时要求 sse4+avx：只有第一个满足（第二个缺 avx）
	got, err = SelectPlatform(idx, PlatformSelector{}.WithFeatures("sse4", "avx"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Descriptor.Digest.Hex() != repeat64("1") {
		t.Fatalf("subset requirement: got %s", got.Descriptor.Digest)
	}
	// 要求一个候选都没有的特性：无匹配
	if _, err = SelectPlatform(idx, PlatformSelector{}.WithFeatures("nope")); !errors.Is(err, ErrPlatformNoMatch) {
		t.Fatalf("want ErrPlatformNoMatch, got %v", err)
	}
	t.Logf("judgement basis: features use subset semantics; unmatched feature -> ErrPlatformNoMatch")
}

// TestSelectorNoMatchVsBadList：筛不到东西与清单本身有问题必须分开判断。
func TestSelectorNoMatchVsBadList(t *testing.T) {
	good := &ImageIndex{SchemaVersion: 2, Manifests: []Descriptor{
		descWithPlatform("sha256:"+repeat64("1"), Platform{OS: "linux", Architecture: "amd64"}),
	}}
	_, err := SelectPlatform(good, PlatformSelector{}.WithOS("plan9"))
	if !errors.Is(err, ErrPlatformNoMatch) {
		t.Fatalf("want ErrPlatformNoMatch, got %v", err)
	}
	t.Logf("input: valid index, no candidate -> %v", err)

	bad := &ImageIndex{SchemaVersion: 2, Manifests: []Descriptor{
		{Digest: "not-a-digest", Platform: &Platform{OS: "linux", Architecture: "amd64"}},
	}}
	_, err = SelectPlatform(bad, PlatformSelector{}.WithOS("linux"))
	if !errors.Is(err, ErrPlatformBad) {
		t.Fatalf("bad list must be ErrPlatformBad, got %v", err)
	}
	t.Logf("judgement basis: malformed entry -> %v; distinct from no-match", err)

	// Discover 同样在坏索引上报 ErrPlatformBad
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	rawBad := []byte(`{"schemaVersion":2,"manifests":[{"digest":"oops","size":1,` +
		`"platform":{"architecture":"amd64","os":"linux"}}]}`)
	badD := putBytes(t, s, rawBad)
	if _, err := s.Discover(context.Background(), badD.String()); !errors.Is(err, ErrPlatformBad) {
		t.Fatalf("Discover bad index: want ErrPlatformBad, got %v", err)
	}
	t.Logf("final state: no-match (%v) and bad list (%v) are separately classifiable",
		ErrPlatformNoMatch, ErrPlatformBad)
}

// TestSelectorCanonicalTieBreak：额外量相同的多命中按规范键字典序确定择优。
func TestSelectorCanonicalTieBreak(t *testing.T) {
	idx := &ImageIndex{SchemaVersion: 2, Manifests: []Descriptor{
		descWithPlatform("sha256:"+repeat64("9"), Platform{OS: "linux", Architecture: "zzz"}),
		descWithPlatform("sha256:"+repeat64("1"), Platform{OS: "linux", Architecture: "aaa"}),
	}}
	// 只给 OS：两个都满足，额外量同为 0；规范键 aaa<zzz，且与摘要无关，选 aaa
	got, err := SelectPlatform(idx, PlatformSelector{}.WithOS("linux"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Platform.Architecture != "aaa" {
		t.Fatalf("canonical tie-break got %s, want aaa", got.Platform.Architecture)
	}
	t.Logf("judgement basis: equal surplus -> canonical key (os/arch/...) picks aaa deterministically")
}
