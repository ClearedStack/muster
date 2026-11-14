package feed

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ClearedStack/muster/internal/lifecycle"
	"github.com/ClearedStack/muster/internal/logbook"
	"github.com/ClearedStack/muster/internal/logbook/memory"
)

var update = flag.Bool("update", false, "rewrite the saved stream")

var ctx = context.Background()

type clock struct{ t time.Time }

func (c *clock) Now() time.Time { return c.t }

func setup(t *testing.T) (*Hub, *memory.Store, *clock) {
	t.Helper()
	store := memory.New()
	seed, err := memory.ParseSeed([]byte(`{"items":[
		{"id":"PA-0001","state":"recorded","lane":"urgent","next":"sextant"},
		{"id":"PA-0002","state":"recorded","lane":"normal","next":"sextant"},
		{"id":"PA-0003","state":"observed","lane":"normal","next":"assay"},
		{"id":"PA-0004","state":"discovered","lane":"backfill","next":"trawl"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Load(seed); err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)}
	return &Hub{Store: store, Clock: clk.Now, Timeout: time.Second}, store, clk
}

func drain(sub *Subscriber) []byte {
	var out []byte
	for {
		select {
		case f, ok := <-sub.C:
			if !ok {
				return out
			}
			out = append(out, f...)
		default:
			return out
		}
	}
}

// The saved stream: exactly the bytes a subscriber is sent over one
// short story. It opens with the current picture, then one batch per
// tick, each led by the depths.
func TestSavedStream(t *testing.T) {
	hub, store, clk := setup(t)
	if err := store.Update(ctx, func(tx logbook.Tx) error {
		tx.PutComponent(logbook.Component{Slug: "sextant", State: "healthy", At: clk.t.Add(-time.Minute)})
		tx.PutComponent(logbook.Component{Slug: "assay", State: "degraded", Code: "E_TIMEOUT", At: clk.t.Add(-2 * time.Minute)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sub, err := hub.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	got = append(got, drain(sub)...)

	// A quiet tick still says something: the depths, with the time.
	clk.t = clk.t.Add(5 * time.Second)
	hub.Tick(ctx)
	got = append(got, drain(sub)...)

	// Between ticks: an item is paused, two health reports arrive for
	// one component, one for another, and a proposal changes status
	// twice.
	if err := store.Update(ctx, func(tx logbook.Tx) error {
		it, _ := tx.Item("PA-0002")
		it.State, it.PausedBy = lifecycle.NeedsSubtool, "sextant"
		tx.PutItem(it)
		tx.DeleteEntry("PA-0002")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Second)
	hub.Health("trawl", "idle", "", clk.t)
	hub.Edit("EP-0001", lifecycle.PendingApproval, clk.t)
	clk.t = clk.t.Add(time.Second)
	hub.Health("sextant", "degraded", "E_PARSE", clk.t)
	clk.t = clk.t.Add(time.Second)
	hub.Health("sextant", "paused", "E_NEEDS_SUBTOOL", clk.t)
	hub.Edit("EP-0001", lifecycle.Approved, clk.t)
	clk.t = clk.t.Add(2 * time.Second)
	hub.Tick(ctx)
	got = append(got, drain(sub)...)

	// Nothing is sent twice.
	clk.t = clk.t.Add(5 * time.Second)
	hub.Tick(ctx)
	got = append(got, drain(sub)...)

	const path = "testdata/stream.txt"
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the stream no longer matches %s\n got:\n%s", path, got)
	}
}

func TestSubscriberLimit(t *testing.T) {
	hub, _, _ := setup(t)
	var subs []*Subscriber
	for i := 0; i < MaxSubscribers; i++ {
		sub, err := hub.Subscribe(ctx)
		if err != nil {
			t.Fatalf("subscriber %d: %v", i+1, err)
		}
		subs = append(subs, sub)
	}
	if _, err := hub.Subscribe(ctx); err != ErrFull {
		t.Fatalf("one subscriber too many: %v", err)
	}
	hub.Unsubscribe(subs[0])
	if _, err := hub.Subscribe(ctx); err != nil {
		t.Fatalf("after one left: %v", err)
	}
	hub.Unsubscribe(subs[0]) // letting go twice is harmless
}

// A subscriber that does not read is let go once its queue cannot take
// a whole batch. The others are not held up.
func TestSlowSubscriberIsLetGo(t *testing.T) {
	hub, _, clk := setup(t)
	slow, _ := hub.Subscribe(ctx)
	quick, _ := hub.Subscribe(ctx)
	for i := 0; i < QueuedFrames+5; i++ {
		clk.t = clk.t.Add(5 * time.Second)
		hub.Tick(ctx)
		drain(quick)
	}
	frames, open := 0, true
	for open {
		select {
		case _, ok := <-slow.C:
			if !ok {
				open = false
			} else {
				frames++
			}
		default:
			t.Fatal("the slow subscriber was never let go")
		}
	}
	if frames > QueuedFrames {
		t.Errorf("the slow subscriber was sent %d frames", frames)
	}
	clk.t = clk.t.Add(5 * time.Second)
	hub.Tick(ctx)
	if len(drain(quick)) == 0 {
		t.Error("the quick subscriber stopped hearing")
	}
}

// More pending changes than the hub may hold cannot be sent whole, so
// every subscriber is let go to start again from a fresh picture.
func TestTooManyPendingEdits(t *testing.T) {
	hub, _, clk := setup(t)
	sub, _ := hub.Subscribe(ctx)
	drain(sub)
	for i := 0; i <= MaxPendingEdit; i++ {
		hub.Edit(fmt.Sprintf("EP-%04d", i+1), lifecycle.PendingApproval, clk.t)
	}
	if _, ok := <-sub.C; ok {
		t.Error("the subscriber was kept on an incomplete stream")
	}
}

type slowStore struct{ logbook.Store }

func (slowStore) View(context.Context, func(logbook.Tx) error) error {
	return context.DeadlineExceeded
}

// When the records cannot be read in time the feed says nothing, which
// a subscriber correctly sees as stale; what was pending goes out on
// the next tick that works.
func TestSilentWhenTheStoreIsSlow(t *testing.T) {
	hub, store, clk := setup(t)
	sub, _ := hub.Subscribe(ctx)
	drain(sub)
	hub.Health("sextant", "healthy", "", clk.t)

	hub.Store = slowStore{store}
	hub.Tick(ctx)
	if out := drain(sub); len(out) != 0 {
		t.Errorf("sent %q while the store was slow", out)
	}
	if _, err := hub.Subscribe(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a new subscriber: %v", err)
	}

	hub.Store = store
	hub.Tick(ctx)
	if out := drain(sub); !bytes.Contains(out, []byte(`"component":"sextant"`)) {
		t.Errorf("the pending report was lost: %q", out)
	}
}

func TestRunStops(t *testing.T) {
	hub, _, _ := setup(t)
	sub, _ := hub.Subscribe(ctx)
	drain(sub)
	running, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { hub.Run(running, time.Hour); close(done) }()
	stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	if _, ok := <-sub.C; ok {
		t.Error("a subscriber was kept after the feed stopped")
	}
	if _, err := hub.Subscribe(ctx); err != ErrFull {
		t.Errorf("a subscriber was taken after the feed stopped: %v", err)
	}
}
