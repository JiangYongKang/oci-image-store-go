package ocistore

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tLog 统一打印“输入 / 判定依据 / 最终状态”，满足单测日志可追溯要求。
func tLog(t *testing.T, stage string, kv ...any) {
	t.Helper()
	t.Logf("[%s]", stage)
	for i := 0; i+1 < len(kv); i += 2 {
		t.Logf("    %v = %v", kv[i], kv[i+1])
	}
}

// newTestStore 在临时目录打开一个可选配额的存储。
func newTestStore(t *testing.T, quota int64) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir, Options{QuotaBytes: quota})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}

func mustPut(t *testing.T, s *Store, mediaType string, data []byte) Descriptor {
	t.Helper()
	d, err := s.PutBlobBytes(context.Background(), mediaType, data)
	if err != nil {
		t.Fatalf("PutBlobBytes: %v", err)
	}
	return d
}

func mustReadBlob(t *testing.T, s *Store, d Digest) []byte {
	t.Helper()
	rc, _, err := s.GetBlob(context.Background(), d)
	if err != nil {
		t.Fatalf("GetBlob(%s): %v", d, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read blob %s: %v", d, err)
	}
	return b
}

// buildImage 构造 config/layers 与一份单平台清单，全部写入存储。
type builtImage struct {
	configDesc Descriptor
	layers     []Descriptor
	manifest   []byte
	manifestD  Descriptor
}

func buildImage(t *testing.T, s *Store, media string, layerPayloads ...string) builtImage {
	t.Helper()
	cfg := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers"}}`)
	cfgDesc := mustPut(t, s, MediaTypeOCIConfig, cfg)

	var layers []Descriptor
	for _, p := range layerPayloads {
		layers = append(layers, mustPut(t, s, MediaTypeOCILayer, []byte(p)))
	}

	m := ImageManifest{
		SchemaVersion: 2,
		MediaType:     media,
		Config:        cfgDesc,
		Layers:        layers,
	}
	mb, _ := json.Marshal(&m)
	mDesc := mustPut(t, s, media, mb)
	return builtImage{configDesc: cfgDesc, layers: layers, manifest: mb, manifestD: mDesc}
}

// buildIndex 构造一个多架构索引并写入存储，返回索引原文与描述符。
func buildIndex(t *testing.T, s *Store, media string, entries ...Descriptor) ([]byte, Descriptor) {
	t.Helper()
	idx := Index{SchemaVersion: 2, MediaType: media, Manifests: entries}
	b, _ := json.Marshal(&idx)
	d := mustPut(t, s, media, b)
	return b, d
}

// buildImageCustom 构造使用自定义 config 内容与给定层的镜像清单并写入。
func buildImageCustom(t *testing.T, s *Store, configJSON string, layers ...Descriptor) []byte {
	t.Helper()
	cfgDesc := mustPut(t, s, MediaTypeOCIConfig, []byte(configJSON))
	m := ImageManifest{
		SchemaVersion: 2,
		MediaType:     MediaTypeOCIManifest,
		Config:        cfgDesc,
		Layers:        layers,
	}
	mb, _ := json.Marshal(&m)
	_ = mustPut(t, s, MediaTypeOCIManifest, mb)
	return mb
}

// buildImageWithLayers 构造一份复用给定层的镜像清单并写入，返回清单原文。
func buildImageWithLayers(t *testing.T, s *Store, layers ...Descriptor) []byte {
	t.Helper()
	cfg := []byte(`{"architecture":"amd64","os":"linux"}`)
	cfgDesc := mustPut(t, s, MediaTypeOCIConfig, cfg)
	m := ImageManifest{
		SchemaVersion: 2,
		MediaType:     MediaTypeOCIManifest,
		Config:        cfgDesc,
		Layers:        layers,
	}
	mb, _ := json.Marshal(&m)
	_ = mustPut(t, s, MediaTypeOCIManifest, mb)
	return mb
}

// corruptBlobOnDisk 直接覆写磁盘上的 blob 文件，模拟内容损坏/被截断。
func corruptBlobOnDisk(t *testing.T, s *Store, d Digest, payload []byte) {
	t.Helper()
	p, _, err := s.digestPaths(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, payload, 0o644); err != nil {
		t.Fatal(err)
	}
}

// blobFileExists 直接检查磁盘文件是否存在。
func blobFileExists(t *testing.T, s *Store, d Digest) bool {
	t.Helper()
	p, _, err := s.digestPaths(d)
	if err != nil {
		t.Fatal(err)
	}
	_, err = os.Stat(p)
	return !os.IsNotExist(err)
}

func quarantineCount(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "quarantine"))
	if err != nil {
		return 0
	}
	return len(entries)
}

// jsonMarshal 供测试构造清单。
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// filepathWalk 遍历 blob 目录，回调第二个参数表示文件名是否带扩展名。
func filepathWalk(root string, fn func(path string, hasExt bool)) error {
	return filepath.WalkDir(root, func(path string, de fs.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return nil
		}
		fn(path, strings.Contains(filepath.Base(path), "."))
		return nil
	})
}
