// Package token implements fanline's signed channel tokens.
//
// A token is three dot-separated parts:
//
//	fl1.<base64url(JSON claims)>.<base64url(HMAC-SHA256)>
//
// The signature covers the literal string "fl1.<claims-b64>", keyed by the
// secret named in the claims' "kid" field, so a hub holding several keys
// (rotation) picks the right one without trial verification. Base64 is the
// raw (unpadded) URL alphabet, making tokens safe to pass as a query
// parameter — which browsers' EventSource requires, since it cannot set an
// Authorization header.
package token

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/JaydenCJ/fanline/internal/channel"
)

// prefix is the token format/version marker; bump on incompatible change.
const prefix = "fl1"

// Capabilities a token can grant. Publish and subscribe are per-channel
// (gated by the claims' channel pattern); stats is hub-wide.
const (
	CapSubscribe = "sub"
	CapPublish   = "pub"
	CapStats     = "stats"
)

// Sentinel errors, distinguishable by callers via errors.Is.
var (
	ErrMalformed    = errors.New("token: malformed")
	ErrUnknownKey   = errors.New("token: unknown key id")
	ErrBadSignature = errors.New("token: signature mismatch")
	ErrExpired      = errors.New("token: expired")
	ErrNotYetValid  = errors.New("token: not valid yet")
)

// Claims is the signed payload of a token.
type Claims struct {
	KeyID     string   `json:"kid"`
	Channel   string   `json:"ch"`            // channel pattern this token covers
	Caps      []string `json:"cap"`           // subset of {sub, pub, stats}
	IssuedAt  int64    `json:"iat"`           // unix seconds
	ExpiresAt int64    `json:"exp,omitempty"` // unix seconds; 0 = never expires
}

// Keyring maps key IDs to shared secrets. Multiple entries allow zero-
// downtime rotation: mint with the new kid while the old one still verifies.
type Keyring map[string]string

// ParseKeys parses "kid=secret[,kid2=secret2,...]" as accepted by the
// --keys flag and the FANLINE_KEYS environment variable.
func ParseKeys(s string) (Keyring, error) {
	ring := Keyring{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		kid, secret, ok := strings.Cut(pair, "=")
		if !ok || kid == "" || secret == "" {
			return nil, fmt.Errorf("token: key %q is not kid=secret", pair)
		}
		if _, dup := ring[kid]; dup {
			return nil, fmt.Errorf("token: duplicate key id %q", kid)
		}
		ring[kid] = secret
	}
	if len(ring) == 0 {
		return nil, errors.New("token: no keys given")
	}
	return ring, nil
}

// KeyIDs returns the ring's key IDs, sorted for stable output.
func (r Keyring) KeyIDs() []string {
	ids := make([]string, 0, len(r))
	for kid := range r {
		ids = append(ids, kid)
	}
	sort.Strings(ids)
	return ids
}

// Validate checks the claims are internally coherent before signing.
func (c Claims) Validate() error {
	if c.KeyID == "" {
		return errors.New("token: claims missing key id")
	}
	if err := channel.ValidatePattern(c.Channel); err != nil {
		return err
	}
	if len(c.Caps) == 0 {
		return errors.New("token: claims grant no capabilities")
	}
	for _, cap := range c.Caps {
		switch cap {
		case CapSubscribe, CapPublish, CapStats:
		default:
			return fmt.Errorf("token: unknown capability %q", cap)
		}
	}
	if c.ExpiresAt != 0 && c.ExpiresAt < c.IssuedAt {
		return errors.New("token: expires before issued")
	}
	return nil
}

// Allows reports whether the claims grant capability cap on the concrete
// channel name. CapStats ignores the channel pattern.
func (c Claims) Allows(cap, name string) bool {
	granted := false
	for _, g := range c.Caps {
		if g == cap {
			granted = true
			break
		}
	}
	if !granted {
		return false
	}
	if cap == CapStats {
		return true
	}
	return channel.Match(c.Channel, name)
}

// Sign serializes and signs claims with secret, returning the token string.
func Sign(c Claims, secret string) (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	if secret == "" {
		return "", errors.New("token: empty secret")
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	body := prefix + "." + b64.EncodeToString(payload)
	return body + "." + b64.EncodeToString(sign(body, secret)), nil
}

// Decode parses a token's claims WITHOUT verifying the signature. Use it
// for inspection and to discover which kid to verify with; never for auth.
func Decode(tok string) (Claims, error) {
	body, _, err := split(tok)
	if err != nil {
		return Claims{}, err
	}
	return decodeBody(body)
}

// Verify checks structure, signature (constant-time), and the validity
// window against now, returning the trusted claims.
func Verify(tok string, ring Keyring, now time.Time) (Claims, error) {
	body, sig, err := split(tok)
	if err != nil {
		return Claims{}, err
	}
	c, err := decodeBody(body)
	if err != nil {
		return Claims{}, err
	}
	secret, ok := ring[c.KeyID]
	if !ok {
		return Claims{}, fmt.Errorf("%w %q", ErrUnknownKey, c.KeyID)
	}
	if !hmac.Equal(sig, sign(body, secret)) {
		return Claims{}, ErrBadSignature
	}
	// Allow 30 s of clock skew on iat so tokens minted on another machine
	// a moment "in the future" still work; exp has no grace on purpose.
	if now.Unix()+30 < c.IssuedAt {
		return Claims{}, ErrNotYetValid
	}
	if c.ExpiresAt != 0 && now.Unix() >= c.ExpiresAt {
		return Claims{}, ErrExpired
	}
	return c, nil
}

var b64 = base64.RawURLEncoding

func sign(body, secret string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return mac.Sum(nil)
}

// split separates a token into its signed body ("fl1.<claims>") and raw
// signature bytes, rejecting anything structurally off.
func split(tok string) (body string, sig []byte, err error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 || parts[0] != prefix || parts[1] == "" || parts[2] == "" {
		return "", nil, ErrMalformed
	}
	sig, err = b64.DecodeString(parts[2])
	if err != nil || len(sig) != sha256.Size {
		return "", nil, ErrMalformed
	}
	return parts[0] + "." + parts[1], sig, nil
}

func decodeBody(body string) (Claims, error) {
	_, payloadB64, _ := strings.Cut(body, ".")
	payload, err := b64.DecodeString(payloadB64)
	if err != nil {
		return Claims{}, ErrMalformed
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return Claims{}, ErrMalformed
	}
	if err := c.Validate(); err != nil {
		return Claims{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	return c, nil
}
