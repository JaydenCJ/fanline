// Package hub is fanline's in-memory pub/sub core: channels, subscriber
// fanout, and replay, independent of HTTP.
//
// Design choices worth knowing:
//
//   - Fanout never blocks a publisher. Each subscriber has a bounded
//     buffer; a subscriber that falls behind is disconnected instead of
//     stalling the channel. That is safe precisely because SSE clients
//     reconnect automatically and resume via Last-Event-ID replay.
//   - Channels are created implicitly on first publish or subscribe and
//     swept when idle (no subscribers and nothing retained), so the hub
//     needs no channel management API.
//   - Clock and epoch generation are injectable, keeping every test
//     deterministic.
package hub

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/JaydenCJ/fanline/internal/channel"
	"github.com/JaydenCJ/fanline/internal/ring"
)

// ErrTooManyChannels is returned when creating one more channel would
// exceed Options.MaxChannels.
var ErrTooManyChannels = errors.New("hub: channel limit reached")

// Options tunes a Hub. Zero values select the documented defaults.
type Options struct {
	ReplayCap   int           // events retained per channel (default 64)
	ReplayTTL   time.Duration // max event age; 0 = capacity-only eviction
	MaxChannels int           // live channel limit (default 1024)
	SubBuffer   int           // per-subscriber fanout buffer (default 64)
	Now         func() time.Time
	Epoch       func() string // channel epoch generator (random by default)
}

func (o Options) withDefaults() Options {
	if o.ReplayCap == 0 {
		o.ReplayCap = 64
	}
	if o.MaxChannels <= 0 {
		o.MaxChannels = 1024
	}
	if o.SubBuffer <= 0 {
		o.SubBuffer = 64
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Epoch == nil {
		o.Epoch = randomEpoch
	}
	return o
}

func randomEpoch() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is unrecoverable; epochs only need to be
		// unlikely to repeat, so fall back to a constant-free panic-less
		// value derived from time.
		return hex.EncodeToString([]byte(time.Now().Format("150405")))[:8]
	}
	return hex.EncodeToString(b[:])
}

// Hub is safe for concurrent use.
type Hub struct {
	opts Options

	mu       sync.Mutex
	channels map[string]*chanState
}

type chanState struct {
	epoch     string
	seq       uint64
	published uint64
	ring      *ring.Ring
	subs      map[*Subscription]struct{}
}

// Subscription is one attached consumer. Read Events until it is closed;
// a closed channel means the hub dropped the subscriber (slow consumer)
// or Close was called. Replay holds the catch-up events computed at
// subscribe time, already in order.
type Subscription struct {
	Channel string
	Epoch   string
	Replay  []ring.Event
	Gap     bool // events between the client's Last-Event-ID and Replay[0] were lost

	events chan ring.Event
	hub    *Hub
	once   sync.Once
}

// Events yields live events published after the subscription was created.
func (s *Subscription) Events() <-chan ring.Event { return s.events }

// Close detaches the subscription and closes its Events channel.
func (s *Subscription) Close() { s.hub.unsubscribe(s) }

// New returns an empty hub.
func New(opts Options) *Hub {
	return &Hub{opts: opts.withDefaults(), channels: map[string]*chanState{}}
}

