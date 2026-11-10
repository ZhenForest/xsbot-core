package sdk

import (
	"strings"
	"unicode"
)

// QQFaceName returns a QQ system face's display name, never an inferred emotion.
// Only the received native name is used; no offline ID/name table is bundled.
// The original segment and metadata are unchanged. Missing names return false.
func QQFaceName(s Segment) (string, bool) {
	if s.Kind != Face {
		return "", false
	}
	if value := s.Metadata["raw.faceText"]; value.State == Present && value.String != nil {
		if name := cleanFaceName(*value.String); name != "" {
			return name, true
		}
	}
	return "", false
}

func cleanFaceName(value string) string {
	if len(value) > 256 {
		return ""
	}
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		value = value[1 : len(value)-1]
	}
	value = strings.TrimSpace(strings.TrimPrefix(value, "/"))
	if strings.ContainsAny(value, "[]") || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return ""
	}
	return value
}

// QQFaceText renders a Face for readable chat input, retaining unknown IDs.
// Other segment kinds return an empty string; this is not a Router text view.
func QQFaceText(s Segment) string {
	if s.Kind != Face {
		return ""
	}
	if name, ok := QQFaceName(s); ok {
		return "[QQ表情：" + name + "]"
	}
	if id := s.Metadata["id"]; id.State == Present && id.String != nil {
		value := *id.String
		if len(value) > 0 && len(value) <= 20 && strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) < 0 {
			return "[QQ表情：未知，ID=" + value + "]"
		}
	}
	return "[QQ表情：未知]"
}
