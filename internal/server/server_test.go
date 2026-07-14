// HTTP-layer tests, run against httptest with real streamed responses.
// Auth decisions (401 vs 403), the SSE handshake (ready event, replay,
// Last-Event-ID), CORS, and body limits are all observable behavior here —
// no handler internals are poked. Synchronization is protocol-driven: a
// publisher acts only after reading the subscriber's fanline.ready frame,
// so there are no sleeps and no races.
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JaydenCJ/fanline/internal/hub"
	"github.com/JaydenCJ/fanline/internal/sse"
	"github.com/JaydenCJ/fanline/internal/token"
)

var testNow = time.Unix(1_700_000_000, 0)

const testSecret = "test-secret"

// newTestServer starts a hub+handler pair with a deterministic clock and
// keepalives disabled in practice (interval far beyond any test's life).
func newTestServer(t *testing.T, cfg Config) (*httptest.Server, *hub.Hub) {
	t.Helper()
	epoch := 0
	h := hub.New(hub.Options{
		ReplayCap: 8,
		Now:       func() time.Time { return testNow },
		Epoch:     func() string { epoch++; return "tep" + string(rune('0'+epoch)) },
	})
	if cfg.Keys == nil && !cfg.Dev {
		cfg.Keys = token.Keyring{"main": testSecret}
	}
	if cfg.KeepAlive == 0 {
		cfg.KeepAlive = time.Hour
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return testNow }
	}
	srv := httptest.NewServer(New(h, cfg))
	t.Cleanup(srv.Close)
	return srv, h
}

