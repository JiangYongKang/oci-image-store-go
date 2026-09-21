package ocistore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
)

// TestConcurrentPutSameBlob：N 个 goroutine 并发写入相同内容，
// 结果必须全部成功、摘要一致，且磁盘只有一份副本、用量只计一次。
func TestConcurrentPutSameBlob(t *testing.T) {
	s, dir := newTestStore(t, 0)
	ctx := context.Background()
	data := []byte("concurrent-identical-layer-payload")

	const n = 32
	var wg sync.WaitGroup
	errs := make(chan error, n)
	digests := make([]Digest, n)
	var start sync.WaitGroup
	start.Add(1)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			start.Wait()
			d, err := s.PutBlobBytes(ctx, MediaTypeOCILayer, bytes.Clone(data))
			if err == nil {
				digests[idx] = d.Digest
			}
			errs <- err
		}(i)
	}
	start.Done()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("并发写入失败: %v", err)
		}
	}
	want := digestBytes(data)
	for i, d := range digests {
		if d != want {
			t.Fatalf("goroutine %d 摘要=%s, 期望 %s", i, d, want)
		}
	}
	used, _ := s.Usage(ctx)
	tLog(t, "concurrent-same-blob",
		"writers", n, "digest", want,
		"磁盘副本数", countBlobFiles(t, dir),
		"usedBytes", used)
	if used != int64(len(data)) {
		t.Fatalf("并发去重失败: used=%d, 期望 %d", used, len(data))
	}
	if count := countBlobFiles(t, dir); count != 1 {
		t.Fatalf("磁盘副本数=%d, 期望 1", count)
	}
}

// TestConcurrentPutSameTag：N 个 goroutine 并发迁移同一标签到各自的清单。
// 最终标签必须恰好指向其中一个完整清单，任何时刻读到的都是完整清单。
func TestConcurrentPutSameTag(t *testing.T) {
	s, _ := newTestStore(t, 0)
	ctx := context.Background()

	// 准备 K 份不同的合法清单。
	const k = 16
	var manifests [][]byte
	validDigests := make(map[Digest]bool)
	for i := 0; i < k; i++ {
		payload := []byte("cfg-" + itoaTest(i))
		img := buildImageCustom(t, s, string(payload),
			mustPut(t, s, MediaTypeOCILayer, []byte("layer-"+itoaTest(i))))
		manifests = append(manifests, img)
		validDigests[digestBytes(img)] = true
	}

	const writers = 64
	var wg sync.WaitGroup
	var start sync.WaitGroup
	start.Add(1)
	var badReads int64
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			start.Wait()
			mb := manifests[idx%k]
			if _, err := s.PutManifest(ctx, "moving", MediaTypeOCIManifest, mb); err != nil {
				t.Errorf("并发写标签失败: %v", err)
				return
			}
			// 立即读回：必须要么不存在，要么是一份完整且摘要正确的清单。
			data, desc, err := s.GetManifest(ctx, "moving")
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					return // 其它写者尚未发布，可接受的中间态
				}
				t.Errorf("读到异常: %v", err)
				return
			}
			if digestBytes(data) != desc.Digest || !validDigests[desc.Digest] {
				atomic.AddInt64(&badReads, 1)
			}
		}(w)
	}
	start.Done()
	wg.Wait()

	if badReads != 0 {
		t.Fatalf("观察到 %d 次半写入/不一致清单", badReads)
	}
	final, err := s.ResolveTag(ctx, "moving")
	if err != nil {
		t.Fatal(err)
	}
	if !validDigests[final.Digest] {
		t.Fatalf("最终标签指向非法摘要 %s", final.Digest)
	}
	tLog(t, "concurrent-tag", "writers", writers, "最终指向", final.Digest,
		"判定依据", "每次读回都重新计算摘要并与指针比对")
}

