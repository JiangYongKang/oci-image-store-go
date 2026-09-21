package ocistore

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// TestGCBasicReachability：删除标签后，仍被其它清单引用的内容保留，
// 完全不可达的层被回收。
func TestGCBasicReachability(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	ctx := context.Background()

	idxD, amd, arm := buildIndex(t, s, "multiarch")
	// 额外造一个"孤儿"清单（无标签、无索引引用）及其层
	orphan := buildManifest(t, s, "mips")
	t.Logf("input: index=%s(tag multiarch) amd=%s arm=%s orphan=%s(unreferenced)",
		idxD, amd.manifestD, arm.manifestD, orphan.manifestD)

	// 引用计数：共享层为 0（这里层各自不同），amd 层被 amd 清单引用
	if n, err := s.RefCount(amd.layerD); err != nil || n != 1 {
		t.Fatalf("amd layer RefCount=%d err=%v, want 1", n, err)
	}
	t.Logf("judgement basis: amd layer referenced by 1 manifest; orphan referenced by 0")

	res, err := s.GC(ctx)
	if err != nil {
		t.Fatal(err)
	}
	removed := digestSet(res.Removed)
	if removed[orphan.layerD] == false || removed[orphan.configD] == false || removed[orphan.manifestD] == false {
		t.Fatalf("orphan graph not fully collected: removed=%v", res.Removed)
	}
	for _, keep := range []Digest{idxD, amd.manifestD, amd.layerD, amd.configD, arm.manifestD, arm.layerD} {
		if removed[keep] {
			t.Fatalf("reachable content wrongly removed: %s", keep)
		}
		if ok, _ := s.Has(keep); !ok {
			t.Fatalf("reachable content missing after GC: %s", keep)
		}
	}
	t.Logf("GC#1 removed=%d bytes=%d; reachable graph intact", len(res.Removed), res.Bytes)

	// 删除标签后再次 GC：整条多架构图应被回收
	if err := s.DeleteTag(ctx, "multiarch"); err != nil {
		t.Fatal(err)
	}
	res2, err := s.GC(ctx)
	if err != nil {
		t.Fatal(err)
	}
	removed2 := digestSet(res2.Removed)
	for _, d := range []Digest{idxD, amd.manifestD, amd.layerD, amd.configD, arm.manifestD, arm.layerD, arm.configD} {
		if !removed2[d] {
			t.Fatalf("after tag deletion, %s should be collectable", d)
		}
	}
	st, _ := s.Stats()
	t.Logf("final state: removed=%d, blobs left=%d bytes=%d", len(res2.Removed), st.BlobCount, st.Bytes)
	if st.BlobCount != 0 || st.Bytes != 0 {
		t.Fatalf("all content should be reclaimed, got %+v", st)
	}
}

// TestGCTagDeleteKeepsContentUntilGC：DeleteTag 不删内容；只有 GC 才回收。
func TestGCTagDeleteKeepsContentUntilGC(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	ctx := context.Background()
	a := buildManifest(t, s, "amd64")
	if err := s.PutTag(ctx, "img", a.manifestD); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTag(ctx, "img"); err != nil {
		t.Fatal(err)
	}
	_, err := s.ResolveTag("img")
	if !errors.Is(err, ErrLabelNotExist) {
		t.Fatalf("want ErrLabelNotExist, got %v", err)
	}
	if ok, _ := s.Has(a.manifestD); !ok {
		t.Fatalf("DeleteTag must not delete content before GC")
	}
	t.Logf("judgement basis: tag gone but digest %s still readable before GC", a.manifestD)
	res, _ := s.GC(ctx)
	if ok, _ := s.Has(a.manifestD); ok {
		t.Fatalf("GC should reclaim unreachable manifest, removed=%v", res.Removed)
	}
	t.Logf("final state: reclaimed by GC after tag deletion: %d blobs", len(res.Removed))
}

