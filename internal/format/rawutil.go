package format

// Shared raw-JSON helpers for the format adapters.

import (
	"encoding/json"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// NormalizeExtensionObject returns ABSENT for a member-less object after
// canonical-field removal (adapter input parity: a request with no unknown
// fields carries no extensions). Present objects are returned unchanged;
// malformed bytes yield an error.
func NormalizeExtensionObject(ext engine.OptionalJSONObject) (engine.OptionalJSONObject, error) {
	if ext.IsAbsent() {
		return ext, nil
	}
	m, _, err := ext.DecodeObject()
	if err != nil {
		return ext, err
	}
	if len(m) == 0 {
		return engine.OptionalJSONObject{}, nil
	}
	return ext, nil
}

// MergeRawMembers splices the members of a raw object into dst, values
// verbatim. Deterministic: the caller marshals dst via encoding/json, which
// sorts keys. Returns an error if raw is not a valid object — never silently
// drops the extension payload.
func MergeRawMembers(dst map[string]json.RawMessage, raw []byte) error {
	var src map[string]json.RawMessage
	if err := json.Unmarshal(raw, &src); err != nil {
		return err
	}
	for k, v := range src {
		dst[k] = v
	}
	return nil
}

// MergeRawMembersFiltered is MergeRawMembers with a keep filter (nil keeps
// all members).
func MergeRawMembersFiltered(dst map[string]json.RawMessage, raw []byte, keep func(string) bool) error {
	var src map[string]json.RawMessage
	if err := json.Unmarshal(raw, &src); err != nil {
		return err
	}
	for k, v := range src {
		if keep == nil || keep(k) {
			dst[k] = v
		}
	}
	return nil
}
