package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/JaydenCJ/fanline/internal/token"
)

const tokenUsageText = `Usage:
  fanline token new [flags]              mint a signed channel token
  fanline token inspect [flags] <token>  decode a token; verify it with --keys

Run "fanline token <subcommand> -h" for subcommand flags.
`

func runToken(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 {
		fmt.Fprintf(stderr, "fanline token: expected subcommand: new | inspect\n\n%s", tokenUsageText)
		return ExitUsage
	}
	switch args[0] {
	case "new":
		return runTokenNew(args[1:], stdout, stderr, getenv)
	case "inspect":
		return runTokenInspect(args[1:], stdout, stderr, getenv)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, tokenUsageText)
		return ExitOK
	default:
		fmt.Fprintf(stderr, "fanline token: unknown subcommand %q (want new | inspect)\n", args[0])
		return ExitUsage
	}
}

func runTokenNew(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := newFlagSet("fanline token new", stderr)
	var (
		keysSpec = fs.String("keys", getenv("FANLINE_KEYS"), "signing keys as kid=secret[,…] (or FANLINE_KEYS)")
		kid      = fs.String("kid", "", "key id to sign with (defaults to the only key)")
		ch       = fs.String("channel", "", "channel pattern the token covers, e.g. orders.*")
		caps     = fs.String("cap", "sub", "comma-separated capabilities: sub,pub,stats")
		ttl      = fs.Duration("ttl", time.Hour, "validity window; 0 means the token never expires")
		nowSpec  = fs.String("now", "", "issue timestamp as RFC3339 (defaults to wall clock; for reproducible mints)")
	)
	if code, done := parseFlags(fs, args); done {
		return code
	}
	if *ch == "" {
		fmt.Fprintln(stderr, "fanline token new: --channel is required")
		return ExitUsage
	}
	ring, err := parseKeysArg(*keysSpec)
	if err != nil {
		fmt.Fprintf(stderr, "fanline token new: %v\n", err)
		return ExitUsage
	}
	signKid, secret, err := pickKey(ring, *kid)
	if err != nil {
		fmt.Fprintf(stderr, "fanline token new: %v\n", err)
		return ExitUsage
	}
	now := time.Now()
	if *nowSpec != "" {
		now, err = time.Parse(time.RFC3339, *nowSpec)
		if err != nil {
			fmt.Fprintf(stderr, "fanline token new: --now: %v\n", err)
			return ExitUsage
		}
	}
	claims := token.Claims{
		KeyID:    signKid,
		Channel:  *ch,
		Caps:     splitCaps(*caps),
		IssuedAt: now.Unix(),
	}
	if *ttl > 0 {
		claims.ExpiresAt = now.Add(*ttl).Unix()
	}
	tok, err := token.Sign(claims, secret)
	if err != nil {
		fmt.Fprintf(stderr, "fanline token new: %v\n", err)
		return ExitUsage
	}
	fmt.Fprintln(stdout, tok)
	return ExitOK
}

// inspectOutput is the JSON printed by `fanline token inspect`.
type inspectOutput struct {
	Claims    token.Claims `json:"claims"`
	Signature string       `json:"signature"` // valid | invalid | expired | unverified
	Expires   string       `json:"expires"`   // RFC3339 or "never"
}

func runTokenInspect(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := newFlagSet("fanline token inspect", stderr)
	var (
		keysSpec = fs.String("keys", getenv("FANLINE_KEYS"), "verification keys; omit to decode without verifying")
		nowSpec  = fs.String("now", "", "verification timestamp as RFC3339 (defaults to wall clock)")
	)
	if code, done := parseFlags(fs, args); done {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "fanline token inspect: expected exactly one token argument")
		return ExitUsage
	}
	tok := fs.Arg(0)
	claims, err := token.Decode(tok)
	if err != nil {
		fmt.Fprintf(stderr, "fanline token inspect: %v\n", err)
		return ExitRuntime
	}
	out := inspectOutput{Claims: claims, Signature: "unverified", Expires: "never"}
	if claims.ExpiresAt != 0 {
		out.Expires = time.Unix(claims.ExpiresAt, 0).UTC().Format(time.RFC3339)
	}
	verifyFailed := false
	if *keysSpec != "" {
		ring, kerr := token.ParseKeys(*keysSpec)
		if kerr != nil {
			fmt.Fprintf(stderr, "fanline token inspect: %v\n", kerr)
			return ExitUsage
		}
		now := time.Now()
		if *nowSpec != "" {
			now, err = time.Parse(time.RFC3339, *nowSpec)
			if err != nil {
				fmt.Fprintf(stderr, "fanline token inspect: --now: %v\n", err)
				return ExitUsage
			}
		}
		switch _, verr := token.Verify(tok, ring, now); {
		case verr == nil:
			out.Signature = "valid"
		case errors.Is(verr, token.ErrExpired):
			out.Signature = "expired"
			verifyFailed = true
		default:
			out.Signature = "invalid"
			verifyFailed = true
		}
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
	if verifyFailed {
		return ExitRuntime
	}
	return ExitOK
}

// parseKeysArg wraps token.ParseKeys with a friendlier empty-input message.
func parseKeysArg(spec string) (token.Keyring, error) {
	if spec == "" {
		return nil, errors.New("no keys; pass --keys kid=secret or set FANLINE_KEYS")
	}
	return token.ParseKeys(spec)
}

// pickKey selects the signing key: an explicit --kid, or the ring's only
// entry.
func pickKey(ring token.Keyring, kid string) (string, string, error) {
	if kid != "" {
		secret, ok := ring[kid]
		if !ok {
			return "", "", fmt.Errorf("key id %q not in --keys (have %s)", kid, strings.Join(ring.KeyIDs(), ", "))
		}
		return kid, secret, nil
	}
	if len(ring) == 1 {
		for k, s := range ring {
			return k, s, nil
		}
	}
	return "", "", fmt.Errorf("multiple keys loaded (%s); choose one with --kid", strings.Join(ring.KeyIDs(), ", "))
}

func splitCaps(s string) []string {
	var caps []string
	for _, c := range strings.Split(s, ",") {
		if c = strings.TrimSpace(c); c != "" {
			caps = append(caps, c)
		}
	}
	return caps
}