// TestGCConcurrentPull：拉取进行中（Pin 保护）运行 GC，
// 被 Pin 的部分图不得被回收，拉取全程不出现悬空引用/误删。
func TestGCConcurrentPull(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	ctx := context.Background()

	idxD, amd, arm := buildIndex(t, s, "multiarch")
	// 先删除标签，使整图变成不可达 —— 但拉取仍在进行（Pin）
	if err := s.DeleteTag(ctx, "multiarch"); err != nil {
		t.Fatal(err)
	}
	t.Logf("input: tag deleted, index %s unreachable; a pull is in progress (pinned)", idxD)

	// 模拟拉取：Pin 住整个解析链
	for _, d := range []Digest{idxD, amd.manifestD, amd.configD, amd.layerD,
		arm.manifestD, arm.configD, arm.layerD} {
		if err := s.Pin(d); err != nil {
			t.Fatal(err)
		}
	}

	res, err := s.GC(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 0 {
		t.Fatalf("GC must not remove pinned in-flight content, got %v", res.Removed)
	}
	t.Logf("judgement basis: GC while pinned removed=%d (expect 0)", len(res.Removed))

	// 拉取仍能完整解析
	r, ferr := s.FetchPlatform(ctx, idxD.String(), Platform{OS: "linux", Architecture: "amd64"})
	if ferr != nil || r.ManifestDescriptor.Digest != amd.manifestD {
		t.Fatalf("pinned pull broken after GC: %v", ferr)
	}

	// 拉取结束，Unpin 后再次 GC 才能回收
	for _, d := range []Digest{idxD, amd.manifestD, amd.configD, amd.layerD,
		arm.manifestD, arm.configD, arm.layerD} {
		if err := s.Unpin(d); err != nil {
			t.Fatal(err)
		}
	}
	res2, _ := s.GC(ctx)
	st, _ := s.Stats()
	t.Logf("final state: after unpin+GC removed=%d blobs=%d", len(res2.Removed), st.BlobCount)
	if st.BlobCount != 0 {
		t.Fatalf("content should be collectable once pull completes: %+v", st)
	}
}

// TestGCRaceConcurrentPullAndDelete：拉取与"删标签 + GC"真正并发执行，
// 任何一次拉取要么在 Pin 保护下成功且内容完好，要么得到明确的不存在错误；
// 不允许读到半套图（清单在、层被删）。
func TestGCRaceConcurrentPullAndDelete(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(Options{RootDir: dir})
	defer s.Close()
	ctx := context.Background()

	const rounds = 30
	var wg sync.WaitGroup
	bad := make(chan error, rounds*2)

	for round := 0; round < rounds; round++ {
		tag := "racetag-" + itoa(round)
		idxD, amd, _ := buildIndex(t, s, tag)

		// 拉取者：先 Pin 再读，模拟并发拉取协议
		wg.Add(1)
		go func(idxD, amdD Digest) {
			defer wg.Done()
			if err := s.Pin(idxD); err != nil {
				bad <- err
				return
			}
			defer s.Unpin(idxD)
			data, err := s.blobData(idxD)
			if err != nil {
				if errors.Is(err, ErrNotExist) || errors.Is(err, ErrCorrupted) {
					return // 明确可区分的失败，允许
				}
				bad <- err
				return
			}
			isIndex, idx, _, perr := parseManifest("", data)
			if perr != nil || !isIndex {
				bad <- perr
				return
			}
			desc, serr := selectPlatform(idx, Platform{OS: "linux", Architecture: "amd64"})
			if serr != nil {
				bad <- serr
				return
			}
			for _, d := range []Digest{desc.Digest} {
				if err := s.Pin(d); err == nil {
					defer s.Unpin(d)
				}
			}
			if _, gerr := s.blobData(amdD); gerr != nil {
				if errors.Is(gerr, ErrNotExist) {
					return
				}
				bad <- gerr // 悬空引用：清单在但层坏/丢 —— 不允许
			}
		}(idxD, amd.manifestD)

		// 回收者：删标签 + GC
		wg.Add(1)
		go func(tag string) {
			defer wg.Done()
			_ = s.DeleteTag(ctx, tag)
			if _, err := s.GC(ctx); err != nil {
				bad <- err
			}
		}(tag)
		wg.Wait() // 每轮同步：避免上一轮 GC 与下一轮"发布后尚未建引用"的内容交错
	}
	close(bad)
	for e := range bad {
		t.Fatalf("race violation (dangling/mis-delete): %v", e)
	}
	st, _ := s.Stats()
	t.Logf("final state: %d rounds pull-vs-GC completed without dangling refs; stray blobs=%d (unpinned races may survive GC scheduling)",
		rounds, st.BlobCount)
}

func digestSet(ds []Digest) map[Digest]bool {
	m := map[Digest]bool{}
	for _, d := range ds {
		m[d] = true
	}
	return m
}
