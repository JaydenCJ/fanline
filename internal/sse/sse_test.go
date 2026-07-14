// Tests for the SSE wire codec. Writer and reader are exercised against
// each other (round trips) and against hand-written streams matching the
// WHATWG parsing rules — including the quirks: CRLF lines, no-space
// colons, comments, and multi-line data.
package sse

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func readAll(t *testing.T, stream string) []Event {
	t.Helper()
	r := NewReader(strings.NewReader(stream))
	var events []Event
	for {
		e, err := r.Next()
		if errors.Is(err, io.EOF) {
			return events
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		events = append(events, e)
	}
}

func TestWriteEventBasicFrame(t *testing.T) {
	var b strings.Builder
	if err := WriteEvent(&b, Event{ID: "ep-1", Name: "tick", Data: "hello"}); err != nil {
		t.Fatal(err)
	}
	want := "id: ep-1\nevent: tick\ndata: hello\n\n"
	if b.String() != want {
		t.Errorf("frame = %q, want %q", b.String(), want)
	}
	// Empty id/name fields are omitted, not written as blank lines.
	b.Reset()
	_ = WriteEvent(&b, Event{Data: "x"})
	if got := b.String(); got != "data: x\n\n" {
		t.Errorf("frame = %q, want a bare data frame", got)
	}
}

func TestWriteEventSplitsMultilineData(t *testing.T) {
	var b strings.Builder
	_ = WriteEvent(&b, Event{Data: "line1\nline2\nline3"})
	want := "data: line1\ndata: line2\ndata: line3\n\n"
	if b.String() != want {
		t.Errorf("frame = %q, want %q", b.String(), want)
	}
}

func TestRoundTripPreservesNewlinesInData(t *testing.T) {
	// JSON payloads pretty-printed with newlines must survive the wire —
	// this is the one place SSE actively transforms the payload.
	payload := "{\n  \"a\": 1,\n  \"b\": 2\n}"
	var b strings.Builder
	_ = WriteEvent(&b, Event{ID: "e-1", Name: "doc", Data: payload})
	events := readAll(t, b.String())
	if len(events) != 1 || events[0].Data != payload {
		t.Errorf("round trip mangled data: %+v", events)
	}
}

func TestReaderParsesMultipleEvents(t *testing.T) {
	events := readAll(t, "id: 1\ndata: a\n\nid: 2\nevent: custom\ndata: b\n\n")
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if events[0].Name != "" || events[0].Data != "a" {
		t.Errorf("event 0 = %+v", events[0])
	}
	if events[1].ID != "2" || events[1].Name != "custom" || events[1].Data != "b" {
		t.Errorf("event 1 = %+v", events[1])
	}
}

func TestReaderSpecQuirks(t *testing.T) {
	// The space after ':' is optional per spec.
	events := readAll(t, "data:tight\n\n")
	if len(events) != 1 || events[0].Data != "tight" {
		t.Errorf("no-space colon: events = %+v", events)
	}
	// CRLF line endings are equivalent to LF.
	events = readAll(t, "id: 5\r\ndata: crlf\r\n\r\n")
	if len(events) != 1 || events[0].ID != "5" || events[0].Data != "crlf" {
		t.Errorf("CRLF: events = %+v", events)
	}
	// Unknown fields are ignored, not treated as errors.
	events = readAll(t, "data: x\nfancy: field\n\n")
	if len(events) != 1 || events[0].Data != "x" {
		t.Errorf("unknown field: events = %+v", events)
	}
}

func TestReaderSkipsCommentsAndRetry(t *testing.T) {
	stream := ": keepalive\n\nretry: 3000\n\n: another comment\ndata: real\n\n"
	events := readAll(t, stream)
	if len(events) != 1 || events[0].Data != "real" {
		t.Errorf("events = %+v, want only the data event", events)
	}
}

func TestReaderDiscardsEventWithoutData(t *testing.T) {
	// Per spec an event with no data field is not dispatched.
	events := readAll(t, "event: nudge\n\ndata: kept\n\n")
	if len(events) != 1 || events[0].Data != "kept" {
		t.Errorf("events = %+v", events)
	}
	// But the dataless frame must not leak its name into the next event.
	if events[0].Name != "" {
		t.Errorf("name leaked across dispatch boundary: %q", events[0].Name)
	}
}

func TestReaderLastEventIDPersistsAcrossEvents(t *testing.T) {
	// Spec: the last-event-ID buffer is NOT reset on dispatch, so an event
	// without its own id inherits the previous one. Reconnect logic
	// depends on this.
	events := readAll(t, "id: 7\ndata: a\n\ndata: b\n\n")
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if events[1].ID != "7" {
		t.Errorf("event 2 id = %q, want inherited \"7\"", events[1].ID)
	}
}

func TestReaderJoinsRepeatedDataFields(t *testing.T) {
	events := readAll(t, "data: a\ndata: b\n\n")
	if len(events) != 1 || events[0].Data != "a\nb" {
		t.Errorf("events = %+v, want data \"a\\nb\"", events)
	}
}

func TestReaderEmptyDataLineIsAnEmptyString(t *testing.T) {
	// "data:" alone contributes an empty line, not nothing.
	events := readAll(t, "data:\ndata: after\n\n")
	if len(events) != 1 || events[0].Data != "\nafter" {
		t.Errorf("events = %+v, want data \"\\nafter\"", events)
	}
}

func TestWriteCommentAndRetryFormat(t *testing.T) {
	var b strings.Builder
	_ = WriteComment(&b, "keepalive")
	_ = WriteRetry(&b, 3000)
	if got := b.String(); got != ": keepalive\n\nretry: 3000\n\n" {
		t.Errorf("wire = %q", got)
	}
}
