// Tests for the signed token format. Tokens are the entire security
// boundary of a fanline hub, so beyond round-trips these cover tampering,
// key confusion, expiry edges, and every malformed shape the verifier
// must reject rather than crash on.
package token

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var testNow = time.Unix(1_700_000_000, 0)

func mint(t *testing.T, c Claims, secret string) string {
	t.Helper()
	tok, err := Sign(c, secret)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return tok
}

func baseClaims() Claims {
	return Claims{
		KeyID:     "main",
		Channel:   "orders.*",
		Caps:      []string{CapSubscribe, CapPublish},
		IssuedAt:  testNow.Unix(),
		ExpiresAt: testNow.Add(time.Hour).Unix(),
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	tok := mint(t, baseClaims(), "s3cret")
	got, err := Verify(tok, Keyring{"main": "s3cret"}, testNow)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Channel != "orders.*" || got.KeyID != "main" || len(got.Caps) != 2 {
		t.Errorf("claims mangled in round trip: %+v", got)
	}
	// Decode (no verification) must see the same claims — it is how the
	// verifier discovers the kid and how `token inspect` works keyless.
	decoded, err := Decode(tok)
	if err != nil || decoded.Channel != "orders.*" {
		t.Errorf("Decode = (%+v, %v)", decoded, err)
	}
}

func TestSignIsDeterministic(t *testing.T) {
	// Same claims + secret must yield the same token, so `token new --now`
	// is reproducible in scripts and docs.
	a := mint(t, baseClaims(), "s3cret")
	b := mint(t, baseClaims(), "s3cret")
	if a != b {
		t.Errorf("tokens differ:\n%s\n%s", a, b)
	}
}

func TestTokenIsQueryParameterSafe(t *testing.T) {
	// EventSource can only pass the token in the URL, so the encoding must
	// avoid '+', '/', '=', and anything needing percent-escaping.
	tok := mint(t, baseClaims(), "s3cret")
	if strings.ContainsAny(tok, "+/=&? %") {
		t.Errorf("token contains URL-unsafe characters: %s", tok)
	}
	if !strings.HasPrefix(tok, "fl1.") {
		t.Errorf("token missing fl1. prefix: %s", tok)
	}
}

func TestVerifyRejectsWrongSecret(t *testing.T) {
	tok := mint(t, baseClaims(), "s3cret")
	if _, err := Verify(tok, Keyring{"main": "different"}, testNow); !errors.Is(err, ErrBadSignature) {
		t.Errorf("wrong secret: got %v, want ErrBadSignature", err)
	}
}

func TestVerifyRejectsUnknownKeyID(t *testing.T) {
	tok := mint(t, baseClaims(), "s3cret")
	if _, err := Verify(tok, Keyring{"other": "s3cret"}, testNow); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("unknown kid: got %v, want ErrUnknownKey", err)
	}
}

func TestVerifyRejectsTamperedClaims(t *testing.T) {
	// Re-encode the payload with a widened channel pattern but keep the
	// original signature — the classic privilege-escalation attempt.
	c := baseClaims()
	tok := mint(t, c, "s3cret")
	c.Channel = "**"
	widened := mint(t, c, "s3cret")
	forged := strings.Join([]string{
		strings.SplitN(widened, ".", 3)[0],
		strings.SplitN(widened, ".", 3)[1],
		strings.SplitN(tok, ".", 3)[2],
	}, ".")
	if _, err := Verify(forged, Keyring{"main": "s3cret"}, testNow); !errors.Is(err, ErrBadSignature) {
		t.Errorf("forged payload: got %v, want ErrBadSignature", err)
	}
}

func TestVerifyRejectsTruncatedSignature(t *testing.T) {
	tok := mint(t, baseClaims(), "s3cret")
	if _, err := Verify(tok[:len(tok)-4], Keyring{"main": "s3cret"}, testNow); !errors.Is(err, ErrMalformed) {
		t.Errorf("truncated sig: got %v, want ErrMalformed", err)
	}
}

func TestVerifyRejectsMalformedShapes(t *testing.T) {
	ring := Keyring{"main": "s3cret"}
	for _, tok := range []string{
		"",
		"fl1",
		"fl1..sig",
		"fl1.payload",
		"fl2.abc.def",               // wrong version prefix
		"fl1.!notb64.!notb64",       // invalid base64
		"fl1.eyJmb28iOjF9.c2hvcnQ",  // sig not 32 bytes
		"fl1.abc.def.extra",         // too many parts
		"Bearer fl1.abc.def",        // caller passed the whole header
		strings.Repeat("A", 10_000), // garbage blob
		"fl1.eyJraWQiOiJtIn0." + b64.EncodeToString(make([]byte, 32)), // claims fail validation
	} {
		if _, err := Verify(tok, ring, testNow); !errors.Is(err, ErrMalformed) {
			t.Errorf("Verify(%.40q) = %v, want ErrMalformed", tok, err)
		}
	}
}

