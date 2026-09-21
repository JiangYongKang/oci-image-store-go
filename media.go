package ocistore

// 受支持的媒体类型（OCI 与 Docker v2 兼容清单）。
const (
	MediaTypeOCIIndex    = "application/vnd.oci.image.index.v1+json"
	MediaTypeOCIManifest = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeOCIConfig   = "application/vnd.oci.image.config.v1+json"
	MediaTypeOCILayer    = "application/vnd.oci.image.layer.v1.tar+gzip"

	MediaTypeDockerIndex    = "application/vnd.docker.distribution.manifest.list.v2+json"
	MediaTypeDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeDockerConfig   = "application/vnd.docker.container.image.v1+json"
	MediaTypeDockerLayer    = "application/vnd.docker.image.rootfs.diff.tar.gzip"

	// MediaTypeOctetStream 用于无法明确归类的普通 blob。
	MediaTypeOctetStream = "application/octet-stream"
)

// kind 表示清单媒体类型的分类。
type manifestKind int

const (
	kindUnknown manifestKind = iota
	kindIndex
	kindImage
)

// classifyManifestMediaType 对清单媒体类型做显式分类：
// 受支持则返回类别；不支持返回 kindUnknown，调用方必须据此拒绝。
func classifyManifestMediaType(mediaType string) manifestKind {
	switch mediaType {
	case MediaTypeOCIIndex, MediaTypeDockerIndex:
		return kindIndex
	case MediaTypeOCIManifest, MediaTypeDockerManifest:
		return kindImage
	default:
		return kindUnknown
	}
}

// isManifestMediaType 报告该类型是否为受支持的清单类型。
func isManifestMediaType(mediaType string) bool {
	return classifyManifestMediaType(mediaType) != kindUnknown
}
