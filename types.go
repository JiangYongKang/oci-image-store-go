package ocistore

// Descriptor 是对仓库内某一内容寻址对象的声明，字段与 OCI 描述符对齐。
type Descriptor struct {
	MediaType   string            `json:"mediaType,omitempty"`
	Digest      Digest            `json:"digest"`
	Size        int64             `json:"size"`
	URLs        []string          `json:"urls,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Platform    *Platform         `json:"platform,omitempty"`
	Data        []byte            `json:"data,omitempty"`
}

// Options 控制存储实例的行为。
type Options struct {
	// QuotaBytes 为存储配额（字节）。0 表示不限制。
	QuotaBytes int64
}
