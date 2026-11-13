package memory

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ClearedStack/muster/internal/lifecycle"
	"github.com/ClearedStack/muster/internal/logbook"
)

var ctx = context.Background()

var t0 = time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)

func entry(subject, worker string, lane lifecycle.Lane, arrival uint64) logbook.Entry {
	return logbook.Entry{Subject: subject, Kind: lifecycle.KindItem, Worker: worker, Lane: lane, Arrival: arrival}
}

// Everything a failed transaction changed is undone: items, proposals,
// queue entries, components, counters, the day's counts and the ledger.
func TestFailedTransactionChangesNothing(t *testing.T) {
	s := New()
	must(t, s.Update(ctx, func(tx logbook.Tx) error {
		tx.PutItem(logbook.Item{ID: "PA-0001", State: lifecycle.Recorded, Lane: lifecycle.Normal})
		tx.PutEntry(entry("PA-0001", "sextant", lifecycle.Normal, tx.Next(logbook.CounterArrival)))
		tx.PutEntry(entry("PA-0002", "sextant", lifecycle.Normal, tx.Next(logbook.CounterArrival)))
		tx.PutProposal(logbook.Proposal{ID: "EP-0001", Status: lifecycle.PendingApproval})
		tx.PutComponent(logbook.Component{Slug: "sextant", State: "healthy"})
		tx.Bump("2026-01-05", "made-up-key")
		tx.Append(logbook.Record{Seq: 1, Action: logbook.ActLease})
		return nil
	}))

	boom := errors.New("made-up failure")
	err := s.Update(ctx, func(tx logbook.Tx) error {
		tx.PutItem(logbook.Item{ID: "PA-0001", State: lifecycle.Observed, Lane: lifecycle.Urgent})
		tx.PutItem(logbook.Item{ID: "PA-0009", State: lifecycle.Recorded})
		tx.PutEntry(entry("PA-0001", "assay", lifecycle.Urgent, tx.Next(logbook.CounterArrival)))
		tx.DeleteEntry("PA-0002")
		tx.PutProposal(logbook.Proposal{ID: "EP-0001", Status: lifecycle.Approved})
		tx.PutProposal(logbook.Proposal{ID: "EP-0002", Status: lifecycle.PendingApproval})
		tx.PutComponent(logbook.Component{Slug: "sextant", State: "degraded"})
		tx.PutComponent(logbook.Component{Slug: "assay", State: "healthy"})
		tx.Bump("2026-01-05", "made-up-key")
		tx.Bump("2026-01-06", "another-made-up-key") // a new day clears the old counts
		tx.Append(logbook.Record{Seq: 2, Action: logbook.ActComplete})
		return boom
	})
	if err != boom {
		t.Fatalf("Update returned %v", err)
	}

	must(t, s.View(ctx, func(tx logbook.Tx) error {
		if it, _ := tx.Item("PA-0001"); it.State != lifecycle.Recorded || it.Lane != lifecycle.Normal {
			t.Errorf("item changed: %+v", it)
		}
		if _, ok := tx.Item("PA-0009"); ok {
			t.Error("a new item was kept")
		}
		if e, _ := tx.Entry("PA-0001"); e.Worker != "sextant" || e.Arrival != 1 {
			t.Errorf("entry changed: %+v", e)
		}
		if _, ok := tx.Entry("PA-0002"); !ok {
			t.Error("a deleted entry stayed deleted")
		}
		if p, _ := tx.Proposal("EP-0001"); p.Status != lifecycle.PendingApproval {
			t.Errorf("proposal changed: %+v", p)
		}
		if _, ok := tx.Proposal("EP-0002"); ok {
			t.Error("a new proposal was kept")
		}
		if c, _ := tx.Component("sextant"); c.State != "healthy" {
			t.Errorf("component changed: %+v", c)
		}
		if len(tx.Components()) != 1 {
			t.Error("a new component was kept")
		}
		if n := tx.Next(logbook.CounterArrival); n != 3 {
			t.Errorf("the arrival counter moved: next is %d, want 3", n)
		}
		if n, _ := tx.Bump("2026-01-05", "made-up-key"); n != 2 {
			t.Errorf("the day's count changed: next is %d, want 2", n)
		}
		return nil
	}))
	if l := s.Ledger(); len(l) != 1 || l[0].Seq != 1 {
		t.Errorf("the ledger changed: %+v", l)
	}
}

func TestViewKeepsNothing(t *testing.T) {
	s := New()
	must(t, s.View(ctx, func(tx logbook.Tx) error {
		tx.PutItem(logbook.Item{ID: "PA-0001"})
		tx.Next(logbook.CounterLease)
		return nil
	}))
	must(t, s.View(ctx, func(tx logbook.Tx) error {
		if _, ok := tx.Item("PA-0001"); ok {
			t.Error("a view kept an item")
		}
		if tx.Next(logbook.CounterLease) != 1 {
			t.Error("a view moved a counter")
		}
		return nil
	}))
}

