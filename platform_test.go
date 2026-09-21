package ocistore

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"bytes"
)

// TestResolvePlatformOK：从标签解析索引 -> 精确选择 amd64 平台清单。
func TestResolvePlatformOK(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	idxD, amd, arm := buildIndex(t, s, "multiarch")
	t.Logf("input: index=%s entries=[amd64=%s arm64=%s], want linux/amd64",
		idxD, amd.manifestD, arm.manifestD)

	res, err := s.FetchPlatform(context.Background(), "multiarch", Platform{OS: "linux", Architecture: "amd64"})
	if err != nil {
		t.Fatalf("fetch amd64: %v", err)
	}
	if res.ManifestDescriptor.Digest != amd.manifestD {
		t.Fatalf("selected %s, want %s", res.ManifestDescriptor.Digest, amd.manifestD)
	}
	t.Logf("judgement basis: exact OS/arch match selected %s; index entries=%d",
		res.ManifestDescriptor.Digest, len(res.Index.Manifests))

	// arm64 条目带 variant=v8：严格匹配
	res2, err := s.FetchPlatform(context.Background(), "multiarch", Platform{OS: "linux", Architecture: "arm64", Variant: "v8"})
	if err != nil {
		t.Fatalf("fetch arm64/v8: %v", err)
	}
	if res2.ManifestDescriptor.Digest != arm.manifestD {
		t.Fatalf("arm selected %s want %s", res2.ManifestDescriptor.Digest, arm.manifestD)
	}
	// 要求 arm64/v7：不存在 -> 必须拒绝，不得静默回退到 v8
	if _, err := s.FetchPlatform(context.Background(), "multiarch",
		Platform{OS: "linux", Architecture: "arm64", Variant: "v7"}); !errors.Is(err, ErrPlatformNoMatch) {
		t.Fatalf("want ErrPlatformNoMatch for v7, got %v", err)
	}
	t.Logf("final state: amd64/v1, arm64/v8 resolved exactly; arm64/v7 rejected with ErrPlatformNoMatch")
}

// TestResolvePlatformMissing：无匹配平台必须 ErrPlatformNoMatch，不回退。
func TestResolvePlatformMissing(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	idxD, _, _ := buildIndex(t, s, "multiarch")
	t.Logf("input: index=%s (linux/amd64,linux/arm64), request windows/amd64", idxD)

	_, err := s.FetchPlatform(context.Background(), "multiarch", Platform{OS: "windows", Architecture: "amd64"})
	if !errors.Is(err, ErrPlatformNoMatch) {
		t.Fatalf("want ErrPlatformNoMatch, got %v", err)
	}
	t.Logf("judgement basis: rejected with %v; no fallback to any linux entry", err)
}

// TestTruncatedManifest：被截断的 JSON 必须 ErrManifestBadJSON。
func TestTruncatedManifest(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	ctx := context.Background()

	_, amd, _ := buildIndex(t, s, "multiarch")
	t.Logf("input: truncate amd64 manifest blob %s on disk", amd.manifestD)

	// 写一份截断的清单并打标签
	tD, _, err := s.PutBlob(ctx, "", bytes.NewReader(amd.manifest[:len(amd.manifest)/2]))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.PutTag(ctx, "broken", tD)
	t.Logf("truncated blob digest=%s bytes=%d", tD, len(amd.manifest)/2)

	_, err = s.FetchPlatform(ctx, "broken", Platform{OS: "linux", Architecture: "amd64"})
	if !errors.Is(err, ErrManifestBadJSON) {
		t.Fatalf("want ErrManifestBadJSON, got %v", err)
	}
	t.Logf("judgement basis: truncated manifest rejected with ErrManifestBadJSON")
}

// TestUnsupportedMediaType：明确声明未知媒体类型必须 ErrMediaType，而非忽略。
func TestUnsupportedMediaType(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()

	// 解析一个被标为未知类型的 JSON blob
	raw := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.unknown.weird+json","foo":1}`)
	_, _, _, err := parseManifest("application/vnd.unknown.weird+json", raw)
	if !errors.Is(err, ErrMediaType) {
		t.Fatalf("want ErrMediaType, got %v", err)
	}
	t.Logf("judgement basis: declared unknown media type rejected: %v", err)
}

