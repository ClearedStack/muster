package logbook

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/clearedstack/muster/internal/state"
)

func TestUpdateKeepsNothingWhenRefused(t *testing.T) {
	m := NewMemory()
	if err := m.Load([]Seed{{ID: "PA-0001", State: state.Recorded, Lane: Normal}}); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("stop")
	err := m.Update(context.Background(), func(tx Tx) error {
		it, _ := tx.Item("PA-0001")
		it.State = state.Observed
		tx.Put(it)
		tx.NextLease()
		tx.Append(Entry{Item: it.ID, Call: "complete"})
		if got, _ := tx.Item("PA-0001"); got.State != state.Observed {
			t.Error("a write is not visible later in the same update")
		}
		return stop
	})
	if err != stop {
		t.Fatalf("got %v, want the error the update returned", err)
	}
	if m.Items()[0].State != state.Recorded || len(m.Ledger()) != 0 {
		t.Error("a refused update left something behind")
	}
	m.Update(context.Background(), func(tx Tx) error {
		if n := tx.NextLease(); n != 1 {
			t.Errorf("lease sequence moved on after a refused update: got %d, want 1", n)
		}
		return nil
	})
}

func TestUpdateKeepsNothingWhenTimeRunsOut(t *testing.T) {
	m := NewMemory()
	m.Load([]Seed{{ID: "PA-0001", State: state.Recorded, Lane: Normal}})
	ctx, cancel := context.WithCancel(context.Background())
	err := m.Update(ctx, func(tx Tx) error {
		it, _ := tx.Item("PA-0001")
		it.State = state.Observed
		tx.Put(it)
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || m.Items()[0].State != state.Recorded {
		t.Errorf("got %v and state %s; want the update dropped", err, m.Items()[0].State)
	}
	if err := m.Update(ctx, func(Tx) error { t.Error("ran after time was up"); return nil }); err == nil {
		t.Error("an update started after time was up was accepted")
	}
}

func TestNextWaitingOrder(t *testing.T) {
	m := NewMemory()
	m.Load([]Seed{
		{ID: "PA-0001", State: state.Recorded, Lane: Backfill},
		{ID: "PA-0002", State: state.Recorded, Lane: Normal},
		{ID: "PA-0003", State: state.Observed, Lane: Urgent},
		{ID: "PA-0004", State: state.Recorded, Lane: Urgent},
		{ID: "PA-0005", State: state.Recorded, Lane: Normal},
	})
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var got []string
	m.Update(context.Background(), func(tx Tx) error {
		for {
			it, ok := tx.NextWaiting(state.Recorded, now)
			if !ok {
				return nil
			}
			got = append(got, it.ID)
			it.Queue, it.LeaseExpires = Leased, now.Add(time.Minute)
			tx.Put(it)
		}
	})
	want := []string{"PA-0004", "PA-0002", "PA-0005", "PA-0001"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	// A lease counts as run out at the moment it expires, not before.
	m.Update(context.Background(), func(tx Tx) error {
		if _, ok := tx.NextWaiting(state.Recorded, now.Add(time.Minute-time.Second)); ok {
			t.Error("an item was offered before its lease ran out")
		}
		if it, ok := tx.NextWaiting(state.Recorded, now.Add(time.Minute)); !ok || it.ID != "PA-0004" {
			t.Errorf("got %v %v, want PA-0004 once its lease ran out", it.ID, ok)
		}
		return nil
	})
}

func TestLoadRefusesBadSeeds(t *testing.T) {
	bad := [][]Seed{
		{{ID: "PA-1", State: state.Recorded, Lane: Normal}},
		{{ID: "PA-0001", State: "done", Lane: Normal}},
		{{ID: "PA-0001", State: state.Recorded, Lane: "fast"}},
		{{ID: "PA-0001", State: state.NeedsSubtool, Lane: Normal}},
		{{ID: "PA-0001", State: state.Recorded, Lane: Normal}, {ID: "PA-0001", State: state.Recorded, Lane: Normal}},
		{{ID: "PA-0002", State: state.Recorded, Lane: Normal}, {ID: "PA-1", State: state.Recorded, Lane: Normal}},
	}
	for i, seeds := range bad {
		m := NewMemory()
		if err := m.Load(seeds); err == nil {
			t.Errorf("set %d was accepted", i+1)
		}
		if len(m.Items()) != 0 {
			t.Errorf("set %d was refused and still left items behind", i+1)
		}
	}
	m := NewMemory()
	m.Load([]Seed{{ID: "PA-0001", State: state.Rejected, Lane: Normal}, {ID: "PA-0002", State: state.Live, Lane: Urgent}})
	if it := m.Items(); it[0].Queue != Idle || it[1].Queue != Waiting {
		t.Errorf("got %s and %s; want an end state idle and a live item waiting", it[0].Queue, it[1].Queue)
	}
}
