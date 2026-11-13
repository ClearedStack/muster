// Package queue is the five worker calls: lease, heartbeat, complete,
// fail and needs_subtool, and the replay of paused items.
//
// Every call is one Logbook transaction. A change of state and the next
// queue step are written together, or neither is.
package queue

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ClearedStack/muster/internal/contract"
	"github.com/ClearedStack/muster/internal/lifecycle"
	"github.com/ClearedStack/muster/internal/logbook"
)

// Notifier is told about changes the live feed publishes. It is called
// only after the transaction that made the change has been kept.
type Notifier interface {
	Edit(id string, status lifecycle.State, at time.Time)
}

// Service carries out the worker calls.
type Service struct {
	Store    logbook.Store
	Clock    func() time.Time
	LeaseFor time.Duration
	Notify   Notifier
}

// Grant is the answer to a lease call that found work.
type Grant struct {
	Lease    string                     `json:"lease"`
	Kind     lifecycle.Kind             `json:"kind"`
	ID       string                     `json:"id"`
	State    lifecycle.State            `json:"state"`
	Lane     lifecycle.Lane             `json:"lane"`
	Attempt  int                        `json:"attempt"`
	Expires  string                     `json:"expires"`
	Results  map[string]json.RawMessage `json:"results,omitempty"`
	Proposal *ProposalView              `json:"proposal,omitempty"`
}

// ProposalView is what a worker is shown of an edit proposal.
type ProposalView struct {
	Target     string `json:"target"`
	Repository string `json:"repository"`
	File       string `json:"file"`
	EditKind   string `json:"edit_kind"`
	Change     string `json:"change"`
	Reason     string `json:"reason"`
}

// Beat is the answer to a heartbeat.
type Beat struct {
	Lease   string `json:"lease"`
	Expires string `json:"expires"`
}

const (
	callComplete = "complete"
	callFail     = "fail"
	callPause    = "needs_subtool"
)

