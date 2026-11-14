package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/ClearedStack/muster/internal/lifecycle"
	"github.com/ClearedStack/muster/internal/logbook"
)

// Seed is a set of made-up items to start the stand-in with. A real
// Logbook already holds its records; the stand-in starts empty, and no
// call in the contract creates an item, so a seed is the only way to give
// it any.
type Seed struct {
	Items []SeedItem `json:"items"`
}

// SeedItem is one item. Next names the worker whose step is waiting. It
// may be left out, and then the item is in no queue, with one exception:
// every live item gets a Tidegauge check, so a live item with no Next is
// given one in the backfill lane, due at once.
type SeedItem struct {
	ID    string          `json:"id"`
	State lifecycle.State `json:"state"`
	Lane  lifecycle.Lane  `json:"lane"`
	Next  string          `json:"next,omitempty"`
}

// ParseSeed reads a seed and refuses anything it does not recognise.
func ParseSeed(data []byte) (Seed, error) {
	var seed Seed
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&seed); err != nil {
		return Seed{}, errors.New("the seed could not be read")
	}
	if dec.More() {
		return Seed{}, errors.New("the seed has more than one value")
	}
	return seed, nil
}

// Load puts a seed into the stand-in in one transaction. Items enter the
// queue in the order they are listed.
func (s *Store) Load(seed Seed) error {
	return s.Update(context.Background(), func(tx logbook.Tx) error {
		var highest uint64
		for i, it := range seed.Items {
			if !lifecycle.IsItemID(it.ID) {
				return fmt.Errorf("seed item %d: the identifier is not of the form PA-0001", i+1)
			}
			if n, _ := strconv.ParseUint(it.ID[len("PA-"):], 10, 64); n > highest {
				highest = n
			}
			if _, dup := tx.Item(it.ID); dup {
				return fmt.Errorf("seed item %s: listed twice", it.ID)
			}
			if !lifecycle.Known(lifecycle.KindItem, it.State) {
				return fmt.Errorf("seed item %s: unknown state", it.ID)
			}
			if it.State == lifecycle.NeedsSubtool {
				return fmt.Errorf("seed item %s: a seed cannot say what paused an item", it.ID)
			}
			if lifecycle.Rank(it.Lane) < 0 {
				return fmt.Errorf("seed item %s: unknown lane", it.ID)
			}
			next, lane := it.Next, it.Lane
			if next == "" && it.State == lifecycle.Live {
				next, lane = "tidegauge", lifecycle.Backfill
			}
			tx.PutItem(logbook.Item{ID: it.ID, State: it.State, Lane: lane})
			if next == "" {
				continue
			}
			if !lifecycle.Holds(next, lifecycle.KindItem, it.State) {
				return fmt.Errorf("seed item %s: that worker has no step for an item in that state", it.ID)
			}
			tx.PutEntry(logbook.Entry{
				Key:     it.ID,
				Subject: it.ID,
				Kind:    lifecycle.KindItem,
				Worker:  next,
				Lane:    lane,
			})
		}
		// New items are numbered after the highest one in the seed.
		t := tx.(*txn)
		if old := s.counters[counterItem]; highest > old {
			t.undo = append(t.undo, func() { s.counters[counterItem] = old })
			s.counters[counterItem] = highest
		}
		return nil
	})
}
