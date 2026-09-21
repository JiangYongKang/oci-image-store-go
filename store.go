package ocistore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// Store 是本地 OCI 内容存储。所有方法均可被并发调用。
//
// 并发模型：
//   - 常规读写（PutBlob/GetBlob/PutManifest/标签/Pin/ResolvePlatform）在
//     实际“状态变更/可达性观察”区间持有共享的 RLock，彼此可并行；
//   - GC 持有排他的 Lock，并且会等待所有已经开始的常规操作排空后才动手。
//     因此 GC 期间没有任何拉取会观察到中途被删的内容，也不会出现
//     “GC 删除后新引用又落到已删 blob”的悬空/复活交错。
//   - blob 的字节落地先写到 tmp 且不持锁；只有提交（摘要校验、配额判定、
//     原子 rename）在 RLock 内完成，所以读者绝不可能观察到半写入内容。
type Store struct {
	root  string
	quota int64

	// stateMu 保护 used 与 state.json 的写入。
	stateMu sync.Mutex
	used    int64

	// commitMu 串行化 blob 的“去重检查 -> 原子落盘 -> 配额记账”提交区间，
	// 保证并发写入同一内容时只有一个写者真正落盘（其余复用），
	// 同时让配额预订不会被并发提交击穿。字节落地（spool）在锁外进行。
	commitMu sync.Mutex

	// gcMu 实现“常规操作 vs GC”的共享/排他门闩。
	gcMu sync.RWMutex

	// inflight 追踪 RLock 持有者，供 GC 排空等待。
	// 由 inflightCond.L（inflightMu）保护。
	inflightMu     sync.Mutex
	inflightCond   *sync.Cond
	inflightActive int
	gcDrainWanted  bool // GC 已请求排空：为 true 时 beginOp 必须排队等待

	// activeLeases 追踪进程内所有未释放的租约；GC 必须把它们 Pin 的
	// 内容视为可达根，避免回收正在拉取的内容。
	activeLeases sync.Map // *Lease -> struct{}

	closeOnce sync.Once
	closed    bool
}

// persistedState 是 state.json 的磁盘表示。该信息可由 blob 目录重建，
// 持久化仅是为了避免每次打开都全盘扫描。
type persistedState struct {
	UsedBytes int64 `json:"usedBytes"`
	Version   int   `json:"version"`
}

// Open 打开（必要时初始化）root 下的存储。
func Open(root string, opts Options) (*Store, error) {
	if root == "" {
		return nil, errors.New("ocistore: empty root path")
	}
	for _, d := range []string{root, filepath.Join(root, "blobs"), tagsDir(root),
		leasesDir(root), quarantineDir(root), tmpDir(root)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}

	s := &Store{root: root, quota: opts.QuotaBytes}
	s.inflightCond = sync.NewCond(&s.inflightMu)

	if err := s.loadOrRecomputeState(); err != nil {
		return nil, err
	}
	if err := s.cleanStaleTemp(); err != nil {
		return nil, err
	}
	return s, nil
}

// loadOrRecomputeState 读取 state.json；缺失或与磁盘不符时按 blob 目录重建。
func (s *Store) loadOrRecomputeState() error {
	var ps persistedState
	data, err := os.ReadFile(statePath(s.root))
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &ps); err != nil || ps.Version != 1 {
			ps = persistedState{}
		}
	case errors.Is(err, os.ErrNotExist):
		// 首次打开，下面计算。
	default:
		return err
	}

	used := int64(0)
	aRoot := filepath.Join(s.root, "blobs")
	algDirs, _ := os.ReadDir(aRoot)
	for _, ad := range algDirs {
		if !ad.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(aRoot, ad.Name()))
		if err != nil {
			return err
		}
		for _, e := range entries {
			// 只统计 blob 本体，.meta 等辅助文件不计入配额。
			if e.IsDir() || filepath.Ext(e.Name()) != "" {
				continue
			}
			info, err := e.Info()
			if err != nil {
				return err
			}
			used += info.Size()
		}
	}
	s.used = used

	// state.json 缺失或漂移时纠正它。
	if ps.UsedBytes != used {
		if err := s.persistState(); err != nil {
			return err
		}
	}
	return nil
}

// cleanStaleTemp 清理上次进程崩溃遗留的临时文件。
func (s *Store) cleanStaleTemp() error {
	entries, err := os.ReadDir(tmpDir(s.root))
	if err != nil {
		return err
	}
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(tmpDir(s.root), e.Name()))
	}
	return nil
}

// persistState 必须在持有 stateMu 时调用。
func (s *Store) persistStateLocked() error {
	ps := persistedState{UsedBytes: s.used, Version: 1}
	buf, err := json.MarshalIndent(&ps, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteBytes(s.root, statePath(s.root), buf, 0o644)
}

func (s *Store) persistState() error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.persistStateLocked()
}

// Close 释放存储持有的资源。
func (s *Store) Close() error {
	s.closeOnce.Do(func() { s.closed = true })
	return nil
}

// Usage 返回当前已用字节数与配额（0 表示不限）。
func (s *Store) Usage(_ context.Context) (used, quota int64) {
	s.stateMu.Lock()
	used = s.used
	s.stateMu.Unlock()
	return used, s.quota
}

// ---- 常规操作 / GC 门闩 ----
//
// 协议（避免条件变量丢失唤醒）：
//   - beginOp 在 inflightMu 内看到 gcDrainWanted=false 才把 inflightActive+1，
//     随后获取共享读锁；GC 已请求排空时则在条件变量上等待，排空结束被唤醒后
//     再重试，保证排空窗口内不会有新操作进入。
//   - beginGC 先置 gcDrainWanted=true（不持 gcMu），等待 inflightActive 归零；
//     等待期间在途读者可正常完成（它们持有 RLock），不会死锁。归零后再取排他锁。
//   - endGC 清除 gcDrainWanted 并广播，唤醒所有排队的新操作。

func (s *Store) beginOp() {
	s.inflightMu.Lock()
	for s.gcDrainWanted {
		s.inflightCond.Wait()
	}
	s.inflightActive++
	s.inflightMu.Unlock()

	s.gcMu.RLock()
}

func (s *Store) endOp() {
	// 先释放共享读锁，再递减计数，使等待中的 GC 能尽快观察到归零。
	s.gcMu.RUnlock()

	s.inflightMu.Lock()
	s.inflightActive--
	if s.inflightActive == 0 {
		s.inflightCond.Broadcast()
	}
	s.inflightMu.Unlock()
}

// beginGC 进入排他回收：登记排空意图，等待在途操作全部完成后取排他锁。
func (s *Store) beginGC() {
	s.inflightMu.Lock()
	s.gcDrainWanted = true
	for s.inflightActive > 0 {
		s.inflightCond.Wait()
	}
	s.inflightMu.Unlock()

	// 此刻不会再有新的 beginOp 通过计数（它们会在条件变量上等待），
	// 因而取锁后 in-flight 恒为 0，删除窗口内无并发读/拉取。
	s.gcMu.Lock()
}

func (s *Store) endGC() {
	s.gcMu.Unlock()

	s.inflightMu.Lock()
	s.gcDrainWanted = false
	s.inflightCond.Broadcast()
	s.inflightMu.Unlock()
}
