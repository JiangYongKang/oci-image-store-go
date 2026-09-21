package ocistore

import (
	"context"
	"testing"
)

// TestQuotaAccountingAfterCorruptAndRewrite：损坏隔离释放占用，
// 重写修复不双重计数；关闭重开后用量与 blobs/ 实际一致。
func TestQuotaAccountingAfterCorruptAndRewrite(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Options{QuotaBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	data := []byte("quota-accounting-payload-0123456789")
	desc := mustPut(t, s, MediaTypeOCILayer, data)
	usedAfterPut, _ := s.Usage(ctx)

	// 等长篡改 -> 读取触发隔离（释放原占用）。
	corruptBlobOnDisk(t, s, desc.Digest, []byte("QUOTA-ACCOUNTING-PAYLOAD-9876543210"))
	rc, _, gerr := s.GetBlob(ctx, desc.Digest)
	if gerr == nil {
		_ = func() error { _, e := readAllOf(rc); return e }()
	}
	if rc != nil {
		// 若入口未拦截，则读完以触发 EOF 校验并隔离。
		_, _ = readAllOf(rc)
		_ = rc.Close()
	}
	usedAfterQuarantine, _ := s.Usage(ctx)
	if usedAfterQuarantine != 0 {
		t.Fatalf("隔离后 used=%d, 期望 0（损坏内容移出 blobs/）", usedAfterQuarantine)
	}

	// 重写修复：用量应恰好为一份，不双重计数。
	desc2, err := s.PutBlobBytes(ctx, MediaTypeOCILayer, data)
	if err != nil {
		t.Fatal(err)
	}
	usedAfterRewrite, _ := s.Usage(ctx)
	tLog(t, "quota-corrupt-rewrite",
		"put", usedAfterPut,
		"quarantine", usedAfterQuarantine,
		"rewrite", usedAfterRewrite,
		"digestSame", desc2.Digest == desc.Digest)
	if usedAfterRewrite != int64(len(data)) {
		t.Fatalf("重写后 used=%d, 期望 %d（不得双重计数）", usedAfterRewrite, len(data))
	}

	// 关闭重开：state.json 不得与 blobs/ 重算结果漂移。
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir, Options{QuotaBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if usedReopen, _ := s2.Usage(ctx); usedReopen != int64(len(data)) {
		t.Fatalf("重开后 used=%d, 期望 %d", usedReopen, len(data))
	}
}

func readAllOf(rc interface {
	Read(p []byte) (int, error)
}) ([]byte, error) {
	var out []byte
	buf := make([]byte, 64)
	for {
		n, err := rc.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			return out, nil // EOF 或校验错误都返回，调用方只关心是否触发隔离
		}
	}
}
