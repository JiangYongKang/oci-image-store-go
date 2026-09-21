package ocistore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

// Digest 是 OCI 风格的内容摘要，形如 sha256:<hex>。
type Digest string

// ParseDigest 校验并规范化摘要字符串。仅支持 sha256。
func ParseDigest(s string) (Digest, error) {
	algo, hexpart, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok || algo != "sha256" || len(hexpart) != 64 {
		return "", fmt.Errorf("%w: %q", ErrDigestInvalid, s)
	}
	if _, err := hex.DecodeString(hexpart); err != nil {
		return "", fmt.Errorf("%w: %q: %v", ErrDigestInvalid, s, err)
	}
	return Digest(algo + ":" + hexpart), nil
}

// ComputeDigest 计算字节内容的 sha256 摘要。
func ComputeDigest(data []byte) Digest {
	sum := sha256.Sum256(data)
	return Digest("sha256:" + hex.EncodeToString(sum[:]))
}

func (d Digest) String() string { return string(d) }

// Algorithm 返回算法名。
func (d Digest) Algorithm() string {
	algo, _, ok := strings.Cut(string(d), ":")
	if !ok {
		return ""
	}
	return algo
}

// Hex 返回摘要的十六进制部分。
func (d Digest) Hex() string {
	_, h, ok := strings.Cut(string(d), ":")
	if !ok {
		return ""
	}
	return h
}

// Verify 判断给定数据是否与摘要一致；摘要本身非法也视为不通过。
func (d Digest) Verify(data []byte) bool {
	if _, err := ParseDigest(string(d)); err != nil {
		return false
	}
	return ComputeDigest(data) == d
}

// verifyReader 消费 reader，边读边算摘要并与 want 比较。
// 返回实际摘要；不符时返回 ErrDigestMismatch（包裹 DigestError）。
func verifyReader(r io.Reader, want Digest) (Digest, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", 0, fmt.Errorf("ocistore: read content for digest: %w", err)
	}
	got := Digest("sha256:" + hex.EncodeToString(h.Sum(nil)))
	if got != want {
		return got, n, &DigestError{Declared: want.String(), Actual: got.String(), Op: "verify"}
	}
	return got, n, nil
}
