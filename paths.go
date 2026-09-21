package ocistore

import (
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
)

// 磁盘布局：
//
//	<root>/blobs/sha256/<hex>                 内容寻址 blob（原子 rename 落盘）
//	<root>/blobs/sha256/<hex>.meta           blob 元数据 JSON（Descriptor）
//	<root>/tags/<encoded-tag>                标签指针 JSON（Descriptor）
//	<root>/leases/<id>                       租约固定摘要（每行一个 digest）
//	<root>/quarantine/<hex>                  校验失败被隔离的内容
//	<root>/tmp/                              写入过程中的临时文件
//	<root>/state.json                        已用空间等可重建的统计状态
//
// 所有对外可见的文件都通过“写临时文件 + fsync + rename”原子发布，
// 读取方永远不会观察到半写入内容。
func blobPath(root, alg, hx string) string     { return filepath.Join(root, "blobs", alg, hx) }
func blobMetaPath(root, alg, hx string) string { return blobPath(root, alg, hx) + ".meta" }
func tagsDir(root string) string               { return filepath.Join(root, "tags") }
func leasesDir(root string) string             { return filepath.Join(root, "leases") }
func quarantineDir(root string) string         { return filepath.Join(root, "quarantine") }
func tmpDir(root string) string                { return filepath.Join(root, "tmp") }
func statePath(root string) string             { return filepath.Join(root, "state.json") }

// digestPaths 按摘要推导 blob 与其元数据的绝对路径。
func (s *Store) digestPaths(d Digest) (blob, meta string, err error) {
	if err = d.validate(); err != nil {
		return "", "", err
	}
	blob = blobPath(s.root, d.Algorithm(), d.Encoded())
	meta = blobMetaPath(s.root, d.Algorithm(), d.Encoded())
	return blob, meta, nil
}

// encodeTag 将标签名编码成只含安全字符的文件名。
// 使用 "h" + hex，保证不同标签不会冲突（hex 以 h 开头，彼此可逆）。
func encodeTag(tag string) string {
	return "t" + hex.EncodeToString([]byte(tag))
}

// decodeTag 是 encodeTag 的逆运算。
func decodeTag(name string) (string, bool) {
	if len(name) < 1 || name[0] != 't' {
		return "", false
	}
	b, err := hex.DecodeString(name[1:])
	if err != nil {
		return "", false
	}
	return string(b), true
}

func (s *Store) tagFilePath(tag string) string {
	return filepath.Join(tagsDir(s.root), encodeTag(tag))
}

// atomicWriteFile 在 dir 下创建一个临时文件，写入 r 的内容并 fsync，
// 最后原子 rename 到 target。失败时清理临时文件，不留下任何部分状态。
func atomicWriteFile(dir, target string, r io.Reader, mode os.FileMode) (int64, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(dir, ".incoming-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	// 任一步失败都回收临时文件。
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	n, err := io.Copy(tmp, r)
	if err != nil {
		cleanup()
		return 0, err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return 0, err
	}
	if err := tmp.Chmod(mode); err != nil {
		cleanup()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return 0, err
	}
	if err := os.Rename(tmpName, target); err != nil {
		_ = os.Remove(tmpName)
		return 0, err
	}
	// 尽力 fsync 所在目录以持久化 rename 条目。
	if df, err := os.Open(dir); err == nil {
		_ = df.Sync()
		_ = df.Close()
	}
	return n, nil
}

// atomicWriteBytes 是 atomicWriteFile 的字节便捷形式。
func atomicWriteBytes(dir, target string, data []byte, mode os.FileMode) error {
	_, err := atomicWriteFile(dir, target, byteReader(data), mode)
	return err
}

// fsyncDir 尽力同步目录元数据。
func fsyncDir(dir string) {
	if df, err := os.Open(dir); err == nil {
		_ = df.Sync()
		_ = df.Close()
	}
}
