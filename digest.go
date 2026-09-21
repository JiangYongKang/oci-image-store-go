package ocistore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"strings"
)

// Digest 是 "algorithm:encoded" 形式的内容摘要，例如 sha256:hex...
type Digest string

const defaultAlgorithm = "sha256"

// ParseDigest 严格校验摘要的算法与编码形式。
func ParseDigest(s string) (Digest, error) {
	algo, enc, ok := strings.Cut(s, ":")
	if !ok || algo == "" || enc == "" {
		return "", fmt.Errorf("%w: %q", ErrInvalidRef, s)
	}
	if err := validateEncoded(algo, enc); err != nil {
		return "", err
	}
	return Digest(s), nil
}

func validateEncoded(algo, enc string) error {
	switch algo {
	case "sha256":
		if len(enc) != 64 {
			return fmt.Errorf("%w: bad sha256 digest length", ErrInvalidRef)
		}
		if _, err := hex.DecodeString(enc); err != nil {
			return fmt.Errorf("%w: bad sha256 digest encoding: %v", ErrInvalidRef, err)
		}
	default:
		return fmt.Errorf("%w: unsupported digest algorithm %q", ErrInvalidRef, algo)
	}
	return nil
}

func (d Digest) Algorithm() string {
	algo, _, _ := strings.Cut(string(d), ":")
	return algo
}

func (d Digest) Encoded() string {
	_, enc, _ := strings.Cut(string(d), ":")
	return enc
}

func (d Digest) String() string { return string(d) }

// validate 保证摘要自身格式合法。
func (d Digest) validate() error {
	_, err := ParseDigest(string(d))
	return err
}

// newHash 返回指定算法的哈希实例。
func newHash(alg string) (hash.Hash, error) {
	switch alg {
	case "sha256", "":
		return sha256.New(), nil
	default:
		return nil, fmt.Errorf("%w: unsupported digest algorithm %q", ErrInvalidRef, alg)
	}
}

// digestFromHash 从已完成的哈希实例生成摘要。
func digestFromHash(alg string, h hash.Hash) Digest {
	if alg == "" {
		alg = defaultAlgorithm
	}
	return Digest(alg + ":" + hex.EncodeToString(h.Sum(nil)))
}

// digestReader 计算 r 全部内容的摘要。
func digestReader(alg string, r io.Reader) (Digest, error) {
	h, err := newHash(alg)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	if alg == "" {
		alg = defaultAlgorithm
	}
	return Digest(alg + ":" + hex.EncodeToString(h.Sum(nil))), nil
}