func TestPanicChangesNothing(t *testing.T) {
	s := New()
	func() {
		defer func() { recover() }()
		s.Update(ctx, func(tx logbook.Tx) error {
			tx.PutItem(logbook.Item{ID: "PA-0001"})
			panic("made-up panic")
		})
	}()
	must(t, s.View(ctx, func(tx logbook.Tx) error {
		if _, ok := tx.Item("PA-0001"); ok {
			t.Error("a transaction that panicked kept an item")
		}
		return nil
	}))
}

func TestEndedContextIsRefused(t *testing.T) {
	s := New()
	gone, cancel := context.WithCancel(ctx)
	cancel()
	ran := false
	err := s.Update(gone, func(logbook.Tx) error { ran = true; return nil })
	if !errors.Is(err, context.Canceled) || ran {
		t.Errorf("err = %v, ran = %v", err, ran)
	}
}

func TestNextEntryOrder(t *testing.T) {
	s := New()
	must(t, s.Update(ctx, func(tx logbook.Tx) error {
		tx.PutEntry(entry("PA-0001", "sextant", lifecycle.Backfill, 1))
		tx.PutEntry(entry("PA-0002", "sextant", lifecycle.Normal, 2))
		tx.PutEntry(entry("PA-0003", "sextant", lifecycle.Urgent, 3))
		tx.PutEntry(entry("PA-0004", "sextant", lifecycle.Normal, 4))
		tx.PutEntry(entry("PA-0005", "assay", lifecycle.Urgent, 5))
		held := entry("PA-0006", "sextant", lifecycle.Urgent, 6)
		held.Held = true
		tx.PutEntry(held)
		leased := entry("PA-0007", "sextant", lifecycle.Urgent, 7)
		leased.Lease, leased.Expires = "PA-0007.1", t0.Add(time.Minute)
		tx.PutEntry(leased)
		return nil
	}))
	var got []string
	must(t, s.Update(ctx, func(tx logbook.Tx) error {
		for {
			e, ok := tx.NextEntry("sextant", t0)
			if !ok {
				return nil
			}
			got = append(got, e.Subject)
			tx.DeleteEntry(e.Subject)
		}
	}))
	want := "[PA-0003 PA-0002 PA-0004 PA-0001]"
	if fmt.Sprint(got) != want {
		t.Errorf("order %v, want %s", got, want)
	}
	// A lease protects its entry until the moment it ends, and not after.
	must(t, s.View(ctx, func(tx logbook.Tx) error {
		if _, ok := tx.NextEntry("sextant", t0.Add(time.Minute-time.Nanosecond)); ok {
			t.Error("an entry under a live lease was offered")
		}
		if e, ok := tx.NextEntry("sextant", t0.Add(time.Minute)); !ok || e.Subject != "PA-0007" {
			t.Error("an entry whose lease had ended was not offered")
		}
		return nil
	}))
}

func TestDepths(t *testing.T) {
	s := New()
	must(t, s.Update(ctx, func(tx logbook.Tx) error {
		tx.PutEntry(entry("PA-0001", "sextant", lifecycle.Urgent, 1))
		tx.PutEntry(entry("PA-0002", "assay", lifecycle.Normal, 2))
		tx.PutEntry(entry("PA-0003", "assay", lifecycle.Normal, 3))
		tx.PutEntry(entry("PA-0004", "trawl", lifecycle.Backfill, 4))
		held := entry("PA-0005", "trawl", lifecycle.Backfill, 5)
		held.Held = true
		tx.PutEntry(held)
		leased := entry("PA-0006", "trawl", lifecycle.Backfill, 6)
		leased.Lease, leased.Expires = "PA-0006.1", t0.Add(time.Minute)
		tx.PutEntry(leased)
		tx.PutItem(logbook.Item{ID: "PA-0007", State: lifecycle.NeedsSubtool, PausedBy: "sextant"})
		tx.PutItem(logbook.Item{ID: "PA-0008", State: lifecycle.Live})
		return nil
	}))
	must(t, s.View(ctx, func(tx logbook.Tx) error {
		if d := tx.Depths(t0); d != (logbook.Depths{Urgent: 1, Normal: 2, Backfill: 1, NeedsSubtool: 1}) {
			t.Errorf("depths %+v", d)
		}
		// Once the lease has ended its entry is waiting again.
		if d := tx.Depths(t0.Add(time.Minute)); d.Backfill != 2 {
			t.Errorf("after the lease ended: %+v", d)
		}
		return nil
	}))
}

