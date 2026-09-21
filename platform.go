package ocistore

// Platform 与 OCI 平台属性对齐。
type Platform struct {
	Architecture string   `json:"architecture"`
	OS           string   `json:"os"`
	OSVersion    string   `json:"os.version,omitempty"`
	OSFeatures   []string `json:"os.features,omitempty"`
	Variant      string   `json:"variant,omitempty"`
	Features     []string `json:"features,omitempty"`
}

// String 便于日志与错误信息输出。
func (p Platform) String() string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// matchPlatform 按 OCI 镜像索引规则判定候选平台是否满足请求平台。
//
// 规则要点：
//   - architecture 与 os 必须完全相等（空请求字段表示不约束该维度）；
//   - variant、os.version 仅在请求方给出时要求相等；
//   - 请求方给出的 features / os.features 必须全部被候选包含；
//   - 候选可额外携带请求未要求的特性（不构成失配）；
//   - 不会把 arm/v7 静默回退到 arm/v6 等其它平台。
func matchPlatform(want, got Platform) bool {
	if want.Architecture != "" && want.Architecture != got.Architecture {
		return false
	}
	if want.OS != "" && want.OS != got.OS {
		return false
	}
	if want.Variant != "" && want.Variant != got.Variant {
		return false
	}
	if want.OSVersion != "" && want.OSVersion != got.OSVersion {
		return false
	}
	if !containsAll(got.Features, want.Features) {
		return false
	}
	if !containsAll(got.OSFeatures, want.OSFeatures) {
		return false
	}
	return true
}

// containsAll 报告 required 中的每个元素是否都出现在 provided 中。
func containsAll(provided, required []string) bool {
	for _, r := range required {
		found := false
		for _, p := range provided {
			if p == r {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
