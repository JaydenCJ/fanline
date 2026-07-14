// Package sse encodes and decodes the text/event-stream wire format
// (WHATWG HTML §9.2 "Server-sent events").
//
// The writer half is used by the hub's HTTP handler; the reader half by
// the built-in `fanline tail` client and the test suite. Both sides follow
// the spec's parsing rules: fields split on the first ':', one optional
// space after it, multi-line data joined with '\n', events dispatched on a
// blank line, lines ending LF or CRLF, comments (lines starting ':')
// ignored by the parser but usable as keepalives.
package sse

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// Event is one decoded server-sent event.
type Event struct {
	ID   string
	Name string // the `event:` field; "" means the default "message"
	Data string
}

// WriteEvent encodes e to w. Multi-line data becomes one `data:` line per
// line, which the receiving parser rejoins — this is how SSE ships payloads
// containing newlines. IDs and names are single-line by construction
// (validated at publish time), so they are written as-is.
func WriteEvent(w io.Writer, e Event) error {
	var b strings.Builder
	if e.ID != "" {
		b.WriteString("id: ")
		b.WriteString(e.ID)
		b.WriteByte('\n')
	}
	if e.Name != "" {
		b.WriteString("event: ")
		b.WriteString(e.Name)
		b.WriteByte('\n')
	}
	for _, line := range strings.Split(e.Data, "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	_, err := io.WriteString(w, b.String())
	return err
}

// WriteComment writes a comment line. Proxies and clients ignore it, which
// makes it the standard SSE keepalive.
func WriteComment(w io.Writer, text string) error {
	_, err := fmt.Fprintf(w, ": %s\n\n", text)
	return err
}

// WriteRetry tells the client how long to wait before reconnecting.
func WriteRetry(w io.Writer, ms int) error {
	_, err := fmt.Fprintf(w, "retry: %d\n\n", ms)
	return err
}

// Reader incrementally decodes events from a stream.
type Reader struct {
	s *bufio.Scanner

	id, name string
	data     []string
	hasData  bool
}

// NewReader wraps r. Lines longer than 1 MiB abort the stream.
func NewReader(r io.Reader) *Reader {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64*1024), 1<<20)
	return &Reader{s: s}
}

// Next returns the next complete event, or io.EOF when the stream ends.
// Comments and unknown fields are skipped per spec; a `retry:` field is
// also skipped (the CLI client manages its own reconnect policy).
func (r *Reader) Next() (Event, error) {
	for r.s.Scan() {
		line := strings.TrimSuffix(r.s.Text(), "\r")
		if line == "" {
			// Blank line: dispatch if the pending event has any data.
			// Per spec, an event with no data field is discarded.
			if r.hasData {
				e := Event{ID: r.id, Name: r.name, Data: strings.Join(r.data, "\n")}
				r.reset()
				return e, nil
			}
			r.reset()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // comment / keepalive
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "data":
			r.data = append(r.data, value)
			r.hasData = true
		case "event":
			r.name = value
		case "id":
			// Spec: an id containing NUL is ignored.
			if !strings.ContainsRune(value, 0) {
				r.id = value
			}
		}
	}
	if err := r.s.Err(); err != nil {
		return Event{}, err
	}
	return Event{}, io.EOF
}

func (r *Reader) reset() {
	// The last seen id persists across events per spec ("last event ID
	// buffer" is not reset on dispatch), but name and data are.
	r.name = ""
	r.data = nil
	r.hasData = false
}