// Stamp writes a time the way every answer and feed event carries it.
func Stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// Lease offers the worker its next queue step. It returns nil when there
// is nothing for that worker to do.
func (s *Service) Lease(ctx context.Context, worker string) (*Grant, error) {
	var grant *Grant
	var setAside bool
	err := s.Store.Update(ctx, func(tx logbook.Tx) error {
		grant, setAside = nil, false
		now := s.Clock()
		e, ok := tx.NextEntry(worker, now)
		if !ok {
			return nil
		}
		state, found := subjectState(tx, e)
		if !found || !lifecycle.Holds(worker, e.Kind, state) {
			// The queue says this worker is next, and the tables say it
			// has nothing to do with a subject in this state. Stop work
			// on the subject so the rest of the queue is not blocked.
			e.Held, e.Lease, e.Expires = true, "", time.Time{}
			tx.PutEntry(e)
			record(tx, now, worker, logbook.ActSetAside, e.Subject, state, state, string(contract.Parse))
			setAside = true
			return nil
		}
		e.Attempt++
		e.Lease = e.Subject + "." + strconv.FormatUint(tx.Next(logbook.CounterLease), 10)
		e.Expires = now.Add(s.LeaseFor)
		tx.PutEntry(e)
		record(tx, now, worker, logbook.ActLease, e.Subject, state, state, "")

		grant = &Grant{
			Lease:   e.Lease,
			Kind:    e.Kind,
			ID:      e.Subject,
			State:   state,
			Lane:    e.Lane,
			Attempt: e.Attempt,
			Expires: Stamp(e.Expires),
		}
		switch e.Kind {
		case lifecycle.KindItem:
			it, _ := tx.Item(e.Subject)
			grant.Results = it.Results
		case lifecycle.KindProposal:
			p, _ := tx.Proposal(e.Subject)
			grant.Proposal = &ProposalView{
				Target: p.Target, Repository: p.Repository, File: p.File,
				EditKind: p.EditKind, Change: p.Change, Reason: p.Reason,
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if setAside {
		return nil, contract.Inconsistent
	}
	return grant, nil
}

// Heartbeat extends a lease the worker still holds.
func (s *Service) Heartbeat(ctx context.Context, worker, lease string) (*Beat, error) {
	var beat *Beat
	err := s.Store.Update(ctx, func(tx logbook.Tx) error {
		e, ok := held(tx, worker, lease)
		if !ok {
			return contract.LeaseNotHeld
		}
		e.Expires = s.Clock().Add(s.LeaseFor)
		tx.PutEntry(e)
		beat = &Beat{Lease: e.Lease, Expires: Stamp(e.Expires)}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return beat, nil
}

// Complete records a finished step: the result, the new state and the
// next queue step, together.
func (s *Service) Complete(ctx context.Context, worker, lease string, to lifecycle.State, result json.RawMessage) (*logbook.Ack, error) {
	var ack logbook.Ack
	var after func()
	err := s.Store.Update(ctx, func(tx logbook.Tx) error {
		after = nil
		e, ok := held(tx, worker, lease)
		if !ok {
			return repeated(tx, worker, lease, callComplete, string(to), &ack)
		}
		now := s.Clock()
		from, _ := subjectState(tx, e)
		route, ok := lifecycle.Find(worker, e.Kind, from, to)
		if !ok {
			return contract.MoveNotAllowed
		}
		if from != to && !lifecycle.Allowed(e.Kind, from, to) {
			return contract.MoveNotAllowed
		}
		lane := e.Lane
		if route.Lane != "" {
			lane = route.Lane
		}
		ack = logbook.Ack{ID: e.Subject, State: to, Next: route.Next}
		done := logbook.Finished{Lease: lease, Worker: worker, Call: callComplete, Arg: string(to), Ack: ack}

		switch e.Kind {
		case lifecycle.KindItem:
			it, _ := tx.Item(e.Subject)
			it.State, it.Lane, it.Done = to, lane, done
			if result != nil {
				it.Results = withResult(it.Results, worker, result)
			}
			tx.PutItem(it)
		case lifecycle.KindProposal:
			if result != nil {
				// A decision on a proposal carries no result, and there
				// is nowhere to keep one.
				return contract.Malformed
			}
			p, _ := tx.Proposal(e.Subject)
			p.Status, p.Done = to, done
			tx.PutProposal(p)
			if from != to && s.Notify != nil {
				id := p.ID
				after = func() { s.Notify.Edit(id, to, now) }
			}
		}

		if route.Next == "" {
			tx.DeleteEntry(e.Subject)
		} else {
			tx.PutEntry(logbook.Entry{
				Subject: e.Subject,
				Kind:    e.Kind,
				Worker:  route.Next,
				Lane:    lane,
				Arrival: tx.Next(logbook.CounterArrival),
			})
		}
		record(tx, now, worker, logbook.ActComplete, e.Subject, from, to, "")
		return nil
	})
	if err != nil {
		return nil, err
	}
	if after != nil {
		after()
	}
	return &ack, nil
}

// Fail records that a step could not be done.
//
// E_TIMEOUT leaves the subject in the queue to be tried again. E_PARSE
// stops work on it: its state is kept and it is never offered again.
// E_NEEDS_SUBTOOL has a call of its own and is refused here.
func (s *Service) Fail(ctx context.Context, worker, lease string, code contract.Code) (*logbook.Ack, error) {
	switch code {
	case contract.Timeout, contract.Parse:
	case contract.NeedsSubtool:
		return nil, contract.UseNeedsSubtool
	default:
		return nil, contract.Malformed
	}
	var ack logbook.Ack
	err := s.Store.Update(ctx, func(tx logbook.Tx) error {
		e, ok := held(tx, worker, lease)
		if !ok {
			return repeated(tx, worker, lease, callFail, string(code), &ack)
		}
		now := s.Clock()
		state, _ := subjectState(tx, e)
		e.Lease, e.Expires = "", time.Time{}
		ack = logbook.Ack{ID: e.Subject, State: state}
		if code == contract.Parse {
			e.Held = true
			ack.Held = true
		} else {
			ack.Next = e.Worker
		}
		tx.PutEntry(e)
		remember(tx, e, logbook.Finished{Lease: lease, Worker: worker, Call: callFail, Arg: string(code), Ack: ack})
		record(tx, now, worker, logbook.ActFail, e.Subject, state, state, string(code))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &ack, nil
}

// NeedsSubtool pauses an item the worker has no rule for. The item
// remembers the state it was in and which worker paused it, and leaves
// the queue until that worker is replayed.
func (s *Service) NeedsSubtool(ctx context.Context, worker, lease string) (*logbook.Ack, error) {
	var ack logbook.Ack
	err := s.Store.Update(ctx, func(tx logbook.Tx) error {
		e, ok := held(tx, worker, lease)
		if !ok {
			return repeated(tx, worker, lease, callPause, "", &ack)
		}
		if e.Kind != lifecycle.KindItem {
			return contract.MoveNotAllowed
		}
		it, _ := tx.Item(e.Subject)
		if !lifecycle.Pausable(it.State) {
			return contract.MoveNotAllowed
		}
		now := s.Clock()
		from := it.State
		ack = logbook.Ack{ID: it.ID, State: lifecycle.NeedsSubtool}
		seq := record(tx, now, worker, logbook.ActPause, it.ID, from, lifecycle.NeedsSubtool, string(contract.NeedsSubtool))
		it.State = lifecycle.NeedsSubtool
		it.PausedFrom, it.PausedBy, it.PausedSeq = from, worker, seq
		it.Done = logbook.Finished{Lease: lease, Worker: worker, Call: callPause, Ack: ack}
		tx.PutItem(it)
		tx.DeleteEntry(e.Subject)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &ack, nil
}

// Replay puts every item a worker paused back in the queue, in the order
// they were paused: each returns to the state it was paused from, in the
// urgent lane, with that worker's step waiting. It returns how many items
// it replayed. It runs inside the caller's transaction.
func Replay(tx logbook.Tx, worker string, now time.Time) int {
	paused := tx.Paused(worker)
	for _, it := range paused {
		back := it.PausedFrom
		it.State, it.Lane = back, lifecycle.Urgent
		it.PausedFrom, it.PausedBy, it.PausedSeq = "", "", 0
		tx.PutItem(it)
		tx.PutEntry(logbook.Entry{
			Subject: it.ID,
			Kind:    lifecycle.KindItem,
			Worker:  worker,
			Lane:    lifecycle.Urgent,
			Arrival: tx.Next(logbook.CounterArrival),
		})
		record(tx, now, worker, logbook.ActReplay, it.ID, lifecycle.NeedsSubtool, back, "")
	}
	return len(paused)
}

var leaseForm = regexp.MustCompile(`^((?:PA|EP)-[0-9]{4,9})\.[1-9][0-9]{0,18}$`)

// subjectOf returns the subject a lease was issued for.
func subjectOf(lease string) (string, bool) {
	if !leaseForm.MatchString(lease) {
		return "", false
	}
	return lease[:strings.IndexByte(lease, '.')], true
}

// held returns the entry a worker holds under a lease. A lease is held
// until the step ends or the entry is leased again after it expired; an
// expired lease nobody has taken over still counts, so a finished result
// is not thrown away because it arrived late.
func held(tx logbook.Tx, worker, lease string) (logbook.Entry, bool) {
	subject, ok := subjectOf(lease)
	if !ok {
		return logbook.Entry{}, false
	}
	e, ok := tx.Entry(subject)
	if !ok || e.Lease != lease || e.Worker != worker {
		return logbook.Entry{}, false
	}
	return e, true
}

// repeated answers a call made under a lease that is no longer held. If
// the same worker already made the same call under that lease, it gets
// the same answer again; a worker repeats a call until it is
// acknowledged. Anything else is refused.
func repeated(tx logbook.Tx, worker, lease, call, arg string, ack *logbook.Ack) error {
	subject, ok := subjectOf(lease)
	if !ok {
		return contract.LeaseNotHeld
	}
	var done logbook.Finished
	if it, ok := tx.Item(subject); ok {
		done = it.Done
	} else if p, ok := tx.Proposal(subject); ok {
		done = p.Done
	}
	if done.Lease != lease || done.Worker != worker || done.Call != call || done.Arg != arg {
		return contract.LeaseNotHeld
	}
	*ack = done.Ack
	return nil
}

func remember(tx logbook.Tx, e logbook.Entry, done logbook.Finished) {
	switch e.Kind {
	case lifecycle.KindItem:
		if it, ok := tx.Item(e.Subject); ok {
			it.Done = done
			tx.PutItem(it)
		}
	case lifecycle.KindProposal:
		if p, ok := tx.Proposal(e.Subject); ok {
			p.Done = done
			tx.PutProposal(p)
		}
	}
}

func subjectState(tx logbook.Tx, e logbook.Entry) (lifecycle.State, bool) {
	switch e.Kind {
	case lifecycle.KindItem:
		it, ok := tx.Item(e.Subject)
		return it.State, ok
	case lifecycle.KindProposal:
		p, ok := tx.Proposal(e.Subject)
		return p.Status, ok
	}
	return "", false
}

func withResult(old map[string]json.RawMessage, worker string, result json.RawMessage) map[string]json.RawMessage {
	next := make(map[string]json.RawMessage, len(old)+1)
	for k, v := range old {
		next[k] = v
	}
	next[worker] = append(json.RawMessage(nil), result...)
	return next
}

func record(tx logbook.Tx, at time.Time, actor, action, subject string, from, to lifecycle.State, code string) uint64 {
	seq := tx.Next(logbook.CounterLedger)
	tx.Append(logbook.Record{
		Seq: seq, At: at.UTC(), Actor: actor, Action: action,
		Subject: subject, From: from, To: to, Code: code,
	})
	return seq
}

// Record writes one ledger line. It is for the other packages that write
// to the ledger inside their own transactions.
func Record(tx logbook.Tx, at time.Time, actor, action, subject string, from, to lifecycle.State, code string) uint64 {
	return record(tx, at, actor, action, subject, from, to, code)
}
