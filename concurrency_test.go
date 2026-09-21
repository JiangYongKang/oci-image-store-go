package ocistore

import (
	"bytes"
	"context"
	"sync"
	"testing"
)

// TestConcurrentPutSameBlob：N 个 goroutine 并发写相同内容，结果一致且只有一份副本。
func TestConcurrentPutSameBlob(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	ctx := context.Background()

	payload := bytes.Repeat([]byte("concurrent-blob-"), 4)
	want := ComputeDigest(payload)
	const n = 32
	t.Logf("input: %d goroutines put identical content %s (%d bytes)", n, want, len(payload))

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, size, err := s.PutBlob(ctx, want, bytes.NewReader(payload))
			if err != nil || d != want || size != int64(len(payload)) {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("concurrent put failed: %v", e)
	}
	st, _ := s.Stats()
	if st.BlobCount != 1 || st.Bytes != int64(len(payload)) {
		t.Fatalf("judgement basis: expected 1 unique blob, got %+v", st)
	}
	// 无残留临时文件
	if l := countFiles(t, dir, dirTmp); l != 0 {
		t.Fatalf("leftover temp files: %d", l)
	}
	t.Logf("final state: blobs=%d bytes=%d tmp=0 across %d writers", st.BlobCount, st.Bytes, n)
}

// TestConcurrentReadWhileMigrate：并发读取期间反复迁移标签，
// 每次读取都必须观察到一个完整、自洽且与其摘要一致的清单。
func TestConcurrentReadWhileMigrate(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	ctx := context.Background()

	a := buildManifest(t, s, "amd64")
	b := buildManifest(t, s, "arm64")
	if err := s.PutTag(ctx, "moving", a.manifestD); err != nil {
		t.Fatal(err)
	}
	t.Logf("input: tag 'moving' alternates between %s and %s under concurrent readers",
		a.manifestD, b.manifestD)

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// 迁移者
	wg.Add(1)
	go func() {
		defer wg.Done()
		targets := []Digest{a.manifestD, b.manifestD}
		for i := 0; i < 200; i++ {
			if err := s.PutTag(ctx, "moving", targets[i%2]); err != nil {
				t.Errorf("migrate: %v", err)
				return
			}
		}
		close(stop)
	}()

	// 读取者：任何观察结果都必须是两个完整版本之一
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			d, err := s.ResolveTag("moving")
			if err != nil {
				t.Errorf("resolve: %v", err)
				return
			}
			data, gerr := s.blobData(d) // 完整性校验读取
			if gerr != nil {
				t.Errorf("read partial/unfinished content: %v", gerr)
				return
			}
			if d != a.manifestD && d != b.manifestD {
				t.Errorf("observed unknown digest %s", d)
				return
			}
			isIndex, _, mani, perr := parseManifest("", data)
			if perr != nil || isIndex || len(mani.Layers) != 1 {
				t.Errorf("observed half-written or malformed manifest: %v", perr)
				return
			}
			if _, gerr := s.blobData(mani.Config.Digest); gerr != nil {
				t.Errorf("config of observed manifest unreadable: %v", gerr)
				return
			}
		}
	}()
	wg.Wait()

	got, _ := s.ResolveTag("moving")
	t.Logf("final state: tag=%s, all observations were complete versions, no partial manifest", got)
}

// TestConcurrentPutSameLayerDifferentManifests：多个清单并发引用同一层，
// 层只存一份，所有清单读取一致。
func TestConcurrentPutSameLayerDifferentManifests(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	ctx := context.Background()

	layer := []byte("shared-layer-by-all")
	layerD := ComputeDigest(layer)
	const n = 16
	t.Logf("input: %d manifests sharing layer %s", n, layerD)

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ld, _, err := s.PutBlob(ctx, layerD, bytes.NewReader(layer))
			if err != nil || ld != layerD {
				errs <- err
				return
			}
			a := buildManifestWithLayer(t, s, "arch-"+itoa(i), layer, layerD)
			if err := s.PutTag(ctx, "m"+itoa(i), a.manifestD); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("worker error: %v", e)
	}
	st, _ := s.Stats()
	// 1 共享层 + n 个 config + n 个 manifest
	if st.BlobCount != 1+2*n {
		t.Fatalf("judgement basis: expected %d blobs, got %d", 1+2*n, st.BlobCount)
	}
	for i := 0; i < n; i++ {
		if _, err := s.FetchPlatform(ctx, "m"+itoa(i), Platform{}); err != nil {
			// Platform{} 不参与：来源是镜像清单（非索引），直接返回
			t.Fatalf("manifest m%d fetch: %v", i, err)
		}
	}
	t.Logf("final state: %d manifests consistent, single shared layer, blobs=%d", n, st.BlobCount)
}
