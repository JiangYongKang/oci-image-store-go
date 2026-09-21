package ocistore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// blobMeta 是 .meta 文件的磁盘表示。
type blobMeta struct {
	MediaType string `json:"mediaType,omitempty"`
	Size      int64  `json:"size"`
}

// spoolTemp 把 r 的内容先完整落到 tmp（不持锁），返回临时路径与字节数。
// 这样配额判定可以在锁内一次性完成，提交失败不会留下半写入状态。
func (s *Store) spoolTemp(r io.Reader) (string, int64, error) {
	if err := os.MkdirAll(tmpDir(s.root), 0o755); err != nil {
		return "", 0, err
	}
	f, err := os.CreateTemp(tmpDir(s.root), ".upload-*")
	if err != nil {
		return "", 0, err
	}
	name := f.Name()
	n, err := io.Copy(f, r)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", 0, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", 0, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", 0, err
	}
	return name, n, nil
}

// commitBlob 在“常规操作”门闩 + 状态锁内校验摘要、执行配额检查并原子落盘。
// 相同内容已存在时直接复用，不重复占用空间。
func (s *Store) commitBlob(ctx context.Context, tmpName string, n int64, mediaType string) (Descriptor, error) {
	s.beginOp()
	defer s.endOp()
	// 串行化提交：保证“去重检查 -> rename -> 记账”整体原子，
	// 消除并发写同一内容时多份副本的竞态。
	s.commitMu.Lock()
	defer s.commitMu.Unlock()

	// 先按临时文件计算真实摘要，再决定目标路径。
	h, err := newHash(defaultAlgorithm)
	if err != nil {
		return Descriptor{}, err
	}
	f, err := os.Open(tmpName)
	if err != nil {
		return Descriptor{}, err
	}
	if _, err := io.Copy(h, f); err != nil {
		_ = f.Close()
		return Descriptor{}, err
	}
	_ = f.Close()
	dgst := digestFromHash(defaultAlgorithm, h)

	blobFile, metaFile, err := s.digestPaths(dgst)
	if err != nil {
		return Descriptor{}, err
	}

	// 去重：内容已存在且完好则复用，删除临时文件，不占用额外配额。
	switch desc, presence, merr := s.readMeta(dgst); {
	case merr != nil:
		return Descriptor{}, merr
	case presence == presenceOK:
		_ = os.Remove(tmpName)
		// 允许用新的媒体类型补全此前未知的类型。
		if mediaType != "" && desc.MediaType == "" {
			if err := s.writeMeta(dgst, mediaType, desc.Size); err != nil {
				return Descriptor{}, err
			}
			desc.MediaType = mediaType
		}
		return desc, nil
	case presence == presenceCorrupt:
		// 损坏副本：先隔离并释放其原占用，再按全新内容预订配额，
		// 避免“原写入已计费 + 重写再计费”的双重计数。
		s.quarantine(dgst)
	}

	// 配额预订与记账在同一把状态锁内，保证并发提交不同 blob 时
	// 不会各自通过检查而合计超限；预订先反映到 used 并持久化。
	s.stateMu.Lock()
	if s.quota > 0 && s.used+n > s.quota {
		s.stateMu.Unlock()
		_ = os.Remove(tmpName)
		return Descriptor{}, &QuotaExceededError{Quota: s.quota, Used: s.used, Required: n}
	}
	s.used += n
	if err := s.persistStateLocked(); err != nil {
		s.used -= n
		s.stateMu.Unlock()
		_ = os.Remove(tmpName)
		return Descriptor{}, err
	}
	s.stateMu.Unlock()
	// rollback：任何落盘步骤失败都回滚预订，不留临时状态或未释放配额。
	rollback := func() {
		s.stateMu.Lock()
		s.used -= n
		_ = s.persistStateLocked()
		s.stateMu.Unlock()
	}

	if err := os.MkdirAll(filepath.Dir(blobFile), 0o755); err != nil {
		_ = os.Remove(tmpName)
		rollback()
		return Descriptor{}, err
	}
	// 原子发布：rename 之后读者才能按摘要发现该 blob。
	if err := os.Rename(tmpName, blobFile); err != nil {
		_ = os.Remove(tmpName)
		rollback()
		return Descriptor{}, err
	}
	if err := os.Chmod(blobFile, 0o444); err != nil {
		_ = os.Remove(blobFile)
		rollback()
		return Descriptor{}, err
	}
	if err := s.writeMeta(dgst, mediaType, n); err != nil {
		_ = os.Remove(blobFile)
		_ = os.Remove(metaFile)
		rollback()
		return Descriptor{}, err
	}
	fsyncDir(filepath.Dir(blobFile))

	desc := Descriptor{MediaType: mediaType, Digest: dgst, Size: n}
	return desc, nil
}

