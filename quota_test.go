package ocistore

import (
	"context"
	"errors"
	"os"
	"testing"
)

// TestQuotaRejectsAndKeepsExisting：超配额写入被明确拒绝，
// 已有数据保持可用，且不留临时文件、配额不泄漏。
func TestQuotaRejectsAndKeepsExisting(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Options{QuotaBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	first := make([]byte, 60)
	for i := range first {
		first[i] = 'a'
	}
	d1, err := s.PutBlobBytes(ctx, MediaTypeOCILayer, first)
	if err != nil {
		t.Fatalf("配额内写入应成功: %v", err)
	}

	// 再写 50 字节会使 60+50=110 > 100，必须拒绝。
	second := make([]byte, 50)
	_, qerr := s.PutBlobBytes(ctx, MediaTypeOCILayer, second)
	tLog(t, "quota-exceed",
		"quota", 100, "used", 60, "required", 50,
		"错误", qerr,
		"判定依据", "errors.Is(ErrQuotaExceeded)")
	if !errors.Is(qerr, ErrQuotaExceeded) {
		t.Fatalf("超配额必须返回 ErrQuotaExceeded, got %v", qerr)
	}
	var qe *QuotaExceededError
	if !errors.As(qerr, &qe) {
		t.Fatal("应可 As 为 *QuotaExceededError")
	}
	if qe.Quota != 100 || qe.Used != 60 || qe.Required != 50 {
		t.Fatalf("配额错误细节错误: %+v", qe)
	}

	// 已有数据仍可用。
	rc, _, err := s.GetBlob(ctx, d1.Digest)
	if err != nil {
		t.Fatalf("超配额拒绝后已有数据不可用: %v", err)
	}
	rc.Close()

	// used 不得因失败写入而变化（配额不泄漏）。
	if used, quota := s.Usage(ctx); used != 60 || quota != 100 {
		t.Fatalf("失败后 used=%d quota=%d, 期望 60/100", used, quota)
	}
	// 不得遗留临时文件。
	if entries, _ := os.ReadDir(tmpDir(dir)); len(entries) != 0 {
		t.Fatalf("失败写入遗留 %d 个临时文件", len(entries))
	}
}

// TestQuotaExactBoundary：恰好等于配额应成功，超出 1 字节失败。
func TestQuotaExactBoundary(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Options{QuotaBytes: 40})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	if _, err := s.PutBlobBytes(ctx, MediaTypeOCILayer, makeBytes(40)); err != nil {
		t.Fatalf("恰好等于配额应成功: %v", err)
	}
	_, err = s.PutBlobBytes(ctx, MediaTypeOCILayer, []byte("x"))
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("超出 1 字节必须拒绝, got %v", err)
	}
	// 不同内容的小 blob 仍计入；相同内容去重不受配额影响（重写成功）。
	if _, err := s.PutBlobBytes(ctx, MediaTypeOCILayer, makeBytes(40)); err != nil {
		t.Fatalf("重复写入相同内容即使配额已满也应幂等成功: %v", err)
	}
	tLog(t, "quota-boundary", "quota", 40, "结论", "=40 成功，41 拒绝，去重重写成功")
}

// TestQuotaNoPartialStateOnFailure：构造失败场景，确认无部分文件残留。
func TestQuotaNoPartialStateOnFailure(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, Options{QuotaBytes: 10})
	defer s.Close()

	_, err := s.PutBlobBytes(context.Background(), MediaTypeOCILayer, makeBytes(50))
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("应被配额拒绝, got %v", err)
	}
	// blobs 目录下不得有任何本体文件（临时文件在 tmp 中且应已清理）。
	if n := countBlobFiles(t, dir); n != 0 {
		t.Fatalf("失败后遗留 %d 个 blob 本体", n)
	}
	if entries, _ := os.ReadDir(tmpDir(dir)); len(entries) != 0 {
		t.Fatalf("失败后遗留临时文件")
	}
}
