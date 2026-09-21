package ocistore

import (
	"context"
	"errors"
	"io"
	"testing"
)

// TestGetBlobCorruptionDetectedAndQuarantined：磁盘内容被篡改后，
// 读取必须在 EOF 报 ErrDigestMismatch，内容被隔离，且不再缓存命中。
func TestGetBlobCorruptionDetectedAndQuarantined(t *testing.T) {
	s, dir := newTestStore(t, 0)
	ctx := context.Background()

	data := []byte("integrity-check-payload")
	desc := mustPut(t, s, MediaTypeOCILayer, data)
	before := blobFileExists(t, s, desc.Digest)

	// 直接篡改磁盘文件（绕过存储 API）。长度级损坏在入口即被发现。
	corruptBlobOnDisk(t, s, desc.Digest, []byte("tampered-payload-content"))

	rc, _, gerr := s.GetBlob(ctx, desc.Digest)
	var rerr error
	if gerr == nil {
		// 若长度恰好相同、仅内容被改，则在读取到 EOF 时才暴露。
		_, rerr = io.ReadAll(rc)
	} else {
		rerr = gerr
	}
	tLog(t, "corrupt-read",
		"declared", desc.Digest,
		"原文件存在", before,
		"读取错误", rerr,
		"判定依据", "errors.Is(ErrDigestMismatch)")
	if !errors.Is(rerr, ErrDigestMismatch) {
		t.Fatalf("损坏内容必须报 ErrDigestMismatch, got %v", rerr)
	}

	// 损坏内容已隔离，不再以原摘要命中。
	if s.Exists(ctx, desc.Digest) {
		t.Fatal("损坏内容不得继续作为缓存命中返回")
	}
	if n := quarantineCount(t, dir); n != 1 {
		t.Fatalf("损坏内容应被隔离 1 份, got %d", n)
	}
}

// TestPutManifestDeclaredDigestMismatch：显式按摘要写入但内容与摘要不符，必须拒绝。
func TestPutManifestDeclaredDigestMismatch(t *testing.T) {
	s, _ := newTestStore(t, 0)
	ctx := context.Background()

	img := buildImage(t, s, MediaTypeOCIManifest, "layer-x")
	fakeDigest := "sha256:" + repeat('a', 64)

	_, err := s.PutManifest(ctx, fakeDigest, MediaTypeOCIManifest, img.manifest)
	tLog(t, "digest-mismatch-write",
		"声明摘要", fakeDigest,
		"实际摘要", img.manifestD.Digest,
		"错误", err)
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("声明摘要不符必须报 ErrDigestMismatch, got %v", err)
	}
	var dm *DigestMismatchError
	if !errors.As(err, &dm) {
		t.Fatal("错误应可 As 为 *DigestMismatchError 以提供细节")
	}
	if dm.Actual != img.manifestD.Digest {
		t.Fatalf("错误中实际摘要=%s, 期望 %s", dm.Actual, img.manifestD.Digest)
	}
}

// TestTruncatedBlobRead：blob 在磁盘上被截断（长度小于声明），读取必须拒绝。
func TestTruncatedBlobRead(t *testing.T) {
	s, dir := newTestStore(t, 0)
	ctx := context.Background()

	data := []byte("0123456789-TRUNCATABLE-PAYLOAD")
	desc := mustPut(t, s, MediaTypeOCILayer, data)
	corruptBlobOnDisk(t, s, desc.Digest, data[:5]) // 截断到前 5 字节

	rc, _, gerr := s.GetBlob(ctx, desc.Digest)
	var rerr error
	if gerr != nil {
		rerr = gerr // 长度截断在入口即被识别
	} else {
		_, rerr = io.ReadAll(rc)
	}
	tLog(t, "truncated-read", "声明长度", desc.Size, "实际长度", 5, "错误", rerr)
	if !errors.Is(rerr, ErrDigestMismatch) {
		t.Fatalf("截断内容必须按摘要/长度不一致拒绝, got %v", rerr)
	}
	if n := quarantineCount(t, dir); n != 1 {
		t.Fatalf("截断内容应被隔离, count=%d", n)
	}
}

func repeat(b byte, n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return string(out)
}

// TestSameLengthTamperDetectedAtEOF：篡改后长度不变，
// meta/大小检查无法发现，必须在读取 EOF 的哈希校验中拒绝并隔离。
func TestSameLengthTamperDetectedAtEOF(t *testing.T) {
	s, dir := newTestStore(t, 0)
	ctx := context.Background()

	data := []byte("ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") // 36 字节
	desc := mustPut(t, s, MediaTypeOCILayer, data)

	// 等长替换：长度完全一致，仅内容不同。
	tampered := []byte("xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
	if len(tampered) != len(data) {
		t.Fatal("测试前置：篡改内容必须等长")
	}
	corruptBlobOnDisk(t, s, desc.Digest, tampered)

	rc, _, gerr := s.GetBlob(ctx, desc.Digest)
	if gerr != nil {
		t.Fatalf("等长篡改不应在入口被发现, got %v", gerr)
	}
	_, rerr := io.ReadAll(rc)
	tLog(t, "same-length-tamper", "长度", len(data), "EOF错误", rerr)
	if !errors.Is(rerr, ErrDigestMismatch) {
		t.Fatalf("等长篡改必须在 EOF 哈希校验时报 ErrDigestMismatch, got %v", rerr)
	}
	if s.Exists(ctx, desc.Digest) {
		t.Fatal("损坏内容不得继续命中")
	}
	if n := quarantineCount(t, dir); n != 1 {
		t.Fatalf("应隔离 1 份, got %d", n)
	}
}

// TestRewriteRepairsCorrupt：损坏后重新写入正确内容应能修复，不永久卡死。
func TestRewriteRepairsCorrupt(t *testing.T) {
	s, dir := newTestStore(t, 0)
	ctx := context.Background()

	data := []byte("repairable-content-payload")
	desc := mustPut(t, s, MediaTypeOCILayer, data)
	corruptBlobOnDisk(t, s, desc.Digest, []byte("broken"))
	if s.Exists(ctx, desc.Digest) {
		t.Fatal("前置：内容应已损坏")
	}

	// 重新写入相同的正确内容。
	desc2, err := s.PutBlobBytes(ctx, MediaTypeOCILayer, data)
	if err != nil {
		t.Fatalf("重写修复失败: %v", err)
	}
	if desc2.Digest != desc.Digest {
		t.Fatalf("修复后摘要应一致: %s vs %s", desc2.Digest, desc.Digest)
	}
	if !s.Exists(ctx, desc.Digest) {
		t.Fatal("重写后内容应恢复可用")
	}
	rc, _, err := s.GetBlob(ctx, desc.Digest)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("修复后读取仍失败: %v", err)
	}
	if string(got) != string(data) {
		t.Fatal("修复后内容不一致")
	}
	tLog(t, "rewrite-repair", "digest", desc.Digest, "隔离目录数", quarantineCount(t, dir), "状态", "recovered")
}
