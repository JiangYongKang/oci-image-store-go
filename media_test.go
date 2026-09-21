package ocistore

import (
	"context"
	"errors"
	"testing"
)

// TestUnsupportedMediaTypeRejected：未知媒体类型必须被明确拒绝，而不是忽略/猜测。
func TestUnsupportedMediaTypeRejected(t *testing.T) {
	s, _ := newTestStore(t, 0)
	ctx := context.Background()

	// 一个结构像清单、但 mediaType 不被支持的负载。
	payload := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.example.weird+json","manifests":[]}`)
	_, err := s.PutManifest(ctx, "weird", "application/vnd.example.weird+json", payload)
	tLog(t, "unknown-media", "mediaType", "application/vnd.example.weird+json", "错误", err)
	if !errors.Is(err, ErrUnsupportedMediaType) {
		t.Fatalf("未知媒体类型必须返回 ErrUnsupportedMediaType, got %v", err)
	}
	var um *UnsupportedMediaTypeError
	if !errors.As(err, &um) {
		t.Fatal("应可 As 为 *UnsupportedMediaTypeError")
	}
}

// TestExtraFieldsTolerated：清单包含未知额外字段时解析结论仍确定。
func TestExtraFieldsTolerated(t *testing.T) {
	s, _ := newTestStore(t, 0)
	ctx := context.Background()

	payload := []byte(`{
		"schemaVersion": 2,
		"mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": {"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:` + repeat('c', 64) + `","size":10},
		"layers": [],
		"futureUnknownField": {"nested": [1,2,3]},
		"anotherFuture": "value"
	}`)
	// config blob 也需要存在（PutManifest 不强制子内容存在，但解析路径会；
	// 这里仅验证 PutManifest 的解析兼容性）。
	rm, err := parseManifest(payload, MediaTypeOCIManifest)
	if err != nil {
		t.Fatalf("额外字段应被容忍: %v", err)
	}
	if rm.kind != kindImage {
		t.Fatal("应被确定地解析为 image manifest")
	}
	if rm.image.Config.Digest.Encoded() != repeat('c', 64) {
		t.Fatal("额外字段不得影响已知字段解析")
	}

	// 端到端：写入与读取保持一致。
	_, _ = s.PutBlobBytes(ctx, MediaTypeOCIConfig, makeBytes(10))
	d, err := s.PutManifest(ctx, "future", MediaTypeOCIManifest, payload)
	if err != nil {
		t.Fatalf("含额外字段清单应允许写入: %v", err)
	}
	got, _, err := s.GetManifest(ctx, "future")
	if err != nil {
		t.Fatal(err)
	}
	if digestBytes(got) != d.Digest {
		t.Fatal("读取摘要不一致")
	}
	tLog(t, "extra-fields", "digest", d.Digest, "结论", "accepted as image manifest")
}

// TestDockerMediaTypesAccepted：Docker v2 旧格式清单应被接受为受支持格式。
func TestDockerMediaTypesAccepted(t *testing.T) {
	s, _ := newTestStore(t, 0)
	ctx := context.Background()

	img := buildImage(t, s, MediaTypeDockerManifest, "docker-layer")
	ma := buildMultiArch(t, s, MediaTypeDockerIndex, MediaTypeDockerManifest, MediaTypeDockerManifest)

	if _, err := s.PutManifest(ctx, "dimg", MediaTypeDockerManifest, img.manifest); err != nil {
		t.Fatalf("Docker manifest 应被接受: %v", err)
	}
	if _, err := s.PutManifest(ctx, "didx", MediaTypeDockerIndex, ma.indexData); err != nil {
		t.Fatalf("Docker manifest list 应被接受: %v", err)
	}
	r, err := s.ResolvePlatform(ctx, "didx", Platform{OS: "linux", Architecture: "amd64"})
	if err != nil {
		t.Fatalf("Docker 索引应能解析平台: %v", err)
	}
	defer r.Release()
	if r.Manifest.Digest != ma.amdD {
		t.Fatalf("Docker 索引平台解析错误: %s", r.Manifest.Digest)
	}
	tLog(t, "docker-compat", "选中", r.Manifest.Digest, "结论", "docker v2 formats accepted")
}

// TestTrailingJSONRejected：尾随垃圾数据必须按截断/损坏拒绝。
func TestTrailingJSONRejected(t *testing.T) {
	payload := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:` + repeat('c', 64) + `","size":1},"layers":[]} GARBAGE`)
	_, err := parseManifest(payload, MediaTypeOCIManifest)
	if !errors.Is(err, ErrManifestTruncated) {
		t.Fatalf("尾随数据必须按 malformed(ErrManifestTruncated) 拒绝, got %v", err)
	}
}

func makeBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('x')
	}
	return b
}
