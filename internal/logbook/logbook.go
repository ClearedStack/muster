// Package logbook is the small interface Muster keeps Logbook behind.
//
// Logbook is the platform's record of state. Muster is the only service
// that reads and writes its working records. Logbook does not exist yet,
// so this package defines what Muster needs from it and the package
// memory provides a stand-in that keeps everything in memory.
//
// The one promise Muster relies on is the transaction: everything a
// function passed to Update changes is kept together, or none of it is.
// That is what lets a change of state and the next queue step happen
// together or not at all.
package logbook

import (
	"context"
	"encoding/json"
	"time"

	"github.com/ClearedStack/muster/internal/lifecycle"
)

// Store is Logbook as Muster sees it.
type Store interface {
	// Update runs fn in one transaction. If fn returns an error, nothing
	// it changed is kept. An implementation may run fn more than once,
	// so fn must not keep anything from an earlier run.
	//
	// Update returns the context's error when the context ends first.
	Update(ctx context.Context, fn func(Tx) error) error

	// View runs fn against one consistent picture of the records.
	// Anything fn changes is thrown away.
	View(ctx context.Context, fn func(Tx) error) error
}

// Tx is one transaction.
//
// The methods return no errors. An implementation that can fail in the
// middle of a transaction returns empty values from then on and reports
// the failure from Update or View.
type Tx interface {
	Item(id string) (Item, bool)
	PutItem(Item)
	// Paused lists the items a worker paused, in the order it paused them.
	Paused(worker string) []Item

	Proposal(id string) (Proposal, bool)
	PutProposal(Proposal)

	// Entry returns the queue entry for a subject. A subject has at most
	// one: its next step.
	Entry(subject string) (Entry, bool)
	PutEntry(Entry)
	DeleteEntry(subject string)
	// NextEntry returns the entry a worker should be offered now: among
	// that worker's entries that are not set aside and not under a live
	// lease, the one in the earliest lane, and within the lane the one
	// that arrived first.
	NextEntry(worker string, now time.Time) (Entry, bool)

	Component(slug string) (Component, bool)
	PutComponent(Component)
	// Components lists every component with a record, ordered by slug.
	Components() []Component

	// Depths counts what is waiting at this moment.
	Depths(now time.Time) Depths

	// Next advances a named counter and returns its new value. Counters
	// start at zero, so the first value is 1.
	Next(counter string) uint64

	// Bump adds one to the count kept for a key on a day and returns the
	// new count. It reports false, and counts nothing, when the key is
	// new and no more keys can be kept for that day.
	Bump(day, key string) (int, bool)

	// Append adds one record to the append-only ledger.
	Append(Record)
}

// Names of the counters Muster uses.
const (
	CounterArrival  = "arrival"
	CounterLease    = "lease"
	CounterProposal = "proposal"
	CounterLedger   = "ledger"
)

// Item is the working record of one idea for a tool.
type Item struct {
	ID    string
	State lifecycle.State
	Lane  lifecycle.Lane

	// Set while the item is in needs_subtool.
	PausedFrom lifecycle.State
	PausedBy   string
	PausedSeq  uint64

	// Results holds the latest result each worker submitted for the
	// item, by worker. A stored map is never changed in place.
	Results map[string]json.RawMessage

	Done Finished
}

// Proposal is one edit proposal. It never holds the address it came from,
// or anything derived from it.
type Proposal struct {
	ID         string
	Target     string
	Repository string
	File       string
	EditKind   string
	Change     string
	Reason     string
	Status     lifecycle.State
	Received   time.Time

	Done Finished
}

// Entry is one queue step: the next thing to be done to a subject, and by
// which worker.
type Entry struct {
	Subject string
	Kind    lifecycle.Kind
	Worker  string
	Lane    lifecycle.Lane
	Arrival uint64 // order of arrival; lower is earlier
	Attempt int    // how many times it has been leased

	Lease   string    // empty while waiting
	Expires time.Time // when the lease stops protecting the entry

	// Held is set when work on the subject was stopped. A held entry is
	// never offered.
	Held bool
}

// Finished remembers how the last lease on a subject ended, so that a
// worker repeating a call it was never told had succeeded gets the same
// answer again.
type Finished struct {
	Lease  string
	Worker string
	Call   string
	Arg    string
	Ack    Ack
}

// Ack is the answer to a call that ended a lease.
type Ack struct {
	ID    string          `json:"id"`
	State lifecycle.State `json:"state"`
	Next  string          `json:"next,omitempty"`
	Held  bool            `json:"held,omitempty"`
}

// Component is the last health report received for one component.
type Component struct {
	Slug   string
	State  string
	Code   string
	Digest string
	At     time.Time
}

// Depths is what is waiting: entries in each lane that could be leased
// now, and items paused for a missing rule.
type Depths struct {
	Urgent       int
	Normal       int
	Backfill     int
	NeedsSubtool int
}

// Record is one line of the append-only ledger.
type Record struct {
	Seq     uint64
	At      time.Time
	Actor   string
	Action  string
	Subject string
	From    lifecycle.State
	To      lifecycle.State
	Code    string
}

// Ledger actions.
const (
	ActLease    = "lease"
	ActComplete = "complete"
	ActFail     = "fail"
	ActPause    = "needs_subtool"
	ActReplay   = "replay"
	ActPropose  = "propose"
	ActSetAside = "set_aside"
)
