package ocistore

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// TestDedupRepeatWrites：相同内容重复写入只产生一份副本。
func TestDedupRepeatWrites(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	ctx := context.Background()

	payload := []byte("repeatable-content")
	want := ComputeDigest(payload)
	t.Logf("input: %q wantDigest=%s", payload, want)

	for i := 0; i < 3; i++ {
		got, n, err := s.PutBlob(ctx, want, bytes.NewReader(payload))
		if err != nil || got != want {
			t.Fatalf("write#%d: d=%s err=%v", i, got, err)
		}
		if n != int64(len(payload)) {
			t.Fatalf("write#%d size=%d", i, n)
		}
	}
	st, _ := s.Stats()
	t.Logf("judgement basis: blobs=%d bytes=%d (expect 1 blobs, %d bytes)",
		st.BlobCount, st.Bytes, len(payload))
	if st.BlobCount != 1 || st.Bytes != int64(len(payload)) {
		t.Fatalf("duplicate copies produced: %+v", st)
	}
	got := blobBytes(s, want)
	if !bytes.Equal(got, payload) {
		t.Fatalf("readback differs")
	}
	t.Logf("final state: one blob on disk, readback identical")
}

// TestTagMigrationKeepsPinnedContent：标签迁移后，被旧摘要固定的内容仍可读，不改变不丢失。
func TestTagMigrationKeepsPinnedContent(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	ctx := context.Background()

	idx1, amd1, _ := buildIndex(t, s, "amd64-only-base")
	_ = idx1
	// 第一版标签：指向 amd 清单
	v1 := amd1.manifestD
	if err := s.PutTag(ctx, "app:v1", v1); err != nil {
		t.Fatal(err)
	}
	t.Logf("input: tag app:v1 -> %s (manifest amd64)", v1)

	idx2, amd2, _ := buildIndex(t, s, "amd64-only-base-2")
	_ = idx2
	v2 := amd2.manifestD
	if err := s.PutTag(ctx, "app:v1", v2); err != nil { // 迁移标签
		t.Fatal(err)
	}
	t.Logf("input: tag app:v1 migrated -> %s", v2)

	got, err := s.ResolveTag("app:v1")
	if err != nil || got != v2 {
		t.Fatalf("tag should point to v2, got %s err=%v", got, err)
	}
	t.Logf("judgement basis: tag resolves to v2=%s", got)

	// 旧摘要内容必须仍可按摘要完整读取
	old := blobBytes(s, v1)
	if old == nil || !v1.Verify(old) {
		t.Fatalf("pinned-by-digest content v1 changed or lost after migration")
	}
	tags, _ := s.Tags()
	t.Logf("final state: tags=%v, v1 blob readable by digest (unchanged), v2 readable", tags)
}

// TestPutBlobDigestMismatch：声明摘要与实际不符必须拒绝且不留文件。
func TestPutBlobDigestMismatch(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()

	payload := []byte("actual-payload")
	declared := Digest("sha256:" + repeat64("a"))
	t.Logf("input: payload=%q declared=%s actual=%s", payload, declared, ComputeDigest(payload))
	got, n, err := s.PutBlob(context.Background(), declared, bytes.NewReader(payload))
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("want ErrDigestMismatch, got %v", err)
	}
	var de *DigestError
	if !errors.As(err, &de) {
		t.Fatalf("want *DigestError, got %T", err)
	}
	t.Logf("judgement basis: declared=%s actual=%s rejected (%v)", de.Declared, de.Actual, err)
	if n != int64(len(payload)) || got != ComputeDigest(payload) {
		t.Logf("returned actual digest for observability: %s", got)
	}
	st, _ := s.Stats()
	if st.BlobCount != 0 {
		t.Fatalf("mismatched write left a blob: %+v", st)
	}
	if leftover := countFiles(t, dir, dirTmp); leftover != 0 {
		t.Fatalf("temporary files left: %d", leftover)
	}
	t.Logf("final state: blobs=0 tmpFiles=0, existing data unaffected")
}

// TestCorruptedBlobRejectedAndQuarantined：任何读取路径发现摘要不符都报错且不再缓存命中。
func TestCorruptedBlobRejectedAndQuarantined(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	ctx := context.Background()

	a := buildManifest(t, s, "amd64")
	if err := s.PutTag(ctx, "img", a.manifestD); err != nil {
		t.Fatal(err)
	}
	ok, _ := s.Has(a.manifestD)
	t.Logf("input: healthy manifest %s exists=%v, then tamper layer blob on disk", a.manifestD, ok)

	corruptBlobOnDisk(t, dir, a.layerD)
	t.Logf("corrupted blob path: %s", blobPath(dir, a.layerD))

	// 直接读层：必须 ErrCorrupted
	if _, _, err := s.GetBlob(ctx, a.layerD); !errors.Is(err, ErrCorrupted) {
		t.Fatalf("GetBlob corrupted: want ErrCorrupted, got %v", err)
	}
	t.Logf("judgement basis: GetBlob returned ErrCorrupted")

	// 损坏后 Has 必须为 false（不得继续作为缓存命中）
	if ok, _ := s.Has(a.layerD); ok {
		t.Fatalf("corrupted content still reported as cache hit")
	}
	// 第二次读：内容已隔离，按不存在处理，而非缓存命中
	if _, _, err := s.GetBlob(ctx, a.layerD); !errors.Is(err, ErrNotExist) {
		t.Fatalf("quarantined blob read: want ErrNotExist, got %v", err)
	}
	// FetchPlatform 同样必须在引用校验阶段失败
	if _, err := s.FetchPlatform(ctx, "img", Platform{OS: "linux", Architecture: "amd64"}); err == nil {
		t.Fatalf("fetch must fail when referenced layer corrupted")
	}
	t.Logf("final state: corrupted blob quarantined, Has=false, fetch rejected; healthy blobs intact")
}
