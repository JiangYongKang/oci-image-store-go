package ocistore

import "bytes"

// byteReader 返回基于字节切片的 Reader，供原子写入复用。
func byteReader(data []byte) *bytes.Reader { return bytes.NewReader(data) }