func TestVerifyExpiryBoundary(t *testing.T) {
	c := baseClaims()
	tok := mint(t, c, "s3cret")
	ring := Keyring{"main": "s3cret"}
	// One second before exp: valid. At exp exactly: expired (exp is exclusive).
	if _, err := Verify(tok, ring, time.Unix(c.ExpiresAt-1, 0)); err != nil {
		t.Errorf("1s before exp: %v, want valid", err)
	}
	if _, err := Verify(tok, ring, time.Unix(c.ExpiresAt, 0)); !errors.Is(err, ErrExpired) {
		t.Errorf("at exp: got %v, want ErrExpired", err)
	}
	// exp=0 means the token never expires, even in the far future.
	c.ExpiresAt = 0
	eternal := mint(t, c, "s3cret")
	if _, err := Verify(eternal, ring, testNow.Add(100*365*24*time.Hour)); err != nil {
		t.Errorf("exp=0 token expired: %v", err)
	}
}

func TestVerifyClockSkewGrace(t *testing.T) {
	// A token minted 20s "in the future" (another machine's clock) still
	// verifies; 60s in the future does not.
	c := baseClaims()
	tok := mint(t, c, "s3cret")
	ring := Keyring{"main": "s3cret"}
	if _, err := Verify(tok, ring, testNow.Add(-20*time.Second)); err != nil {
		t.Errorf("20s skew: %v, want valid", err)
	}
	if _, err := Verify(tok, ring, testNow.Add(-60*time.Second)); !errors.Is(err, ErrNotYetValid) {
		t.Errorf("60s skew: got %v, want ErrNotYetValid", err)
	}
}

func TestKeyRotationOldAndNewBothVerify(t *testing.T) {
	oldClaims := baseClaims()
	newClaims := baseClaims()
	newClaims.KeyID = "2026-q3"
	oldTok := mint(t, oldClaims, "old-secret")
	newTok := mint(t, newClaims, "new-secret")
	ring := Keyring{"main": "old-secret", "2026-q3": "new-secret"}
	if _, err := Verify(oldTok, ring, testNow); err != nil {
		t.Errorf("old-key token: %v", err)
	}
	if _, err := Verify(newTok, ring, testNow); err != nil {
		t.Errorf("new-key token: %v", err)
	}
}

func TestAllowsRespectsCapAndPattern(t *testing.T) {
	c := Claims{Channel: "orders.*", Caps: []string{CapSubscribe}}
	if !c.Allows(CapSubscribe, "orders.eu") {
		t.Error("sub on matching channel should be allowed")
	}
	if c.Allows(CapPublish, "orders.eu") {
		t.Error("pub was never granted")
	}
	if c.Allows(CapSubscribe, "invoices.eu") {
		t.Error("channel outside the pattern must be denied")
	}
	stats := Claims{Channel: "orders.*", Caps: []string{CapStats}}
	if !stats.Allows(CapStats, "") {
		t.Error("stats ignores the channel pattern")
	}
}

func TestSignRejectsInvalidClaims(t *testing.T) {
	bad := []Claims{
		{KeyID: "", Channel: "a", Caps: []string{"sub"}},                              // no kid
		{KeyID: "k", Channel: "", Caps: []string{"sub"}},                              // no channel
		{KeyID: "k", Channel: "UPPER", Caps: []string{"sub"}},                         // bad pattern
		{KeyID: "k", Channel: "a", Caps: nil},                                         // no caps
		{KeyID: "k", Channel: "a", Caps: []string{"admin"}},                           // unknown cap
		{KeyID: "k", Channel: "a", Caps: []string{"sub"}, IssuedAt: 10, ExpiresAt: 5}, // exp < iat
	}
	for i, c := range bad {
		if _, err := Sign(c, "s"); err == nil {
			t.Errorf("case %d: Sign accepted invalid claims %+v", i, c)
		}
	}
	if _, err := Sign(baseClaims(), ""); err == nil {
		t.Error("Sign accepted an empty secret")
	}
}

func TestParseKeys(t *testing.T) {
	ring, err := ParseKeys("main=alpha, backup=beta")
	if err != nil {
		t.Fatalf("ParseKeys: %v", err)
	}
	if ring["main"] != "alpha" || ring["backup"] != "beta" {
		t.Errorf("ring = %v", ring)
	}
	if ids := ring.KeyIDs(); len(ids) != 2 || ids[0] != "backup" {
		t.Errorf("KeyIDs = %v, want sorted [backup main]", ids)
	}
	// Base64-ish secrets often end in '='; only the FIRST '=' splits.
	ring, err = ParseKeys("main=abc=def==")
	if err != nil || ring["main"] != "abc=def==" {
		t.Errorf("equals-bearing secret: (%v, %v)", ring, err)
	}
	for _, spec := range []string{"", "noequals", "=secret", "kid=", "a=1,a=2"} {
		if _, err := ParseKeys(spec); err == nil {
			t.Errorf("ParseKeys(%q) accepted, want error", spec)
		}
	}
}
