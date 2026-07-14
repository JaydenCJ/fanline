// Tests for the pub/sub core: fanout, replay selection, gap and epoch
// semantics, slow-consumer eviction, channel limits, and sweeping. The
// clock and epoch generator are injected, so every case is deterministic.
package hub

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/JaydenCJ/fanline/internal/ring"
)

// clock is a manually advanced time source.
type clock struct{ now time.Time }

func (c *clock) Now() time.Time          { return c.now }
func (c *clock) Advance(d time.Duration) { c.now = c.now.Add(d) }
func newClock() *clock                   { return &clock{now: time.Unix(1_700_000_000, 0)} }
func fixedEpochs(prefix string) func() string {
	n := 0
	return func() string { n++; return fmt.Sprintf("%s%d", prefix, n) }
}

func newTestHub(opts Options) (*Hub, *clock) {
	c := newClock()
	opts.Now = c.Now
	if opts.Epoch == nil {
		opts.Epoch = fixedEpochs("ep")
	}
	return New(opts), c
}

// drain reads every buffered event from a subscription without blocking.
func drain(s *Subscription) []ring.Event {
	var out []ring.Event
	for {
		select {
		case e, ok := <-s.Events():
			if !ok {
				return out
			}
			out = append(out, e)
		default:
			return out
		}
	}
}

func TestPublishFansOutToAllSubscribers(t *testing.T) {
	h, _ := newTestHub(Options{})
	a, _ := h.Subscribe("news", "", 0)
	b, _ := h.Subscribe("news", "", 0)
	e, delivered, err := h.Publish("news", "", "hello")
	if err != nil || delivered != 2 {
		t.Fatalf("Publish = (%v, %d, %v), want delivered 2", e, delivered, err)
	}
	for i, sub := range []*Subscription{a, b} {
		got := drain(sub)
		if len(got) != 1 || got[0].Data != "hello" {
			t.Errorf("subscriber %d received %v", i, got)
		}
	}
	// A publish on an unrelated channel must not leak into these
	// subscriptions or count them as receivers.
	_, delivered2, _ := h.Publish("weather", "", "sunny")
	if delivered2 != 0 {
		t.Errorf("unrelated channel delivered to %d subscribers", delivered2)
	}
	if got := drain(a); len(got) != 0 {
		t.Errorf("news subscriber leaked weather events: %v", got)
	}
}

func TestPublishAssignsSequentialIDsPerChannel(t *testing.T) {
	h, _ := newTestHub(Options{})
	e1, _, _ := h.Publish("a", "", "1")
	e2, _, _ := h.Publish("a", "", "2")
	other, _, _ := h.Publish("b", "", "1")
	if e1.Seq != 1 || e2.Seq != 2 {
		t.Errorf("seqs = %d,%d, want 1,2", e1.Seq, e2.Seq)
	}
	if e1.ID != "ep1-1" || e2.ID != "ep1-2" {
		t.Errorf("ids = %s,%s", e1.ID, e2.ID)
	}
	if other.Seq != 1 {
		t.Errorf("channel b seq = %d, want its own counter starting at 1", other.Seq)
	}
}

func TestPublishRejectsInvalidChannelName(t *testing.T) {
	h, _ := newTestHub(Options{})
	if _, _, err := h.Publish("Bad Name", "", "x"); err == nil {
		t.Error("invalid channel name accepted")
	}
}

