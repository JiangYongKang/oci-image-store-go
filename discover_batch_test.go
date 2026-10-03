package ocistore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"testing"
)

// richArtifacts 聚合一个三平台富索引的构件。
type richArtifacts struct {
	indexD Digest
	plats  map[string]testArtifacts
	order  []string // 索引中平台的内容定义顺序
	// attestations: 挂在索引上的无平台附属条目（签名/证明材料），
	// key 为用途说明，value 为内容摘要；它们不是平台，不应混入平台列表与统计。
	attestations map[string]Digest
}

// buildRichIndex 构造 linux/amd64、linux/arm64/v8（带 osVersion/osFeatures/features）、
// linux/arm64/v7 三个平台的索引并打标签。索引里额外挂两个没有 platform 信息的
// 附属条目（签名、证明材料），用于验证发现/批量只覆盖真正声明平台的条目。
func buildRichIndex(t *testing.T, s *Store, tag string) richArtifacts {
	t.Helper()
	amd := buildManifest(t, s, "amd64")
	armv8 := buildManifest(t, s, "arm64-v8")
	armv7 := buildManifest(t, s, "arm64-v7")
	sig := putBytes(t, s, []byte("fake-signature-payload"))
	att := putBytes(t, s, []byte("fake-attestation-payload"))
	descs := []Descriptor{
		{MediaType: MediaTypeOCIManifest, Digest: amd.manifestD, Size: int64(len(amd.manifest)),
			Platform: &Platform{Architecture: "amd64", OS: "linux"}},
		{MediaType: MediaTypeOCIManifest, Digest: armv8.manifestD, Size: int64(len(armv8.manifest)),
			Platform: &Platform{Architecture: "arm64", OS: "linux", Variant: "v8",
				OSVersion: "8.0", OSFeatures: []string{"foo", "bar"}, Features: []string{"custom"}}},
		// 附属条目 1：签名，没有 platform
		{MediaType: "application/vnd.oci.image.signature.v1+json", Digest: sig, Size: 24},
		{MediaType: MediaTypeOCIManifest, Digest: armv7.manifestD, Size: int64(len(armv7.manifest)),
			Platform: &Platform{Architecture: "arm64", OS: "linux", Variant: "v7"}},
		// 附属条目 2：证明材料，没有 platform
		{MediaType: "application/vnd.oci.image.attestation.v1+json", Digest: att, Size: 26},
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
		plats:        map[string]testArtifacts{"amd64": amd, "v8": armv8, "v7": armv7},
		attestations: map[string]Digest{"signature": sig, "attestation": att}}
}