func mintToken(t *testing.T, pattern string, caps ...string) string {
	t.Helper()
	tok, err := token.Sign(token.Claims{
		KeyID:    "main",
		Channel:  pattern,
		Caps:     caps,
		IssuedAt: testNow.Unix(),
	}, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func doRequest(t *testing.T, method, url, bearer, body string, header map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decodeError(t *testing.T, resp *http.Response) string {
	t.Helper()
	var e struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("error body not JSON: %v", err)
	}
	return e.Error
}

// subscribe opens an SSE stream and returns its reader plus the decoded
// ready payload; the returned response is cleaned up with the test.
func subscribe(t *testing.T, url, bearer string, header map[string]string) (*sse.Reader, readyPayload) {
	t.Helper()
	resp := doRequest(t, http.MethodGet, url, bearer, "", header)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("subscribe: status %d: %s", resp.StatusCode, decodeError(t, resp))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	r := sse.NewReader(resp.Body)
	first, err := r.Next()
	if err != nil {
		t.Fatalf("reading ready frame: %v", err)
	}
	if first.Name != "fanline.ready" {
		t.Fatalf("first event = %q, want fanline.ready", first.Name)
	}
	var ready readyPayload
	if err := json.Unmarshal([]byte(first.Data), &ready); err != nil {
		t.Fatalf("ready payload: %v", err)
	}
	return r, ready
}

func TestHealthzNeedsNoAuth(t *testing.T) {
	srv, _ := newTestServer(t, Config{})
	resp := doRequest(t, http.MethodGet, srv.URL+"/v1/healthz", "", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"ok":true`) {
		t.Errorf("body = %s", body)
	}
}

func TestPublishRequiresToken(t *testing.T) {
	srv, _ := newTestServer(t, Config{})
	resp := doRequest(t, http.MethodPost, srv.URL+"/v1/publish/news", "", "x", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if msg := decodeError(t, resp); !strings.Contains(msg, "missing token") {
		t.Errorf("error = %q", msg)
	}
	// A structurally broken token is also a 401, not a 500.
	garbage := doRequest(t, http.MethodPost, srv.URL+"/v1/publish/news", "fl1.garbage.token", "x", nil)
	if garbage.StatusCode != http.StatusUnauthorized {
		t.Fatalf("garbage token: status = %d, want 401", garbage.StatusCode)
	}
}

func TestPublishRejectsExpiredTokenWithSpecificMessage(t *testing.T) {
	srv, _ := newTestServer(t, Config{})
	expired, err := token.Sign(token.Claims{
		KeyID: "main", Channel: "news", Caps: []string{token.CapPublish},
		IssuedAt: testNow.Add(-2 * time.Hour).Unix(), ExpiresAt: testNow.Add(-time.Hour).Unix(),
	}, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	resp := doRequest(t, http.MethodPost, srv.URL+"/v1/publish/news", expired, "x", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if msg := decodeError(t, resp); msg != "token expired" {
		t.Errorf("error = %q, want the expiry called out", msg)
	}
}

func TestPublishForbiddenWithoutGrant(t *testing.T) {
	srv, _ := newTestServer(t, Config{})
	// Right capability, wrong channel: 403 (identity fine, grant not).
	wrongChannel := mintToken(t, "orders.*", token.CapPublish)
	resp := doRequest(t, http.MethodPost, srv.URL+"/v1/publish/invoices.eu", wrongChannel, "x", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong channel: status = %d, want 403", resp.StatusCode)
	}
	// Right channel, missing capability: also 403, naming the gap.
	subOnly := mintToken(t, "news", token.CapSubscribe)
	resp = doRequest(t, http.MethodPost, srv.URL+"/v1/publish/news", subOnly, "x", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("sub-only token: status = %d, want 403", resp.StatusCode)
	}
	if msg := decodeError(t, resp); !strings.Contains(msg, `"pub"`) {
		t.Errorf("error should name the missing capability: %q", msg)
	}
}

func TestPublishReturnsEventMetadata(t *testing.T) {
	srv, _ := newTestServer(t, Config{})
	tok := mintToken(t, "news", token.CapPublish)
	resp := doRequest(t, http.MethodPost, srv.URL+"/v1/publish/news", tok, "hello", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, decodeError(t, resp))
	}
	var out publishResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Seq != 1 || out.Channel != "news" || out.Subscribers != 0 || out.ID == "" {
		t.Errorf("response = %+v", out)
	}
}

func TestPublishRejectsOversizedBody(t *testing.T) {
	srv, _ := newTestServer(t, Config{MaxBody: 16})
	tok := mintToken(t, "news", token.CapPublish)
	resp := doRequest(t, http.MethodPost, srv.URL+"/v1/publish/news", tok, strings.Repeat("x", 64), nil)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

func TestPublishRejectsBadEventNames(t *testing.T) {
	srv, _ := newTestServer(t, Config{})
	tok := mintToken(t, "news", token.CapPublish)
	// "fanline." names are reserved for protocol frames like ready.
	resp := doRequest(t, http.MethodPost, srv.URL+"/v1/publish/news", tok, "x",
		map[string]string{"X-Fanline-Event": "fanline.ready"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("reserved name: status = %d, want 400", resp.StatusCode)
	}
	if msg := decodeError(t, resp); !strings.Contains(msg, "reserved") {
		t.Errorf("error = %q", msg)
	}
	// A newline in the event name would let a publisher inject arbitrary
	// SSE frames into every subscriber's stream.
	resp = doRequest(t, http.MethodPost, srv.URL+"/v1/publish/news?event="+
		"tick%0Adata%3A%20forged", tok, "x", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("newline injection: status = %d, want 400", resp.StatusCode)
	}
}

func TestSubscribeStreamsPublishedEvents(t *testing.T) {
	srv, h := newTestServer(t, Config{})
	tok := mintToken(t, "news", token.CapSubscribe)
	r, ready := subscribe(t, srv.URL+"/v1/sse/news", tok, nil)
	if ready.Channel != "news" || ready.Replayed != 0 || ready.Gap {
		t.Fatalf("ready = %+v", ready)
	}
	// The subscription is registered before ready is written, so this
	// publish is guaranteed to reach the open stream.
	h.Publish("news", "tick", "payload-1")
	e, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != "tick" || e.Data != "payload-1" || !strings.HasSuffix(e.ID, "-1") {
		t.Errorf("event = %+v", e)
	}
	// EventSource cannot set headers; the same token must also work as a
	// ?token= query parameter.
	_, ready = subscribe(t, srv.URL+"/v1/sse/news?token="+tok, "", nil)
	if ready.Channel != "news" {
		t.Errorf("query-token ready = %+v", ready)
	}
}

func TestSubscribeReplayQueryDeliversHistoryThenLive(t *testing.T) {
	srv, h := newTestServer(t, Config{})
	h.Publish("news", "", "old-1")
	h.Publish("news", "", "old-2")
	h.Publish("news", "", "old-3")
	tok := mintToken(t, "news", token.CapSubscribe)
	r, ready := subscribe(t, srv.URL+"/v1/sse/news?replay=2", tok, nil)
	if ready.Replayed != 2 || ready.Gap {
		t.Fatalf("ready = %+v, want 2 replayed without gap", ready)
	}
	for i, want := range []string{"old-2", "old-3"} {
		e, err := r.Next()
		if err != nil {
			t.Fatal(err)
		}
		if e.Data != want {
			t.Errorf("replay %d = %q, want %q", i, e.Data, want)
		}
	}
	h.Publish("news", "", "live-1")
	if e, _ := r.Next(); e.Data != "live-1" {
		t.Errorf("live event = %+v", e)
	}
}

func TestSubscribeLastEventIDHeaderResumes(t *testing.T) {
	srv, h := newTestServer(t, Config{})
	e1, _, _ := h.Publish("news", "", "m1")
	h.Publish("news", "", "m2")
	h.Publish("news", "", "m3")
	tok := mintToken(t, "news", token.CapSubscribe)
	r, ready := subscribe(t, srv.URL+"/v1/sse/news", tok,
		map[string]string{"Last-Event-ID": e1.ID})
	if ready.Replayed != 2 || ready.Gap {
		t.Fatalf("ready = %+v", ready)
	}
	got := []string{}
	for i := 0; i < 2; i++ {
		e, err := r.Next()
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, e.Data)
	}
	if got[0] != "m2" || got[1] != "m3" {
		t.Errorf("resumed events = %v", got)
	}
}

func TestSubscribeStaleLastEventIDReportsGap(t *testing.T) {
	srv, h := newTestServer(t, Config{})
	h.Publish("news", "", "m1")
	tok := mintToken(t, "news", token.CapSubscribe)
	_, ready := subscribe(t, srv.URL+"/v1/sse/news", tok,
		map[string]string{"Last-Event-ID": "preboot-99"})
	if !ready.Gap || ready.Replayed != 1 {
		t.Errorf("ready = %+v, want gap with full replay", ready)
	}
}

func TestSubscribeWildcardTokenCoversManyChannels(t *testing.T) {
	srv, _ := newTestServer(t, Config{})
	tok := mintToken(t, "dash.**", token.CapSubscribe)
	_, ready := subscribe(t, srv.URL+"/v1/sse/dash.tenant-7.cpu", tok, nil)
	if ready.Channel != "dash.tenant-7.cpu" {
		t.Errorf("ready = %+v", ready)
	}
	resp := doRequest(t, http.MethodGet, srv.URL+"/v1/sse/other.chan", tok, "", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("outside pattern: status = %d, want 403", resp.StatusCode)
	}
}

func TestSubscribeRejectsBadReplayParam(t *testing.T) {
	srv, _ := newTestServer(t, Config{})
	tok := mintToken(t, "news", token.CapSubscribe)
	for _, v := range []string{"abc", "-1", "1.5"} {
		resp := doRequest(t, http.MethodGet, srv.URL+"/v1/sse/news?replay="+v, tok, "", nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("replay=%s: status = %d, want 400", v, resp.StatusCode)
		}
	}
}

func TestStatsEndpoint(t *testing.T) {
	srv, h := newTestServer(t, Config{})
	h.Publish("alpha", "", "1")
	h.Publish("beta", "", "1")
	// Even a token holding both data capabilities on every channel cannot
	// read stats — that needs the dedicated cap.
	subOnly := mintToken(t, "**", token.CapSubscribe, token.CapPublish)
	resp := doRequest(t, http.MethodGet, srv.URL+"/v1/stats", subOnly, "", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	tok := mintToken(t, "**", token.CapStats)
	resp = doRequest(t, http.MethodGet, srv.URL+"/v1/stats", tok, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out struct {
		Version  string `json:"version"`
		Channels []struct {
			Channel   string `json:"channel"`
			Published uint64 `json:"published"`
		} `json:"channels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Channels) != 2 || out.Channels[0].Channel != "alpha" || out.Channels[0].Published != 1 {
		t.Errorf("stats = %+v", out)
	}
}

func TestDevModeSkipsAuthEntirely(t *testing.T) {
	srv, _ := newTestServer(t, Config{Dev: true})
	resp := doRequest(t, http.MethodPost, srv.URL+"/v1/publish/anything", "", "x", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dev publish status = %d", resp.StatusCode)
	}
	_, ready := subscribe(t, srv.URL+"/v1/sse/anything?replay=1", "", nil)
	if ready.Replayed != 1 {
		t.Errorf("ready = %+v", ready)
	}
}

func TestCORSHeadersWhenConfigured(t *testing.T) {
	srv, _ := newTestServer(t, Config{CORSOrigin: "https://app.example.test"})
	resp := doRequest(t, http.MethodGet, srv.URL+"/v1/healthz", "", "", nil)
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://app.example.test" {
		t.Errorf("ACAO = %q", got)
	}
	pre := doRequest(t, http.MethodOptions, srv.URL+"/v1/sse/news", "", "", nil)
	if pre.StatusCode != http.StatusNoContent {
		t.Errorf("preflight status = %d, want 204", pre.StatusCode)
	}
	if !strings.Contains(pre.Header.Get("Access-Control-Allow-Headers"), "Last-Event-ID") {
		t.Errorf("preflight must allow Last-Event-ID, got %q", pre.Header.Get("Access-Control-Allow-Headers"))
	}
	// And with CORS unconfigured, no ACAO header leaks out.
	plain, _ := newTestServer(t, Config{})
	resp = doRequest(t, http.MethodGet, plain.URL+"/v1/healthz", "", "", nil)
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("ACAO = %q, want unset by default", got)
	}
}

