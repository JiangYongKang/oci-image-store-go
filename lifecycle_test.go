package ocistore

import (
	"context"
	"errors"
	"io"
	"testing"
)

// TestReopenPersistence：关闭重开后，内容、标签、用量都应保持一致。
func TestReopenPersistence(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	s1, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	img := buildImage(t, s1, MediaTypeOCIManifest, "persist-layer")
	if _, err := s1.PutManifest(ctx, "keep", MediaTypeOCIManifest, img.manifest); err != nil {
		t.Fatal(err)
	}
	used1, _ := s1.Usage(ctx)
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	used2, _ := s2.Usage(ctx)
	tLog(t, "reopen", "usedBefore", used1, "usedAfter", used2,
		"manifestExists", s2.Exists(ctx, img.manifestD.Digest))
	if used2 != used1 {
		t.Fatalf("重开后用量=%d, 期望 %d", used2, used1)
	}
	got, desc, err := s2.GetManifest(ctx, "keep")
	if err != nil {
		t.Fatalf("重开后标签解析失败: %v", err)
	}
	if digestBytes(got) != desc.Digest {
		t.Fatal("重开后内容摘要不一致")
	}
}

// TestGCReclaimsQuota：回收后 used 下降，且可再写入此前超配额的内容。
func TestGCReclaimsQuota(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Options{QuotaBytes: 200})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	big := makeBytes(150)
	dBig, err := s.PutBlobBytes(ctx, MediaTypeOCILayer, big)
	if err != nil {
		t.Fatal(err)
	}
	// 给它一个标签，再删标签，使它成为唯一不可达内容。
	if err := s.Tag(ctx, "temp", dBig.Digest); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTag(ctx, "temp"); err != nil {
		t.Fatal(err)
	}

	rep, err := s.GC(ctx)
	if err != nil {
		t.Fatal(err)
	}
	used, quota := s.Usage(ctx)
	tLog(t, "gc-reclaim", "reclaimed", rep.BytesReclaimed, "usedAfter", used, "quota", quota)
	if used != 0 {
		t.Fatalf("回收后 used=%d, 期望 0", used)
	}
	// 现在重新写入 150 字节应成功（此前配额下若未回收则会失败）。
	if _, err := s.PutBlobBytes(ctx, MediaTypeOCILayer, big); err != nil {
		t.Fatalf("回收释放配额后写入应成功: %v", err)
	}
}

// TestTagLifecycle：打标签、列举、删除、删除不存在标签报错。
func TestTagLifecycle(t *testing.T) {
	s, _ := newTestStore(t, 0)
	ctx := context.Background()
	img := buildImage(t, s, MediaTypeOCIManifest, "L")
	if _, err := s.PutManifest(ctx, "v1.0", MediaTypeOCIManifest, img.manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutManifest(ctx, "latest", MediaTypeOCIManifest, img.manifest); err != nil {
		t.Fatal(err)
	}

	tags, err := s.Tags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 {
		t.Fatalf("标签数=%d, 期望 2 (%v)", len(tags), tags)
	}

	if err := s.DeleteTag(ctx, "v1.0"); err != nil {
		t.Fatal(err)
	}
	err = s.DeleteTag(ctx, "v1.0")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除不存在标签应 ErrNotFound, got %v", err)
	}
	if _, err := s.ResolveTag(ctx, "v1.0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("已删标签解析应 ErrNotFound, got %v", err)
	}
	// latest 仍在。
	if _, err := s.ResolveTag(ctx, "latest"); err != nil {
		t.Fatalf("未删标签应仍可解析: %v", err)
	}
	tLog(t, "tag-lifecycle", "tags", tags, "结论", "v1.0 删除，latest 保留")
}

// TestGetManifestByDigestStable：直接按摘要读取不受标签变化影响。
func TestGetManifestByDigestStable(t *testing.T) {
	s, _ := newTestStore(t, 0)
	ctx := context.Background()
	a := buildImageCustom(t, s, `{"v":"a"}`, mustPut(t, s, MediaTypeOCILayer, []byte("la")))
	b := buildImageCustom(t, s, `{"v":"b"}`, mustPut(t, s, MediaTypeOCILayer, []byte("lb")))
	da, _ := s.PutManifest(ctx, "x", MediaTypeOCIManifest, a)
	_, _ = s.PutManifest(ctx, "x", MediaTypeOCIManifest, b) // 迁移标签

	got, desc, err := s.GetManifest(ctx, string(da.Digest))
	if err != nil {
		t.Fatalf("按旧摘要读取失败: %v", err)
	}
	if !bytesEqual(got, a) {
		t.Fatal("按摘要读取到的内容被标签迁移影响")
	}
	_ = desc
	tLog(t, "digest-stable", "digest", da.Digest, "结论", "标签迁移不改变摘要固定内容")
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var _ = io.EOF