// TestConcurrentReadersNoHalfWrites：写者持续发布不同 blob，
// 读者只读取已存在摘要并完整校验，任何读取要么成功且内容正确，要么明确 NotFound。
func TestConcurrentReadersNoHalfWrites(t *testing.T) {
	s, _ := newTestStore(t, 0)
	ctx := context.Background()

	const rounds = 40
	known := make(chan Descriptor, rounds)
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 读者池
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				case d := <-known:
					rc, _, err := s.GetBlob(ctx, d.Digest)
					if err != nil {
						if errors.Is(err, ErrNotFound) {
							known <- d
							continue
						}
						t.Errorf("读者异常: %v", err)
						return
					}
					b, rerr := io.ReadAll(rc)
					_ = rc.Close()
					if rerr != nil {
						t.Errorf("完整性校验失败（疑似半写入）: %v", rerr)
						return
					}
					if digestBytes(b) != d.Digest {
						t.Errorf("读到内容摘要不符")
						return
					}
				}
			}
		}()
	}

	for i := 0; i < rounds; i++ {
		payload := []byte("payload-round-" + itoaTest(i) + "-padding")
		d := mustPut(t, s, MediaTypeOCILayer, payload)
		known <- d
	}
	close(stop)
	wg.Wait()
	tLog(t, "concurrent-readers", "rounds", rounds, "结论", "无半写入、无校验失败")
}

// TestGCVsConcurrentPull：GC 与大量解析/拉取并发，
// 不得出现悬空引用或误删（被租约保护的内容必须可读）。
func TestGCVsConcurrentPull(t *testing.T) {
	s, _ := newTestStore(t, 0)
	ctx := context.Background()

	// 固定一份带标签的多架构镜像。
	ma := buildMultiArch(t, s, MediaTypeOCIIndex, MediaTypeOCIManifest, MediaTypeOCIManifest)
	if _, err := s.PutManifest(ctx, "root", MediaTypeOCIIndex, ma.indexData); err != nil {
		t.Fatal(err)
	}

	const pulls = 50
	var wg sync.WaitGroup
	var failures int64
	var start sync.WaitGroup
	start.Add(1)

	// 拉取者
	for i := 0; i < pulls; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start.Wait()
			plat := Platform{OS: "linux", Architecture: "amd64"}
			if i%2 == 0 {
				plat = Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}
			}
			r, err := s.ResolvePlatform(ctx, "root", plat)
			if err != nil {
				atomic.AddInt64(&failures, 1)
				t.Errorf("pull 解析失败（GC 竞态）: %v", err)
				return
			}
			// 在租约保护下读取层，验证不被 GC 删除。
			for _, ld := range r.Layers {
				rc, _, gerr := s.GetBlob(ctx, ld.Digest)
				if gerr != nil {
					atomic.AddInt64(&failures, 1)
					t.Errorf("pull 取层失败: %v", gerr)
					r.Release()
					return
				}
				if _, rerr := io.ReadAll(rc); rerr != nil {
					atomic.AddInt64(&failures, 1)
					t.Errorf("pull 读层校验失败: %v", rerr)
				}
				_ = rc.Close()
			}
			r.Release()
		}(i)
	}

	// GC 者：反复回收（标签始终在，可达内容不应被删）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		start.Wait()
		for i := 0; i < 20; i++ {
			if _, err := s.GC(ctx); err != nil {
				t.Errorf("GC 失败: %v", err)
				return
			}
		}
	}()

	start.Done()
	wg.Wait()

	if failures != 0 {
		t.Fatalf("GC/拉取竞态产生 %d 次失败", failures)
	}
	// 最终所有可达内容仍在。
	if !s.Exists(ctx, ma.amdD) || !s.Exists(ctx, ma.armD) {
		t.Fatal("GC 后可达平台清单丢失")
	}
	tLog(t, "gc-vs-pull", "pulls", pulls, "gcRuns", 20, "failures", failures)
}

func itoaTest(i int) string { return itoa(i) }
