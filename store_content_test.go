package ocistore

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// TestDedupWrites：相同内容重复写入不得产生重复副本。
func TestDedupWrites(t *testing.T) {
	s, dir := newTestStore(t, 0)
	ctx := context.Background()

	data := []byte("layer-content-AAA")
	d1, err := s.PutBlobBytes(ctx, MediaTypeOCILayer, data)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := s.PutBlobBytes(ctx, MediaTypeOCILayer, data)
	if err != nil {
		t.Fatal(err)
	}
	used, quota := s.Usage(ctx)
	tLog(t, "dedup",
		"input", string(data),
		"digest", d1.Digest,
		"secondDigest", d2.Digest,
		"usedBytes", used, "quota", quota)

	if d1.Digest != d2.Digest {
		t.Fatalf("相同内容摘要必须一致: %s vs %s", d1.Digest, d2.Digest)
	}
	if used != int64(len(data)) {
		t.Fatalf("重复写入后 used=%d, 期望 %d（不得产生重复副本）", used, len(data))
	}
	// 磁盘上也只能有一份 blob 文件。
	if count := countBlobFiles(t, dir); count != 1 {
		t.Fatalf("磁盘 blob 文件数=%d, 期望 1", count)
	}
}

// TestTagMigrationKeepsDigestPinnedContent：标签迁移后，
// 旧摘要固定的内容依然可按摘要读取，不被改变或丢失。
func TestTagMigrationKeepsDigestPinnedContent(t *testing.T) {
	s, _ := newTestStore(t, 0)
	ctx := context.Background()

	imgV1 := buildImage(t, s, MediaTypeOCIManifest, "layer-v1")
	imgV2 := buildImage(t, s, MediaTypeOCIManifest, "layer-v2")

	if _, err := s.PutManifest(ctx, "app:v1", MediaTypeOCIManifest, imgV1.manifest); err != nil {
		t.Fatal(err)
	}
	// 迁移同一标签到新内容。
	if _, err := s.PutManifest(ctx, "app:v1", MediaTypeOCIManifest, imgV2.manifest); err != nil {
		t.Fatal(err)
	}

	cur, err := s.ResolveTag(ctx, "app:v1")
	if err != nil {
		t.Fatal(err)
	}
	tLog(t, "tag-migration",
		"tagNow", cur.Digest,
		"v1Manifest", imgV1.manifestD.Digest,
		"v2Manifest", imgV2.manifestD.Digest)

	if cur.Digest != imgV2.manifestD.Digest {
		t.Fatalf("标签应迁移到 v2，实际=%s", cur.Digest)
	}

	// 关键：v1 内容仍按摘要稳定存在（GC 之前）。
	if !s.Exists(ctx, imgV1.manifestD.Digest) {
		t.Fatal("标签迁移后旧摘要内容丢失")
	}
	oldData, _, err := s.GetManifest(ctx, string(imgV1.manifestD.Digest))
	if err != nil {
		t.Fatalf("按旧摘要读取失败: %v", err)
	}
	if !bytes.Equal(oldData, imgV1.manifest) {
		t.Fatal("旧摘要内容被改变")
	}
}

// TestDeleteTagThenGC：删除标签后，仅不可达内容被回收，仍被引用的层保留。
func TestDeleteTagThenGC(t *testing.T) {
	s, dir := newTestStore(t, 0)
	ctx := context.Background()

	// 两个镜像共享同一基础层，但各自有唯一 config 与私有层。
	sharedDesc := mustPut(t, s, MediaTypeOCILayer, []byte("shared-base-layer"))
	imgA := buildImageCustom(t, s, `{"image":"A"}`, sharedDesc, mustPut(t, s, MediaTypeOCILayer, []byte("only-A-layer")))
	imgB := buildImageCustom(t, s, `{"image":"B"}`, sharedDesc, mustPut(t, s, MediaTypeOCILayer, []byte("only-B-layer")))
	if _, err := s.PutManifest(ctx, "img/a", MediaTypeOCIManifest, imgA); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutManifest(ctx, "img/b", MediaTypeOCIManifest, imgB); err != nil {
		t.Fatal(err)
	}

	imgADigest := digestBytes(imgA)
	onlyA := digestBytes([]byte("only-A-layer"))

	if err := s.DeleteTag(ctx, "img/a"); err != nil {
		t.Fatal(err)
	}
	rep, err := s.GC(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tLog(t, "gc-after-delete",
		"deletedCount", len(rep.Deleted),
		"bytesReclaimed", rep.BytesReclaimed,
		"sharedLayerKept", s.Exists(ctx, sharedDesc.Digest),
		"imgAManifestRemoved", !s.Exists(ctx, imgADigest),
		"onlyALayerRemoved", !s.Exists(ctx, onlyA))

	// 共享层仍被 img/b 引用，必须保留。
	if !s.Exists(ctx, sharedDesc.Digest) {
		t.Fatal("仍被 img/b 引用的共享层被误删")
	}
	// img/a 的清单与私有层不可达，应被回收。
	if s.Exists(ctx, imgADigest) {
		t.Fatal("已删标签且不可达的清单未被回收")
	}
	if s.Exists(ctx, onlyA) {
		t.Fatal("仅被 img/a 引用的私有层未被回收")
	}
	// img/b 及其私有层必须完好。
	if !s.Exists(ctx, digestBytes(imgB)) {
		t.Fatal("仍有标签的 img/b 被误删")
	}
	if n := quarantineCount(t, dir); n != 0 {
		t.Fatalf("正常 GC 不应产生隔离文件, got %d", n)
	}
}

// TestErrorSentinels：错误必须可通过 errors.Is 区分。
func TestErrorSentinels(t *testing.T) {
	_, d := func() (string, Digest) {
		return "x", Digest("sha256:0000000000000000000000000000000000000000000000000000000000000000")
	}()
	s, _ := newTestStore(t, 0)
	_, _, err := s.GetBlob(context.Background(), d)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("期望 ErrNotFound, got %v", err)
	}
	tLog(t, "error-sentinel", "op", "GetBlob(missing)", "判定依据", "errors.Is(ErrNotFound)", "err", err)
}

// countBlobFiles 统计 blobs 目录下的本体文件数。
func countBlobFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	_ = filepathWalk(filepath.Join(dir, "blobs"), func(_ string, hasExt bool) {
		if !hasExt {
			n++
		}
	})
	return n
}
