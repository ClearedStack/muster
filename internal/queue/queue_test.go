package queue_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ClearedStack/muster/internal/contract"
	"github.com/ClearedStack/muster/internal/lifecycle"
	"github.com/ClearedStack/muster/internal/logbook"
	"github.com/ClearedStack/muster/internal/logbook/memory"
	"github.com/ClearedStack/muster/internal/queue"
)

var update = flag.Bool("update", false, "rewrite the saved ledger")

var ctx = context.Background()

type clock struct{ t time.Time }

func (c *clock) Now() time.Time { return c.t }

func setup(t *testing.T, seed string) (*queue.Service, *memory.Store, *clock) {
	t.Helper()
	store := memory.New()
	s, err := memory.ParseSeed([]byte(seed))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Load(s); err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)}
	return &queue.Service{Store: store, Clock: clk.Now, LeaseFor: time.Minute}, store, clk
}

func must[T any](v T, err error) func(*testing.T) T {
	return func(t *testing.T) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

// Every change of state and every refusal to carry on is written to the
// ledger. This is the saved ledger of one short story that uses each
// kind of line: lease, complete, fail, needs_subtool and replay.
func TestSavedLedger(t *testing.T) {
	q, store, clk := setup(t, `{"items":[
		{"id":"PA-0001","state":"recorded","lane":"normal","next":"sextant"},
		{"id":"PA-0002","state":"recorded","lane":"normal","next":"sextant"},
		{"id":"PA-0003","state":"recorded","lane":"normal","next":"sextant"}]}`)
	step := func() { clk.t = clk.t.Add(10 * time.Second) }

	g := must(q.Lease(ctx, "sextant"))(t)
	step()
	must(q.Complete(ctx, "sextant", g.Lease, lifecycle.Observed, json.RawMessage(`{"job":"made-up job"}`)))(t)
	step()
	g = must(q.Lease(ctx, "sextant"))(t)
	step()
	must(q.Fail(ctx, "sextant", g.Lease, contract.Timeout))(t)
	step()
	g = must(q.Lease(ctx, "sextant"))(t)
	step()
	must(q.Fail(ctx, "sextant", g.Lease, contract.Parse))(t)
	step()
	g = must(q.Lease(ctx, "sextant"))(t)
	step()
	must(q.NeedsSubtool(ctx, "sextant", g.Lease))(t)
	step()
	if err := store.Update(ctx, func(tx logbook.Tx) error {
		if n := queue.Replay(tx, "sextant", clk.Now()); n != 1 {
			t.Errorf("replayed %d items, want 1", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	type line struct {
		Seq     uint64 `json:"seq"`
		At      string `json:"at"`
		Actor   string `json:"actor"`
		Action  string `json:"action"`
		Subject string `json:"subject"`
		From    string `json:"from"`
		To      string `json:"to"`
		Code    string `json:"code,omitempty"`
	}
	var lines []line
	for _, r := range store.Ledger() {
		lines = append(lines, line{r.Seq, queue.Stamp(r.At), r.Actor, r.Action, r.Subject, string(r.From), string(r.To), r.Code})
	}
	var buf bytes.Buffer
	for _, l := range lines {
		b, _ := json.Marshal(l)
		buf.Write(b)
		buf.WriteByte('\n')
	}
	const path = "testdata/ledger.jsonl"
	if *update {
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("the ledger no longer matches %s\n got:\n%s", path, buf.Bytes())
	}
}

// failing is a Logbook whose transactions run and are then lost, as when
// the record store fails at the last moment.
type failing struct {
	logbook.Store
	err error
}

func (f failing) Update(ctx context.Context, fn func(logbook.Tx) error) error {
	return f.Store.Update(ctx, func(tx logbook.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		return f.err
	})
}

// A change of state and the next queue step happen together or not at
// all. When the transaction is lost, the worker is told nothing was
// done, and nothing was.
func TestLostTransactionChangesNothing(t *testing.T) {
	q, store, _ := setup(t, `{"items":[{"id":"PA-0001","state":"recorded","lane":"normal","next":"sextant"}]}`)
	g := must(q.Lease(ctx, "sextant"))(t)

	lost := errors.New("made-up failure")
	broken := *q
	broken.Store = failing{store, lost}
	if _, err := broken.Complete(ctx, "sextant", g.Lease, lifecycle.Observed, json.RawMessage(`{}`)); err != lost {
		t.Fatalf("Complete returned %v", err)
	}
	if _, err := broken.NeedsSubtool(ctx, "sextant", g.Lease); err != lost {
		t.Fatalf("NeedsSubtool returned %v", err)
	}
	if _, err := broken.Fail(ctx, "sextant", g.Lease, contract.Parse); err != lost {
		t.Fatalf("Fail returned %v", err)
	}
	if _, err := broken.Lease(ctx, "assay"); err != lost {
		t.Fatalf("Lease returned %v", err)
	}

	if err := store.View(ctx, func(tx logbook.Tx) error {
		it, _ := tx.Item("PA-0001")
		if it.State != lifecycle.Recorded || it.Results != nil || it.Done.Lease != "" {
			t.Errorf("the item changed: %+v", it)
		}
		e, ok := tx.Entry("PA-0001")
		if !ok || e.Worker != "sextant" || e.Lease != g.Lease || e.Held {
			t.Errorf("the queue entry changed: %+v", e)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n := len(store.Ledger()); n != 1 {
		t.Errorf("the ledger has %d lines, want the one lease", n)
	}
	// The same call, once the store works again, goes through.
	ack := must(q.Complete(ctx, "sextant", g.Lease, lifecycle.Observed, json.RawMessage(`{}`)))(t)
	if ack.State != lifecycle.Observed || ack.Next != "assay" {
		t.Errorf("ack %+v", ack)
	}
}

// A queue entry that says a worker is next for a subject it has no step
// for is not handed out and not guessed at. It is set aside, the worker
// is told plainly, and the rest of the queue carries on.
func TestInconsistentEntryIsSetAside(t *testing.T) {
	q, store, clk := setup(t, `{"items":[{"id":"PA-0002","state":"recorded","lane":"normal","next":"sextant"}]}`)
	if err := store.Update(ctx, func(tx logbook.Tx) error {
		// Live is not a state Sextant has anything to do with.
		tx.PutItem(logbook.Item{ID: "PA-0001", State: lifecycle.Live, Lane: lifecycle.Urgent})
		tx.PutEntry(logbook.Entry{Key: "PA-0001", Subject: "PA-0001", Kind: lifecycle.KindItem, Worker: "sextant", Lane: lifecycle.Urgent, Arrival: 99})
		// And this entry is about an item that does not exist.
		tx.PutEntry(logbook.Entry{Key: "PA-0003", Subject: "PA-0003", Kind: lifecycle.KindItem, Worker: "sextant", Lane: lifecycle.Urgent, Arrival: 100})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if g, err := q.Lease(ctx, "sextant"); g != nil || err != contract.BadRecord {
			t.Fatalf("lease %d: got %+v, %v", i+1, g, err)
		}
	}
	g := must(q.Lease(ctx, "sextant"))(t)
	if g == nil || g.ID != "PA-0002" {
		t.Fatalf("the good item was not offered next: %+v", g)
	}
	clk.t = clk.t.Add(24 * time.Hour)
	if err := store.View(ctx, func(tx logbook.Tx) error {
		for _, id := range []string{"PA-0001", "PA-0003"} {
			if e, _ := tx.Entry(id); !e.Held || e.Lease != "" {
				t.Errorf("%s was not set aside: %+v", id, e)
			}
		}
		if it, _ := tx.Item("PA-0001"); it.State != lifecycle.Live {
			t.Errorf("the item's state was changed: %+v", it)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var setAside int
	for _, r := range store.Ledger() {
		if r.Action == logbook.ActSetAside && r.Code == string(contract.Parse) {
			setAside++
		}
	}
	if setAside != 2 {
		t.Errorf("the ledger records %d set-aside entries, want 2", setAside)
	}
	// Bellbuoy is told about the item that exists, and is not sent after
	// one that does not.
	inc := must(q.Lease(ctx, "bellbuoy"))(t)
	if inc == nil || inc.Kind != lifecycle.KindIncident || *inc.Incident != (queue.IncidentView{ID: "PA-0001", Worker: "sextant", Code: "E_PARSE", State: lifecycle.Live}) {
		t.Errorf("the incident: %+v", inc)
	}
	if more := must(q.Lease(ctx, "bellbuoy"))(t); more != nil {
		t.Errorf("a second incident: %+v", more)
	}
}

// An item has at most one incident step. If it is stopped again before
// Bellbuoy has dealt with the first, the newer incident takes its place.
func TestOneIncidentPerItem(t *testing.T) {
	q, store, _ := setup(t, `{"items":[{"id":"PA-0001","state":"recorded","lane":"normal","next":"sextant"}]}`)
	g := must(q.Lease(ctx, "sextant"))(t)
	must(q.NeedsSubtool(ctx, "sextant", g.Lease))(t)
	if err := store.Update(ctx, func(tx logbook.Tx) error {
		queue.Replay(tx, "sextant", q.Clock())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	g = must(q.Lease(ctx, "sextant"))(t)
	must(q.Fail(ctx, "sextant", g.Lease, contract.Parse))(t)

	inc := must(q.Lease(ctx, "bellbuoy"))(t)
	if inc == nil || inc.Incident.Code != "E_PARSE" || inc.Incident.State != lifecycle.Recorded {
		t.Fatalf("the incident: %+v", inc)
	}
	if more := must(q.Lease(ctx, "bellbuoy"))(t); more != nil {
		t.Errorf("a second incident for one item: %+v", more)
	}
	// The incident step and the item's own step end separately, and a
	// repeat of either call still gets its own answer.
	must(q.Complete(ctx, "bellbuoy", inc.Lease, lifecycle.Recorded, nil))(t)
	again := must(q.Fail(ctx, "sextant", g.Lease, contract.Parse))(t)
	if !again.Held {
		t.Errorf("the repeated fail: %+v", again)
	}
	done := must(q.Complete(ctx, "bellbuoy", inc.Lease, lifecycle.Recorded, nil))(t)
	if done.ID != "PA-0001" || done.State != lifecycle.Recorded {
		t.Errorf("the repeated complete: %+v", done)
	}
}

// The owner's decision to release puts a stopped item at the end of the
// urgent lane, in the state it was stopped in, for the worker it was
// stopped at.
func TestReleaseJoinsTheEndOfTheUrgentLane(t *testing.T) {
	q, _, _ := setup(t, `{"items":[
		{"id":"PA-0001","state":"recorded","lane":"backfill","next":"sextant"},
		{"id":"PA-0002","state":"recorded","lane":"urgent","next":"sextant"},
		{"id":"PA-0003","state":"recorded","lane":"normal","next":"sextant"}]}`)
	if g := must(q.Lease(ctx, "countersign"))(t); g != nil {
		t.Fatalf("a release step with nothing stopped: %+v", g)
	}
	// Stop the backfill item: take the two ahead of it out of the way
	// first, then put them back.
	urgent := must(q.Lease(ctx, "sextant"))(t)
	normal := must(q.Lease(ctx, "sextant"))(t)
	g := must(q.Lease(ctx, "sextant"))(t)
	must(q.Fail(ctx, "sextant", g.Lease, contract.Parse))(t)
	must(q.Fail(ctx, "sextant", urgent.Lease, contract.Timeout))(t)
	must(q.Fail(ctx, "sextant", normal.Lease, contract.Timeout))(t)

	rel := must(q.Lease(ctx, "countersign"))(t)
	if rel == nil || rel.Kind != lifecycle.KindRelease || rel.ID != "PA-0001" || rel.State != lifecycle.Recorded ||
		string(rel.Results["held"]) != `{"worker":"sextant","code":"E_PARSE"}` {
		t.Fatalf("the release step: %+v", rel)
	}
	release := json.RawMessage(`{"decision":"release"}`)
	// What a release step refuses.
	refused := []struct {
		name   string
		state  lifecycle.State
		result json.RawMessage
		want   error
	}{
		{"another state", lifecycle.Observed, release, contract.MoveNotAllowed},
		{"no result", lifecycle.Recorded, nil, contract.MissingField},
		{"no decision", lifecycle.Recorded, json.RawMessage(`{}`), contract.BadField},
		{"a decision that does not exist", lifecycle.Recorded, json.RawMessage(`{"decision":"retry"}`), contract.BadField},
		{"a decision of the wrong type", lifecycle.Recorded, json.RawMessage(`{"decision":true}`), contract.BadField},
		{"another field", lifecycle.Recorded, json.RawMessage(`{"decision":"release","lane":"normal"}`), contract.BadField},
	}
	for _, c := range refused {
		if _, err := q.Complete(ctx, "countersign", rel.Lease, c.state, c.result); err != c.want {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	if _, err := q.NeedsSubtool(ctx, "countersign", rel.Lease); err != contract.MoveNotAllowed {
		t.Errorf("pausing a release step: %v", err)
	}
	if _, err := q.Complete(ctx, "sextant", rel.Lease, lifecycle.Recorded, release); err != contract.LeaseNotHeld {
		t.Errorf("another worker deciding: %v", err)
	}

	ack := must(q.Complete(ctx, "countersign", rel.Lease, lifecycle.Recorded, release))(t)
	if ack.ID != "PA-0001" || ack.State != lifecycle.Recorded || ack.Next != "sextant" || ack.Held {
		t.Errorf("the release: %+v", ack)
	}
	// The same call again gets the same answer; the other decision under
	// the same lease does not.
	again := must(q.Complete(ctx, "countersign", rel.Lease, lifecycle.Recorded, release))(t)
	if *again != *ack {
		t.Errorf("the repeated release: %+v", again)
	}
	if _, err := q.Complete(ctx, "countersign", rel.Lease, lifecycle.Recorded, json.RawMessage(`{"decision":"keep"}`)); err != contract.LeaseNotHeld {
		t.Errorf("a different decision under the same lease: %v", err)
	}
	if g := must(q.Lease(ctx, "countersign"))(t); g != nil {
		t.Errorf("a second release step: %+v", g)
	}

	var order []string
	for {
		g := must(q.Lease(ctx, "sextant"))(t)
		if g == nil {
			break
		}
		order = append(order, g.ID+" "+string(g.Lane))
	}
	want := "[PA-0002 urgent PA-0001 urgent PA-0003 normal]"
	if got := fmt.Sprint(order); got != want {
		t.Errorf("order after release %s, want %s", got, want)
	}
}

// The owner's decision to keep leaves the item held. The release step
// ends, and nothing offers the item again.
func TestKeepLeavesTheItemHeld(t *testing.T) {
	q, store, clk := setup(t, `{"items":[{"id":"PA-0001","state":"recorded","lane":"normal","next":"sextant"}]}`)
	g := must(q.Lease(ctx, "sextant"))(t)
	must(q.Fail(ctx, "sextant", g.Lease, contract.Parse))(t)
	rel := must(q.Lease(ctx, "countersign"))(t)
	ack := must(q.Complete(ctx, "countersign", rel.Lease, lifecycle.Recorded, json.RawMessage(`{"decision":"keep"}`)))(t)
	if ack.ID != "PA-0001" || ack.State != lifecycle.Recorded || ack.Next != "" || !ack.Held {
		t.Errorf("the answer: %+v", ack)
	}
	clk.t = clk.t.Add(30 * 24 * time.Hour)
	for _, worker := range []string{"sextant", "countersign"} {
		if g := must(q.Lease(ctx, worker))(t); g != nil {
			t.Errorf("%s was offered %+v", worker, g)
		}
	}
	if err := store.View(ctx, func(tx logbook.Tx) error {
		if e, _ := tx.Entry("PA-0001"); !e.Held {
			t.Errorf("the item's step: %+v", e)
		}
		if it, _ := tx.Item("PA-0001"); it.State != lifecycle.Recorded || it.Results["countersign"] != nil {
			t.Errorf("the item: %+v", it)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var kept int
	for _, r := range store.Ledger() {
		if r.Action == logbook.ActKeep && r.Actor == "countersign" && r.Subject == "PA-0001" {
			kept++
		}
	}
	if kept != 1 {
		t.Errorf("the ledger records %d keep decisions, want 1", kept)
	}
}

// A paused item gets an incident step and no release step: it runs
// again by itself when its worker is redeployed.
func TestPausedItemHasNoReleaseStep(t *testing.T) {
	q, _, _ := setup(t, `{"items":[{"id":"PA-0001","state":"recorded","lane":"normal","next":"sextant"}]}`)
	g := must(q.Lease(ctx, "sextant"))(t)
	must(q.NeedsSubtool(ctx, "sextant", g.Lease))(t)
	if g := must(q.Lease(ctx, "countersign"))(t); g != nil {
		t.Errorf("a release step for a paused item: %+v", g)
	}
	if g := must(q.Lease(ctx, "bellbuoy"))(t); g == nil || g.Kind != lifecycle.KindIncident {
		t.Errorf("the incident step: %+v", g)
	}
}
