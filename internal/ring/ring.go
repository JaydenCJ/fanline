// Package ring implements the per-channel replay buffer: a fixed-capacity
// circular log of published events with lazy TTL pruning.
//
// Every event carries a per-channel sequence number (1, 2, 3, …) and a wire
// ID "<epoch>-<seq>". The epoch is a random string chosen when the channel
// is (re)created, so a client resuming with a Last-Event-ID from before a
// hub restart is detected as a gap instead of silently replaying the wrong
// range of a reset sequence.
package ring

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Event is one published message as retained for replay and fanout.
type Event struct {
	Seq   uint64    `json:"seq"`
	ID    string    `json:"id"`   // "<epoch>-<seq>", the SSE `id:` field
	Name  string    `json:"name"` // SSE `event:` field; "" means default "message"
	Data  string    `json:"data"`
	At    time.Time `json:"at"`
	Epoch string    `json:"-"`
}

// FormatID renders the wire ID for an epoch/seq pair.
func FormatID(epoch string, seq uint64) string {
	return epoch + "-" + strconv.FormatUint(seq, 10)
}

// ParseID splits a wire ID back into epoch and seq. It accepts epochs
// containing '-' by splitting on the LAST dash.
func ParseID(id string) (epoch string, seq uint64, err error) {
	i := strings.LastIndexByte(id, '-')
	if i <= 0 || i == len(id)-1 {
		return "", 0, fmt.Errorf("ring: malformed event id %q", id)
	}
	seq, err = strconv.ParseUint(id[i+1:], 10, 64)
	if err != nil || seq == 0 {
		return "", 0, fmt.Errorf("ring: malformed event id %q", id)
	}
	return id[:i], seq, nil
}

// Ring is a fixed-capacity circular buffer of events. It is NOT safe for
// concurrent use; the hub serializes access under its own lock.
type Ring struct {
	buf   []Event
	start int // index of the oldest retained event
	n     int // number of retained events
	ttl   time.Duration
}

// New returns a ring retaining at most capacity events, each for at most
// ttl (0 = keep until evicted by capacity). Capacity 0 disables replay.
func New(capacity int, ttl time.Duration) *Ring {
	if capacity < 0 {
		capacity = 0
	}
	return &Ring{buf: make([]Event, capacity), ttl: ttl}
}

// Cap returns the ring's fixed capacity.
func (r *Ring) Cap() int { return len(r.buf) }

// Append retains e, evicting the oldest event when full.
func (r *Ring) Append(e Event) {
	if len(r.buf) == 0 {
		return
	}
	if r.n == len(r.buf) {
		r.buf[r.start] = e
		r.start = (r.start + 1) % len(r.buf)
		return
	}
	r.buf[(r.start+r.n)%len(r.buf)] = e
	r.n++
}

// Len returns how many events are retained after TTL pruning at now.
func (r *Ring) Len(now time.Time) int {
	r.prune(now)
	return r.n
}

// OldestSeq returns the sequence number of the oldest retained event,
// or 0 when the ring is empty.
func (r *Ring) OldestSeq(now time.Time) uint64 {
	r.prune(now)
	if r.n == 0 {
		return 0
	}
	return r.buf[r.start].Seq
}

// Since returns retained events with Seq > after, in order. gap is true
// when events between after and the oldest retained one have already been
// evicted — i.e. the client missed messages that cannot be replayed.
func (r *Ring) Since(after uint64, now time.Time) (events []Event, gap bool) {
	r.prune(now)
	if r.n == 0 {
		// An empty ring cannot prove continuity unless the client is
		// exactly caught up; the hub checks that against its own seq.
		return nil, false
	}
	oldest := r.buf[r.start].Seq
	if after+1 < oldest {
		gap = true
	}
	for i := 0; i < r.n; i++ {
		e := r.buf[(r.start+i)%len(r.buf)]
		if e.Seq > after {
			events = append(events, e)
		}
	}
	return events, gap
}

// LastN returns up to n most recent retained events, oldest first.
func (r *Ring) LastN(n int, now time.Time) []Event {
	r.prune(now)
	if n <= 0 || r.n == 0 {
		return nil
	}
	if n > r.n {
		n = r.n
	}
	out := make([]Event, 0, n)
	for i := r.n - n; i < r.n; i++ {
		out = append(out, r.buf[(r.start+i)%len(r.buf)])
	}
	return out
}

// prune drops events older than the TTL. Lazy: called from every reader,
// so the ring never needs its own timer goroutine.
func (r *Ring) prune(now time.Time) {
	if r.ttl <= 0 {
		return
	}
	cutoff := now.Add(-r.ttl)
	for r.n > 0 && r.buf[r.start].At.Before(cutoff) {
		r.buf[r.start] = Event{} // release the payload for GC
		r.start = (r.start + 1) % len(r.buf)
		r.n--
	}
	if r.n == 0 {
		r.start = 0
	}
}
