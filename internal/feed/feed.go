// Package feed is the live update stream Conn subscribes to, sent as
// Server-Sent Events.
//
// It carries three kinds of event:
//
//	depths  what is waiting in each lane, and how many items are paused
//	health  the latest health report for one component
//	edit    an edit proposal's change of status
//
// Updates go out in one batch on every tick, never one at a time. Each
// batch starts with a depths event, so a subscriber hears from Muster on
// every tick whether or not anything changed, and every event carries
// the time it was produced. A subscriber that hears nothing for longer
// than a few ticks should treat the feed as stale.
//
// A new subscriber is first sent the current depths and the latest
// health of every component, then batches as they happen.
//
// Memory is bounded: a fixed number of subscribers, a fixed queue for
// each, and a fixed number of pending events. A subscriber that falls
// behind is disconnected and can reconnect for a fresh picture.
package feed

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/ClearedStack/muster/internal/lifecycle"
	"github.com/ClearedStack/muster/internal/logbook"
	"github.com/ClearedStack/muster/internal/queue"
)

// Limits.
const (
	MaxSubscribers = 8
	QueuedFrames   = 64
	MaxPendingEdit = 256
)

// ErrFull means no more subscribers can be taken.
var ErrFull = errors.New("feed: no room for another subscriber")

// Hub collects events and sends them to subscribers.
type Hub struct {
	Store   logbook.Store
	Clock   func() time.Time
	Timeout time.Duration // how long a read of the records may take

	mu     sync.Mutex
	closed bool
	subs   map[*Subscriber]struct{}
	nextID uint64
	health map[string]healthEvent
	edits  []editEvent
}

// Subscriber is one open stream. Frames arrive on C, each a complete
// event ready to be written. C is closed when the hub lets go of the
// subscriber.
type Subscriber struct {
	C chan []byte
}

type depthsEvent struct {
	At           string `json:"at"`
	Urgent       int    `json:"urgent"`
	Normal       int    `json:"normal"`
	Backfill     int    `json:"backfill"`
	NeedsSubtool int    `json:"needs_subtool"`
}

type healthEvent struct {
	At        string `json:"at"`
	Component string `json:"component"`
	State     string `json:"state"`
	Code      string `json:"code,omitempty"`
}

type editEvent struct {
	At     string          `json:"at"`
	ID     string          `json:"id"`
	Status lifecycle.State `json:"status"`
}

// Health notes a health report for the next batch. A later report for
// the same component replaces an earlier one that has not gone out yet.
func (h *Hub) Health(component, state, code string, at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.health == nil {
		h.health = map[string]healthEvent{}
	}
	h.health[component] = healthEvent{At: queue.Stamp(at), Component: component, State: state, Code: code}
}

// Edit notes a proposal's change of status for the next batch. If more
// changes are waiting than the hub may hold, every subscriber is
// disconnected, because the stream they would get is no longer complete.
func (h *Hub) Edit(id string, status lifecycle.State, at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.edits) >= MaxPendingEdit {
		h.edits = nil
		h.dropAll()
		return
	}
	h.edits = append(h.edits, editEvent{At: queue.Stamp(at), ID: id, Status: status})
}

// Subscribe opens a stream, starting with the current picture.
func (h *Hub) Subscribe(ctx context.Context) (*Subscriber, error) {
	now := h.Clock()
	var depths logbook.Depths
	var components []logbook.Component
	if err := h.view(ctx, func(tx logbook.Tx) error {
		depths = tx.Depths(now)
		components = tx.Components()
		return nil
	}); err != nil {
		return nil, err
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || len(h.subs) >= MaxSubscribers {
		return nil, ErrFull
	}
	sub := &Subscriber{C: make(chan []byte, QueuedFrames)}
	sub.C <- h.frame("depths", depthsOf(depths, now))
	for _, c := range components {
		sub.C <- h.frame("health", healthEvent{At: queue.Stamp(c.At), Component: c.Slug, State: c.State, Code: c.Code})
	}
	if h.subs == nil {
		h.subs = map[*Subscriber]struct{}{}
	}
	h.subs[sub] = struct{}{}
	return sub, nil
}

// Unsubscribe lets go of a subscriber.
func (h *Hub) Unsubscribe(sub *Subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.drop(sub)
}

// Tick sends one batch: the depths, then pending health events in order
// of component name, then pending edit events in the order they
// happened. If the records cannot be read in time nothing is sent and
// pending events wait for the next tick; a feed that says nothing is
// then correctly seen as stale.
func (h *Hub) Tick(ctx context.Context) {
	now := h.Clock()
	var depths logbook.Depths
	if err := h.view(ctx, func(tx logbook.Tx) error {
		depths = tx.Depths(now)
		return nil
	}); err != nil {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	health, edits := h.health, h.edits
	h.health, h.edits = nil, nil
	if len(h.subs) == 0 {
		return
	}
	frames := make([][]byte, 0, 1+len(health)+len(edits))
	frames = append(frames, h.frame("depths", depthsOf(depths, now)))
	names := make([]string, 0, len(health))
	for name := range health {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		frames = append(frames, h.frame("health", health[name]))
	}
	for _, e := range edits {
		frames = append(frames, h.frame("edit", e))
	}
	for sub := range h.subs {
		if cap(sub.C)-len(sub.C) < len(frames) {
			h.drop(sub)
			continue
		}
		for _, f := range frames {
			sub.C <- f
		}
	}
}

// Run sends a batch on every tick until the context ends, then lets go
// of every subscriber and takes no more.
func (h *Hub) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			h.mu.Lock()
			h.closed = true
			h.dropAll()
			h.mu.Unlock()
			return
		case <-t.C:
			h.Tick(ctx)
		}
	}
}

func (h *Hub) view(ctx context.Context, fn func(logbook.Tx) error) error {
	if h.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, h.Timeout)
		defer cancel()
	}
	return h.Store.View(ctx, fn)
}

func (h *Hub) drop(sub *Subscriber) {
	if _, ok := h.subs[sub]; ok {
		delete(h.subs, sub)
		close(sub.C)
	}
}

func (h *Hub) dropAll() {
	for sub := range h.subs {
		h.drop(sub)
	}
}

// frame renders one event. The caller holds the lock.
func (h *Hub) frame(event string, data any) []byte {
	h.nextID++
	body, err := json.Marshal(data)
	if err != nil {
		// The event types hold only strings and integers, so this
		// cannot happen. If it ever does, send nothing misleading.
		body = []byte("{}")
	}
	out := make([]byte, 0, len(body)+48)
	out = append(out, "id: "...)
	out = strconv.AppendUint(out, h.nextID, 10)
	out = append(out, "\nevent: "...)
	out = append(out, event...)
	out = append(out, "\ndata: "...)
	out = append(out, body...)
	out = append(out, "\n\n"...)
	return out
}

func depthsOf(d logbook.Depths, at time.Time) depthsEvent {
	return depthsEvent{
		At: queue.Stamp(at), Urgent: d.Urgent, Normal: d.Normal,
		Backfill: d.Backfill, NeedsSubtool: d.NeedsSubtool,
	}
}
