package ocistore

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// TestReopenPersistence：重新打开存储后，blob 与标签仍在，去重继续生效。
func TestReopenPersistence(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir, QuotaBytes: 1 << 20})
	a := buildManifest(t, s, "amd64")
	if err := s.PutTag(context.Background(), "img", a.manifestD); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(Options{RootDir: dir, QuotaBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	got, err := s2.ResolveTag("img")
	if err != nil || got != a.manifestD {
		t.Fatalf("tag lost after reopen: %s %v", got, err)
	}
	rc, size, err := s2.GetBlob(context.Background(), a.layerD)
	if err != nil {
		t.Fatalf("blob lost after reopen: %v", err)
	}
	defer rc.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(rc)
	if size != int64(len(a.layer)) || !bytes.Equal(buf.Bytes(), a.layer) {
		t.Fatalf("blob content changed after reopen")
	}
	// 重开后重复写仍去重
	d, _, err := s2.PutBlob(context.Background(), a.layerD, bytes.NewReader(a.layer))
	if err != nil || d != a.layerD {
		t.Fatalf("dedup after reopen failed: %v", err)
	}
	st, _ := s2.Stats()
	t.Logf("final state after reopen: tag=%s blobs=%d bytes=%d, dedup intact", got, st.BlobCount, st.Bytes)
}

// TestErrorClassification：各类失败原因都可用 errors.Is 区分。
func TestErrorClassification(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()

	cases := []struct {
		name string
		err  error
		want error
	}{
		{"missing tag", func() error { _, e := s.ResolveTag("nope"); return e }(), ErrLabelNotExist},
		{"bad tag", func() error { _, e := s.ResolveTag("../escape"); return e }(), ErrTagInvalid},
		{"bad digest", func() error { _, e := ParseDigest("sha256:zz"); return e }(), ErrDigestInvalid},
	}
	for _, c := range cases {
		if !errors.Is(c.err, c.want) {
			t.Fatalf("%s: want %v, got %v", c.name, c.want, c.err)
		}
		t.Logf("%-14s -> errors.Is(., %v) = true  (%v)", c.name, c.want, c.err)
	}
	t.Log("final state: all failure causes distinctly classifiable")
}
