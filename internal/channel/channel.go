// Package channel defines fanline channel names and the pattern language
// used by signed tokens to grant access to families of channels.
//
// A channel name is 1–16 dot-separated segments, each 1–64 characters of
// [a-z0-9_-] (lowercase by design: names appear in URLs and event IDs).
// A pattern is shaped like a name but may use `*` to match exactly one
// segment, or `**` as the final segment to match one or more remaining
// segments. Matching is purely structural — no regular expressions.
package channel

import (
	"fmt"
	"strings"
)

const (
	// MaxSegments bounds how deep a channel hierarchy can nest.
	MaxSegments = 16
	// MaxSegmentLen bounds a single dot-separated segment.
	MaxSegmentLen = 64
)

// ValidateName reports whether name is a well-formed concrete channel name.
func ValidateName(name string) error {
	return validate(name, false)
}

// ValidatePattern reports whether pattern is a well-formed channel pattern.
// `*` and `**` are permitted; `**` only as the final segment.
func ValidatePattern(pattern string) error {
	return validate(pattern, true)
}

func validate(s string, allowWildcards bool) error {
	if s == "" {
		return fmt.Errorf("channel: empty name")
	}
	segs := strings.Split(s, ".")
	if len(segs) > MaxSegments {
		return fmt.Errorf("channel: %q has %d segments (max %d)", s, len(segs), MaxSegments)
	}
	for i, seg := range segs {
		switch seg {
		case "":
			return fmt.Errorf("channel: %q has an empty segment", s)
		case "*":
			if !allowWildcards {
				return fmt.Errorf("channel: %q: wildcards are only valid in token patterns", s)
			}
			continue
		case "**":
			if !allowWildcards {
				return fmt.Errorf("channel: %q: wildcards are only valid in token patterns", s)
			}
			if i != len(segs)-1 {
				return fmt.Errorf("channel: %q: `**` may only be the final segment", s)
			}
			continue
		}
		if len(seg) > MaxSegmentLen {
			return fmt.Errorf("channel: segment %q exceeds %d characters", seg, MaxSegmentLen)
		}
		for _, r := range seg {
			if !isNameRune(r) {
				return fmt.Errorf("channel: segment %q contains %q (allowed: a-z 0-9 _ -)", seg, r)
			}
		}
	}
	return nil
}

func isNameRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-'
}

// Match reports whether the concrete channel name is covered by pattern.
// Both inputs are assumed to be individually valid; Match on invalid
// input simply returns false for anything that does not line up.
func Match(pattern, name string) bool {
	ps := strings.Split(pattern, ".")
	ns := strings.Split(name, ".")
	for i, p := range ps {
		if p == "**" {
			// `**` is only valid at the end and must consume ≥1 segment.
			return i == len(ps)-1 && len(ns) >= len(ps)
		}
		if i >= len(ns) {
			return false
		}
		if p != "*" && p != ns[i] {
			return false
		}
	}
	return len(ps) == len(ns)
}
