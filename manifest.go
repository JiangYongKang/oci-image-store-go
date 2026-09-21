package ocistore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// OCI / Docker 清单媒体类型。未知类型必须被明确拒绝。
const (
	MediaTypeOCIIndex    = "application/vnd.oci.image.index.v1+json"
	MediaTypeDockerIndex = "application/vnd.docker.distribution.manifest.list.v2+json"
	MediaTypeOCIManifest = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeDockerMani  = "application/vnd.docker.distribution.manifest.v2+json"
)

var indexMediaTypes = map[string]bool{
	MediaTypeOCIIndex:    true,
	MediaTypeDockerIndex: true,
}

var manifestMediaTypes = map[string]bool{
	MediaTypeOCIManifest: true,
	MediaTypeDockerMani:  true,
}

// Descriptor 描述一块内容寻址数据。
type Descriptor struct {
	MediaType   string            `json:"mediaType,omitempty"`
	Digest      Digest            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
	URLs        []string          `json:"urls,omitempty"`
	Platform    *Platform         `json:"platform,omitempty"`
}

// Platform 标识索引条目的目标平台。
type Platform struct {
	Architecture string   `json:"architecture"`
	OS           string   `json:"os"`
	OSVersion    string   `json:"os.version,omitempty"`
	OSFeatures   []string `json:"os.features,omitempty"`
	Variant      string   `json:"variant,omitempty"`
	Features     []string `json:"features,omitempty"`
}

func (p Platform) String() string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// ImageManifest 是单平台镜像清单（OCI 与 Docker v2 同构）。
type ImageManifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType,omitempty"`
	Config        Descriptor        `json:"config"`
	Layers        []Descriptor      `json:"layers"`
	Annotations   map[string]string `json:"annotations,omitempty"`
	// Unknown 保留上游对象中的额外字段：兼容接收但不参与解析结论。
	Unknown map[string]json.RawMessage `json:"-"`
}

// ImageIndex 是多架构清单索引。
type ImageIndex struct {
	SchemaVersion int                        `json:"schemaVersion"`
	MediaType     string                     `json:"mediaType,omitempty"`
	Manifests     []Descriptor               `json:"manifests"`
	Annotations   map[string]string          `json:"annotations,omitempty"`
	Unknown       map[string]json.RawMessage `json:"-"`
}

// knownIndexFields / knownManifestFields 用于从原始 JSON 中摘出额外字段。
var knownIndexFields = map[string]bool{
	"schemaVersion": true, "mediaType": true, "manifests": true, "annotations": true,
}

var knownManifestFields = map[string]bool{
	"schemaVersion": true, "mediaType": true, "config": true, "layers": true, "annotations": true,
}

// decodeStrict 解析 JSON：截断/非法 JSON 一律 ErrManifestBadJSON，不做任何容忍修复。
func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %v", ErrManifestBadJSON, err)
	}
	// 拒绝尾部多余 JSON 值；纯空白尾随允许。
	var extra json.RawMessage
	err := dec.Decode(&extra)
	if err == nil {
		return fmt.Errorf("%w: unexpected trailing data", ErrManifestBadJSON)
	}
	if err != io.EOF {
		return fmt.Errorf("%w: %v", ErrManifestBadJSON, err)
	}
	return nil
}

// parseManifest 根据声明/内嵌媒体类型解析清单字节。
//   - 截断或非法 JSON            -> ErrManifestBadJSON
//   - 明确给出但不支持的媒体类型  -> ErrMediaType（绝不忽略）
//   - 媒体类型缺失时按结构嗅探   -> "manifests" 为索引，"config" 为镜像清单
//
// 上游额外字段被保留但不影响解析结论。
func parseManifest(declaredMediaType string, data []byte) (isIndex bool, idx *ImageIndex, mani *ImageManifest, err error) {
	var raw map[string]json.RawMessage
	if e := decodeStrict(data, &raw); e != nil {
		return false, nil, nil, e
	}

	var embedded string
	if mt, ok := raw["mediaType"]; ok {
		_ = json.Unmarshal(mt, &embedded)
	}
	mt := declaredMediaType
	if mt == "" {
		mt = embedded
	}

	switch {
	case mt != "" && indexMediaTypes[mt]:
		isIndex = true
	case mt != "" && manifestMediaTypes[mt]:
		isIndex = false
	case mt != "":
		return false, nil, nil, fmt.Errorf("%w: %q", ErrMediaType, mt)
	default:
		// 无任何媒体类型（OCI 清单允许省略）：按结构确定性嗅探。
		if _, ok := raw["manifests"]; ok {
			isIndex = true
		} else if _, ok := raw["config"]; ok {
			isIndex = false
		} else {
			return false, nil, nil, fmt.Errorf("%w: cannot infer manifest kind", ErrMediaType)
		}
	}

	if isIndex {
		var ix ImageIndex
		if e := decodeStrict(data, &ix); e != nil {
			return false, nil, nil, e
		}
		ix.Unknown = unknownFields(raw, knownIndexFields)
		if ix.SchemaVersion == 0 {
			ix.SchemaVersion = 2
		}
		return true, &ix, nil, nil
	}

	var im ImageManifest
	if e := decodeStrict(data, &im); e != nil {
		return false, nil, nil, e
	}
	im.Unknown = unknownFields(raw, knownManifestFields)
	if im.SchemaVersion == 0 {
		im.SchemaVersion = 2
	}
	return false, nil, &im, nil
}

func unknownFields(raw map[string]json.RawMessage, known map[string]bool) map[string]json.RawMessage {
	var u map[string]json.RawMessage
	for k, v := range raw {
		if known[k] {
			continue
		}
		if u == nil {
			u = map[string]json.RawMessage{}
		}
		u[k] = v
	}
	return u
}

// platformEqual 严格匹配平台：OS、架构、变体必须完全一致，不做隐式回退。
func platformEqual(d *Descriptor, want Platform) bool {
	if d.Platform == nil {
		return false
	}
	got := *d.Platform
	if got.OS != want.OS || got.Architecture != want.Architecture {
		return false
	}
	return got.Variant == want.Variant
}

// selectPlatform 在索引中严格选择平台。
//   - 条目摘要非法 -> ErrPlatformBad（不静默跳过坏条目）
//   - 无匹配       -> ErrPlatformNoMatch（绝不回退到其它平台）
func selectPlatform(idx *ImageIndex, want Platform) (Descriptor, error) {
	if idx == nil {
		return Descriptor{}, fmt.Errorf("%w: not an index", ErrPlatformBad)
	}
	for i := range idx.Manifests {
		d := idx.Manifests[i]
		if _, err := ParseDigest(string(d.Digest)); err != nil {
			return Descriptor{}, fmt.Errorf("%w: manifests[%d] has invalid digest %q", ErrPlatformBad, i, d.Digest)
		}
		if platformEqual(&d, want) {
			return d, nil
		}
	}
	return Descriptor{}, fmt.Errorf("%w: %s in index with %d entries", ErrPlatformNoMatch, want, len(idx.Manifests))
}

// referencedDigests 返回清单直接引用的内容摘要（config + layers，去重保序）。
func (m *ImageManifest) referencedDigests() []Digest {
	seen := map[Digest]bool{}
	var out []Digest
	add := func(d Digest) {
		if d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	add(m.Config.Digest)
	for _, l := range m.Layers {
		add(l.Digest)
	}
	return out
}
