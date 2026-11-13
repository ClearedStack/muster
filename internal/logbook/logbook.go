// Package logbook is the small interface Muster uses to reach Logbook, the
// platform's record of state, together with an in-memory stand-in.
//
// Logbook itself is not built here. Everything Muster needs from it is one
// operation: run a set of reads and writes so that all of them happen or
// none do.
package logbook

import (
	"context"
	"encoding/json"
	"time"

	"github.com/clearedstack/muster/internal/state"
)

// Lane is one of the three priority lanes.
type Lane string

const (
	Urgent   Lane = "urgent"
	Normal   Lane = "normal"
	Backfill Lane = "backfill"
)

// Rank gives the order lanes are emptied in: lower first. The second result
// is false for anything that is not a lane.
func (l Lane) Rank() (int, bool) {
	switch l {
	case Urgent:
		return 0, true
	case Normal:
		return 1, true
	case Backfill:
		return 2, true
	}
	return 0, false
}

// Queue says where an item stands in the queue.
type Queue string

const (
	Waiting Queue = "waiting" // may be leased
	Leased  Queue = "leased"  // a worker holds it
	Stopped Queue = "stopped" // a worker reported broken data; work on it has stopped
	Idle    Queue = "idle"    // not in the queue: paused for a sub-tool, or in an end state
)

// Item is one working record.
type Item struct {
	ID    string
	State state.State
	Lane  Lane
	Seq   uint64 // order of arrival; lower is earlier
	Queue Queue

	// Set while Queue is Leased.
	LeaseID      string
	LeaseWorker  string
	LeaseExpires time.Time

	// Set while State is needs_subtool.
	PausedFrom state.State
	PausedBy   string

	// Last is the most recent report a worker made on this item. It lets a
	// report that is delivered twice be acknowledged twice.
	Last *Report
}

// Report is what a worker last told Muster about an item, and the outcome.
type Report struct {
	LeaseID string
	Worker  string
	Call    string
	To      state.State // complete only
	Code    string      // fail only
	Digest  string      // SHA-256 of the result, complete only
	State   state.State // the item's state afterwards
	Queue   Queue       // the item's place in the queue afterwards
}

// Entry is one line of the append-only ledger.
type Entry struct {
	Seq     uint64          `json:"seq"`
	At      time.Time       `json:"at"`
	Item    string          `json:"item"`
	Call    string          `json:"call"`
	Worker  string          `json:"worker"`
	LeaseID string          `json:"lease_id"`
	From    state.State     `json:"from"`
	To      state.State     `json:"to"`
	Code    string          `json:"code,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
}

// Tx is the set of reads and writes available inside one Update.
type Tx interface {
	// Item returns the record with the given identifier.
	Item(id string) (Item, bool)
	// NextWaiting returns the item in the given state that should be handed
	// out next: urgent before normal before backfill, and within a lane in
	// order of arrival. An item whose lease ran out at or before now counts
	// as waiting.
	NextWaiting(st state.State, now time.Time) (Item, bool)
	// Put writes a record back.
	Put(it Item)
	// NextLease returns the next number in the lease sequence.
	NextLease() uint64
	// Append adds a line to the ledger. Seq is assigned by the store.
	Append(e Entry)
}

// Logbook runs fn so that everything it writes is kept together, or, if fn
// returns an error or ctx ends first, nothing is.
type Logbook interface {
	Update(ctx context.Context, fn func(tx Tx) error) error
}
