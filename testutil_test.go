package ocistore

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// testArtifacts 聚合一次造数的内容与摘要，便于断言。
type testArtifacts struct {
	config      []byte
	configD     Digest
	configDesc  Descriptor
	layer       []byte
	layerD      Digest
	layerDesc   Descriptor
	manifest    []byte
	manifestD   Digest
	manifestObj *ImageManifest
}

func putBytes(t *testing.T, s *Store, data []byte) Digest {
	t.Helper()
	d := ComputeDigest(data)
	got, _, err := s.PutBlob(context.Background(), d, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("PutBlob %s: %v", d, err)
	}
	return got
}

func blobBytes(s *Store, d Digest) []byte {
	rc, _, err := s.GetBlob(context.Background(), d)
	if err != nil {
		return nil
	}
	defer rc.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(rc)
	return buf.Bytes()
}

// buildManifest 造一个 config+单 layer 的镜像清单并写入全部 blob，返回构件。
func buildManifest(t *testing.T, s *Store, arch string) testArtifacts {
	t.Helper()
	cfg := []byte(`{"architecture":"` + arch + `","os":"linux"}`)
	cfgD := putBytes(t, s, cfg)
	layer := []byte("layer-data-for-" + arch)
	layerD := putBytes(t, s, layer)

	m := &ImageManifest{
		SchemaVersion: 2,
		MediaType:     MediaTypeOCIManifest,
		Config:        Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: cfgD, Size: int64(len(cfg))},
		Layers:        []Descriptor{{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Digest: layerD, Size: int64(len(layer))}},
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	mD := putBytes(t, s, raw)
	var parsed ImageManifest
	_ = json.Unmarshal(raw, &parsed)
	return testArtifacts{
		config: cfg, configD: cfgD,
		configDesc: m.Config,
		layer:      layer, layerD: layerD, layerDesc: m.Layers[0],
		manifest: raw, manifestD: mD, manifestObj: &parsed,
	}
}

// buildManifestWithLayer 使用指定共享层构造一个镜像清单。
func buildManifestWithLayer(t *testing.T, s *Store, arch string, layer []byte, layerD Digest) testArtifacts {
	t.Helper()
	cfg := []byte(`{"architecture":"` + arch + `","os":"linux"}`)
	cfgD := putBytes(t, s, cfg)
	m := &ImageManifest{
		SchemaVersion: 2,
		MediaType:     MediaTypeOCIManifest,
		Config:        Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: cfgD, Size: int64(len(cfg))},
		Layers:        []Descriptor{{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Digest: layerD, Size: int64(len(layer))}},
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	mD := putBytes(t, s, raw)
	var parsed ImageManifest
	_ = json.Unmarshal(raw, &parsed)
	return testArtifacts{
		config: cfg, configD: cfgD, configDesc: m.Config,
		layer: layer, layerD: layerD, layerDesc: m.Layers[0],
		manifest: raw, manifestD: mD, manifestObj: &parsed,
	}
}

// buildIndex 造一个多架构索引（含 amd64/arm64 两个平台）并打标签。
func buildIndex(t *testing.T, s *Store, tag string) (Digest, testArtifacts, testArtifacts) {
	t.Helper()
	amd := buildManifest(t, s, "amd64")
	arm := buildManifest(t, s, "arm64")
	idx := &ImageIndex{
		SchemaVersion: 2,
		MediaType:     MediaTypeOCIIndex,
		Manifests: []Descriptor{
			{MediaType: MediaTypeOCIManifest, Digest: amd.manifestD, Size: int64(len(amd.manifest)),
				Platform: &Platform{Architecture: "amd64", OS: "linux"}},
			{MediaType: MediaTypeOCIManifest, Digest: arm.manifestD, Size: int64(len(arm.manifest)),
				Platform: &Platform{Architecture: "arm64", OS: "linux", Variant: "v8"}},
		},
	}
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
	return idxD, amd, arm
}

// corruptBlobOnDisk 直接改写磁盘上的 blob 字节，模拟位腐烂/被外部篡改。
func corruptBlobOnDisk(t *testing.T, root string, d Digest) {
	t.Helper()
	p := blobPath(root, d)
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("CORRUPTED-BYTES"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func countFiles(t *testing.T, root, sub string) int {
	t.Helper()
	n := 0
	_ = filepath.Walk(filepath.Join(root, sub), func(path string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			n++
		}
		return nil
	})
	return n
}
