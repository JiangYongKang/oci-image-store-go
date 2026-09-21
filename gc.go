package ocistore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// GCResult 报告一次垃圾回收结果。
type GCResult struct {
	Scanned     int
	Kept        int
	Removed     []Digest
	Bytes       int64
	Quarantined []Digest // 扫描时发现并隔离的损坏内容
}

// refsSnapshot 是引用图的一致性快照。
//   - roots: 标签 -> 该标签固定的摘要（引用计数 +1/标签）
//   - edges: 清单/索引摘要 -> 其直接引用的摘要集合
//   - blobs: 全部现存 blob 的字节数（用于回收空间统计）
//   - corrupted: 磁盘字节与摘要不符、已被隔离的内容
type refsSnapshot struct {
	roots     map[string]Digest
	edges     map[Digest]map[Digest]struct{}
	blobs     map[Digest]int64
	corrupted []Digest
}

// snapshotRefs 在变更冻结窗口内构建引用图快照：
// 标签目录与 blob 目录都不会被并发修改（调用方持有 gcmu）。
// 快照期间发现的损坏内容会被立即隔离，绝不参与引用判定。
func (s *Store) snapshotRefs() (refsSnapshot, error) {
	snap := refsSnapshot{
		roots: map[string]Digest{},
		edges: map[Digest]map[Digest]struct{}{},
		blobs: map[Digest]int64{},
	}

	// 根：标签
	tags, err := s.Tags()
	if err != nil {
		return snap, err
	}
	for _, t := range tags {
		d, err := s.ResolveTag(t)
		if err != nil {
			continue // 标签内容非法：忽略该根，交给修复手段处理
		}
		snap.roots[t] = d
	}

	// 遍历全部 blob：校验 -> 解析 -> 建边
	root := filepath.Join(s.opt.RootDir, dirBlobs)
	err = filepath.WalkDir(root, func(path string, e os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if e.IsDir() {
			return nil
		}
		info, ierr := e.Info()
		if ierr != nil {
			return ierr
		}
		hexname := e.Name()
		d, perr := ParseDigest("sha256:" + hexname)
		if perr != nil {
			// 非摘要命名的杂项文件不计入 blob 空间
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if !d.Verify(data) {
			// 损坏：立即隔离，不作为缓存/引用节点
			s.quarantine(path, d)
			snap.corrupted = append(snap.corrupted, d)
			return nil
		}
		snap.blobs[d] = info.Size()
		addEdges(&snap, d, data)
		return nil
	})
	if err != nil {
		return snap, err
	}
	return snap, nil
}

// addEdges 尝试把 blob 解析为清单/索引并登记出边；其它类型无出边。
func addEdges(snap *refsSnapshot, d Digest, data []byte) {
	isIndex, idx, mani, err := parseManifest("", data)
	if err != nil {
		return // 层/config 或不支持的媒体类型：无出边
	}
	set := map[Digest]struct{}{}
	if isIndex {
		for _, c := range idx.Manifests {
			if _, err := ParseDigest(string(c.Digest)); err == nil {
				set[c.Digest] = struct{}{}
			}
		}
	} else {
		for _, rd := range mani.referencedDigests() {
			set[rd] = struct{}{}
		}
	}
	if len(set) > 0 {
		snap.edges[d] = set
	}
}

// markReachable 从"标签根 + 进行中拉取的 Pin 根"出发沿引用图标记可达集合。
func markReachable(snap refsSnapshot, pins map[Digest]int) map[Digest]bool {
	reach := map[Digest]bool{}
	var stack []Digest
	for _, d := range snap.roots {
		if !reach[d] {
			reach[d] = true
			stack = append(stack, d)
		}
	}
	for d := range pins {
		if !reach[d] {
			reach[d] = true
			stack = append(stack, d)
		}
	}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for next := range snap.edges[cur] {
			if !reach[next] {
				reach[next] = true
				stack = append(stack, next)
			}
		}
	}
	return reach
}

// GC 回收不再被任何标签、清单引用或 Pin 保护的内容。
//
// 并发语义：
//   - 持有 gcmu：与 PutBlob 发布、标签增改互斥，引用图在整个回收期间冻结；
//   - 持有 pinmu：Pin 集合在回收期间不变，已 Pin 的拉取所依赖的内容不会被删；
//   - 删除按摘要逐个进行，不触碰临时目录，失败不影响其它内容。
//
// 因而不可能出现：清单还在但被引层被删（悬空引用）、
// 正在进行的拉取丢失依赖（误删）、或已删内容因旧引用复活。
func (s *Store) GC(ctx context.Context) (GCResult, error) {
	s.gcmu.Lock()
	defer s.gcmu.Unlock()
	s.pinmu.Lock()
	pins := make(map[Digest]int, len(s.pins))
	for k, v := range s.pins {
		pins[k] = v
	}
	s.pinmu.Unlock()

	snap, err := s.snapshotRefs()
	if err != nil {
		return GCResult{}, fmt.Errorf("ocistore: gc snapshot: %w", err)
	}
	reach := markReachable(snap, pins)

	res := GCResult{Scanned: len(snap.blobs), Kept: 0}
	// 确定顺序，保证测试日志稳定
	all := make([]Digest, 0, len(snap.blobs))
	for d := range snap.blobs {
		all = append(all, d)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })

	for _, d := range all {
		if reach[d] {
			res.Kept++
			continue
		}
		path := blobPath(s.opt.RootDir, d)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return res, fmt.Errorf("ocistore: gc remove %s: %w", d, err)
		}
		res.Removed = append(res.Removed, d)
		res.Bytes += snap.blobs[d]
		_ = syncDir(filepath.Dir(path))
	}
	res.Quarantined = snap.corrupted
	return res, nil
}

// RefCount 返回某内容当前的强引用数：指向它的标签数 + 引用它的清单数。
func (s *Store) RefCount(d Digest) (int, error) {
	if _, err := ParseDigest(string(d)); err != nil {
		return 0, err
	}
	s.gcmu.Lock()
	defer s.gcmu.Unlock()
	snap, err := s.snapshotRefs()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, root := range snap.roots {
		if root == d {
			n++
		}
	}
	for parent, refs := range snap.edges {
		if parent == d {
			continue
		}
		if _, ok := refs[d]; ok {
			n++
		}
	}
	return n, nil
}
