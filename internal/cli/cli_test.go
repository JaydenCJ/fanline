// In-process CLI tests: cli.Run is invoked exactly as main does, with
// captured writers and a fake environment, against a hub served by
// httptest. This covers the full user-visible surface — token minting and
// inspection, publish, tail with replay/resume, exit codes — without
// building a binary or touching the network beyond 127.0.0.1 loopback
// owned by httptest.
package cli

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JaydenCJ/fanline/internal/hub"
	"github.com/JaydenCJ/fanline/internal/server"
	"github.com/JaydenCJ/fanline/internal/token"
	"github.com/JaydenCJ/fanline/internal/version"
)

const testKeys = "main=cli-test-secret"

// run executes the CLI and returns (exit, stdout, stderr).
func run(env map[string]string, args ...string) (int, string, string) {
	var out, errOut strings.Builder
	getenv := func(k string) string { return env[k] }
	code := Run(args, &out, &errOut, getenv)
	return code, out.String(), errOut.String()
}

// newTestHub serves a real fanline hub over httptest and returns its URL.
func newTestHub(t *testing.T) string {
	t.Helper()
	keys, err := token.ParseKeys(testKeys)
	if err != nil {
		t.Fatal(err)
	}
	h := hub.New(hub.Options{ReplayCap: 8})
	srv := httptest.NewServer(server.New(h, server.Config{Keys: keys, KeepAlive: time.Hour}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// mintCLI mints a token through the CLI itself.
func mintCLI(t *testing.T, pattern, caps string) string {
	t.Helper()
	code, out, errOut := run(nil, "token", "new", "--keys", testKeys,
		"--channel", pattern, "--cap", caps, "--ttl", "1h")
	if code != ExitOK {
		t.Fatalf("token new failed (%d): %s", code, errOut)
	}
	return strings.TrimSpace(out)
}

func TestVersionCommand(t *testing.T) {
	code, out, _ := run(nil, "version")
	if code != ExitOK || out != "fanline "+version.Version+"\n" {
		t.Errorf("version: code=%d out=%q", code, out)
	}
}

func TestNoArgsOrUnknownCommandIsUsageError(t *testing.T) {
	code, _, errOut := run(nil)
	if code != ExitUsage || !strings.Contains(errOut, "Usage:") {
		t.Errorf("no args: code=%d stderr=%q", code, errOut)
	}
	code, _, errOut = run(nil, "frobnicate")
	if code != ExitUsage || !strings.Contains(errOut, "unknown command") {
		t.Errorf("unknown: code=%d stderr=%q", code, errOut)
	}
}

func TestHelpAlwaysExitsZero(t *testing.T) {
	// -h/--help is a successful outcome (exit 0), never a usage error —
	// scripts probe flags with `cmd -h && …` and CI-less users read these
	// screens constantly.
	code, out, _ := run(nil, "--help")
	if code != ExitOK || !strings.Contains(out, "Usage:") {
		t.Errorf("--help: code=%d out=%q", code, out)
	}
	for _, args := range [][]string{
		{"serve", "-h"}, {"publish", "--help"}, {"tail", "-h"},
		{"token", "new", "-h"}, {"token", "inspect", "--help"},
	} {
		if code, _, _ := run(nil, args...); code != ExitOK {
			t.Errorf("%v: code=%d, want %d", args, code, ExitOK)
		}
	}
	code, out, _ = run(nil, "token", "--help")
	if code != ExitOK || !strings.Contains(out, "inspect") {
		t.Errorf("token --help: code=%d out=%q", code, out)
	}
}

func TestTokenNewIsReproducibleWithNow(t *testing.T) {
	args := []string{"token", "new", "--keys", testKeys, "--channel", "orders.*",
		"--cap", "sub,pub", "--ttl", "1h", "--now", "2026-07-13T00:00:00Z"}
	_, a, _ := run(nil, args...)
	_, b, _ := run(nil, args...)
	if a != b || !strings.HasPrefix(a, "fl1.") {
		t.Errorf("mints differ or malformed:\n%q\n%q", a, b)
	}
}

func TestTokenNewThenInspectRoundTrip(t *testing.T) {
	tok := mintCLI(t, "orders.*", "sub,pub")
	code, out, errOut := run(nil, "token", "inspect", "--keys", testKeys, tok)
	if code != ExitOK {
		t.Fatalf("inspect failed (%d): %s", code, errOut)
	}
	var got struct {
		Claims    token.Claims `json:"claims"`
		Signature string       `json:"signature"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("inspect output not JSON: %v\n%s", err, out)
	}
	if got.Signature != "valid" || got.Claims.Channel != "orders.*" || len(got.Claims.Caps) != 2 {
		t.Errorf("inspect = %+v", got)
	}
	// Without keys, inspect still decodes but marks the signature
	// unverified — useful for looking inside third-party tokens.
	code, out, _ = run(nil, "token", "inspect", tok)
	if code != ExitOK || !strings.Contains(out, `"unverified"`) {
		t.Errorf("keyless inspect: code=%d out=%s", code, out)
	}
}

func TestTokenInspectDetectsWrongKey(t *testing.T) {
	tok := mintCLI(t, "orders.*", "sub")
	code, out, _ := run(nil, "token", "inspect", "--keys", "main=wrong-secret", tok)
	if code != ExitRuntime || !strings.Contains(out, `"invalid"`) {
		t.Errorf("code=%d out=%s", code, out)
	}
}

func TestChannelFlagIsRequired(t *testing.T) {
	code, _, errOut := run(nil, "token", "new", "--keys", testKeys)
	if code != ExitUsage || !strings.Contains(errOut, "--channel") {
		t.Errorf("token new: code=%d stderr=%q", code, errOut)
	}
	code, _, errOut = run(nil, "publish", "--data", "x")
	if code != ExitUsage || !strings.Contains(errOut, "--channel") {
		t.Errorf("publish: code=%d stderr=%q", code, errOut)
	}
}

func TestTokenNewKeySelection(t *testing.T) {
	// With several keys loaded, --kid is mandatory and must exist.
	code, _, errOut := run(nil, "token", "new", "--keys", "a=1,b=2", "--channel", "x")
	if code != ExitUsage || !strings.Contains(errOut, "--kid") {
		t.Errorf("code=%d stderr=%q", code, errOut)
	}
	code, out, _ := run(nil, "token", "new", "--keys", "a=1,b=2", "--kid", "b", "--channel", "x")
	if code != ExitOK || !strings.HasPrefix(out, "fl1.") {
		t.Errorf("explicit --kid: code=%d out=%q", code, out)
	}
	// Keys can come from the environment instead of the flag.
	env := map[string]string{"FANLINE_KEYS": testKeys}
	code, out, errOut = run(env, "token", "new", "--channel", "env.chan", "--cap", "sub")
	if code != ExitOK || !strings.HasPrefix(out, "fl1.") {
		t.Errorf("env keys: code=%d out=%q stderr=%q", code, out, errOut)
	}
}

func TestPublishThenTailWithReplay(t *testing.T) {
	url := newTestHub(t)
	pubTok := mintCLI(t, "orders.**", "pub")
	subTok := mintCLI(t, "orders.**", "sub")

	code, out, errOut := run(nil, "publish", "--url", url, "--channel", "orders.eu",
		"--token", pubTok, "--event", "created", "--data", `{"id":41}`)
	if code != ExitOK {
		t.Fatalf("publish failed (%d): %s", code, errOut)
	}
	var pub struct {
		Seq uint64 `json:"seq"`
	}
	if err := json.Unmarshal([]byte(out), &pub); err != nil || pub.Seq != 1 {
		t.Fatalf("publish output = %q (err %v)", out, err)
	}
	run(nil, "publish", "--url", url, "--channel", "orders.eu",
		"--token", pubTok, "--event", "created", "--data", `{"id":42}`)

	// tail --replay 2 --max 2 sees both retained events, then exits 0.
	code, out, errOut = run(nil, "tail", "--url", url, "--channel", "orders.eu",
		"--token", subTok, "--replay", "2", "--max", "2")
	if code != ExitOK {
		t.Fatalf("tail failed (%d): %s", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `{"id":41}`) || !strings.Contains(lines[1], `{"id":42}`) {
		t.Errorf("tail output = %q", out)
	}
	if !strings.Contains(errOut, "replayed=2") {
		t.Errorf("tail stderr should report the ready line, got %q", errOut)
	}
}

func TestTailResumesFromLastID(t *testing.T) {
	url := newTestHub(t)
	pubTok := mintCLI(t, "feed", "pub")
	subTok := mintCLI(t, "feed", "sub")
	for _, msg := range []string{"m1", "m2", "m3"} {
		if code, _, errOut := run(nil, "publish", "--url", url, "--channel", "feed",
			"--token", pubTok, "--data", msg); code != ExitOK {
			t.Fatalf("publish %s: %s", msg, errOut)
		}
	}
	// First, learn m1's id from a replay-all tail.
	_, out, _ := run(nil, "tail", "--url", url, "--channel", "feed",
		"--token", subTok, "--replay", "3", "--max", "1")
	firstID := strings.SplitN(strings.TrimSpace(out), "\t", 2)[0]
	if firstID == "" {
		t.Fatalf("no id in %q", out)
	}
	// Resuming after m1 must deliver exactly m2 and m3.
	code, out, errOut := run(nil, "tail", "--url", url, "--channel", "feed",
		"--token", subTok, "--last-id", firstID, "--max", "2")
	if code != ExitOK {
		t.Fatalf("resume tail failed (%d): %s", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], "m2") || !strings.HasSuffix(lines[1], "m3") {
		t.Errorf("resumed tail = %q", out)
	}
}

func TestTailJSONOutput(t *testing.T) {
	url := newTestHub(t)
	pubTok := mintCLI(t, "feed", "pub")
	subTok := mintCLI(t, "feed", "sub")
	run(nil, "publish", "--url", url, "--channel", "feed", "--token", pubTok,
		"--event", "tick", "--data", "multi\nline")
	code, out, _ := run(nil, "tail", "--url", url, "--channel", "feed",
		"--token", subTok, "--replay", "1", "--max", "1", "--json")
	if code != ExitOK {
		t.Fatalf("tail --json failed: %d", code)
	}
	var e struct{ ID, Event, Data string }
	if err := json.Unmarshal([]byte(out), &e); err != nil {
		t.Fatalf("not JSON: %v (%q)", err, out)
	}
	if e.Event != "tick" || e.Data != "multi\nline" {
		t.Errorf("event = %+v (newlines must survive the round trip)", e)
	}
}

func TestPublishRejectedWithWrongCapability(t *testing.T) {
	url := newTestHub(t)
	subOnly := mintCLI(t, "feed", "sub")
	code, _, errOut := run(nil, "publish", "--url", url, "--channel", "feed",
		"--token", subOnly, "--data", "x")
	if code != ExitRuntime || !strings.Contains(errOut, "403") {
		t.Errorf("code=%d stderr=%q, want runtime failure mentioning 403", code, errOut)
	}
}

func TestServeFlagsRequireKeysOrDev(t *testing.T) {
	var errOut strings.Builder
	_, err := parseServeFlags(nil, &errOut, func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "--dev") {
		t.Errorf("keyless serve: err = %v, want a hint about --keys/--dev", err)
	}
}

func TestServeFlagsDevRefusesNonLoopback(t *testing.T) {
	var errOut strings.Builder
	getenv := func(string) string { return "" }
	if _, err := parseServeFlags([]string{"--dev", "--addr", "0.0.0.0:8787"}, &errOut, getenv); err == nil {
		t.Error("--dev on 0.0.0.0 must be refused")
	}
	for _, addr := range []string{"127.0.0.1:0", "localhost:9000", "[::1]:8080"} {
		if _, err := parseServeFlags([]string{"--dev", "--addr", addr}, &errOut, getenv); err != nil {
			t.Errorf("--dev on %s: %v, want accepted", addr, err)
		}
	}
}

func TestServeFlagsParseKeysAndDefaults(t *testing.T) {
	var errOut strings.Builder
	cfg, err := parseServeFlags([]string{"--keys", "main=s,backup=t"}, &errOut, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.addr != "127.0.0.1:8787" || len(cfg.keys) != 2 || cfg.replayCap != 64 {
		t.Errorf("cfg = %+v", cfg)
	}
	env := map[string]string{"FANLINE_KEYS": testKeys, "FANLINE_ADDR": "127.0.0.1:9999"}
	cfg, err = parseServeFlags(nil, &errOut, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.addr != "127.0.0.1:9999" || cfg.keys["main"] == "" {
		t.Errorf("env cfg = %+v", cfg)
	}
}

func TestServeFlagsRejectBadValues(t *testing.T) {
	var errOut strings.Builder
	getenv := func(string) string { return "" }
	for _, args := range [][]string{
		{"--keys", "notapair"},
		{"--keys", "main=s", "--replay", "-1"},
		{"--keys", "main=s", "--sub-buffer", "0"},
	} {
		if _, err := parseServeFlags(args, &errOut, getenv); err == nil {
			t.Errorf("parseServeFlags(%v) accepted, want error", args)
		}
	}
}