// TestDiscoverIndex：发现多架构索引，字段完整、顺序与索引内容定义一致、可复现；
// 无平台信息的附属条目（签名/证明材料）不出现在平台列表里。
func TestDiscoverIndex(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	rich := buildRichIndex(t, s, "multi:v1")
	t.Logf("input: tag multi:v1 -> index %s with 5 manifests entries (3 platforms + signature %s + attestation %s)",
		rich.indexD, rich.attestations["signature"], rich.attestations["attestation"])

	disc, err := s.DiscoverPlatforms(context.Background(), "multi:v1")
	if err != nil {
		t.Fatal(err)
	}
	if disc.Kind != KindIndex || disc.Root != rich.indexD || len(disc.Entries) != 3 {
		t.Fatalf("judgement basis: want 3 platform entries (attached non-platform entries excluded), got kind=%s root=%s n=%d",
			disc.Kind, disc.Root, len(disc.Entries))
	}
	// 附属条目摘要不得出现在发现结果中
	for _, e := range disc.Entries {
		if e.Descriptor.Digest == rich.attestations["signature"] ||
			e.Descriptor.Digest == rich.attestations["attestation"] {
			t.Fatalf("attached non-platform entry leaked into platforms: %+v", e)
		}
	}
	// 顺序必须是内容定义顺序，且稳定可复现；Index 保留在原 manifests 数组中的下标
	// （附属条目被跳过后下标不连续：v7 在数组中是下标 3）
	gotOrder := []string{}
	wantIndex := []int{0, 1, 3}
	for i, e := range disc.Entries {
		if e.Index != wantIndex[i] {
			t.Fatalf("entry %d original index = %d, want %d", i, e.Index, wantIndex[i])
		}
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
	t.Logf("final state: discovered 3 of 5 entries in stable order %v (original indexes %v); attached entries excluded; optional fields on v8 preserved",
		gotOrder, wantIndex)
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

// TestDiscoverBadEntryDistinctFromNoMatch：索引条目摘要非法、或声明了平台但
// os/architecture 残缺，都属于清单问题，必须与"条件没筛中"分开。
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
	t.Logf("judgement basis: invalid digest rejected with ErrPlatformBad (distinct from no-match)")

	// 声明了 platform 但缺少 architecture：清单本身有问题，不是"没筛中"
	missingArch := []byte(`{"schemaVersion":2,"mediaType":"` + MediaTypeOCIIndex + `","manifests":[` +
		`{"mediaType":"` + MediaTypeOCIManifest + `","digest":"sha256:` + repeat64("a") + `","size":1,` +
		`"platform":{"os":"linux"}}]}`)
	missingArchD := putBytes(t, s, missingArch)
	if _, err := s.DiscoverPlatforms(context.Background(), missingArchD.String()); !errors.Is(err, ErrPlatformBad) {
		t.Fatalf("want ErrPlatformBad for platform missing architecture, got %v", err)
	}
	t.Logf("judgement basis: platform declared without architecture rejected with ErrPlatformBad")

	// 声明了 platform 但缺少 os，同理
	missingOS := []byte(`{"schemaVersion":2,"mediaType":"` + MediaTypeOCIIndex + `","manifests":[` +
		`{"mediaType":"` + MediaTypeOCIManifest + `","digest":"sha256:` + repeat64("b") + `","size":1,` +
		`"platform":{"architecture":"amd64"}}]}`)
	missingOSD := putBytes(t, s, missingOS)
	if _, err := s.DiscoverPlatforms(context.Background(), missingOSD.String()); !errors.Is(err, ErrPlatformBad) {
		t.Fatalf("want ErrPlatformBad for platform missing os, got %v", err)
	}
	t.Logf("judgement basis: platform declared without os rejected with ErrPlatformBad")

	// 对照：同样是"拿不到想要的平台"，没有 platform 的附属条目只是被排除，
	// 条件没筛中时 SelectPlatform 返回 nil + nil，而不是 ErrPlatformBad。
	rich := buildRichIndex(t, s, "rich")
	t.Logf("control: index %s has 5 manifests entries (3 platforms + 2 attached), selector os=windows",
		rich.indexD)
	disc, m, err := s.SelectPlatform(context.Background(), "rich",
		NewPlatformSelector().WithOS("windows"))
	if err != nil || m != nil {
		t.Fatalf("no-match must be nil match + nil err, got m=%+v err=%v", m, err)
	}
	if len(disc.Entries) != 3 {
		t.Fatalf("control index must still discover its 3 real platforms, got %d", len(disc.Entries))
	}
	t.Logf("final state: bad listing -> ErrPlatformBad; valid listing with no hit -> nil+nil, never conflated")
}

// TestDiscoverAttachedEntriesIgnored：只有附属条目、没有任何真正平台的索引，
// 发现结果是零个平台的合法 KindIndex，而不是错误或空白平台。
func TestDiscoverAttachedEntriesIgnored(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()

	sig := putBytes(t, s, []byte("signature-only"))
	idx := &ImageIndex{SchemaVersion: 2, MediaType: MediaTypeOCIIndex, Manifests: []Descriptor{
		{MediaType: "application/vnd.oci.image.signature.v1+json", Digest: sig, Size: int64(len("signature-only"))},
	}}
	raw, _ := json.Marshal(idx)
	idxD := putBytes(t, s, raw)
	_ = s.PutTag(context.Background(), "artifacts-only", idxD)
	t.Logf("input: index %s has one manifests entry without platform (signature %s)", idxD, sig)

	disc, err := s.DiscoverPlatforms(context.Background(), "artifacts-only")
	if err != nil {
		t.Fatalf("attached-only index must not be an error: %v", err)
	}
	if disc.Kind != KindIndex || len(disc.Entries) != 0 {
		t.Fatalf("want zero platforms for attached-only index, got %+v", disc)
	}
	// 全部平台批量准备：零个镜像，附属内容不参与，也不算 Unmatched
	res, err := s.ResolveAllPlatforms(context.Background(), "artifacts-only")
	if err != nil || len(res.Images) != 0 {
		t.Fatalf("resolve-all on attached-only index: images=%d err=%v, want 0/nil", len(res.Images), err)
	}
	t.Logf("final state: attached-only index -> 0 platforms, 0 prepared images, no error")
}

// TestDiscoverAttachedBlobAbsent：索引里挂着无平台附属条目，但其内容本地不存在
// （如预热时签名/证明材料尚未同步）。发现与批量准备都不应触碰附属内容，
// 三个真正平台必须照常成功，统计也不包含附属字节。
func TestDiscoverAttachedBlobAbsent(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	ctx := context.Background()
	rich := buildRichIndex(t, s, "multi:v1")

	// 删除两个附属条目的 blob（保留索引与全部平台内容）
	for name, d := range rich.attestations {
		if err := os.Remove(blobPath(dir, d)); err != nil {
			t.Fatalf("remove attached blob %s (%s): %v", name, d, err)
		}
	}
	t.Logf("input: index %s intact, but attached blobs %v deleted locally (not yet synced)",
		rich.indexD, rich.attestations)

	disc, err := s.DiscoverPlatforms(ctx, "multi:v1")
	if err != nil {
		t.Fatalf("discovery must not read attached blobs: %v", err)
	}
	if len(disc.Entries) != 3 {
		t.Fatalf("judgement basis: want 3 platforms despite missing attached blobs, got %d", len(disc.Entries))
	}
	res, err := s.ResolveAllPlatforms(ctx, "multi:v1")
	if err != nil || len(res.Images) != 3 {
		t.Fatalf("resolve-all must ignore absent attached blobs: images=%d err=%v", len(res.Images), err)
	}
	for _, img := range res.Images {
		if img.Err != nil {
			t.Fatalf("real platform %s must be ready while attached blobs absent: %v", img.Entry.Platform, img.Err)
		}
	}
	t.Logf("final state: 3 platforms discovered and prepared with attached blobs absent; attached bytes never read")
}

// TestSelectPartialConditions：只给部分条件时，未给维度不参与比较，候选多写信息不被排除；
// SelectPlatform 直接返回唯一一个最合适的平台；没筛中是 nil+nil 而不是错误。
func TestSelectPartialConditions(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	rich := buildRichIndex(t, s, "multi:v1")
	ctx := context.Background()
	t.Logf("input: index %s platforms=[linux/amd64, linux/arm64/v8{osVer=8.0 os.features=[foo,bar] features=[custom]}, linux/arm64/v7] + 2 attached entries",
		rich.indexD)

	pick := func(q *PlatformSelector, what string) *PlatformMatch {
		disc, m, err := s.SelectPlatform(ctx, "multi:v1", q)
		if err != nil {
			t.Fatalf("%s: unexpected error %v", what, err)
		}
		if disc == nil {
			t.Fatalf("%s: discovery result must always be returned", what)
		}
		if m == nil {
			t.Fatalf("%s: expected one platform, got nil", what)
		}
		return m
	}

	// 只给 OS：三个 linux 命中，amd64 额外特化计数 0（无 variant/osVersion/features），
	// v8 计数 3，v7 计数 1 -> 直接选 amd64，调用方无需二次选择
	m := pick(NewPlatformSelector().WithOS("linux"), "OS-only filter")
	if m.Entry.Platform.Architecture != "amd64" {
		t.Fatalf("OS-only should prefer least-specialized amd64, got %s", m.Entry.Platform)
	}
	// 只给架构：v8 与 v7 都命中；v8 还携带 variant+osVersion+两类特性（额外特化 4），
	// v7 只多一个 variant（额外特化 1）-> 额外特化更少的 v7 胜出
	m = pick(NewPlatformSelector().WithArchitecture("arm64"), "arch-only filter")
	if m.Entry.Platform.Variant != "v7" {
		t.Fatalf("arch-only should prefer less-specialized v7 over feature-rich v8, got %s", m.Entry.Platform)
	}
	// 系统+架构+变体：精确命中 v7
	m = pick(NewPlatformSelector().WithOS("linux").WithArchitecture("arm64").WithVariant("v7"),
		"full-condition filter")
	if m.Entry.Platform.Variant != "v7" {
		t.Fatalf("full-condition filter should hit v7, got %s", m.Entry.Platform)
	}
	// 显式要求 variant 为空：只命中 amd64，v8 不回退
	m = pick(NewPlatformSelector().WithArchitecture("amd64").WithVariant(""), "empty-variant constraint")
	if m.Entry.Platform.Architecture != "amd64" {
		t.Fatalf("empty-variant constraint should hit amd64, got %s", m.Entry.Platform)
	}
	// 系统版本约束：唯一命中 v8
	m = pick(NewPlatformSelector().WithOSVersion("8.0"), "osVersion filter")
	if m.Entry.Platform.Variant != "v8" {
		t.Fatalf("osVersion filter should hit v8, got %s", m.Entry.Platform)
	}
	// 特性子集合：要求 foo 时，携带 [foo,bar] 的 v8 命中（额外特性不排除）
	m = pick(NewPlatformSelector().WithOSFeatures("foo"), "os.features subset filter")
	if m.Entry.Platform.Variant != "v8" {
		t.Fatalf("os.features subset filter should hit v8, got %s", m.Entry.Platform)
	}
	// 附加特性 features 维度同理
	m = pick(NewPlatformSelector().WithFeatures("custom"), "features subset filter")
	if m.Entry.Platform.Variant != "v8" {
		t.Fatalf("features subset filter should hit v8, got %s", m.Entry.Platform)
	}
	// 要求候选没有的特性：零命中 = nil match + nil err（不是错误，也不返回附属条目）
	_, m, err := s.SelectPlatform(ctx, "multi:v1", NewPlatformSelector().WithOSFeatures("missing"))
	if err != nil || m != nil {
		t.Fatalf("missing feature: want nil match + nil err, got m=%+v err=%v", m, err)
	}
	// 附属条目没有平台，不可能被任何条件选中
	_, m, err = s.SelectPlatform(ctx, "multi:v1", NewPlatformSelector())
	if err != nil || m == nil {
		t.Fatalf("empty selector: want one best platform, got m=%v err=%v", m, err)
	}
	if m.Entry.Descriptor.Digest == rich.attestations["signature"] ||
		m.Entry.Descriptor.Digest == rich.attestations["attestation"] {
		t.Fatalf("attached non-platform entry must never be selected")
	}
	t.Logf("final state: partial conditions judged only on set dimensions; one best platform returned directly; no-match is nil+nil; attached entries never selectable")
}

// TestSelectTieBreakDeterministic：多个命中时择优规则确定可解释——
// 额外特化更少者优先，相同则按索引内容定义顺序；同一索引同一条件反复选择结果恒定。
func TestSelectTieBreakDeterministic(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()

	// 专门构造顺序敏感的索引：[arm64/v7, arm64/v8, arm64(裸)]，
	// 三个候选对 arch=arm64 都命中；裸 arm64 无 variant（额外特化更少）必须胜出。
	v7 := buildManifest(t, s, "tie-arm64-v7")
	v8 := buildManifest(t, s, "tie-arm64-v8")
	bare := buildManifest(t, s, "tie-arm64-bare")
	descs := []Descriptor{
		{MediaType: MediaTypeOCIManifest, Digest: v7.manifestD, Size: int64(len(v7.manifest)),
			Platform: &Platform{Architecture: "arm64", OS: "linux", Variant: "v7"}},
		{MediaType: MediaTypeOCIManifest, Digest: v8.manifestD, Size: int64(len(v8.manifest)),
			Platform: &Platform{Architecture: "arm64", OS: "linux", Variant: "v8"}},
		{MediaType: MediaTypeOCIManifest, Digest: bare.manifestD, Size: int64(len(bare.manifest)),
			Platform: &Platform{Architecture: "arm64", OS: "linux"}},
	}
	idx := &ImageIndex{SchemaVersion: 2, MediaType: MediaTypeOCIIndex, Manifests: descs}
	raw, _ := json.Marshal(idx)
	idxD := putBytes(t, s, raw)
	_ = s.PutTag(context.Background(), "tie", idxD)
	t.Logf("input: index %s order=[arm64/v7@0, arm64/v8@1, arm64/bare@2]; selector arch=arm64", idxD)

	// 规则 1：额外特化更少优先——即便裸 arm64 在数组最后，也选它而不是 v7/v8
	_, m, err := s.SelectPlatform(context.Background(), "tie", NewPlatformSelector().WithArchitecture("arm64"))
	if err != nil || m == nil {
		t.Fatalf("judgement basis: want one winner, got m=%v err=%v", m, err)
	}
	if m.Entry.Descriptor.Digest != bare.manifestD {
		t.Fatalf("least-specialized bare arm64 must win regardless of index position, got %s", m.Entry.Platform)
	}
	t.Logf("judgement basis: v7 extra=1, v8 extra=1, bare extra=0 -> winner=bare@%d", m.Entry.Index)

	// 规则 2：特化计数相同（v7、v8 各 1）按索引顺序取最靠前；用裸候选不存在的索引验证
	idx2 := &ImageIndex{SchemaVersion: 2, MediaType: MediaTypeOCIIndex, Manifests: descs[:2]}
	raw2, _ := json.Marshal(idx2)
	idx2D := putBytes(t, s, raw2)
	_, m2, err := s.SelectPlatform(context.Background(), idx2D.String(), NewPlatformSelector().WithArchitecture("arm64"))
	if err != nil || m2.Entry.Descriptor.Digest != v7.manifestD {
		t.Fatalf("equal specialization must resolve in index order -> v7, got %+v err=%v", m2, err)
	}
	t.Logf("judgement basis: v7@0 and v8@1 tied at extra=1 -> first in index order wins (v7)")

	// 同一索引同一条件反复选择，结果必须恒定
	for i := 0; i < 20; i++ {
		_, rep, err := s.SelectPlatform(context.Background(), "tie", NewPlatformSelector().WithArchitecture("arm64"))
		if err != nil || rep.Entry.Descriptor.Digest != bare.manifestD {
			t.Fatalf("selection not deterministic at iteration %d: %+v %v", i, rep, err)
		}
	}
	t.Logf("final state: tie-break is deterministic (less-specialized wins, then index order); 20 repeated picks identical")
}

// TestSelectSingleManifest：引用本身是单平台镜像时，空条件选中唯一明确结果，
// 表现不因本次修改而变含糊；非空条件零命中（nil+nil）。
func TestSelectSingleManifest(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	a := buildManifest(t, s, "amd64")
	_ = s.PutTag(context.Background(), "img", a.manifestD)
	t.Logf("input: tag img -> single manifest %s (not an index)", a.manifestD)

	disc, m, err := s.SelectPlatform(context.Background(), "img", NewPlatformSelector())
	if err != nil {
		t.Fatalf("single manifest must not be an error: %v", err)
	}
	if disc.Kind != KindManifest || m == nil || m.Entry.Descriptor.Digest != a.manifestD {
		t.Fatalf("single manifest must yield one unambiguous match, got kind=%s m=%+v", disc.Kind, m)
	}
	// 非空条件：零值平台无法满足显式约束 -> nil + nil，而不是错误
	_, m2, err := s.SelectPlatform(context.Background(), "img", NewPlatformSelector().WithOS("linux"))
	if err != nil || m2 != nil {
		t.Fatalf("non-empty selector on single manifest: want nil+nil, got %+v %v", m2, err)
	}
	t.Logf("final state: single manifest keeps one unambiguous result; constrained selector yields nil+nil")
}

// TestResolveAllPlatformsBatch：批量准备全部平台，汇总层数与总大小；
// 无平台附属条目不参与准备，层数与总大小里不混入它们的数据。
func TestResolveAllPlatformsBatch(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	rich := buildRichIndex(t, s, "multi:v1")
	t.Logf("input: resolve all platforms of index %s (5 manifests entries, only 3 declare platforms)",
		rich.indexD)

	res, err := s.ResolveAllPlatforms(context.Background(), "multi:v1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != KindIndex || len(res.Images) != 3 || len(res.Unmatched) != 0 {
		t.Fatalf("judgement basis: want exactly 3 platform images (attached entries excluded), got %+v", res)
	}
	sizeByVariant := map[string]int64{}
	var totalSize int64
	for _, img := range res.Images {
		if img.Err != nil {
			t.Fatalf("platform %s unexpectedly failed: %v", img.Entry.Platform, img.Err)
		}
		if img.LayerCount != 1 {
			t.Fatalf("platform %s layer count = %d, want 1", img.Entry.Platform, img.LayerCount)
		}
		if img.ManifestDescriptor.Digest == rich.attestations["signature"] ||
			img.ManifestDescriptor.Digest == rich.attestations["attestation"] {
			t.Fatalf("attached non-platform entry leaked into batch images")
		}
		key := img.Entry.Platform.Architecture + "/" + img.Entry.Platform.Variant
		sizeByVariant[key] = img.TotalSize
		totalSize += img.TotalSize
	}
	for k, art := range map[string]testArtifacts{
		"amd64/": rich.plats["amd64"], "arm64/v8": rich.plats["v8"], "arm64/v7": rich.plats["v7"]} {
		if sizeByVariant[k] != int64(len(art.layer)) {
			t.Fatalf("total size for %s = %d, want %d", k, sizeByVariant[k], len(art.layer))
		}
	}
	// 总大小只含三个平台层的字节，不含附属条目（24+26 字节）
	wantTotal := int64(len(rich.plats["amd64"].layer) + len(rich.plats["v8"].layer) +
		len(rich.plats["v7"].layer))
	if totalSize != wantTotal {
		t.Fatalf("aggregated total size = %d, want %d (attached-entry bytes must not count)", totalSize, wantTotal)
	}
	t.Logf("final state: 3 platforms ready, each layers=1, sizes=%v total=%d; attached entries absent from counts",
		sizeByVariant, totalSize)
}

// TestBatchSelectorsDedupAndUnmatched：每个条件择优唯一一个平台，重复择优去重；
// 没选中的条件下标与平台失败分开报告。
func TestBatchSelectorsDedupAndUnmatched(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	rich := buildRichIndex(t, s, "multi:v1")
	t.Logf("input: selectors [arch=arm64, arch=arm64, linux/arm64/v8, windows/amd64] against %s",
		rich.indexD)

	conds := []*PlatformSelector{
		NewPlatformSelector().WithArchitecture("arm64"),                                   // v8/v7 命中，v7 额外特化更少 -> 选 v7
		NewPlatformSelector().WithArchitecture("arm64"),                                   // 重复择优：v7 只准备一次
		NewPlatformSelector().WithOS("linux").WithArchitecture("arm64").WithVariant("v8"), // 精确选 v8
		NewPlatformSelector().WithOS("windows"),                                           // 零命中：进入 Unmatched
	}
	res, err := s.BatchResolve(context.Background(), "multi:v1", conds)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Images) != 2 {
		t.Fatalf("selectors should resolve to distinct {v8, v7}, got %d images", len(res.Images))
	}
	got := []string{
		res.Images[0].Entry.Platform.Variant, res.Images[1].Entry.Platform.Variant,
	}
	if fmt.Sprint(got) != "[v8 v7]" {
		t.Fatalf("judgement basis: prepared order = %v, want [v8 v7] (discovery order)", got)
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

// checkBatchSelfConsistent 校验一次批量结果的自洽性：
// 平台级失败必须可区分且不带半成品；成功项的层数汇总一致，且 config/层在
// 检查时刻仍可读（不允许"清单在、层已被回收"的悬空结果）。
func checkBatchSelfConsistent(t *testing.T, s *Store, res *BatchResult, bad chan<- error) {
	t.Helper()
	for _, img := range res.Images {
		if img.Err != nil {
			// 平台级失败必须可区分；失败项不得带半成品
			if !(errors.Is(img.Err, ErrNotExist) || errors.Is(img.Err, ErrCorrupted) ||
				errors.Is(img.Err, ErrMediaType) || errors.Is(img.Err, ErrManifestBadJSON) ||
				errors.Is(img.Err, ErrPlatformBad)) {
				bad <- fmt.Errorf("platform error not classifiable: %w", img.Err)
			}
			if img.Manifest != nil || img.Layers != nil || img.LayerCount != 0 || img.TotalSize != 0 {
				bad <- fmt.Errorf("half-prepared platform exposed: %s", img.Entry.Platform)
			}
			continue
		}
		if img.Manifest == nil || img.LayerCount != len(img.Manifest.Layers) {
			bad <- fmt.Errorf("layer summary inconsistent for %s", img.Entry.Platform)
			continue
		}
		var wantSize int64
		for _, l := range img.Manifest.Layers {
			wantSize += l.Size
		}
		if img.TotalSize != wantSize {
			bad <- fmt.Errorf("total size inconsistent for %s: reported=%d recomputed=%d",
				img.Entry.Platform, img.TotalSize, wantSize)
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
}

// TestBatchConcurrentTagMigrateAndGC：批量解析（"全部平台"与"条件择优"两种）
// 与标签迁移、删除、GC 真并发：成功的批量必须每个成功平台都自洽完整；
// 失败必须是可区分的根级错误；不允许"清单在、层被回收"的悬空结果。
func TestBatchConcurrentTagMigrateAndGC(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	ctx := context.Background()

	const rounds = 40
	var wg sync.WaitGroup
	bad := make(chan error, rounds*2)

	classifiableRoot := func(err error) bool {
		return errors.Is(err, ErrLabelNotExist) || errors.Is(err, ErrNotExist) ||
			errors.Is(err, ErrCorrupted) || errors.Is(err, ErrManifestBadJSON) ||
			errors.Is(err, ErrMediaType) || errors.Is(err, ErrPlatformBad)
	}

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

		// 批量解析者 A：全部平台
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.ResolveAllPlatforms(ctx, tag)
			if err != nil {
				// 根级失败必须是可区分的明确原因（标签已删/内容不存在/损坏）
				if !classifiableRoot(err) {
					bad <- fmt.Errorf("batch root error not classifiable: %w", err)
				}
				return
			}
			checkBatchSelfConsistent(t, s, res, bad)
		}()

		// 批量解析者 B：按部分条件择优（只给架构，命中多个时由接口直接选定）
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.BatchResolve(ctx, tag,
				[]*PlatformSelector{NewPlatformSelector().WithArchitecture("arm64")})
			if err != nil {
				if !classifiableRoot(err) {
					bad <- fmt.Errorf("selector batch root error not classifiable: %w", err)
				}
				return
			}
			// 至多一个平台（每个条件只择优一个）；Unmatched 只能是空或 [0]
			if len(res.Images) > 1 || (len(res.Unmatched) != 0 && fmt.Sprint(res.Unmatched) != "[0]") {
				bad <- fmt.Errorf("selector batch shape invalid: images=%d unmatched=%v",
					len(res.Images), res.Unmatched)
				return
			}
			checkBatchSelfConsistent(t, s, res, bad)
		}()
		wg.Wait()
	}
	close(bad)
	for e := range bad {
		t.Fatalf("race violation: %v", e)
	}
	st, _ := s.Stats()
	t.Logf("final state: %d rounds batch(all+selector)-vs-migrate/delete/GC completed without dangling refs; blobs=%d",
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
			[]*PlatformSelector{
				NewPlatformSelector().WithArchitecture("arm64").WithVariant("v8"),
				NewPlatformSelector().WithArchitecture("arm64").WithVariant("v7"),
			})
		if err != nil {
			bad <- err
			return
		}
		if len(res.Images) != 2 { // 两个条件分别择优到 v8、v7
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
