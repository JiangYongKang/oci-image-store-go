package ocistore

import "errors"

// 可区分的失败原因：调用方可用 errors.Is 精确判定，而不必解析错误文本。
var (
	ErrNotExist        = errors.New("ocistore: content does not exist")
	ErrCorrupted       = errors.New("ocistore: content corrupted: digest does not match stored bytes")
	ErrDigestMismatch  = errors.New("ocistore: declared digest does not match actual content")
	ErrDigestInvalid   = errors.New("ocistore: invalid digest")
	ErrManifestBadJSON = errors.New("ocistore: manifest truncated or not valid JSON")
	ErrMediaType       = errors.New("ocistore: unsupported media type")
	ErrPlatformNoMatch = errors.New("ocistore: no manifest matched the requested platform")
	ErrPlatformBad     = errors.New("ocistore: invalid platform descriptor")
	ErrQuotaExceeded   = errors.New("ocistore: storage quota exceeded")
	ErrLabelNotExist   = errors.New("ocistore: label does not exist")
	ErrTagInvalid      = errors.New("ocistore: invalid tag")
)

// DigestError 携带声明摘要与实际摘要，便于测试日志判定失败依据。
type DigestError struct {
	Declared string
	Actual   string
	Op       string
}

func (e *DigestError) Error() string {
	return "ocistore: digest mismatch during " + e.Op +
		": declared=" + e.Declared + " actual=" + e.Actual
}

// Unwrap 使 errors.Is(err, ErrDigestMismatch) 成立；
// 对于已落盘内容的损坏，调用方同时可用 ErrCorrupted 判定。
func (e *DigestError) Unwrap() error { return ErrDigestMismatch }