// Publish appends an event to name's replay ring and fans it out to every
// live subscriber. It returns the stored event and the number of
// subscribers it was delivered to (slow ones being dropped don't count).
func (h *Hub) Publish(name, eventName, data string) (ring.Event, int, error) {
	if err := channel.ValidateName(name); err != nil {
		return ring.Event{}, 0, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	cs, err := h.channelLocked(name)
	if err != nil {
		return ring.Event{}, 0, err
	}
	cs.seq++
	cs.published++
	e := ring.Event{
		Seq:   cs.seq,
		ID:    ring.FormatID(cs.epoch, cs.seq),
		Name:  eventName,
		Data:  data,
		At:    h.opts.Now(),
		Epoch: cs.epoch,
	}
	cs.ring.Append(e)

	delivered := 0
	for sub := range cs.subs {
		select {
		case sub.events <- e:
			delivered++
		default:
			// Buffer full: this consumer cannot keep up. Cut it loose —
			// its reconnect will replay from Last-Event-ID.
			delete(cs.subs, sub)
			close(sub.events)
		}
	}
	return e, delivered, nil
}

// Subscribe attaches a consumer to name. Catch-up events are chosen by,
// in priority order:
//
//  1. lastID "<epoch>-<seq>" — resume after seq (same epoch), flagging a
//     gap when evicted events were missed or the epoch changed;
//  2. replayN > 0 — replay the last N retained events;
//  3. otherwise — live events only.
func (h *Hub) Subscribe(name, lastID string, replayN int) (*Subscription, error) {
	if err := channel.ValidateName(name); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	cs, err := h.channelLocked(name)
	if err != nil {
		return nil, err
	}
	sub := &Subscription{
		Channel: name,
		Epoch:   cs.epoch,
		events:  make(chan ring.Event, h.opts.SubBuffer),
		hub:     h,
	}
	now := h.opts.Now()
	switch {
	case lastID != "":
		epoch, seq, perr := ring.ParseID(lastID)
		if perr != nil || epoch != cs.epoch {
			// Unknown or pre-restart ID: everything retained is "new" to
			// this client, and continuity cannot be proven.
			sub.Replay = cs.ring.LastN(cs.ring.Cap(), now)
			sub.Gap = true
		} else {
			sub.Replay, sub.Gap = cs.ring.Since(seq, now)
			if len(sub.Replay) == 0 && seq < cs.seq {
				// Ring emptied by TTL but the channel moved on: lost events.
				sub.Gap = true
			}
		}
	case replayN > 0:
		sub.Replay = cs.ring.LastN(replayN, now)
	}
	cs.subs[sub] = struct{}{}
	return sub, nil
}

func (h *Hub) unsubscribe(sub *Subscription) {
	sub.once.Do(func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		cs := h.channels[sub.Channel]
		if cs == nil {
			return
		}
		if _, live := cs.subs[sub]; live {
			delete(cs.subs, sub)
			close(sub.events)
		}
	})
}

// channelLocked returns (creating if needed) the state for name.
// Creation sweeps idle channels first, so a hub cycling through many
// short-lived channels never hits the limit spuriously.
func (h *Hub) channelLocked(name string) (*chanState, error) {
	if cs, ok := h.channels[name]; ok {
		return cs, nil
	}
	if len(h.channels) >= h.opts.MaxChannels {
		h.sweepLocked()
		if len(h.channels) >= h.opts.MaxChannels {
			return nil, ErrTooManyChannels
		}
	}
	cs := &chanState{
		epoch: h.opts.Epoch(),
		ring:  ring.New(h.opts.ReplayCap, h.opts.ReplayTTL),
		subs:  map[*Subscription]struct{}{},
	}
	h.channels[name] = cs
	return cs, nil
}

// sweepLocked removes channels with no subscribers and nothing retained.
func (h *Hub) sweepLocked() {
	now := h.opts.Now()
	for name, cs := range h.channels {
		if len(cs.subs) == 0 && cs.ring.Len(now) == 0 {
			delete(h.channels, name)
		}
	}
}

// ChannelStats is one row of Stats output.
type ChannelStats struct {
	Channel     string `json:"channel"`
	Epoch       string `json:"epoch"`
	Subscribers int    `json:"subscribers"`
	Published   uint64 `json:"published"`
	Retained    int    `json:"retained"`
	LastSeq     uint64 `json:"last_seq"`
}

// Stats returns a snapshot of every live channel, sorted by name.
func (h *Hub) Stats() []ChannelStats {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sweepLocked()
	now := h.opts.Now()
	out := make([]ChannelStats, 0, len(h.channels))
	for name, cs := range h.channels {
		out = append(out, ChannelStats{
			Channel:     name,
			Epoch:       cs.epoch,
			Subscribers: len(cs.subs),
			Published:   cs.published,
			Retained:    cs.ring.Len(now),
			LastSeq:     cs.seq,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Channel < out[j].Channel })
	return out
}

// SubscriberCount reports how many consumers are attached to name.
func (h *Hub) SubscriberCount(name string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cs := h.channels[name]; cs != nil {
		return len(cs.subs)
	}
	return 0
}
