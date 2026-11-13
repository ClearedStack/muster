package logbook

import (
	"context"
	"errors"
	"regexp"
	"sync"
	"time"

	"github.com/clearedstack/muster/internal/state"
)

// Memory is the in-memory stand-in for Logbook. It keeps nothing when the
// process stops, and exists so that Muster can run and be tested alone.
type Memory struct {
	mu     sync.Mutex
	items  map[string]Item
	leases uint64
	ledger []Entry
	seq    uint64
}

// NewMemory returns an empty stand-in.
func NewMemory() *Memory {
	return &Memory{items: map[string]Item{}}
}

// Seed is one made-up item to load into the stand-in.
type Seed struct {
	ID    string      `json:"id"`
	State state.State `json:"state"`
	Lane  Lane        `json:"lane"`
}

var idForm = regexp.MustCompile(`^PA-[0-9]{4,9}$`)

// ValidID reports whether id has the form of an item identifier.
func ValidID(id string) bool { return idForm.MatchString(id) }

// Load adds made-up items in the order given, which becomes their order of
// arrival. It adds nothing if any one of them is not acceptable.
func (m *Memory) Load(seeds []Seed) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]bool{}
	for _, s := range seeds {
		if !ValidID(s.ID) {
			return errors.New("seed: an identifier is not of the form PA-0001")
		}
		if seen[s.ID] {
			return errors.New("seed: an identifier appears twice")
		}
		if _, ok := m.items[s.ID]; ok {
			return errors.New("seed: an identifier is already loaded")
		}
		seen[s.ID] = true
		if !state.Valid(s.State) {
			return errors.New("seed: a state is not one of the fourteen")
		}
		if s.State == state.NeedsSubtool {
			return errors.New("seed: an item cannot start in needs_subtool")
		}
		if _, ok := s.Lane.Rank(); !ok {
			return errors.New("seed: a lane is not urgent, normal or backfill")
		}
	}
	for _, s := range seeds {
		m.seq++
		q := Idle
		if state.Leasable(s.State) {
			q = Waiting
		}
		m.items[s.ID] = Item{ID: s.ID, State: s.State, Lane: s.Lane, Seq: m.seq, Queue: q}
	}
	return nil
}

// Ledger returns a copy of the ledger.
func (m *Memory) Ledger() []Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Entry, len(m.ledger))
	copy(out, m.ledger)
	return out
}

// Items returns a copy of every record, in order of arrival.
func (m *Memory) Items() []Item {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Item, len(m.items))
	for _, it := range m.items {
		out[it.Seq-1] = it
	}
	return out
}

// Update implements Logbook.
func (m *Memory) Update(ctx context.Context, fn func(tx Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	tx := &memTx{m: m, puts: map[string]Item{}, leases: m.leases}
	if err := fn(tx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for id, it := range tx.puts {
		m.items[id] = it
	}
	m.leases = tx.leases
	for _, e := range tx.entries {
		e.Seq = uint64(len(m.ledger)) + 1
		m.ledger = append(m.ledger, e)
	}
	return nil
}

type memTx struct {
	m       *Memory
	puts    map[string]Item
	leases  uint64
	entries []Entry
}

func (t *memTx) Item(id string) (Item, bool) {
	if it, ok := t.puts[id]; ok {
		return it, true
	}
	it, ok := t.m.items[id]
	return it, ok
}

func (t *memTx) NextWaiting(st state.State, now time.Time) (Item, bool) {
	var best Item
	found := false
	consider := func(it Item) {
		if it.State != st {
			return
		}
		free := it.Queue == Waiting || (it.Queue == Leased && !now.Before(it.LeaseExpires))
		if !free {
			return
		}
		if !found || before(it, best) {
			best, found = it, true
		}
	}
	for id, it := range t.m.items {
		if _, changed := t.puts[id]; !changed {
			consider(it)
		}
	}
	for _, it := range t.puts {
		consider(it)
	}
	return best, found
}

// before reports whether a is handed out ahead of b.
func before(a, b Item) bool {
	ra, _ := a.Lane.Rank()
	rb, _ := b.Lane.Rank()
	if ra != rb {
		return ra < rb
	}
	return a.Seq < b.Seq
}

func (t *memTx) Put(it Item) { t.puts[it.ID] = it }

func (t *memTx) NextLease() uint64 {
	t.leases++
	return t.leases
}

func (t *memTx) Append(e Entry) { t.entries = append(t.entries, e) }
