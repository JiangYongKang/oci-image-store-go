package ocistore

import (
	"context"
	"errors"
	"testing"
)

// TestLeaseProtectsFromGC：被活跃租约 Pin 的内容，即使无标签也不会被 GC 回收；
// 租约释放后再 GC 才回收。
func TestLeaseProtectsFromGC(t *testing.T) {
	s, _ := newTestStore(t, 0)
	ctx := context.Background()

	// 一个没有任何标签的孤立层。
	d := mustPut(t, s, MediaTypeOCILayer, []byte("orphan-but-pinned"))

	lease := s.NewLease()
	if err := lease.Pin(ctx, d.Digest); err != nil {
		t.Fatal(err)
	}

	rep, err := s.GC(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tLog(t, "lease-protect",
		"pinned", d.Digest,
		"gcDeletedWhilePinned", len(rep.Deleted),
		"exists", s.Exists(ctx, d.Digest))
	if !s.Exists(ctx, d.Digest) {
		t.Fatal("被活跃租约固定的内容被 GC 误删")
	}

	lease.Release()
	rep2, err := s.GC(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Exists(ctx, d.Digest) {
		t.Fatal("租约释放后，无引用内容应被回收")
	}
	if len(rep2.Deleted) != 1 {
		t.Fatalf("第二次 GC 应回收 1 项, got %d", len(rep2.Deleted))
	}
}

// TestLeaseDoubleReleaseAndPinAfterRelease：重复 Release 安全；
// Release 后再 Pin 返回 ErrLeaseReleased。
func TestLeaseDoubleReleaseAndPinAfterRelease(t *testing.T) {
	s, _ := newTestStore(t, 0)
	d := mustPut(t, s, MediaTypeOCILayer, []byte("x"))
	l := s.NewLease()
	l.Release()
	l.Release() // 不 panic
	err := l.Pin(context.Background(), d.Digest)
	if !errors.Is(err, ErrLeaseReleased) {
		t.Fatalf("Release 后 Pin 必须 ErrLeaseReleased, got %v", err)
	}
}

// TestPinMissingContent：Pin 不存在的摘要必须 ErrNotFound。
func TestPinMissingContent(t *testing.T) {
	s, _ := newTestStore(t, 0)
	l := s.NewLease()
	defer l.Release()
	missing := Digest("sha256:" + repeat('9', 64))
	if err := l.Pin(context.Background(), missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Pin 缺失内容必须 ErrNotFound, got %v", err)
	}
}

// TestGCNoResurrection：GC 删除不可达内容后，该摘要重新可用的唯一途径
// 是重新写入；旧标签指针不会让已删数据“复活”。
func TestGCNoResurrection(t *testing.T) {
	s, _ := newTestStore(t, 0)
	ctx := context.Background()

	img := buildImageCustom(t, s, `{"v":1}`, mustPut(t, s, MediaTypeOCILayer, []byte("r-layer")))
	d, err := s.PutManifest(ctx, "ephemeral", MediaTypeOCIManifest, img)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTag(ctx, "ephemeral"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GC(ctx); err != nil {
		t.Fatal(err)
	}
	// 已删除：按摘要读取必须 NotFound，不会“复活”。
	if _, _, err := s.GetBlob(ctx, d.Digest); !errors.Is(err, ErrNotFound) {
		t.Fatalf("已 GC 内容必须 NotFound, got %v", err)
	}
	// 重新显式打标签到已删摘要必须失败（不能凭空复活）。
	if err := s.Tag(ctx, "ghost", d.Digest); !errors.Is(err, ErrNotFound) {
		t.Fatalf("指向已删内容打标签必须 ErrNotFound, got %v", err)
	}
	// 重新写入相同内容后，摘要再次可用（这是合法的重新引入，而非复活）。
	if _, err := s.PutManifest(ctx, "reborn", MediaTypeOCIManifest, img); err != nil {
		t.Fatalf("重新写入应成功: %v", err)
	}
	if !s.Exists(ctx, d.Digest) {
		t.Fatal("重新写入后摘要应再次存在")
	}
	tLog(t, "no-resurrection", "digest", d.Digest, "结论", "删除后不复活，仅重写可重新引入")
}
