package ocistore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// GCReport 描述一次垃圾回收的结果。
type GCReport struct {
	Deleted        []Digest
	BytesReclaimed int64
	Start          time.Time
	Finish         time.Time
}

// GC 删除从标签与活跃租约不可达的内容。与并发拉取/写入互斥：
// beginGC 会排空所有进行中的常规操作并阻止新操作进入，因此
//   - 不会把仍被清单引用的层/配置误删；
//   - 不会有拉取在删除窗口内观察到悬空引用；
//   - 删除之后不可能有新引用成功指向已删 blob（那时 Pin/Tag 会阻塞或报错），
//     因而不会发生数据复活。
func (s *Store) GC(ctx context.Context) (*GCReport, error) {
	report := &GCReport{Start: time.Now()}
	s.beginGC()
	defer func() {
		report.Finish = time.Now()
		s.endGC()
	}()

	reachable, err := s.collectRoots(ctx)
	if err != nil {
		return nil, err
	}

	// 枚举全部 blob，删除不在可达集合中的内容。
	blobRoot := filepath.Join(s.root, "blobs")
	algDirs, err := os.ReadDir(blobRoot)
	if err != nil {
		return nil, err
	}
	for _, ad := range algDirs {
		if !ad.IsDir() {
			continue
		}
		alg := ad.Name()
		entries, err := os.ReadDir(filepath.Join(blobRoot, alg))
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != "" {
				continue // 只处理 blob 本体，.meta 随本体处理
			}
			dgst, perr := ParseDigest(alg + ":" + e.Name())
			if perr != nil {
				continue // 非摘要命名的杂项，保守保留
			}
			if reachable[dgst] {
				continue
			}
			info, ierr := e.Info()
			size := int64(0)
			if ierr == nil {
				size = info.Size()
			}
			if err := s.removeBlobLocked(dgst); err != nil {
				return nil, err
			}
			report.Deleted = append(report.Deleted, dgst)
			report.BytesReclaimed += size
		}
	}

	s.stateMu.Lock()
	s.used -= report.BytesReclaimed
	if s.used < 0 {
		s.used = 0
	}
	if err := s.persistStateLocked(); err != nil {
		s.stateMu.Unlock()
		return nil, err
	}
	s.stateMu.Unlock()

	return report, nil
}

// removeBlobLocked 删除 blob 本体与元数据。GC 已持排他锁，直接操作文件。
func (s *Store) removeBlobLocked(dgst Digest) error {
	blobFile, metaFile, err := s.digestPaths(dgst)
	if err != nil {
		return err
	}
	// 本体若是只读，先恢复可写位再删除。
	_ = os.Chmod(blobFile, 0o644)
	if err := os.Remove(blobFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(metaFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fsyncDir(filepath.Dir(blobFile))
	return nil
}

// collectRoots 以“标签 + 活跃租约”为根，沿清单引用做闭包追踪。
// 必须在 GC 排他锁内调用（期间不会有写入移动引用）。
func (s *Store) collectRoots(ctx context.Context) (map[Digest]bool, error) {
	reachable := make(map[Digest]bool)
	var stack []Digest

	// 根 1：所有标签指向的内容。
	tags, err := s.Tags(ctx)
	if err != nil {
		return nil, err
	}
	for _, tag := range tags {
		rec, err := s.readTagRecord(tag)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue // 标签指针与文件可能正在迁移；GC 持锁，正常不会发生
			}
			return nil, err
		}
		stack = append(stack, rec.Digest)
	}

	// 根 2：进程内活跃租约固定的内容（并发拉取保护）。
	stack = append(stack, s.activeLeasePins()...)

	// 根 3：磁盘上的持久租约（兼容外部写入 leases 目录的场景）。
	leasePins, err := s.readLeasePins()
	if err != nil {
		return nil, err
	}
	stack = append(stack, leasePins...)

	// 闭包追踪：清单引用 config/layers/子清单。
	for len(stack) > 0 {
		d := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if reachable[d] {
			continue
		}
		desc, presence, err := s.readMeta(d)
		if err != nil {
			return nil, err
		}
		if presence != presenceOK {
			// 根指向缺失/损坏内容：悬空或损坏指针本身不致命，跳过；
			// 其引用的内容若没有别的根会被正常回收。
			continue
		}
		reachable[d] = true

		// 只有清单才需要展开引用关系；普通 layer/config 是叶子。
		if !isManifestMediaType(desc.MediaType) {
			continue
		}
		data, err := os.ReadFile(mustBlobPath(s, d))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				delete(reachable, d)
				continue
			}
			return nil, err
		}
		// GC 中遇到损坏/截断清单：不静默忽略，显式报错，避免误判引用闭包。
		rm, err := parseManifest(data, desc.MediaType)
		if err != nil {
			return nil, fmt.Errorf("ocistore: gc cannot parse manifest %s: %w", d, err)
		}
		switch rm.kind {
		case kindIndex:
			for _, m := range rm.index.Manifests {
				stack = append(stack, m.Digest)
			}
		case kindImage:
			stack = append(stack, rm.image.Config.Digest)
			for _, l := range rm.image.Layers {
				stack = append(stack, l.Digest)
			}
		}
	}
	return reachable, nil
}

// mustBlobPath 返回 blob 路径，调用方已知摘要合法。
func mustBlobPath(s *Store, d Digest) string {
	p, _, err := s.digestPaths(d)
	if err != nil {
		return ""
	}
	return p
}

// readLeasePins 读取持久化租约固定的摘要。当前进程内 Lease 不强制落盘，
// 这里扫描 leases 目录以兼容外部写入的租约文件。
func (s *Store) readLeasePins() ([]Digest, error) {
	entries, err := os.ReadDir(leasesDir(s.root))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Digest
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		buf, err := os.ReadFile(filepath.Join(leasesDir(s.root), e.Name()))
		if err != nil {
			return nil, err
		}
		for _, line := range splitLines(buf) {
			if line == "" {
				continue
			}
			d, err := ParseDigest(line)
			if err != nil {
				continue // 忽略损坏的租约行
			}
			out = append(out, d)
		}
	}
	return out, nil
}

// splitLines 将字节按换行切分。
func splitLines(b []byte) []string {
	var lines []string
	start := 0
	for i, c := range b {
		if c == '\n' {
			lines = append(lines, string(b[start:i]))
			start = i + 1
		}
	}
	if start < len(b) {
		lines = append(lines, string(b[start:]))
	}
	return lines
}
