// Tests for the replay ring: capacity eviction, TTL pruning with an
// injected clock, gap detection, and wire-ID parsing. Replay correctness
// is what makes "reconnect and miss nothing" true, so the boundary cases
// (exactly caught up, one event evicted, empty ring) get explicit tests.
package ring

import (
	"testing"
	"time"
)

var t0 = time.Unix(1_700_000_000, 0)

// fill appends n events with seq 1..n, one second apart starting at t0.
func fill(r *Ring, n int) {
	for i := 1; i <= n; i++ {
		r.Append(Event{
			Seq:  uint64(i),
			ID:   FormatID("ep0", uint64(i)),
			Data: "payload",
			At:   t0.Add(time.Duration(i) * time.Second),
		})
	}
}

func seqs(events []Event) []uint64 {
	out := make([]uint64, len(events))
	for i, e := range events {
		out[i] = e.Seq
	}
	return out
}

func TestAppendAndLastN(t *testing.T) {
	r := New(8, 0)
	fill(r, 5)
	got := r.LastN(3, t0.Add(time.Minute))
	if len(got) != 3 || got[0].Seq != 3 || got[2].Seq != 5 {
		t.Errorf("LastN(3) seqs = %v, want [3 4 5]", seqs(got))
	}
	// N beyond retention clamps; N=0 asks for nothing.
	if got := r.LastN(100, t0.Add(time.Minute)); len(got) != 5 {
		t.Errorf("LastN(100) returned %d events, want 5", len(got))
	}
	if got := r.LastN(0, t0.Add(time.Minute)); got != nil {
		t.Errorf("LastN(0) = %v, want nil", seqs(got))
	}
}

func TestCapacityEvictionKeepsNewest(t *testing.T) {
	r := New(3, 0)
	fill(r, 5)
	got := r.LastN(10, t0.Add(time.Minute))
	if len(got) != 3 || got[0].Seq != 3 || got[2].Seq != 5 {
		t.Errorf("after overflow, retained = %v, want [3 4 5]", seqs(got))
	}
	if r.OldestSeq(t0.Add(time.Minute)) != 3 {
		t.Errorf("OldestSeq = %d, want 3", r.OldestSeq(t0.Add(time.Minute)))
	}
}

func TestSinceReturnsOnlyNewerEvents(t *testing.T) {
	r := New(8, 0)
	fill(r, 5)
	got, gap := r.Since(2, t0.Add(time.Minute))
	if gap {
		t.Error("no eviction happened, gap must be false")
	}
	if len(got) != 3 || got[0].Seq != 3 {
		t.Errorf("Since(2) = %v, want [3 4 5]", seqs(got))
	}
}

func TestSinceExactlyCaughtUp(t *testing.T) {
	r := New(8, 0)
	fill(r, 5)
	got, gap := r.Since(5, t0.Add(time.Minute))
	if len(got) != 0 || gap {
		t.Errorf("caught-up client got %v (gap=%v), want nothing", seqs(got), gap)
	}
}

func TestSinceFlagsGapAfterEviction(t *testing.T) {
	r := New(3, 0)
	fill(r, 5) // retained: 3,4,5
	got, gap := r.Since(1, t0.Add(time.Minute))
	if !gap {
		t.Error("event 2 was evicted, gap must be true")
	}
	if len(got) != 3 || got[0].Seq != 3 {
		t.Errorf("Since(1) = %v, want [3 4 5]", seqs(got))
	}
	// Client holding seq 2 is contiguous with retained 3: no gap.
	if _, gap := r.Since(2, t0.Add(time.Minute)); gap {
		t.Error("Since(oldest-1) must not flag a gap")
	}
}

func TestTTLPruneDropsOldEvents(t *testing.T) {
	r := New(8, 10*time.Second)
	fill(r, 5) // events at t0+1s … t0+5s
	// At t0+13s, events older than t0+3s (seq 1,2) are expired.
	now := t0.Add(13 * time.Second)
	if n := r.Len(now); n != 3 {
		t.Fatalf("Len = %d, want 3", n)
	}
	got, gap := r.Since(1, now)
	if !gap {
		t.Error("seq 2 expired, gap must be true")
	}
	if len(got) != 3 || got[0].Seq != 3 {
		t.Errorf("after TTL prune, Since(1) = %v, want [3 4 5]", seqs(got))
	}
}

func TestTTLPruneCanEmptyTheRing(t *testing.T) {
	r := New(8, time.Second)
	fill(r, 3)
	now := t0.Add(time.Hour)
	if n := r.Len(now); n != 0 {
		t.Fatalf("Len = %d, want 0 after everything expired", n)
	}
	got, gap := r.Since(1, now)
	if got != nil || gap {
		t.Errorf("empty ring Since = (%v, %v); continuity is the hub's call", seqs(got), gap)
	}
}

func TestZeroCapacityRingRetainsNothing(t *testing.T) {
	r := New(0, 0)
	fill(r, 3)
	if n := r.Len(t0.Add(time.Minute)); n != 0 {
		t.Errorf("Len = %d, want 0 (replay disabled)", n)
	}
	if got := r.LastN(5, t0.Add(time.Minute)); got != nil {
		t.Errorf("LastN on cap-0 ring = %v, want nil", seqs(got))
	}
}

func TestAppendAfterTTLEmptyReusesBuffer(t *testing.T) {
	// Regression guard: prune resets start when empty; appends afterwards
	// must land in order, not on a stale offset.
	r := New(3, time.Second)
	fill(r, 3)
	_ = r.Len(t0.Add(time.Hour)) // expire everything
	r.Append(Event{Seq: 10, At: t0.Add(2 * time.Hour)})
	r.Append(Event{Seq: 11, At: t0.Add(2 * time.Hour)})
	got := r.LastN(5, t0.Add(2*time.Hour))
	if len(got) != 2 || got[0].Seq != 10 || got[1].Seq != 11 {
		t.Errorf("after empty+append, retained = %v, want [10 11]", seqs(got))
	}
}

func TestEventIDFormatAndParse(t *testing.T) {
	id := FormatID("a1b2c3d4", 42)
	if id != "a1b2c3d4-42" {
		t.Fatalf("FormatID = %q", id)
	}
	epoch, seq, err := ParseID(id)
	if err != nil || epoch != "a1b2c3d4" || seq != 42 {
		t.Errorf("ParseID = (%q, %d, %v)", epoch, seq, err)
	}
	// Defensive: epochs are hex today, but the parser must not break if
	// they ever contain dashes — it splits on the LAST dash.
	epoch, seq, err = ParseID("ep-with-dashes-7")
	if err != nil || epoch != "ep-with-dashes" || seq != 7 {
		t.Errorf("ParseID = (%q, %d, %v)", epoch, seq, err)
	}
	// Garbage IDs (a hostile Last-Event-ID header) must error, not panic.
	for _, id := range []string{"", "noseq", "-5", "ep-", "ep-0", "ep-notanumber", "ep--"} {
		if _, _, err := ParseID(id); err == nil {
			t.Errorf("ParseID(%q) accepted, want error", id)
		}
	}
}
