// Tests for channel name validation and the token pattern language.
// Matching is security-relevant (tokens grant channel families), so the
// edge cases here are the ones an attacker would probe: empty segments,
// prefix confusion, and `**` placement.
package channel

import "testing"

func TestValidateNameAcceptsTypicalNames(t *testing.T) {
	for _, name := range []string{
		"orders",
		"orders.eu",
		"dash.tenant-42.cpu_load",
		"a.b.c.d",
		"0-9_z",
	} {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", name, err)
		}
	}
}

func TestValidateNameRejectsMalformedNames(t *testing.T) {
	for _, name := range []string{
		"",            // empty
		".",           // two empty segments
		"orders.",     // trailing dot
		".orders",     // leading dot
		"a..b",        // empty middle segment
		"Orders",      // uppercase is disallowed by design
		"a b",         // whitespace
		"a/b",         // path separator
		"a\nb",        // control character (would break the SSE frame)
		"café",        // non-ASCII
		"orders.*",    // wildcard in a concrete name
		"orders.\x00", // NUL
	} {
		if err := ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", name)
		}
	}
}

func TestValidateNameRejectsOversizedNames(t *testing.T) {
	long := make([]byte, MaxSegmentLen+1)
	for i := range long {
		long[i] = 'a'
	}
	if err := ValidateName(string(long)); err == nil {
		t.Fatalf("segment of %d chars accepted, want error", len(long))
	}
	deep := "a"
	for i := 0; i < MaxSegments; i++ { // MaxSegments+1 segments total
		deep += ".a"
	}
	if err := ValidateName(deep); err == nil {
		t.Fatalf("name with %d segments accepted, want error", MaxSegments+1)
	}
}

func TestValidatePattern(t *testing.T) {
	for _, p := range []string{"*", "orders.*", "*.eu", "orders.*.updates", "orders.**", "**"} {
		if err := ValidatePattern(p); err != nil {
			t.Errorf("ValidatePattern(%q) = %v, want nil", p, err)
		}
	}
	// `**` anywhere but the final segment is rejected — anything else
	// would make grants ambiguous.
	for _, p := range []string{"**.orders", "a.**.b", "or**", "***"} {
		if err := ValidatePattern(p); err == nil {
			t.Errorf("ValidatePattern(%q) = nil, want error", p)
		}
	}
}

func TestMatchTable(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
		why           string
	}{
		{"orders.eu", "orders.eu", true, "exact pattern matches itself"},
		{"orders.eu", "orders.us", false, "exact pattern must not match a sibling"},
		{"orders", "orders2", false, "no prefix-matching inside a segment"},
		{"orders", "orders.eu", false, "bare pattern must not match children"},
		{"orders.*.updates", "orders.eu.updates", true, "mid-pattern * matches one segment"},
		{"orders.*.updates", "orders.eu.west.updates", false, "mid-pattern * must not span two segments"},
		{"*.eu", "orders.eu", true, "leading * matches one segment"},
		{"*.eu", "orders.eu.returns", false, "*.eu must not match three segments"},
		{"**", "a", true, "bare ** matches everything"},
		{"**", "deep.ly.nested.chan", true, "bare ** matches any depth"},
	}
	for _, c := range cases {
		if got := Match(c.pattern, c.name); got != c.want {
			t.Errorf("Match(%q, %q) = %v: %s", c.pattern, c.name, got, c.why)
		}
	}
}

func TestMatchSingleStarMatchesExactlyOneSegment(t *testing.T) {
	if !Match("orders.*", "orders.eu") {
		t.Error("orders.* should match orders.eu")
	}
	if Match("orders.*", "orders") {
		t.Error("orders.* must not match the bare parent")
	}
	if Match("orders.*", "orders.eu.returns") {
		t.Error("orders.* must not match two levels down")
	}
}

func TestMatchDoubleStarMatchesOneOrMoreSegments(t *testing.T) {
	for _, name := range []string{"orders.eu", "orders.eu.returns", "orders.a.b.c"} {
		if !Match("orders.**", name) {
			t.Errorf("orders.** should match %q", name)
		}
	}
	if Match("orders.**", "orders") {
		t.Error("orders.** must not match the bare parent (** requires >=1 segment)")
	}
	if Match("orders.**", "invoices.eu") {
		t.Error("orders.** must not match a different prefix")
	}
}