func TestPausedOrder(t *testing.T) {
	s := New()
	must(t, s.Update(ctx, func(tx logbook.Tx) error {
		tx.PutItem(logbook.Item{ID: "PA-0001", State: lifecycle.NeedsSubtool, PausedBy: "sextant", PausedSeq: 9})
		tx.PutItem(logbook.Item{ID: "PA-0002", State: lifecycle.NeedsSubtool, PausedBy: "sextant", PausedSeq: 4})
		tx.PutItem(logbook.Item{ID: "PA-0003", State: lifecycle.NeedsSubtool, PausedBy: "assay", PausedSeq: 1})
		tx.PutItem(logbook.Item{ID: "PA-0004", State: lifecycle.Recorded, PausedBy: "sextant"})
		got := tx.Paused("sextant")
		if len(got) != 2 || got[0].ID != "PA-0002" || got[1].ID != "PA-0001" {
			t.Errorf("paused %+v", got)
		}
		return nil
	}))
}

func TestBump(t *testing.T) {
	s := New()
	must(t, s.Update(ctx, func(tx logbook.Tx) error {
		for want := 1; want <= 3; want++ {
			if n, ok := tx.Bump("2026-01-05", "a"); n != want || !ok {
				t.Errorf("count %d, %v; want %d", n, ok, want)
			}
		}
		if n, _ := tx.Bump("2026-01-05", "b"); n != 1 {
			t.Errorf("another key started at %d", n)
		}
		// A new day forgets the day before.
		if n, _ := tx.Bump("2026-01-06", "a"); n != 1 {
			t.Errorf("a new day started at %d", n)
		}
		// The table holds a fixed number of keys; one more is refused
		// and a key already in it is still counted.
		for i := 1; i < KeysPerDay; i++ {
			if _, ok := tx.Bump("2026-01-06", fmt.Sprint("key-", i)); !ok {
				t.Fatalf("key %d was refused", i)
			}
		}
		if _, ok := tx.Bump("2026-01-06", "one-too-many"); ok {
			t.Error("a key beyond the limit was counted")
		}
		if n, ok := tx.Bump("2026-01-06", "a"); n != 2 || !ok {
			t.Errorf("a known key: %d, %v", n, ok)
		}
		return nil
	}))
}

func TestLedgerIsBounded(t *testing.T) {
	s := New()
	const total = 5*LedgerKept + 7
	for i := 1; i <= total; i++ {
		must(t, s.Update(ctx, func(tx logbook.Tx) error {
			tx.Append(logbook.Record{Seq: uint64(i)})
			return nil
		}))
	}
	l := s.Ledger()
	if len(l) < LedgerKept || len(l) > 2*LedgerKept {
		t.Fatalf("the stand-in holds %d records", len(l))
	}
	if l[len(l)-1].Seq != total {
		t.Errorf("the newest record is %d", l[len(l)-1].Seq)
	}
	for i := 1; i < len(l); i++ {
		if l[i].Seq != l[i-1].Seq+1 {
			t.Fatalf("records %d and %d are not in order", l[i-1].Seq, l[i].Seq)
		}
	}
}

func TestSeedRefusals(t *testing.T) {
	good := `{"items":[{"id":"PA-0001","state":"recorded","lane":"normal","next":"sextant"},{"id":"PA-0002","state":"live","lane":"backfill"}]}`
	seed, err := ParseSeed([]byte(good))
	must(t, err)
	must(t, New().Load(seed))

	unreadable := []string{`items`, `{"items":[],"note":"x"}`, `{"items":[{"id":"PA-0001","colour":"red"}]}`, good + good}
	for _, text := range unreadable {
		if _, err := ParseSeed([]byte(text)); err == nil {
			t.Errorf("seed %s was read", text)
		}
	}
	bad := map[string]string{
		"an identifier of the wrong form": `{"items":[{"id":"0001","state":"recorded","lane":"normal"}]}`,
		"an item listed twice":            `{"items":[{"id":"PA-0001","state":"recorded","lane":"normal"},{"id":"PA-0001","state":"live","lane":"normal"}]}`,
		"a state that does not exist":     `{"items":[{"id":"PA-0001","state":"finished","lane":"normal"}]}`,
		"a proposal status":               `{"items":[{"id":"PA-0001","state":"pending_approval","lane":"normal"}]}`,
		"a paused item":                   `{"items":[{"id":"PA-0001","state":"needs_subtool","lane":"normal"}]}`,
		"a lane that does not exist":      `{"items":[{"id":"PA-0001","state":"recorded","lane":"fast"}]}`,
		"no lane":                         `{"items":[{"id":"PA-0001","state":"recorded"}]}`,
		"a worker with no step there":     `{"items":[{"id":"PA-0001","state":"recorded","lane":"normal","next":"assay"}]}`,
		"a next step that is no worker":   `{"items":[{"id":"PA-0001","state":"recorded","lane":"normal","next":"conn"}]}`,
	}
	for name, text := range bad {
		seed, err := ParseSeed([]byte(text))
		must(t, err)
		s := New()
		if err := s.Load(seed); err == nil {
			t.Errorf("%s: loaded", name)
		}
		// A refused seed leaves nothing behind.
		must(t, s.View(ctx, func(tx logbook.Tx) error {
			if _, ok := tx.Item("PA-0001"); ok {
				t.Errorf("%s: part of a refused seed was kept", name)
			}
			return nil
		}))
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
