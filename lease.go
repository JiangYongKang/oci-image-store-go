package ocistore

import (
	"context"
	"sync"
)

// Lease 表示一次进行中的拉取/解析，期间被 Pin 的内容不会被 GC 回收。
//
// 租约本身受“常规操作”门闩保护：GC 排空后到结束前，Pin 会被阻塞，
// 因此不可能出现“GC 已判定某内容不可达并删除，随后 Pin 却成功固定到
// 已删内容”的悬空交错。
type Lease struct {
	store *Store

	mu       sync.Mutex
	pinned   map[Digest]int
	released bool
}

// NewLease 创建一个新的空租约。
func (s *Store) NewLease() *Lease {
	l := &Lease{store: s, pinned: make(map[Digest]int)}
	s.activeLeases.Store(l, struct{}{})
	return l
}

// Pin 将摘要固定到租约；内容必须完整存在，否则返回 ErrNotFound。
// 对同一摘要重复 Pin 是安全的，需要相同次数的 Release 才解除。
func (l *Lease) Pin(ctx context.Context, d Digest) error {
	if err := d.validate(); err != nil {
		return err
	}
	l.store.beginOp()
	defer l.store.endOp()

	// 门闩之后再次确认内容确实完整存在（GC 若持锁则根本进不到这里）。
	if !l.store.Exists(ctx, d) {
		return notFound(d)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return ErrLeaseReleased
	}
	l.pinned[d]++
	return nil
}

// Pins 返回当前固定摘要的快照。
func (l *Lease) Pins() []Digest {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Digest, 0, len(l.pinned))
	for d := range l.pinned {
		out = append(out, d)
	}
	return out
}

// activeLeasePins 汇总当前所有未释放租约固定的摘要。GC 持排他锁期间，
// 租约的 Pin 无法进入，已存在的 Pin 集合保持稳定。
func (s *Store) activeLeasePins() []Digest {
	var out []Digest
	s.activeLeases.Range(func(k, _ any) bool {
		l := k.(*Lease)
		l.mu.Lock()
		for d := range l.pinned {
			out = append(out, d)
		}
		l.mu.Unlock()
		return true
	})
	return out
}

// Release 释放租约。可重复调用，之后再 Pin 返回 ErrLeaseReleased。
// 释放后该租约固定的内容不再作为 GC 的可达根。
func (l *Lease) Release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return
	}
	l.released = true
	l.pinned = nil
	l.store.activeLeases.Delete(l)
}
