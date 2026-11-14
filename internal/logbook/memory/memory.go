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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	addresses  map[string]string
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
		addresses:  map[string]string{},
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
	t := &txn{s: s}
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

type txn struct {
	s    *Store
	undo []func()
}

func (t *txn) rollback() {
	for i := len(t.undo) - 1; i >= 0; i-- {
		t.undo[i]()
	}
	t.undo = nil
}

func (t *txn) Item(id string) (logbook.Item, bool) {
	v, ok := t.s.items[id]
	return v, ok
}

func (t *txn) PutItem(v logbook.Item) {
	old, had := t.s.items[v.ID]
	t.undo = append(t.undo, func() { restore(t.s.items, v.ID, old, had) })
	t.s.items[v.ID] = v
}

func (t *txn) Paused(worker string) []logbook.Item {
	var out []logbook.Item
	for _, it := range t.s.items {
		if it.State == lifecycle.NeedsSubtool && it.PausedBy == worker {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PausedSeq < out[j].PausedSeq })
	return out
}

func (t *txn) Proposal(id string) (logbook.Proposal, bool) {
	v, ok := t.s.proposals[id]
	return v, ok
}

func (t *txn) PutProposal(v logbook.Proposal) {
	old, had := t.s.proposals[v.ID]
	t.undo = append(t.undo, func() { restore(t.s.proposals, v.ID, old, had) })
	t.s.proposals[v.ID] = v
}

// AddressKey is what the stand-in knows an address by: the SHA-256 of
// its bytes, as Logbook does.
func AddressKey(address string) string {
	sum := sha256.Sum256([]byte(address))
	return hex.EncodeToString(sum[:])
}

func (t *txn) AddItem(v logbook.Item) (string, bool) {
	key := ""
	if v.Address != "" {
		key = AddressKey(v.Address)
		if id, seen := t.s.addresses[key]; seen {
			return id, true
		}
	}
	n := t.next(counterItem)
	if n > lifecycle.MaxID {
		return "", false
	}
	v.ID = lifecycle.ItemID(n)
	t.PutItem(v)
	if key != "" {
		t.undo = append(t.undo, func() { delete(t.s.addresses, key) })
		t.s.addresses[key] = v.ID
	}
	return v.ID, false
}

func (t *txn) AddProposal(v logbook.Proposal) string {
	n := t.next(counterProposal)
	if n > lifecycle.MaxID {
		return ""
	}
	v.ID = lifecycle.ProposalID(n)
	t.PutProposal(v)
	return v.ID
}

func (t *txn) Building(repository string) []logbook.Proposal {
	var out []logbook.Proposal
	for _, p := range t.s.proposals {
		if p.Status == lifecycle.Building && p.Repository == repository {
			out = append(out, p)
		}
	}
	// Identifiers are handed out in order, so the order of identifiers
	// is the order of arrival. A longer identifier is a later one.
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].ID) != len(out[j].ID) {
			return len(out[i].ID) < len(out[j].ID)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (t *txn) Entry(key string) (logbook.Entry, bool) {
	v, ok := t.s.entries[key]
	return v, ok
}

func (t *txn) PutEntry(v logbook.Entry) {
	if v.Arrival == 0 {
		v.Arrival = t.next(counterArrival)
	}
	old, had := t.s.entries[v.Key]
	t.undo = append(t.undo, func() { restore(t.s.entries, v.Key, old, had) })
	t.s.entries[v.Key] = v
}

func (t *txn) DeleteEntry(key string) {
	old, had := t.s.entries[key]
	if !had {
		return
	}
	t.undo = append(t.undo, func() { t.s.entries[key] = old })
	delete(t.s.entries, key)
}

func waiting(e logbook.Entry, now time.Time) bool {
	return !e.Held && !e.NotBefore.After(now) && (e.Lease == "" || !e.Expires.After(now))
}

func (t *txn) NextEntry(worker string, now time.Time) (logbook.Entry, bool) {
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

func (t *txn) Component(slug string) (logbook.Component, bool) {
	v, ok := t.s.components[slug]
	return v, ok
}

func (t *txn) PutComponent(v logbook.Component) {
	old, had := t.s.components[v.Slug]
	t.undo = append(t.undo, func() { restore(t.s.components, v.Slug, old, had) })
	t.s.components[v.Slug] = v
}

func (t *txn) Components() []logbook.Component {
	out := make([]logbook.Component, 0, len(t.s.components))
	for _, c := range t.s.components {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

func (t *txn) Depths(now time.Time) logbook.Depths {
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

// The stand-in's own counters. Logbook has a sequence for each.
const (
	counterArrival  = "arrival"
	counterItem     = "item"
	counterProposal = "proposal"
	counterLedger   = "ledger"
)

func (t *txn) next(counter string) uint64 {
	old := t.s.counters[counter]
	t.undo = append(t.undo, func() { t.s.counters[counter] = old })
	t.s.counters[counter] = old + 1
	return old + 1
}

func (t *txn) Bump(day, key string) (int, bool) {
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

func (t *txn) Append(r logbook.Record) uint64 {
	r.Seq = t.next(counterLedger)
	n := len(t.s.ledger)
	if n >= 2*LedgerKept {
		old := t.s.ledger
		t.undo = append(t.undo, func() { t.s.ledger = old })
		t.s.ledger = append(make([]logbook.Record, 0, 2*LedgerKept), old[n-LedgerKept:]...)
		n = LedgerKept
	}
	t.undo = append(t.undo, func() { t.s.ledger = t.s.ledger[:n] })
	t.s.ledger = append(t.s.ledger, r)
	return r.Seq
}

// The stand-in has no separate record of evaluations or decisions. What
// Assay and Countersign report is kept with the item's other results.
func (t *txn) Evaluation(string, json.RawMessage) {}
func (t *txn) Decision(string, string, string)    {}

func restore[V any](m map[string]V, key string, old V, had bool) {
	if had {
		m[key] = old
	} else {
		delete(m, key)
	}
}