// writeMeta 写 blob 元数据（不持状态锁，调用方处于 beginOp 区间）。
func (s *Store) writeMeta(dgst Digest, mediaType string, size int64) error {
	_, metaFile, err := s.digestPaths(dgst)
	if err != nil {
		return err
	}
	buf, err := json.Marshal(blobMeta{MediaType: mediaType, Size: size})
	if err != nil {
		return err
	}
	return atomicWriteBytes(filepath.Dir(metaFile), metaFile, buf, 0o644)
}

// blobPresence 描述摘要对应内容的可观察状态。
type blobPresence int

const (
	presenceMissing blobPresence = iota
	presenceOK
	presenceCorrupt
)

// readMeta 返回 blob 描述符及其状态：
//   - presenceOK：本体与元数据一致；
//   - presenceMissing：本体不存在；
//   - presenceCorrupt：本体存在但与声明长度/元数据不符（损坏或截断）。
func (s *Store) readMeta(dgst Digest) (Descriptor, blobPresence, error) {
	blobFile, metaFile, err := s.digestPaths(dgst)
	if err != nil {
		return Descriptor{}, presenceMissing, err
	}
	info, err := os.Stat(blobFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Descriptor{}, presenceMissing, nil
		}
		return Descriptor{}, presenceMissing, err
	}
	desc := Descriptor{Digest: dgst, Size: info.Size()}
	mb, err := os.ReadFile(metaFile)
	switch {
	case err == nil:
		var m blobMeta
		if err := json.Unmarshal(mb, &m); err != nil {
			return Descriptor{}, presenceCorrupt, fmt.Errorf("ocistore: corrupt meta for %s: %w", dgst, err)
		}
		// meta 与磁盘大小不一致本身就是损坏/截断信号。
		if m.Size != 0 && m.Size != info.Size() {
			return desc, presenceCorrupt, nil
		}
		desc.MediaType = m.MediaType
		if m.Size != 0 {
			desc.Size = m.Size
		}
	case errors.Is(err, os.ErrNotExist):
		// 无 meta 也允许读取（兼容直接放入的 blob），用磁盘大小。
	default:
		return Descriptor{}, presenceMissing, err
	}
	return desc, presenceOK, nil
}

// PutBlob 将 r 的全部内容按内容寻址写入存储，返回其描述符。
// 相同内容重复写入幂等，不会产生第二份副本。
func (s *Store) PutBlob(_ context.Context, mediaType string, r io.Reader) (Descriptor, error) {
	tmpName, n, err := s.spoolTemp(r)
	if err != nil {
		return Descriptor{}, err
	}
	return s.commitBlob(context.Background(), tmpName, n, mediaType)
}

// PutBlobBytes 是 PutBlob 的字节数组便捷形式。
func (s *Store) PutBlobBytes(ctx context.Context, mediaType string, data []byte) (Descriptor, error) {
	return s.PutBlob(ctx, mediaType, bytes.NewReader(data))
}

// verifyReader 是带完整性校验的读取器：边读边哈希，读到 EOF 时
// 比对声明摘要与长度，发现损坏立即通过 onCorrupt 隔离内容。
// Close 时才释放“常规操作”门闩，保证流式读取期间 GC 不会删除该 blob。
type verifyReader struct {
	f         *os.File
	h         interface{ io.Writer }
	sum       func() Digest
	declared  Digest
	wantSize  int64
	got       int64
	closed    bool
	onCorrupt func(Digest)
	endOp     func()
}

func (v *verifyReader) Read(p []byte) (int, error) {
	n, err := v.f.Read(p)
	if n > 0 {
		_, _ = v.h.Write(p[:n])
		v.got += int64(n)
	}
	if err == io.EOF {
		actual := v.sum()
		if actual != v.declared || (v.wantSize > 0 && v.got != v.wantSize) {
			if v.onCorrupt != nil {
				v.onCorrupt(v.declared)
			}
			return n, &DigestMismatchError{Op: "GetBlob", Declared: v.declared, Actual: actual}
		}
	}
	return n, err
}