func TestMethodAndRouteMismatches(t *testing.T) {
	srv, _ := newTestServer(t, Config{})
	tok := mintToken(t, "news", token.CapSubscribe, token.CapPublish)
	if resp := doRequest(t, http.MethodGet, srv.URL+"/v1/publish/news", tok, "", nil); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET publish: %d, want 405", resp.StatusCode)
	}
	if resp := doRequest(t, http.MethodPost, srv.URL+"/v1/sse/news", tok, "", nil); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST sse: %d, want 405", resp.StatusCode)
	}
	if resp := doRequest(t, http.MethodGet, srv.URL+"/v1/nope", tok, "", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown route: %d, want 404", resp.StatusCode)
	}
}

func TestSubscribeSendsRetryHint(t *testing.T) {
	srv, _ := newTestServer(t, Config{RetryMS: 1500})
	tok := mintToken(t, "news", token.CapSubscribe)
	resp := doRequest(t, http.MethodGet, srv.URL+"/v1/sse/news", tok, "", nil)
	buf := make([]byte, 256)
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "retry: 1500\n") {
		t.Errorf("stream head = %q, want a retry hint", buf[:n])
	}
}

func TestValidateEventName(t *testing.T) {
	for _, ok := range []string{"", "tick", "order.created", "A-Z_09"} {
		if err := validateEventName(ok); err != nil {
			t.Errorf("validateEventName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"has space", "new\nline", "fanline.gap", strings.Repeat("x", 65), "émoji"} {
		if err := validateEventName(bad); err == nil {
			t.Errorf("validateEventName(%q) accepted", bad)
		}
	}
}