// TestExtraFieldsAndOldFormat：额外字段被保留且不改变解析结论；Docker 旧格式可解析。
func TestExtraFieldsAndOldFormat(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()

	// 额外字段
	raw := []byte(`{"schemaVersion":2,"mediaType":"` + MediaTypeOCIManifest +
		`","config":{"mediaType":"x","digest":"sha256:` + repeat64("b") + `","size":1},` +
		`"layers":[],"futureField":{"keep":true},"subject":null}`)
	isIndex, _, mani, err := parseManifest("", raw)
	if err != nil || isIndex {
		t.Fatalf("extra fields: err=%v isIndex=%v", err, isIndex)
	}
	if _, ok := mani.Unknown["futureField"]; !ok {
		t.Fatalf("extra field must be retained for compatibility")
	}
	t.Logf("judgement basis: extra fields retained=%v, parse conclusion unchanged (manifest)", mani.Unknown)

	// Docker 旧格式清单列表（无 mediaType 字段，结构嗅探为索引）
	dockerIdx := []byte(`{"schemaVersion":2,"manifests":[` +
		`{"mediaType":"application/vnd.docker.distribution.manifest.v2+json",` +
		`"digest":"sha256:` + repeat64("c") + `","size":42,` +
		`"platform":{"architecture":"amd64","os":"linux"}}]}`)
	isIndex2, idx, _, err := parseManifest("", dockerIdx)
	if err != nil || !isIndex2 || len(idx.Manifests) != 1 {
		t.Fatalf("docker old format: err=%v isIndex=%v", err, isIndex2)
	}
	desc, err := selectPlatform(idx, Platform{OS: "linux", Architecture: "amd64"})
	if err != nil || desc.Digest.Hex() != repeat64("c") {
		t.Fatalf("select from docker index: %v %s", err, desc.Digest)
	}
	t.Logf("final state: extra fields tolerated, legacy docker list inferred as index and resolved")
}

// TestDeclaredDigestMismatchOnRead：子清单声明摘要与磁盘内容不符时必须拒绝。
func TestDeclaredDigestMismatchOnRead(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	idxD, amd, _ := buildIndex(t, s, "multiarch")
	t.Logf("input: index=%s, tamper amd64 child manifest bytes", idxD)

	corruptBlobOnDisk(t, dir, amd.manifestD)
	_, err := s.FetchPlatform(context.Background(), "multiarch", Platform{OS: "linux", Architecture: "amd64"})
	if !errors.Is(err, ErrCorrupted) {
		t.Fatalf("want ErrCorrupted for tampered child manifest, got %v", err)
	}
	var de *DigestError
	// 损坏错误以 ErrCorrupted 可区分
	if !errors.Is(err, ErrCorrupted) {
		t.Fatalf("error not classifiable: %v", err)
	}
	t.Logf("judgement basis: child read failed with %v (distinguishable); index fetch aborted", err)
	_ = json.RawMessage(nil)
	_ = de
}

// TestFetchRejectsCorruptedConfig：镜像清单引用的 config 损坏时整次拉取必须失败。
func TestFetchRejectsCorruptedConfig(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	a := buildManifest(t, s, "amd64")
	_ = s.PutTag(context.Background(), "img", a.manifestD)
	t.Logf("input: manifest=%s references config=%s; tamper config on disk",
		a.manifestD, a.configD)
	corruptBlobOnDisk(t, dir, a.configD)
	_, err := s.FetchPlatform(context.Background(), "img", Platform{OS: "linux", Architecture: "amd64"})
	if !errors.Is(err, ErrCorrupted) {
		t.Fatalf("want ErrCorrupted for corrupted config, got %v", err)
	}
	t.Logf("judgement basis: whole fetch rejected with %v; caller cannot use partial result", err)
}

// TestFetchRejectsUnsupportedChildMediaType：索引条目指向一个声明为未知类型的子内容。
func TestFetchRejectsUnsupportedChildMediaType(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()

	child := []byte(`{"weird":true}`)
	childD := putBytes(t, s, child)
	idx := &ImageIndex{
		SchemaVersion: 2,
		MediaType:     MediaTypeOCIIndex,
		Manifests: []Descriptor{{
			MediaType: "application/vnd.unknown.child+json",
			Digest:    childD, Size: int64(len(child)),
			Platform: &Platform{Architecture: "amd64", OS: "linux"},
		}},
	}
	raw, _ := json.Marshal(idx)
	idxD := putBytes(t, s, raw)
	t.Logf("input: index=%s child=%s declared as unknown media type", idxD, childD)
	_, err := s.FetchPlatform(context.Background(), idxD.String(), Platform{OS: "linux", Architecture: "amd64"})
	if !errors.Is(err, ErrMediaType) {
		t.Fatalf("want ErrMediaType for unsupported child, got %v", err)
	}
	t.Logf("judgement basis: unsupported declared media type rejected, not ignored: %v", err)
}
