package ocistore

import "encoding/json"

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func descWithPlatform(digest string, p Platform) Descriptor {
	return Descriptor{Digest: Digest(digest), Size: 1, Platform: &p}
}
