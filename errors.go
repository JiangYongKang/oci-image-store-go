package ocistore

import "fmt"

// 可区分的失败原因。调用方应使用 errors.Is 判定类别，
// 需要细节时再用 errors.As 取具体错误类型。
var (
	ErrNotFound             = sentinelError("ocistore: content not found")
	ErrDigestMismatch       = sentinelError("ocistore: digest mismatch")
	ErrManifestTruncated    = sentinelError("ocistore: manifest truncated or malformed")
	ErrPlatformNotFound     = sentinelError("ocistore: no manifest matching requested platform")
	ErrUnsupportedMediaType = sentinelError("ocistore: unsupported media type")
	ErrQuotaExceeded        = sentinelError("ocistore: storage quota exceeded")
	ErrInvalidRef           = sentinelError("ocistore: invalid reference")
	ErrLeaseReleased        = sentinelError("ocistore: lease already released")
)

type sentinelError string

func (e sentinelError) Error() string { return string(e) }

// notFound 构造带摘要的 ErrNotFound。
func notFound(d Digest) error {
	return fmt.Errorf("%w: %s", ErrNotFound, d)
}

// DigestMismatchError 携带声明摘要与实际摘要，便于测试区分判定依据。
type DigestMismatchError struct {
	Op       string
	Declared Digest
	Actual   Digest
}

func (e *DigestMismatchError) Error() string {
	if e.Op != "" {
		return fmt.Sprintf("ocistore: %s: digest mismatch: declared=%s actual=%s",
			e.Op, e.Declared, e.Actual)
	}
	return fmt.Sprintf("ocistore: digest mismatch: declared=%s actual=%s", e.Declared, e.Actual)
}

func (e *DigestMismatchError) Is(target error) bool { return target == ErrDigestMismatch }

// PlatformNotFoundError 携带请求平台与候选平台集合。
type PlatformNotFoundError struct {
	Wanted    Platform
	Available []Platform
}

func (e *PlatformNotFoundError) Error() string {
	return fmt.Sprintf("ocistore: no manifest for platform %s/%s (variants=%d)",
		e.Wanted.OS, e.Wanted.Architecture, len(e.Available))
}

func (e *PlatformNotFoundError) Is(target error) bool { return target == ErrPlatformNotFound }

// QuotaExceededError 携带配额、已用量与本次需求。
type QuotaExceededError struct {
	Quota    int64
	Used     int64
	Required int64
}

func (e *QuotaExceededError) Error() string {
	return fmt.Sprintf("ocistore: quota exceeded: quota=%d used=%d required=%d",
		e.Quota, e.Used, e.Required)
}

func (e *QuotaExceededError) Is(target error) bool { return target == ErrQuotaExceeded }

// UnsupportedMediaTypeError 携带被拒绝的媒体类型。
type UnsupportedMediaTypeError struct {
	MediaType string
}

func (e *UnsupportedMediaTypeError) Error() string {
	return fmt.Sprintf("ocistore: unsupported media type %q", e.MediaType)
}

func (e *UnsupportedMediaTypeError) Is(target error) bool { return target == ErrUnsupportedMediaType }