func TestSubscribeWithReplayNGetsRecentEvents(t *testing.T) {
	h, _ := newTestHub(Options{ReplayCap: 8})
	for i := 1; i <= 5; i++ {
		h.Publish("feed", "", fmt.Sprintf("m%d", i))
	}
	sub, err := h.Subscribe("feed", "", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(sub.Replay) != 3 || sub.Replay[0].Data != "m3" || sub.Replay[2].Data != "m5" {
		t.Errorf("Replay = %v", sub.Replay)
	}
	if sub.Gap {
		t.Error("replay-N is best-effort by definition; it must not flag a gap")
	}
}

func TestSubscribeWithLastIDResumesExactly(t *testing.T) {
	h, _ := newTestHub(Options{ReplayCap: 8})
	var second ring.Event
	for i := 1; i <= 4; i++ {
		e, _, _ := h.Publish("feed", "", fmt.Sprintf("m%d", i))
		if i == 2 {
			second = e
		}
	}
	sub, _ := h.Subscribe("feed", second.ID, 0)
	if len(sub.Replay) != 2 || sub.Replay[0].Data != "m3" || sub.Replay[1].Data != "m4" {
		t.Errorf("Replay after %s = %v", second.ID, sub.Replay)
	}
	if sub.Gap {
		t.Error("contiguous resume must not flag a gap")
	}
}

func TestSubscribeLastIDBeyondRetentionFlagsGap(t *testing.T) {
	h, _ := newTestHub(Options{ReplayCap: 2})
	first, _, _ := h.Publish("feed", "", "m1")
	h.Publish("feed", "", "m2")
	h.Publish("feed", "", "m3")
	h.Publish("feed", "", "m4") // ring now holds only m3, m4
	sub, _ := h.Subscribe("feed", first.ID, 0)
	if !sub.Gap {
		t.Error("m2 was evicted; the client must learn it missed events")
	}
	if len(sub.Replay) != 2 || sub.Replay[0].Data != "m3" {
		t.Errorf("Replay = %v, want [m3 m4]", sub.Replay)
	}
}

func TestSubscribeLastIDFromOldEpochFlagsGap(t *testing.T) {
	// A Last-Event-ID minted before a hub restart must not silently
	// resume against the new sequence — same numbers, different history.
	h, _ := newTestHub(Options{ReplayCap: 8})
	h.Publish("feed", "", "new1")
	h.Publish("feed", "", "new2")
	sub, _ := h.Subscribe("feed", "oldepoch-2", 0)
	if !sub.Gap {
		t.Error("epoch mismatch must flag a gap")
	}
	if len(sub.Replay) != 2 {
		t.Errorf("Replay = %v, want everything retained", sub.Replay)
	}
	// A malformed Last-Event-ID (hostile or corrupted client state) gets
	// the same conservative treatment: gap + full replay, never a panic.
	sub, _ = h.Subscribe("feed", "not-a-real-id-###", 0)
	if !sub.Gap || len(sub.Replay) != 2 {
		t.Errorf("malformed id: gap=%v replay=%d, want gap with full replay", sub.Gap, len(sub.Replay))
	}
}

func TestSubscribeLastIDAfterTTLExpiryFlagsGap(t *testing.T) {
	h, c := newTestHub(Options{ReplayCap: 8, ReplayTTL: 10 * time.Second})
	first, _, _ := h.Publish("feed", "", "m1")
	h.Publish("feed", "", "m2")
	c.Advance(time.Hour) // everything expires; channel seq is still 2
	sub, _ := h.Subscribe("feed", first.ID, 0)
	if !sub.Gap {
		t.Error("client at seq 1 missed the now-expired m2; gap must be true")
	}
	if len(sub.Replay) != 0 {
		t.Errorf("Replay = %v, want none (all expired)", sub.Replay)
	}
}

func TestSubscribeCaughtUpClientSeesNoGap(t *testing.T) {
	h, c := newTestHub(Options{ReplayCap: 8, ReplayTTL: 10 * time.Second})
	h.Publish("feed", "", "m1")
	last, _, _ := h.Publish("feed", "", "m2")
	c.Advance(time.Hour) // ring empties, but the client saw everything
	sub, _ := h.Subscribe("feed", last.ID, 0)
	if sub.Gap || len(sub.Replay) != 0 {
		t.Errorf("gap=%v replay=%v, want clean live-only resume", sub.Gap, sub.Replay)
	}
}

func TestSlowSubscriberIsDroppedNotBlocked(t *testing.T) {
	h, _ := newTestHub(Options{SubBuffer: 2})
	slow, _ := h.Subscribe("firehose", "", 0)
	fast, _ := h.Subscribe("firehose", "", 0)
	// Fill slow's buffer (2) while fast keeps consuming, then publish one
	// more: slow must be dropped, and the publish itself must not block.
	for i := 0; i < 2; i++ {
		h.Publish("firehose", "", "x")
		drain(fast)
	}
	_, delivered, _ := h.Publish("firehose", "", "overflow")
	if delivered != 1 {
		t.Errorf("delivered = %d, want 1 (only the fast subscriber)", delivered)
	}
	got := drain(slow)
	if len(got) != 2 {
		t.Errorf("slow drained %d events, want the 2 buffered before the drop", len(got))
	}
	if _, open := <-slow.Events(); open {
		t.Error("slow subscriber's channel must be closed after the drop")
	}
	if h.SubscriberCount("firehose") != 1 {
		t.Errorf("SubscriberCount = %d, want 1", h.SubscriberCount("firehose"))
	}
	_ = fast
}

func TestCloseIsIdempotentAndDetaches(t *testing.T) {
	h, _ := newTestHub(Options{})
	sub, _ := h.Subscribe("a", "", 0)
	sub.Close()
	sub.Close() // second close must not panic (double-close of the chan)
	if h.SubscriberCount("a") != 0 {
		t.Errorf("SubscriberCount = %d after Close", h.SubscriberCount("a"))
	}
	if _, _, err := h.Publish("a", "", "x"); err != nil {
		t.Errorf("publish after unsubscribe: %v", err)
	}
}

func TestMaxChannelsEnforcedAfterSweep(t *testing.T) {
	h, _ := newTestHub(Options{MaxChannels: 2, ReplayCap: 4})
	h.Publish("a", "", "x") // retained event keeps "a" alive
	sub, _ := h.Subscribe("b", "", 0)
	if _, _, err := h.Publish("c", "", "x"); !errors.Is(err, ErrTooManyChannels) {
		t.Fatalf("third channel: got %v, want ErrTooManyChannels", err)
	}
	// Freeing "b" (no subs, empty ring) makes room: the sweep runs
	// automatically inside the next create.
	sub.Close()
	if _, _, err := h.Publish("c", "", "x"); err != nil {
		t.Errorf("after freeing b: %v", err)
	}
}

func TestStatsReportsChannelsSorted(t *testing.T) {
	h, _ := newTestHub(Options{ReplayCap: 4})
	h.Publish("zeta", "", "1")
	h.Publish("alpha", "", "1")
	h.Publish("alpha", "", "2")
	sub, _ := h.Subscribe("alpha", "", 0)
	defer sub.Close()
	stats := h.Stats()
	if len(stats) != 2 || stats[0].Channel != "alpha" || stats[1].Channel != "zeta" {
		t.Fatalf("stats = %+v", stats)
	}
	a := stats[0]
	if a.Published != 2 || a.Retained != 2 || a.Subscribers != 1 || a.LastSeq != 2 {
		t.Errorf("alpha stats = %+v", a)
	}
}

func TestNewChannelAfterSweepGetsFreshEpoch(t *testing.T) {
	// If a channel is swept and recreated, its epoch must differ so
	// stale Last-Event-IDs are detected as gaps, not resumed blindly.
	h, c := newTestHub(Options{ReplayCap: 4, ReplayTTL: time.Second})
	e1, _, _ := h.Publish("chan", "", "x")
	c.Advance(time.Hour) // retained event expires; no subscribers
	if stats := h.Stats(); len(stats) != 0 {
		t.Fatalf("stats = %+v, want idle channel swept", stats)
	}
	e2, _, _ := h.Publish("chan", "", "y")
	if e1.Epoch == e2.Epoch {
		t.Errorf("recreated channel reused epoch %q", e1.Epoch)
	}
	if e2.Seq != 1 {
		t.Errorf("recreated channel seq = %d, want 1", e2.Seq)
	}
}