func (v *verifyReader) Close() error {
	if v.closed {
		return nil
	}
	v.closed = true
	err := v.f.Close()
	if v.endOp != nil {
		v.endOp()
	}
	return err
}

// quarantine 将损坏 blob 移出可寻址路径，使其不再命中缓存，并按其在 blobs/
// 下的实际字节释放配额记账（隔离区不计入用量，与重开时按 blobs/ 重算一致）。
// 目标已存在（重复损坏）时直接丢弃本体。调用方处于常规操作门闩内。
func (s *Store) quarantine(dgst Digest) {
	blobFile, _, err := s.digestPaths(dgst)
	if err != nil {
		return
	}
	info, statErr := os.Stat(blobFile)
	onDisk := int64(0)
	if statErr == nil {
		onDisk = info.Size()
	}

	dst := filepath.Join(quarantineDir(s.root), dgst.Algorithm()+"-"+dgst.Encoded())
	if err := os.MkdirAll(quarantineDir(s.root), 0o755); err != nil {
		return
	}
	_ = os.Chmod(blobFile, 0o644)
	if err := os.Rename(blobFile, dst); err != nil {
		// rename 失败则尽力直接删除，保证不再被读到。
		if rmErr := os.Remove(blobFile); rmErr != nil {
			return
		}
	}
	// 元数据一并移除。
	_, metaFile, _ := s.digestPaths(dgst)
	_ = os.Remove(metaFile)

	// 成功移出后释放对应用量。
	if onDisk > 0 {
		s.stateMu.Lock()
		s.used -= onDisk
		if s.used < 0 {
			s.used = 0
		}
		_ = s.persistStateLocked()
		s.stateMu.Unlock()
	}
}

// GetBlob 打开一个 blob 供读取；读取过程中执行完整性校验。
// 发现内容损坏时隔离内容并返回 ErrDigestMismatch，之后不再缓存命中。
func (s *Store) GetBlob(_ context.Context, d Digest) (io.ReadCloser, Descriptor, error) {
	if err := d.validate(); err != nil {
		return nil, Descriptor{}, err
	}
	// 门闩持续到返回的 ReadCloser.Close，覆盖整个流式读取区间。
	s.beginOp()
	desc, presence, err := s.readMeta(d)
	if err != nil {
		s.endOp()
		return nil, Descriptor{}, err
	}
	if presence == presenceMissing {
		s.endOp()
		return nil, Descriptor{}, fmt.Errorf("%w: %s", ErrNotFound, d)
	}
	blobFile, _, err := s.digestPaths(d)
	if err != nil {
		s.endOp()
		return nil, Descriptor{}, err
	}
	if presence == presenceCorrupt {
		// 长度/元数据已表明损坏：立即隔离，返回可区分的错误，
		// 不把损坏内容作为缓存命中返回。
		s.quarantine(d)
		s.endOp()
		return nil, Descriptor{}, &DigestMismatchError{Op: "GetBlob", Declared: d}
	}
	f, err := os.Open(blobFile)
	if err != nil {
		s.endOp()
		if errors.Is(err, os.ErrNotExist) {
			return nil, Descriptor{}, fmt.Errorf("%w: %s", ErrNotFound, d)
		}
		return nil, Descriptor{}, err
	}
	h, _ := newHash(defaultAlgorithm)
	vr := &verifyReader{
		f:         f,
		h:         h,
		sum:       func() Digest { return digestFromHash(defaultAlgorithm, h) },
		declared:  d,
		wantSize:  desc.Size,
		onCorrupt: func(x Digest) { s.quarantine(x) },
		endOp:     s.endOp,
	}
	return vr, desc, nil
}

// StatBlob 返回 blob 描述符；缺失返回 ErrNotFound，损坏返回 ErrDigestMismatch。
// Stat 不读全量字节（大层场景）：长度级损坏由 meta 校验即时捕获，
// 摘要级损坏在 GetBlob / 清单读取路径上捕获。
func (s *Store) StatBlob(_ context.Context, d Digest) (Descriptor, error) {
	if err := d.validate(); err != nil {
		return Descriptor{}, err
	}
	desc, presence, err := s.readMeta(d)
	if err != nil {
		return Descriptor{}, err
	}
	switch presence {
	case presenceMissing:
		return Descriptor{}, fmt.Errorf("%w: %s", ErrNotFound, d)
	case presenceCorrupt:
		return Descriptor{}, &DigestMismatchError{Op: "StatBlob", Declared: d}
	default:
		return desc, nil
	}
}
