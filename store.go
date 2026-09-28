package ocistore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
)

// Options 配置存储。QuotaBytes<=0 表示不限制。
type Options struct {
	RootDir    string
	QuotaBytes int64
}

// Stats 报告当前存储占用。
type Stats struct {
	BlobCount int
	Bytes     int64
	Quota     int64
	TagCount  int
}

// 磁盘布局（详见 docs/STORAGE.md）:
//
//	<root>/blobs/sha256/<ab>/<full64hex>   内容寻址 blob（rename 原子发布）
//	<root>/tags/<tag>                      标签文件，内容为摘要（rename 原子迁移）
//	<root>/tmp/                            临时文件，发布前对读者不可见
//	<root>/quarantine/<hex>                校验失败隔离区，永不作为缓存命中
const (
	dirBlobs      = "blobs/sha256"
	dirTags       = "tags"
	dirTmp        = "tmp"
	dirQuarantine = "quarantine"
	tmpBlobPrefix = ".put-"
	tagFilePerm   = 0o644
	blobFilePerm  = 0o444
)

// 允许常见的 name:tag / name@digest 风格标签字符，但禁止路径分隔与穿越。
var tagNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._:@-]{0,127}$`)

// Store 是本地 OCI 内容存储。并发安全。
type Store struct {
	opt Options

	mu       sync.RWMutex // 保护 tags 视图与配额计算
	gcmu     sync.Mutex   // GC 串行化，并在回收期间冻结发布
	pinmu    sync.Mutex
	pins     map[Digest]int
	inflight flightGroup
}

// Open 打开（必要时初始化）存储目录。
func Open(opt Options) (*Store, error) {
	if opt.RootDir == "" {
		return nil, fmt.Errorf("ocistore: empty RootDir")
	}
	for _, sub := range []string{dirBlobs, dirTags, dirTmp, dirQuarantine} {
		if err := os.MkdirAll(filepath.Join(opt.RootDir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("ocistore: init %s: %w", sub, err)
		}
	}
	if err := cleanupStaleTemp(opt.RootDir); err != nil {
		return nil, err
	}
	s := &Store{opt: opt, pins: map[Digest]int{}}
	return s, nil
}

// Close 释放资源。
func (s *Store) Close() error { return nil }

// ---- 路径辅助 ----

func blobPath(root string, d Digest) string {
	h := d.Hex()
	return filepath.Join(root, dirBlobs, h[:2], h)
}

func tagPath(root, tag string) string { return filepath.Join(root, dirTags, tag) }

func quarantinePath(root string, d Digest) string {
	return filepath.Join(root, dirQuarantine, d.Hex()+"."+randomSuffix())
}

// cleanupStaleTemp 清理上次崩溃遗留的临时文件；正常路径下临时文件会被显式删除。
func cleanupStaleTemp(root string) error {
	tmp := filepath.Join(root, dirTmp)
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return err
	}
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(tmp, e.Name()))
	}
	return nil
}

// ---- 原子发布 ----

// writeAtomic 将 data 写入 dst：先写临时文件并 fsync，再 rename。
// 任何失败路径都清理临时文件，绝不留下半成品。
func writeAtomic(root, dst string, data []byte, perm os.FileMode) (err error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Join(root, dirTmp), tmpBlobPrefix+"*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Chmod(perm); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmpName, dst); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dst))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// ---- Blob 写入 ----

// PutBlob 写入一块内容寻址数据。
//   - declared 非空时必须与实际内容一致，否则 ErrDigestMismatch
//   - 相同内容重复写入不会产生重复副本
//   - 超配额返回 ErrQuotaExceeded，且不遗留临时文件
func (s *Store) PutBlob(ctx context.Context, declared Digest, r io.Reader) (Digest, int64, error) {
	if declared != "" {
		if _, err := ParseDigest(string(declared)); err != nil {
			return "", 0, err
		}
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return "", 0, fmt.Errorf("ocistore: read blob payload: %w", err)
	}
	actual := ComputeDigest(data)
	if declared != "" && declared != actual {
		return actual, int64(len(data)), &DigestError{Declared: declared.String(), Actual: actual.String(), Op: "PutBlob"}
	}

	// 同内容并发写入合并：只允许一个 goroutine 落盘，其余共享结果。
	err = s.inflight.Do("blob:"+actual.String(), func() error {
		return s.publishBlob(actual, data)
	})
	return actual, int64(len(data)), err
}

// publishBlob 在去重 + 配额判定后原子发布 blob。
func (s *Store) publishBlob(d Digest, data []byte) error {
	s.gcmu.Lock() // 与 GC 删除阶段互斥，避免发布/删除竞态
	defer s.gcmu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()

	dst := blobPath(s.opt.RootDir, d)
	if _, err := os.Stat(dst); err == nil {
		// 已存在：校验既有副本，损坏则隔离后重写，绝不把损坏内容当缓存命中。
		existing, rerr := os.ReadFile(dst)
		if rerr == nil && d.Verify(existing) {
			return nil // 去重命中
		}
		if rerr == nil {
			s.quarantine(dst, d)
		}
	}

	if s.opt.QuotaBytes > 0 {
		used, qerr := s.bytesLocked()
		if qerr != nil {
			return qerr
		}
		if used+int64(len(data)) > s.opt.QuotaBytes {
			return fmt.Errorf("%w: used=%d + need=%d > quota=%d",
				ErrQuotaExceeded, used, len(data), s.opt.QuotaBytes)
		}
	}

	return writeAtomic(s.opt.RootDir, dst, data, blobFilePerm)
}

// quarantine 将一个已判定损坏的 blob 移出内容寻址目录，使其永不可被读取命中。
func (s *Store) quarantine(badPath string, d Digest) {
	qdst := quarantinePath(s.opt.RootDir, d)
	if err := os.MkdirAll(filepath.Dir(qdst), 0o755); err != nil {
		_ = os.Remove(badPath)
		return
	}
	if err := os.Rename(badPath, qdst); err != nil {
		_ = os.Remove(badPath)
	}
}

// ---- Blob 读取 ----

// blobData 读取并校验 blob。校验失败的内容被隔离并返回 ErrCorrupted，
// 之后任何路径都无法再把它当作命中结果。
func (s *Store) blobData(d Digest) ([]byte, error) {
	return s.blobDataLocked(d, true)
}

// blobDataGCLocked 与 blobData 相同，但调用方已持有 gcmu
// （损坏隔离的 rename 与 GC 扫描互斥由调用方保证），避免重入死锁。
func (s *Store) blobDataGCLocked(d Digest) ([]byte, error) {
	return s.blobDataLocked(d, false)
}

// blobDataLocked 读取并校验 blob。lockGC=true 时由本函数负责获取 gcmu
// 完成损坏隔离；否则调用方必须已持有 gcmu。
func (s *Store) blobDataLocked(d Digest, lockGC bool) ([]byte, error) {
	if _, err := ParseDigest(string(d)); err != nil {
		return nil, err
	}
	path := blobPath(s.opt.RootDir, d)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("%w: %s", ErrNotExist, d)
	}
	if err != nil {
		return nil, fmt.Errorf("ocistore: read %s: %w", d, err)
	}
	if !d.Verify(data) {
		// 判定损坏：隔离，防止后续缓存命中；返回可区分错误。
		if lockGC {
			s.gcmu.Lock()
		}
		s.quarantine(path, d)
		if lockGC {
			s.gcmu.Unlock()
		}
		return nil, fmt.Errorf("%w: %s", ErrCorrupted, d)
	}
	return data, nil
}

// GetBlob 按摘要读取内容；返回的字节已通过完整性校验。
func (s *Store) GetBlob(ctx context.Context, d Digest) (io.ReadCloser, int64, error) {
	data, err := s.blobData(d)
	if err != nil {
		return nil, 0, err
	}
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}

// Has 报告摘要对应内容是否存在且完好；损坏内容视为不存在。
func (s *Store) Has(d Digest) (bool, error) {
	if _, err := ParseDigest(string(d)); err != nil {
		return false, err
	}
	_, err := s.blobData(d)
	if err != nil {
		if errors.Is(err, ErrNotExist) || errors.Is(err, ErrCorrupted) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ---- 标签 ----

func validateTag(tag string) error {
	if !tagNameRe.MatchString(tag) {
		return fmt.Errorf("%w: %q", ErrTagInvalid, tag)
	}
	return nil
}

// PutTag 将标签固定到某块清单内容。标签可迁移，但旧内容仍可由其摘要访问。
// 目标内容必须存在且完好；标签文件以 rename 原子发布，读者不会读到半写状态。
func (s *Store) PutTag(ctx context.Context, tag string, d Digest) error {
	if err := validateTag(tag); err != nil {
		return err
	}
	if _, err := ParseDigest(string(d)); err != nil {
		return err
	}
	s.gcmu.Lock()
	defer s.gcmu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()

	if ok, _ := s.hasLocked(d); !ok {
		return fmt.Errorf("%w: cannot tag missing content %s", ErrNotExist, d)
	}
	return writeAtomic(s.opt.RootDir, tagPath(s.opt.RootDir, tag),
		[]byte(d.String()), tagFilePerm)
}

// ResolveTag 返回标签当前固定的摘要。
func (s *Store) ResolveTag(tag string) (Digest, error) {
	if err := validateTag(tag); err != nil {
		return "", err
	}
	data, err := os.ReadFile(tagPath(s.opt.RootDir, tag))
	if os.IsNotExist(err) {
		return "", fmt.Errorf("%w: %s", ErrLabelNotExist, tag)
	}
	if err != nil {
		return "", err
	}
	return ParseDigest(string(bytes.TrimSpace(data)))
}

// DeleteTag 删除标签引用；内容本身由后续 GC 按可达性回收。
func (s *Store) DeleteTag(ctx context.Context, tag string) error {
	if err := validateTag(tag); err != nil {
		return err
	}
	s.gcmu.Lock()
	defer s.gcmu.Unlock()
	err := os.Remove(tagPath(s.opt.RootDir, tag))
	if os.IsNotExist(err) {
		return fmt.Errorf("%w: %s", ErrLabelNotExist, tag)
	}
	return err
}

// Tags 返回全部标签（排序，便于测试断言）。
func (s *Store) Tags() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.opt.RootDir, dirTags))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Type().IsRegular() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// ---- 拉取 / 平台解析 ----

// ResolvedImage 是平台解析结果。
type ResolvedImage struct {
	Index              *ImageIndex // 若来源是索引则非空
	Manifest           *ImageManifest
	ManifestDescriptor Descriptor
}

// FetchPlatform 从标签或摘要（ref 以 "sha256:" 开头时按摘要）出发，
// 解析索引并取出目标平台镜像清单。
//   - 清单截断/非法 JSON -> ErrManifestBadJSON
//   - 不支持的媒体类型    -> ErrMediaType
//   - 无匹配平台          -> ErrPlatformNoMatch（不回退其它平台）
//   - 任一内容摘要不符    -> ErrDigestMismatch / ErrCorrupted
func (s *Store) FetchPlatform(ctx context.Context, ref string, p Platform) (*ResolvedImage, error) {
	root, err := s.resolveRef(ref)
	if err != nil {
		return nil, err
	}
	data, err := s.blobData(root)
	if err != nil {
		return nil, err
	}
	isIndex, idx, mani, err := parseManifest("", data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", root, err)
	}

	res := &ResolvedImage{}
	if isIndex {
		res.Index = idx
		desc, perr := selectPlatform(idx, p)
		if perr != nil {
			return nil, perr
		}
		mdata, gerr := s.blobData(desc.Digest)
		if gerr != nil {
			return nil, gerr
		}
		subIsIndex, _, subMani, perr := parseManifest(desc.MediaType, mdata)
		if perr != nil {
			return nil, fmt.Errorf("parse child %s: %w", desc.Digest, perr)
		}
		if subIsIndex {
			return nil, fmt.Errorf("%w: nested index at %s", ErrMediaType, desc.Digest)
		}
		res.Manifest = subMani
		res.ManifestDescriptor = desc
	} else {
		res.Manifest = mani
		res.ManifestDescriptor = Descriptor{Digest: root, Size: int64(len(data))}
	}

	// 校验镜像清单引用的 config/layers 存在且完好。
	for _, rd := range res.Manifest.referencedDigests() {
		if _, herr := s.blobData(rd); herr != nil {
			return nil, fmt.Errorf("referenced content %s: %w", rd, herr)
		}
	}
	return res, nil
}

func (s *Store) resolveRef(ref string) (Digest, error) {
	if d, err := ParseDigest(ref); err == nil {
		return d, nil // 合法摘要引用
	}
	return s.ResolveTag(ref)
}

// ---- Pin ----

// Pin 为进行中的拉取标记保护。调用顺序：先 Pin 根摘要及其将获取的依赖，
// 完成后 Unpin。GC 不会回收被 Pin 的内容。
func (s *Store) Pin(d Digest) error {
	if _, err := ParseDigest(string(d)); err != nil {
		return err
	}
	s.pinmu.Lock()
	s.pins[d]++
	s.pinmu.Unlock()
	return nil
}

// Unpin 释放一次保护计数。
func (s *Store) Unpin(d Digest) error {
	if _, err := ParseDigest(string(d)); err != nil {
		return err
	}
	s.pinmu.Lock()
	defer s.pinmu.Unlock()
	if s.pins[d] <= 0 {
		return fmt.Errorf("ocistore: unpin unpinned digest %s", d)
	}
	s.pins[d]--
	if s.pins[d] == 0 {
		delete(s.pins, d)
	}
	return nil
}

// ---- 统计 / 配额 ----

// hasLocked 调用方需持有相应锁；损坏内容按缺失处理。
func (s *Store) hasLocked(d Digest) (bool, int64) {
	data, err := os.ReadFile(blobPath(s.opt.RootDir, d))
	if err != nil || !d.Verify(data) {
		return false, 0
	}
	return true, int64(len(data))
}

// bytesLocked 遍历内容寻址目录统计实际占用。
func (s *Store) bytesLocked() (int64, error) {
	var total int64
	root := filepath.Join(s.opt.RootDir, dirBlobs)
	err := filepath.WalkDir(root, func(_ string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !e.IsDir() {
			if info, ierr := e.Info(); ierr == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total, err
}

// Stats 返回占用统计。
func (s *Store) Stats() (Stats, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	used, err := s.bytesLocked()
	if err != nil {
		return Stats{}, err
	}
	count := 0
	root := filepath.Join(s.opt.RootDir, dirBlobs)
	_ = filepath.WalkDir(root, func(_ string, e os.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			count++
		}
		return nil
	})
	tags, _ := s.Tags()
	return Stats{BlobCount: count, Bytes: used, Quota: s.opt.QuotaBytes, TagCount: len(tags)}, nil
}
