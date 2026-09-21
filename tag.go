package ocistore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// tagRecord 是标签指针文件的磁盘表示。
type tagRecord struct {
	Digest    Digest `json:"digest"`
	MediaType string `json:"mediaType,omitempty"`
	Size      int64  `json:"size"`
}

// isDigestRef 判断引用是否为 "algorithm:hex" 摘要形式。
func isDigestRef(ref string) bool {
	d, err := ParseDigest(ref)
	return err == nil && d != ""
}

// validateTagName 校验标签名：非空、不含空白、长度受限。
func validateTagName(tag string) error {
	if tag == "" || len(tag) > 256 {
		return fmt.Errorf("%w: tag %q: empty or too long", ErrInvalidRef, tag)
	}
	if strings.TrimSpace(tag) != tag {
		return fmt.Errorf("%w: tag %q: leading/trailing whitespace", ErrInvalidRef, tag)
	}
	if strings.ContainsAny(tag, "\x00") {
		return fmt.Errorf("%w: tag %q: null byte", ErrInvalidRef, tag)
	}
	return nil
}

// PutManifest 写入清单内容，并可同时把 ref（标签或 digest 字符串）指向它。
// ref 为空表示仅按摘要写入。重复写入相同内容幂等，不产生重复副本。
//
// 写入的清单内容按其自身摘要固定；后续标签迁移只改动标签指针，
// 已经按摘要存在的内容不会被改变或删除。
func (s *Store) PutManifest(ctx context.Context, ref, mediaType string, data []byte) (Descriptor, error) {
	if ref != "" && !isDigestRef(ref) {
		if err := validateTagName(ref); err != nil {
			return Descriptor{}, err
		}
	}

	// 先解析校验清单：不支持的媒体类型 / 截断 / 非法子描述符在此拒绝，
	// 避免坏清单进入存储。
	rm, err := parseManifest(data, mediaType)
	if err != nil {
		return Descriptor{}, err
	}
	effectiveMedia := rm.mediaType

	desc, err := s.PutBlobBytes(ctx, effectiveMedia, data)
	if err != nil {
		return Descriptor{}, err
	}

	if ref != "" && !isDigestRef(ref) {
		if err := s.Tag(ctx, ref, desc.Digest); err != nil {
			return Descriptor{}, err
		}
	} else if isDigestRef(ref) && Digest(ref) != desc.Digest {
		// 显式按摘要写入时，声明摘要必须与实际内容一致。
		return Descriptor{}, &DigestMismatchError{
			Op:       "PutManifest",
			Declared: Digest(ref),
			Actual:   desc.Digest,
		}
	}
	return desc, nil
}

// Tag 将标签指向给定摘要（标签迁移）。目标内容必须完整存在。
// 仅改动标签指针；原标签指向的、仍被其它引用可达的内容不受影响。
func (s *Store) Tag(_ context.Context, tag string, d Digest) error {
	if err := validateTagName(tag); err != nil {
		return err
	}
	if err := d.validate(); err != nil {
		return err
	}

	s.beginOp()
	defer s.endOp()

	desc, presence, err := s.readMeta(d)
	if err != nil {
		return err
	}
	switch presence {
	case presenceMissing:
		return fmt.Errorf("%w: cannot tag missing content %s", ErrNotFound, d)
	case presenceCorrupt:
		return &DigestMismatchError{Op: "Tag", Declared: d}
	}
	rec := tagRecord{Digest: d, MediaType: desc.MediaType, Size: desc.Size}
	buf, err := json.Marshal(&rec)
	if err != nil {
		return err
	}
	path := s.tagFilePath(tag)
	return atomicWriteBytes(tagsDir(s.root), path, buf, 0o644)
}

// readTagRecord 读取标签指针；缺失返回 ErrNotFound。
func (s *Store) readTagRecord(tag string) (tagRecord, error) {
	var rec tagRecord
	buf, err := os.ReadFile(s.tagFilePath(tag))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return rec, fmt.Errorf("%w: tag %q", ErrNotFound, tag)
		}
		return rec, err
	}
	if err := json.Unmarshal(buf, &rec); err != nil {
		return rec, fmt.Errorf("ocistore: corrupt tag pointer %q: %w", tag, err)
	}
	if err := rec.Digest.validate(); err != nil {
		return rec, err
	}
	return rec, nil
}

// ResolveTag 解析标签当前指向的描述符。
func (s *Store) ResolveTag(_ context.Context, tag string) (Descriptor, error) {
	if err := validateTagName(tag); err != nil {
		return Descriptor{}, err
	}
	rec, err := s.readTagRecord(tag)
	if err != nil {
		return Descriptor{}, err
	}
	desc, presence, err := s.readMeta(rec.Digest)
	if err != nil {
		return Descriptor{}, err
	}
	switch presence {
	case presenceMissing:
		return Descriptor{}, fmt.Errorf("%w: tag %q points to %s", ErrNotFound, tag, rec.Digest)
	case presenceCorrupt:
		return Descriptor{}, &DigestMismatchError{Op: "ResolveTag:" + tag, Declared: rec.Digest}
	}
	return desc, nil
}

// GetManifest 按标签或摘要读取清单原文，并校验完整性。
// 读取路径上内容与声明摘要不符会返回 ErrDigestMismatch，绝不返回半写入数据。
func (s *Store) GetManifest(ctx context.Context, ref string) ([]byte, Descriptor, error) {
	var target Digest
	if isDigestRef(ref) {
		target = Digest(ref)
	} else {
		if err := validateTagName(ref); err != nil {
			return nil, Descriptor{}, err
		}
		rec, err := s.readTagRecord(ref)
		if err != nil {
			return nil, Descriptor{}, err
		}
		target = rec.Digest
	}

	rc, desc, err := s.GetBlob(ctx, target)
	if err != nil {
		return nil, Descriptor{}, err
	}
	defer rc.Close()
	buf, err := io.ReadAll(rc)
	if err != nil {
		return nil, Descriptor{}, err
	}
	// 摘要校验发生在 EOF（verifyReader），长度也在此复核。
	if int64(len(buf)) != desc.Size {
		s.quarantine(target)
		return nil, Descriptor{}, &DigestMismatchError{Op: "GetManifest", Declared: target}
	}
	// 再次确保它确实是一份受支持的清单，拒绝“把普通 blob 当清单读”。
	if _, err := parseManifest(buf, desc.MediaType); err != nil {
		return nil, Descriptor{}, err
	}
	return buf, desc, nil
}

// DeleteTag 删除标签指针；blob 仅在 GC 且不可达时回收。
func (s *Store) DeleteTag(_ context.Context, tag string) error {
	if err := validateTagName(tag); err != nil {
		return err
	}
	s.beginOp()
	defer s.endOp()
	if err := os.Remove(s.tagFilePath(tag)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: tag %q", ErrNotFound, tag)
		}
		return err
	}
	fsyncDir(tagsDir(s.root))
	return nil
}

// Tags 返回全部标签。
func (s *Store) Tags(_ context.Context) ([]string, error) {
	entries, err := os.ReadDir(tagsDir(s.root))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if tag, ok := decodeTag(e.Name()); ok {
			out = append(out, tag)
		}
	}
	return out, nil
}

// Exists 报告摘要对应内容是否完整存在；损坏内容不算存在。
func (s *Store) Exists(_ context.Context, d Digest) bool {
	_, presence, err := s.readMeta(d)
	return err == nil && presence == presenceOK
}
