// Package queue is the gatekeeper: the rules for the five worker calls.
// Every call is one Logbook update, so a change of state and the item's
// next place in the queue are recorded together or not at all.
package queue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/clearedstack/muster/internal/logbook"
	"github.com/clearedstack/muster/internal/state"
)

// The reasons a call is refused.
var (
	ErrNoItem         = errors.New("no such item")
	ErrLeaseNotHeld   = errors.New("lease not held")
	ErrMoveNotAllowed = errors.New("move not allowed")
	ErrRepeatDiffers  = errors.New("repeat differs")
)

// The two codes a worker may give to fail. E_NEEDS_SUBTOOL is reported
// with the needs_subtool call instead.
const (
	CodeTimeout = "E_TIMEOUT"
	CodeParse   = "E_PARSE"
)

// Gatekeeper applies the rules on top of a Logbook.
type Gatekeeper struct {
	Book logbook.Logbook
	TTL  time.Duration // how long a lease lasts without a heartbeat
}

// Lease is what a worker is given when it is handed an item.
type Lease struct {
	Item      string
	State     state.State
	Lane      logbook.Lane
	LeaseID   string
	ExpiresAt time.Time
}

// Outcome is where an item stands after a report.
type Outcome struct {
	Item  string
	State state.State
	Queue logbook.Queue
}

// Lease hands the worker the next item waiting in the given state, or
// returns nil when there is none.
func (g *Gatekeeper) Lease(ctx context.Context, worker string, st state.State, now time.Time) (*Lease, error) {
	var out *Lease
	err := g.Book.Update(ctx, func(tx logbook.Tx) error {
		it, ok := tx.NextWaiting(st, now)
		if !ok {
			return nil
		}
		if it.Queue == logbook.Leased {
			// The earlier lease ran out. Say so in the ledger before
			// handing the item to someone else.
			tx.Append(logbook.Entry{At: now, Item: it.ID, Call: "expire", Worker: it.LeaseWorker,
				LeaseID: it.LeaseID, From: it.State, To: it.State})
		}
		it.Queue = logbook.Leased
		it.LeaseID = fmt.Sprintf("L-%08d", tx.NextLease())
		it.LeaseWorker = worker
		it.LeaseExpires = now.Add(g.TTL)
		tx.Put(it)
		tx.Append(logbook.Entry{At: now, Item: it.ID, Call: "lease", Worker: worker,
			LeaseID: it.LeaseID, From: it.State, To: it.State})
		out = &Lease{Item: it.ID, State: it.State, Lane: it.Lane, LeaseID: it.LeaseID, ExpiresAt: it.LeaseExpires}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// held reports whether the worker still holds the lease it names. A lease
// that has run out still counts until the item is handed to someone else.
func held(it logbook.Item, worker, leaseID string) bool {
	return it.Queue == logbook.Leased && it.LeaseID == leaseID && it.LeaseWorker == worker
}

// Heartbeat extends a lease the worker still holds.
func (g *Gatekeeper) Heartbeat(ctx context.Context, worker, id, leaseID string, now time.Time) (*Lease, error) {
	var out *Lease
	err := g.Book.Update(ctx, func(tx logbook.Tx) error {
		it, ok := tx.Item(id)
		if !ok {
			return ErrNoItem
		}
		if !held(it, worker, leaseID) {
			return ErrLeaseNotHeld
		}
		it.LeaseExpires = now.Add(g.TTL)
		tx.Put(it)
		out = &Lease{Item: it.ID, State: it.State, Lane: it.Lane, LeaseID: it.LeaseID, ExpiresAt: it.LeaseExpires}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// report is the shape shared by complete, fail and needs_subtool: check for
// a repeat of the last report, check the lease, then apply the change.
func (g *Gatekeeper) report(ctx context.Context, want logbook.Report, id string, now time.Time,
	result json.RawMessage, apply func(it *logbook.Item) error) (*Outcome, error) {
	var out *Outcome
	err := g.Book.Update(ctx, func(tx logbook.Tx) error {
		it, ok := tx.Item(id)
		if !ok {
			return ErrNoItem
		}
		if last := it.Last; last != nil && last.LeaseID == want.LeaseID && last.Worker == want.Worker {
			if last.Call == want.Call && last.To == want.To && last.Code == want.Code && last.Digest == want.Digest {
				out = &Outcome{Item: it.ID, State: last.State, Queue: last.Queue}
				return nil
			}
			return ErrRepeatDiffers
		}
		if !held(it, want.Worker, want.LeaseID) {
			return ErrLeaseNotHeld
		}
		from := it.State
		if err := apply(&it); err != nil {
			return err
		}
		it.LeaseID, it.LeaseWorker, it.LeaseExpires = "", "", time.Time{}
		want.State, want.Queue = it.State, it.Queue
		it.Last = &want
		tx.Put(it)
		tx.Append(logbook.Entry{At: now, Item: it.ID, Call: want.Call, Worker: want.Worker,
			LeaseID: want.LeaseID, From: from, To: it.State, Code: want.Code, Result: result})
		out = &Outcome{Item: it.ID, State: it.State, Queue: it.Queue}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Complete records a result and moves the item to the state the worker
// names, if that move is allowed.
func (g *Gatekeeper) Complete(ctx context.Context, worker, id, leaseID string, to state.State,
	result json.RawMessage, now time.Time) (*Outcome, error) {
	sum := sha256.Sum256(result)
	want := logbook.Report{LeaseID: leaseID, Worker: worker, Call: "complete", To: to, Digest: hex.EncodeToString(sum[:])}
	return g.report(ctx, want, id, now, result, func(it *logbook.Item) error {
		// A pause is reported with needs_subtool, never with complete.
		if to == state.NeedsSubtool || !state.Allowed(it.State, to) {
			return ErrMoveNotAllowed
		}
		it.State = to
		it.Queue = logbook.Idle
		if state.Leasable(to) {
			it.Queue = logbook.Waiting
		}
		return nil
	})
}

// Fail records a failure. The item keeps its state. After E_TIMEOUT it
// waits to be tried again; after E_PARSE work on it stops.
func (g *Gatekeeper) Fail(ctx context.Context, worker, id, leaseID, code string, now time.Time) (*Outcome, error) {
	want := logbook.Report{LeaseID: leaseID, Worker: worker, Call: "fail", Code: code}
	return g.report(ctx, want, id, now, nil, func(it *logbook.Item) error {
		switch code {
		case CodeTimeout:
			it.Queue = logbook.Waiting
		case CodeParse:
			it.Queue = logbook.Stopped
		default:
			return ErrMoveNotAllowed
		}
		return nil
	})
}

// NeedsSubtool pauses the item, remembering the state it was paused in and
// the worker that paused it.
func (g *Gatekeeper) NeedsSubtool(ctx context.Context, worker, id, leaseID string, now time.Time) (*Outcome, error) {
	want := logbook.Report{LeaseID: leaseID, Worker: worker, Call: "needs_subtool", Code: "E_NEEDS_SUBTOOL"}
	return g.report(ctx, want, id, now, nil, func(it *logbook.Item) error {
		if !state.CanPause(it.State) {
			return ErrMoveNotAllowed
		}
		it.PausedFrom, it.PausedBy = it.State, worker
		it.State = state.NeedsSubtool
		it.Queue = logbook.Idle
		return nil
	})
}
