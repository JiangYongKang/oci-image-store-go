package ocistore

import (
	"context"
	"io"
	"testing"
)

func TestSmokeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	data := []byte("hello world blob")
	desc, err := s.PutBlobBytes(ctx, "application/octet-stream", data)
	if err != nil {
		t.Fatal(err)
	}
	if desc.Size != int64(len(data)) {
		t.Fatalf("size = %d, want %d", desc.Size, len(data))
	}

	// 重复写入去重。
	desc2, err := s.PutBlobBytes(ctx, "application/octet-stream", data)
	if err != nil {
		t.Fatal(err)
	}
	if desc2.Digest != desc.Digest {
		t.Fatalf("dedup failed: %s != %s", desc2.Digest, desc.Digest)
	}
	if used, _ := s.Usage(ctx); used != int64(len(data)) {
		t.Fatalf("used = %d, want %d (dedup should not duplicate)", used, len(data))
	}

	rc, _, err := s.GetBlob(ctx, desc.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("content mismatch: %q", got)
	}
}
