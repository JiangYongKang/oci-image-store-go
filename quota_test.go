package ocistore

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// TestQuotaRejectsOversize：超配额写入被明确拒绝（ErrQuotaExceeded），
// 且不落临时文件、不影响已有数据。
func TestQuotaRejectsOversize(t *testing.T) {
	dir := t.TempDir()
	// 配额只够放 4 字节
	s, _ := Open(Options{RootDir: dir, QuotaBytes: 4})
	defer s.Close()
	ctx := context.Background()

	small := []byte("ab")
	smallD := putBytes(t, s, small)
	t.Logf("input: quota=4, wrote %q (%s, %d bytes)", small, smallD, len(small))

	big := []byte("this-payload-is-way-too-big")
	bigD := ComputeDigest(big)
	_, _, err := s.PutBlob(ctx, bigD, bytes.NewReader(big))
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("want ErrQuotaExceeded, got %v", err)
	}
	t.Logf("judgement basis: oversize write rejected: %v", err)

	st, _ := s.Stats()
	if st.Bytes != 2 {
		t.Fatalf("quota failure changed usage: %+v", st)
	}
	if leftover := countFiles(t, dir, dirTmp); leftover != 0 {
		t.Fatalf("temporary state left after quota rejection: %d files", leftover)
	}
	// 既有数据必须仍可完整读取
	if got := blobBytes(s, smallD); !bytes.Equal(got, small) {
		t.Fatalf("existing data unavailable after failed write")
	}
	t.Logf("final state: bytes=%d tmpFiles=0, existing blob readable", st.Bytes)
}

// TestQuotaDedupHitNotCharged：相同内容重复写入不重复计费。
func TestQuotaDedupHitNotCharged(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir, QuotaBytes: 8})
	defer s.Close()
	ctx := context.Background()
	data := []byte("payload8") // 恰好 8 字节
	d := ComputeDigest(data)

	if _, _, err := s.PutBlob(ctx, d, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PutBlob(ctx, d, bytes.NewReader(data)); err != nil {
		t.Fatalf("dedup hit must not be re-charged against quota: %v", err)
	}
	st, _ := s.Stats()
	t.Logf("input: quota=8, two writes of same 8-byte content -> bytes=%d blobs=%d", st.Bytes, st.BlobCount)
	if st.Bytes != 8 || st.BlobCount != 1 {
		t.Fatalf("dedup charged incorrectly: %+v", st)
	}
	t.Logf("final state: quota fully used by one unique blob")
}

// TestQuotaTagMigrationUnaffectedByContentSize：标签文件极小，迁移不复制大内容。
func TestQuotaTagMigrationUnaffectedByContentSize(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir, QuotaBytes: 1 << 20})
	defer s.Close()
	ctx := context.Background()
	a := buildManifest(t, s, "amd64")
	b := buildManifest(t, s, "arm64")
	before, _ := s.Stats()
	if err := s.PutTag(ctx, "img", a.manifestD); err != nil {
		t.Fatal(err)
	}
	if err := s.PutTag(ctx, "img", b.manifestD); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Stats()
	t.Logf("input: tag migrated %s -> %s; blob bytes before=%d after=%d",
		a.manifestD, b.manifestD, before.Bytes, after.Bytes)
	if after.Bytes != before.Bytes {
		t.Fatalf("tag migration must not duplicate content bytes")
	}
	got, _ := s.ResolveTag("img")
	if got != b.manifestD {
		t.Fatalf("migration result %s", got)
	}
	t.Logf("final state: tag points to %s, blob storage unchanged", got)
}
