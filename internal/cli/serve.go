package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/JaydenCJ/fanline/internal/hub"
	"github.com/JaydenCJ/fanline/internal/server"
	"github.com/JaydenCJ/fanline/internal/token"
	"github.com/JaydenCJ/fanline/internal/version"
)

// serveConfig is everything `fanline serve` needs, resolved from flags and
// environment. Split out so flag parsing is unit-testable without binding
// a socket.
type serveConfig struct {
	addr        string
	keys        token.Keyring
	dev         bool
	corsOrigin  string
	replayCap   int
	replayTTL   time.Duration
	keepAlive   time.Duration
	retryMS     int
	maxBody     int64
	maxChannels int
	subBuffer   int
}

// parseServeFlags resolves the serve configuration. It enforces the two
// safety rules: a hub must have keys OR be in --dev mode, and --dev only
// binds loopback (an unauthenticated hub must not face a network).
func parseServeFlags(args []string, stderr io.Writer, getenv func(string) string) (serveConfig, error) {
	fs := newFlagSet("fanline serve", stderr)
	var (
		addr        = fs.String("addr", envDefault(getenv, "FANLINE_ADDR", "127.0.0.1:8787"), "listen address")
		keysSpec    = fs.String("keys", getenv("FANLINE_KEYS"), "signing keys as kid=secret[,kid2=secret2] (or FANLINE_KEYS)")
		dev         = fs.Bool("dev", false, "disable auth entirely; loopback addresses only")
		corsOrigin  = fs.String("cors-origin", "", "Access-Control-Allow-Origin value; empty disables CORS")
		replayCap   = fs.Int("replay", 64, "events retained per channel for replay")
		replayTTL   = fs.Duration("replay-ttl", 0, "max age of replayable events; 0 keeps until evicted")
		keepAlive   = fs.Duration("keepalive", 25*time.Second, "SSE comment keepalive interval")
		retryMS     = fs.Int("retry-ms", 3000, "reconnect delay hint sent to clients")
		maxBody     = fs.Int64("max-body", 256<<10, "publish body limit in bytes")
		maxChannels = fs.Int("max-channels", 1024, "live channel limit")
		subBuffer   = fs.Int("sub-buffer", 64, "per-subscriber fanout buffer before a slow client is dropped")
	)
	if err := fs.Parse(args); err != nil {
		return serveConfig{}, err
	}
	cfg := serveConfig{
		addr: *addr, dev: *dev, corsOrigin: *corsOrigin,
		replayCap: *replayCap, replayTTL: *replayTTL,
		keepAlive: *keepAlive, retryMS: *retryMS, maxBody: *maxBody,
		maxChannels: *maxChannels, subBuffer: *subBuffer,
	}
	if cfg.replayCap < 0 || cfg.maxChannels < 1 || cfg.subBuffer < 1 {
		return serveConfig{}, fmt.Errorf("serve: --replay must be >= 0, --max-channels and --sub-buffer >= 1")
	}
	if cfg.dev {
		if !loopbackAddr(cfg.addr) {
			return serveConfig{}, fmt.Errorf("serve: --dev disables auth and therefore only binds loopback; %q is not a loopback address", cfg.addr)
		}
		return cfg, nil
	}
	if *keysSpec == "" {
		return serveConfig{}, fmt.Errorf("serve: no keys; pass --keys kid=secret (or FANLINE_KEYS), or --dev for a loopback hub without auth")
	}
	keys, err := token.ParseKeys(*keysSpec)
	if err != nil {
		return serveConfig{}, err
	}
	cfg.keys = keys
	return cfg, nil
}

// loopbackAddr reports whether addr's host part is a loopback address.
func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func runServe(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	cfg, err := parseServeFlags(args, stderr, getenv)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK // -h/--help: the FlagSet already printed usage
		}
		fmt.Fprintf(stderr, "fanline: %v\n", err)
		return ExitUsage
	}
	h := hub.New(hub.Options{
		ReplayCap:   cfg.replayCap,
		ReplayTTL:   cfg.replayTTL,
		MaxChannels: cfg.maxChannels,
		SubBuffer:   cfg.subBuffer,
	})
	handler := server.New(h, server.Config{
		Keys:       cfg.keys,
		Dev:        cfg.dev,
		CORSOrigin: cfg.corsOrigin,
		KeepAlive:  cfg.keepAlive,
		RetryMS:    cfg.retryMS,
		MaxBody:    cfg.maxBody,
	})

	// Listen first so ":0" resolves to a concrete port before we log it —
	// scripts (including scripts/smoke.sh) parse this line.
	ln, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		fmt.Fprintf(stderr, "fanline: %v\n", err)
		return ExitRuntime
	}
	mode := fmt.Sprintf("keys=%s", strings.Join(cfg.keys.KeyIDs(), ","))
	if cfg.dev {
		mode = "dev mode, no auth"
	}
	fmt.Fprintf(stderr, "fanline %s listening on http://%s (%s, replay=%d)\n",
		version.Version, ln.Addr(), mode, cfg.replayCap)

	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(stderr, "fanline: %v\n", err)
		return ExitRuntime
	}
	return ExitOK
}
