// Package memory is the in-memory stand-in for Logbook.
//
// It keeps every record in memory and loses all of it when the process
// stops. It exists so that Muster can run and be tested alone. It is not
// a place to keep anything that matters.
//
// One transaction runs at a time. A transaction that fails is undone
// step by step, so the all-or-nothing promise of the interface holds.
package memory

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/ClearedStack/muster/internal/lifecycle"
	"github.com/ClearedStack/muster/internal/logbook"
)

// Limits of the stand-in. The real ledger is kept whole; the stand-in
// keeps only its most recent records so that memory stays bounded.
const (
	LedgerKept = 2048
	KeysPerDay = 8192
)

// Store is the stand-in. The zero value is not ready; use New.
type Store struct {
	mu         sync.Mutex
	items      map[string]logbook.Item
	proposals  map[string]logbook.Proposal
	entries    map[string]logbook.Entry
	components map[string]logbook.Component
	counters   map[string]uint64
	countDay   string
	counts     map[string]int
	ledger     []logbook.Record
}

// New returns an empty stand-in.
func New() *Store {
	return &Store{
		items:      map[string]logbook.Item{},
		proposals:  map[string]logbook.Proposal{},
		entries:    map[string]logbook.Entry{},
		components: map[string]logbook.Component{},
		counters:   map[string]uint64{},
		counts:     map[string]int{},
	}
}

var _ logbook.Store = (*Store)(nil)

// Update runs fn in one transaction.
func (s *Store) Update(ctx context.Context, fn func(logbook.Tx) error) error {
	return s.run(ctx, fn, true)
}

// View runs fn and throws away anything it changed.
func (s *Store) View(ctx context.Context, fn func(logbook.Tx) error) error {
	return s.run(ctx, fn, false)
}

func (s *Store) run(ctx context.Context, fn func(logbook.Tx) error, keep bool) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := &tx{s: s}
	done := false
	defer func() {
		if !done || err != nil || !keep {
			t.rollback()
		}
	}()
	err = fn(t)
	done = true
	return err
}

// Ledger returns the ledger records the stand-in still holds, oldest
// first. It is for tests and is not part of the interface.
func (s *Store) Ledger() []logbook.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]logbook.Record(nil), s.ledger...)
}

type tx struct {
	s    *Store
	undo []func()
}

func (t *tx) rollback() {
	for i := len(t.undo) - 1; i >= 0; i-- {
		t.undo[i]()
	}
	t.undo = nil
}

func (t *tx) Item(id string) (logbook.Item, bool) {
	v, ok := t.s.items[id]
	return v, ok
}

func (t *tx) PutItem(v logbook.Item) {
	old, had := t.s.items[v.ID]
	t.undo = append(t.undo, func() { restore(t.s.items, v.ID, old, had) })
	t.s.items[v.ID] = v
}

func (t *tx) Paused(worker string) []logbook.Item {
	var out []logbook.Item
	for _, it := range t.s.items {
		if it.State == lifecycle.NeedsSubtool && it.PausedBy == worker {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PausedSeq < out[j].PausedSeq })
	return out
}

func (t *tx) Proposal(id string) (logbook.Proposal, bool) {
	v, ok := t.s.proposals[id]
	return v, ok
}

func (t *tx) PutProposal(v logbook.Proposal) {
	old, had := t.s.proposals[v.ID]
	t.undo = append(t.undo, func() { restore(t.s.proposals, v.ID, old, had) })
	t.s.proposals[v.ID] = v
}

func (t *tx) Entry(subject string) (logbook.Entry, bool) {
	v, ok := t.s.entries[subject]
	return v, ok
}

func (t *tx) PutEntry(v logbook.Entry) {
	old, had := t.s.entries[v.Subject]
	t.undo = append(t.undo, func() { restore(t.s.entries, v.Subject, old, had) })
	t.s.entries[v.Subject] = v
}

func (t *tx) DeleteEntry(subject string) {
	old, had := t.s.entries[subject]
	if !had {
		return
	}
	t.undo = append(t.undo, func() { t.s.entries[subject] = old })
	delete(t.s.entries, subject)
}

func waiting(e logbook.Entry, now time.Time) bool {
	return !e.Held && (e.Lease == "" || !e.Expires.After(now))
}

func (t *tx) NextEntry(worker string, now time.Time) (logbook.Entry, bool) {
	var best logbook.Entry
	found := false
	for _, e := range t.s.entries {
		if e.Worker != worker || !waiting(e, now) {
			continue
		}
		if !found || earlier(e, best) {
			best, found = e, true
		}
	}
	return best, found
}

func earlier(a, b logbook.Entry) bool {
	ra, rb := lifecycle.Rank(a.Lane), lifecycle.Rank(b.Lane)
	if ra != rb {
		return ra < rb
	}
	return a.Arrival < b.Arrival
}

func (t *tx) Component(slug string) (logbook.Component, bool) {
	v, ok := t.s.components[slug]
	return v, ok
}

func (t *tx) PutComponent(v logbook.Component) {
	old, had := t.s.components[v.Slug]
	t.undo = append(t.undo, func() { restore(t.s.components, v.Slug, old, had) })
	t.s.components[v.Slug] = v
}

func (t *tx) Components() []logbook.Component {
	out := make([]logbook.Component, 0, len(t.s.components))
	for _, c := range t.s.components {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

func (t *tx) Depths(now time.Time) logbook.Depths {
	var d logbook.Depths
	for _, e := range t.s.entries {
		if !waiting(e, now) {
			continue
		}
		switch e.Lane {
		case lifecycle.Urgent:
			d.Urgent++
		case lifecycle.Normal:
			d.Normal++
		case lifecycle.Backfill:
			d.Backfill++
		}
	}
	for _, it := range t.s.items {
		if it.State == lifecycle.NeedsSubtool {
			d.NeedsSubtool++
		}
	}
	return d
}

func (t *tx) Next(counter string) uint64 {
	old := t.s.counters[counter]
	t.undo = append(t.undo, func() { t.s.counters[counter] = old })
	t.s.counters[counter] = old + 1
	return old + 1
}

func (t *tx) Bump(day, key string) (int, bool) {
	if t.s.countDay != day {
		oldDay, oldCounts := t.s.countDay, t.s.counts
		t.undo = append(t.undo, func() { t.s.countDay, t.s.counts = oldDay, oldCounts })
		t.s.countDay, t.s.counts = day, map[string]int{}
	}
	counts := t.s.counts
	old, had := counts[key]
	if !had && len(counts) >= KeysPerDay {
		return 0, false
	}
	t.undo = append(t.undo, func() { restore(counts, key, old, had) })
	counts[key] = old + 1
	return old + 1, true
}

func (t *tx) Append(r logbook.Record) {
	n := len(t.s.ledger)
	if n >= 2*LedgerKept {
		old := t.s.ledger
		t.undo = append(t.undo, func() { t.s.ledger = old })
		t.s.ledger = append(make([]logbook.Record, 0, 2*LedgerKept), old[n-LedgerKept:]...)
		n = LedgerKept
	}
	t.undo = append(t.undo, func() { t.s.ledger = t.s.ledger[:n] })
	t.s.ledger = append(t.s.ledger, r)
}

func restore[V any](m map[string]V, key string, old V, had bool) {
	if had {
		m[key] = old
	} else {
		delete(m, key)
	}
}
