// Package client is the minimal SSE consumer behind `fanline tail` and
// `fanline publish`. It exists for operators and tests; browsers and other
// services need no library at all — EventSource or any HTTP client that
// can read a stream is enough.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/JaydenCJ/fanline/internal/sse"
)

// Stop is returned by a StreamFunc to end the stream cleanly.
var Stop = errors.New("client: stop")

// Ready mirrors the hub's fanline.ready payload.
type Ready struct {
	Channel  string `json:"channel"`
	Epoch    string `json:"epoch"`
	Replayed int    `json:"replayed"`
	Gap      bool   `json:"gap"`
}

// StreamOptions configure one Stream call.
type StreamOptions struct {
	BaseURL string // e.g. http://127.0.0.1:8787
	Channel string
	Token   string // empty for dev-mode hubs
	LastID  string // resume point, sent as Last-Event-ID
	Replay  int    // request last-N replay when LastID is empty
	OnReady func(Ready)
	Client  *http.Client // defaults to http.DefaultClient
}

// StreamFunc receives each data event; returning Stop ends the stream
// cleanly, any other error aborts it.
type StreamFunc func(sse.Event) error

// Stream subscribes to one channel and invokes fn per event until fn
// returns Stop, the context is canceled, or the connection ends.
func Stream(ctx context.Context, opts StreamOptions, fn StreamFunc) error {
	u, err := subscribeURL(opts)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	if opts.Token != "" {
		req.Header.Set("Authorization", "Bearer "+opts.Token)
	}
	if opts.LastID != "" {
		req.Header.Set("Last-Event-ID", opts.LastID)
	}
	hc := opts.Client
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("client: subscribe failed: %s", readError(resp))
	}

	r := sse.NewReader(resp.Body)
	for {
		e, err := r.Next()
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return err
		}
		if e.Name == "fanline.ready" {
			if opts.OnReady != nil {
				var rd Ready
				if json.Unmarshal([]byte(e.Data), &rd) == nil {
					opts.OnReady(rd)
				}
			}
			continue
		}
		if err := fn(e); err != nil {
			if errors.Is(err, Stop) {
				return nil
			}
			return err
		}
	}
}

func subscribeURL(opts StreamOptions) (string, error) {
	base := strings.TrimSuffix(opts.BaseURL, "/")
	if base == "" {
		return "", errors.New("client: base URL required")
	}
	q := url.Values{}
	if opts.LastID == "" && opts.Replay > 0 {
		q.Set("replay", strconv.Itoa(opts.Replay))
	}
	u := base + "/v1/sse/" + url.PathEscape(opts.Channel)
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u, nil
}

// PublishResult mirrors the hub's publish response.
type PublishResult struct {
	ID          string `json:"id"`
	Seq         uint64 `json:"seq"`
	Channel     string `json:"channel"`
	Subscribers int    `json:"subscribers"`
}

// Publish posts one event to the hub.
func Publish(ctx context.Context, hc *http.Client, baseURL, ch, tok, eventName, data string) (PublishResult, error) {
	base := strings.TrimSuffix(baseURL, "/")
	if base == "" {
		return PublishResult{}, errors.New("client: base URL required")
	}
	u := base + "/v1/publish/" + url.PathEscape(ch)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(data))
	if err != nil {
		return PublishResult{}, err
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if eventName != "" {
		req.Header.Set("X-Fanline-Event", eventName)
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return PublishResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return PublishResult{}, fmt.Errorf("client: publish failed: %s", readError(resp))
	}
	var out PublishResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return PublishResult{}, fmt.Errorf("client: bad publish response: %w", err)
	}
	return out, nil
}

// readError extracts the hub's JSON error message, falling back to the
// HTTP status line.
func readError(resp *http.Response) string {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return fmt.Sprintf("%s (%s)", e.Error, resp.Status)
	}
	return resp.Status
}
