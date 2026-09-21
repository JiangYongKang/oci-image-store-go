package ocistore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Index 对应镜像索引 / manifest list。
// 额外字段通过 json.Decoder 的默认行为被容忍（向前兼容）。
type Index struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType,omitempty"`
	Manifests     []Descriptor      `json:"manifests"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// ImageManifest 对应单平台镜像清单。
type ImageManifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType,omitempty"`
	Config        Descriptor        `json:"config"`
	Layers        []Descriptor      `json:"layers"`
	Subject       *Descriptor       `json:"subject,omitempty"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// rawManifest 保存清单原文、最终采用的媒体类型与解析结果。
type rawManifest struct {
	mediaType string
	data      []byte
	kind      manifestKind
	index     *Index
	image     *ImageManifest
}

// parseManifest 解析清单原文。
//
// declaredMediaType 为标签/外层描述符声明的类型；为空时回退到清单体内
// 的 mediaType 字段（旧格式/按摘要读取时可能没有外层声明）。两者都无法
// 识别时按 ErrUnsupportedMediaType 明确拒绝，绝不猜测解析方向。
//
// JSON 被截断、存在尾随垃圾、关键字段缺失或子描述符非法时返回
// ErrManifestTruncated；额外字段被容忍，不影响解析结论。
func parseManifest(data []byte, declaredMediaType string) (*rawManifest, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: empty payload", ErrManifestTruncated)
	}

	mediaType := declaredMediaType
	if mediaType == "" {
		// 只窥视类型字段，不据此判定 JSON 完整性。
		var probe struct {
			MediaType     string `json:"mediaType"`
			SchemaVersion int    `json:"schemaVersion"`
		}
		if err := decodeJSON(data, &probe); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrManifestTruncated, err)
		}
		mediaType = probe.MediaType
	}

	kind := classifyManifestMediaType(mediaType)
	if kind == kindUnknown {
		return nil, &UnsupportedMediaTypeError{MediaType: mediaType}
	}

	rm := &rawManifest{mediaType: mediaType, data: data, kind: kind}

	switch kind {
	case kindIndex:
		var idx Index
		if err := decodeJSON(data, &idx); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrManifestTruncated, err)
		}
		if idx.SchemaVersion != 0 && idx.SchemaVersion != 2 {
			return nil, &UnsupportedMediaTypeError{
				MediaType: fmt.Sprintf("%s (schemaVersion=%d)", mediaType, idx.SchemaVersion),
			}
		}
		if len(idx.Manifests) == 0 {
			return nil, fmt.Errorf("%w: index contains no manifests", ErrManifestTruncated)
		}
		for i := range idx.Manifests {
			if err := validateChildDescriptor(idx.Manifests[i], "index.manifests["+itoa(i)+"]"); err != nil {
				return nil, err
			}
		}
		rm.index = &idx
	case kindImage:
		var im ImageManifest
		if err := decodeJSON(data, &im); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrManifestTruncated, err)
		}
		if im.SchemaVersion != 0 && im.SchemaVersion != 2 {
			return nil, &UnsupportedMediaTypeError{
				MediaType: fmt.Sprintf("%s (schemaVersion=%d)", mediaType, im.SchemaVersion),
			}
		}
		if err := validateChildDescriptor(im.Config, "manifest.config"); err != nil {
			return nil, err
		}
		for i := range im.Layers {
			if err := validateChildDescriptor(im.Layers[i], "manifest.layers["+itoa(i)+"]"); err != nil {
				return nil, err
			}
		}
		rm.image = &im
	}

	return rm, nil
}

// validateChildDescriptor 校验被引用描述符的最小合法性。
func validateChildDescriptor(d Descriptor, where string) error {
	if d.Digest == "" {
		return fmt.Errorf("%w: %s: missing digest", ErrManifestTruncated, where)
	}
	if err := d.Digest.validate(); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrManifestTruncated, where, err)
	}
	if d.Size < 0 {
		return fmt.Errorf("%w: %s: negative size", ErrManifestTruncated, where)
	}
	return nil
}

// decodeJSON 严格解码：拒绝截断 JSON、尾随垃圾；容忍未知字段。
func decodeJSON(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return errors.New("payload truncated: " + err.Error())
		}
		return errors.New("invalid JSON: " + err.Error())
	}
	// 顶层 JSON 之后只允许空白；出现尾随 token 说明内容被拼接或损坏。
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing data after JSON document")
		}
		return errors.New("invalid JSON: " + err.Error())
	}
	return nil
}

// itoa 避免为单个小整数场景引入 strconv 的噪音（保持依赖最小）。
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
