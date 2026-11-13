// Package queue is the five worker calls: lease, heartbeat, complete,
// fail and needs_subtool, and the replay of paused items.
//
// Every call is one Logbook transaction. A change of state and the next
// queue step are written together, or neither is.
package queue

import (
	"bytes"
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
	Found    *FoundView                 `json:"found,omitempty"`
	Results  map[string]json.RawMessage `json:"results,omitempty"`
	Proposal *ProposalView              `json:"proposal,omitempty"`
	Incident *IncidentView              `json:"incident,omitempty"`
}

// FoundView is where an item was found, as it was recorded.
type FoundView struct {
	Source  string `json:"source"`
	Title   string `json:"title"`
	Address string `json:"address"`
}

// IncidentView is what Bellbuoy is told about an item that was stopped
// or paused: the item, the worker that stopped, the error code, and the
// state the item was in.
type IncidentView struct {
	ID     string          `json:"id"`
	Worker string          `json:"worker"`
	Code   string          `json:"code"`
	State  lifecycle.State `json:"state"`
}

// HeldView is what Countersign is told about a held item under
// results.held: the worker that stopped, and the code it gave.
type HeldView struct {
	Worker string `json:"worker"`
	Code   string `json:"code"`
}

// IncidentWorker is the worker incident steps are for, and ReleaseWorker
// the worker release steps are for.
const (
	IncidentWorker = "bellbuoy"
	ReleaseWorker  = "countersign"
)

// The two decisions the owner can make about a held item.
const (
	DecideRelease = "release"
	DecideKeep    = "keep"
)

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
		state, found := stepState(tx, e)
		if !found || !mayHold(worker, e, state) {
			// The queue says this worker is next, and the tables say it
			// has nothing to do with a subject in this state. Stop work
			// on the subject so the rest of the queue is not blocked.
			e.Held, e.Lease, e.Expires = true, "", time.Time{}
			tx.PutEntry(e)
			record(tx, now, worker, logbook.ActSetAside, e.Key, state, state, string(contract.Parse))
			if _, isItem := tx.Item(e.Subject); isItem && e.Kind == lifecycle.KindItem {
				openIncident(tx, now, e.Subject, worker, contract.Parse, state)
				openRelease(tx, now, e.Subject, worker, contract.Parse, state)
			}
			setAside = true
			return nil
		}
		e.Attempt++
		e.Lease = e.Key + "." + strconv.FormatUint(tx.Next(logbook.CounterLease), 10)
		e.Expires = now.Add(s.LeaseFor)
		tx.PutEntry(e)
		record(tx, now, worker, logbook.ActLease, e.Key, state, state, "")

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
			if it.Address != "" {
				grant.Found = &FoundView{Source: it.Source, Title: it.Title, Address: it.Address}
			}
		case lifecycle.KindProposal:
			p, _ := tx.Proposal(e.Subject)
			grant.Results = p.Results
			grant.Proposal = &ProposalView{
				Target: p.Target, Repository: p.Repository, File: p.File,
				EditKind: p.EditKind, Change: p.Change, Reason: p.Reason,
			}
		case lifecycle.KindRelease:
			it, _ := tx.Item(e.Subject)
			held, err := json.Marshal(HeldView{Worker: e.Incident.Worker, Code: e.Incident.Code})
			if err != nil {
				return contract.Unexpected
			}
			grant.Results = withResult(it.Results, "held", held)
			if it.Address != "" {
				grant.Found = &FoundView{Source: it.Source, Title: it.Title, Address: it.Address}
			}
		case lifecycle.KindIncident:
			grant.Incident = &IncidentView{
				ID: e.Subject, Worker: e.Incident.Worker, Code: e.Incident.Code, State: e.Incident.State,
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if setAside {
		return nil, contract.BadRecord
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
			return repeated(tx, worker, lease, callComplete, completeArg(lease, to, result), &ack)
		}
		now := s.Clock()
		if e.Kind == lifecycle.KindRelease {
			// A release step carries the owner's decision about a held
			// item and changes no state.
			if to != e.Incident.State {
				return contract.MoveNotAllowed
			}
			decision, problem := decisionOf(result)
			if problem != nil {
				return problem
			}
			ack = logbook.Ack{ID: e.Subject, State: to}
			action := logbook.ActKeep
			own, queued := tx.Entry(e.Subject)
			switch {
			case decision == DecideKeep || !queued || !own.Held:
				// Kept, or there is nothing held to release any more.
				ack.Held = queued && own.Held
			default:
				action = logbook.ActRelease
				own.Held, own.Lease, own.Expires = false, "", time.Time{}
				own.Lane, own.Arrival = lifecycle.Urgent, tx.Next(logbook.CounterArrival)
				tx.PutEntry(own)
				ack.Next = own.Worker
			}
			if it, ok := tx.Item(e.Subject); ok {
				if ack.Next != "" {
					it.Lane = lifecycle.Urgent
				}
				it.ReleaseDone = logbook.Finished{Lease: lease, Worker: worker, Call: callComplete, Arg: completeArg(lease, to, result), Ack: ack}
				tx.PutItem(it)
			}
			tx.DeleteEntry(e.Key)
			record(tx, now, worker, action, e.Subject, to, to, "")
			return nil
		}
		if e.Kind == lifecycle.KindIncident {
			// An incident step changes no state. Completing it says the
			// incident has been dealt with, and nothing more.
			if to != e.Incident.State {
				return contract.MoveNotAllowed
			}
			if result != nil {
				return contract.BadField
			}
			ack = logbook.Ack{ID: e.Subject, State: to}
			if it, ok := tx.Item(e.Subject); ok {
				it.IncidentDone = logbook.Finished{Lease: lease, Worker: worker, Call: callComplete, Arg: string(to), Ack: ack}
				tx.PutItem(it)
			}
			tx.DeleteEntry(e.Key)
			record(tx, now, worker, logbook.ActComplete, e.Key, to, to, "")
			return nil
		}

		from, _ := stepState(tx, e)
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
			p, _ := tx.Proposal(e.Subject)
			p.Status, p.Done = to, done
			if result != nil {
				p.Results = withResult(p.Results, worker, result)
			}
			tx.PutProposal(p)
			if from != to && s.Notify != nil {
				id := p.ID
				after = func() { s.Notify.Edit(id, to, now) }
			}
		}

		if route.Next == "" {
			tx.DeleteEntry(e.Key)
		} else {
			next := logbook.Entry{
				Key:     e.Key,
				Subject: e.Subject,
				Kind:    e.Kind,
				Worker:  route.Next,
				Lane:    lane,
				Arrival: tx.Next(logbook.CounterArrival),
			}
			if route.AfterDays > 0 {
				next.NotBefore = now.AddDate(0, 0, route.AfterDays).UTC()
			}
			tx.PutEntry(next)
		}
		record(tx, now, worker, logbook.ActComplete, e.Key, from, to, "")
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
// E_TIMEOUT leaves the step in the queue to be tried again. E_PARSE
// stops work on it: the subject keeps its state and the step is never
// offered again, unless the owner releases it. When the step is an
// item's own, Bellbuoy is given one incident step about it and
// Countersign one release step.
// E_NEEDS_SUBTOOL has a call of its own and is refused here.
func (s *Service) Fail(ctx context.Context, worker, lease string, code contract.Code) (*logbook.Ack, error) {
	switch code {
	case contract.Timeout, contract.Parse:
	case contract.NeedsSubtool:
		return nil, contract.UseNeedsSubtool
	default:
		return nil, contract.BadField
	}
	var ack logbook.Ack
	err := s.Store.Update(ctx, func(tx logbook.Tx) error {
		e, ok := held(tx, worker, lease)
		if !ok {
			return repeated(tx, worker, lease, callFail, string(code), &ack)
		}
		now := s.Clock()
		state, _ := stepState(tx, e)
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
		record(tx, now, worker, logbook.ActFail, e.Key, state, state, string(code))
		if code == contract.Parse && e.Kind == lifecycle.KindItem {
			openIncident(tx, now, e.Subject, worker, contract.Parse, state)
			openRelease(tx, now, e.Subject, worker, contract.Parse, state)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &ack, nil
}

// NeedsSubtool pauses an item the worker has no rule for. The item
// remembers the state it was in and which worker paused it, and leaves
// the queue until that worker is replayed. Bellbuoy is given one
// incident step about it.
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
		tx.DeleteEntry(e.Key)
		openIncident(tx, now, it.ID, worker, contract.NeedsSubtool, from)
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
			Key:     it.ID,
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

// openRelease gives Countersign one release step about a held item, so
// that the owner can decide whether it runs again.
func openRelease(tx logbook.Tx, now time.Time, itemID, worker string, code contract.Code, state lifecycle.State) {
	tx.PutEntry(logbook.Entry{
		Key:      logbook.ReleaseKey(itemID),
		Subject:  itemID,
		Kind:     lifecycle.KindRelease,
		Worker:   ReleaseWorker,
		Lane:     lifecycle.Normal,
		Arrival:  tx.Next(logbook.CounterArrival),
		Incident: logbook.Incident{Worker: worker, Code: string(code), State: state},
	})
}

// decisionOf reads the owner's decision from the result of a release
// step: exactly {"decision": "release"} or {"decision": "keep"}.
func decisionOf(result json.RawMessage) (string, *contract.Problem) {
	if result == nil {
		return "", contract.MissingField
	}
	var r struct {
		Decision *string `json:"decision"`
	}
	dec := json.NewDecoder(bytes.NewReader(result))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil || r.Decision == nil {
		return "", contract.BadField
	}
	if *r.Decision != DecideRelease && *r.Decision != DecideKeep {
		return "", contract.BadField
	}
	return *r.Decision, nil
}

// completeArg is what tells one complete call from another under the
// same lease: the state, and for a release step the decision too.
func completeArg(lease string, to lifecycle.State, result json.RawMessage) string {
	arg := string(to)
	if strings.HasPrefix(lease, logbook.ReleaseKey("")) {
		if decision, problem := decisionOf(result); problem == nil {
			arg += " " + decision
		}
	}
	return arg
}

// openIncident gives Bellbuoy one incident step about an item. An item
// has at most one; a new incident takes the place of one not yet dealt
// with.
func openIncident(tx logbook.Tx, now time.Time, itemID, worker string, code contract.Code, state lifecycle.State) {
	tx.PutEntry(logbook.Entry{
		Key:      logbook.IncidentKey(itemID),
		Subject:  itemID,
		Kind:     lifecycle.KindIncident,
		Worker:   IncidentWorker,
		Lane:     lifecycle.Normal,
		Arrival:  tx.Next(logbook.CounterArrival),
		Incident: logbook.Incident{Worker: worker, Code: string(code), State: state},
	})
	record(tx, now, worker, logbook.ActIncident, itemID, state, state, string(code))
}

var leaseForm = regexp.MustCompile(`^((?:incident:|release:)?` + lifecycle.ItemIDForm + `|` + lifecycle.ProposalIDForm + `)\.[1-9][0-9]{0,18}$`)

// keyOf returns the queue key a lease was issued for.
func keyOf(lease string) (string, bool) {
	if !leaseForm.MatchString(lease) {
		return "", false
	}
	return lease[:strings.LastIndexByte(lease, '.')], true
}

// held returns the entry a worker holds under a lease. A lease is held
// until the step ends or the entry is leased again after it expired; an
// expired lease nobody has taken over still counts, so a finished result
// is not thrown away because it arrived late.
func held(tx logbook.Tx, worker, lease string) (logbook.Entry, bool) {
	key, ok := keyOf(lease)
	if !ok {
		return logbook.Entry{}, false
	}
	e, ok := tx.Entry(key)
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
	key, ok := keyOf(lease)
	if !ok {
		return contract.LeaseNotHeld
	}
	var done logbook.Finished
	if id, isIncident := strings.CutPrefix(key, logbook.IncidentKey("")); isIncident {
		if it, ok := tx.Item(id); ok {
			done = it.IncidentDone
		}
	} else if id, isRelease := strings.CutPrefix(key, logbook.ReleaseKey("")); isRelease {
		if it, ok := tx.Item(id); ok {
			done = it.ReleaseDone
		}
	} else if it, ok := tx.Item(key); ok {
		done = it.Done
	} else if p, ok := tx.Proposal(key); ok {
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
	case lifecycle.KindItem, lifecycle.KindIncident, lifecycle.KindRelease:
		if it, ok := tx.Item(e.Subject); ok {
			switch e.Kind {
			case lifecycle.KindIncident:
				it.IncidentDone = done
			case lifecycle.KindRelease:
				it.ReleaseDone = done
			default:
				it.Done = done
			}
			tx.PutItem(it)
		}
	case lifecycle.KindProposal:
		if p, ok := tx.Proposal(e.Subject); ok {
			p.Done = done
			tx.PutProposal(p)
		}
	}
}

// stepState returns the state a step is about: the item's state, the
// proposal's status, or for an incident or a release the state the item
// was stopped in.
func stepState(tx logbook.Tx, e logbook.Entry) (lifecycle.State, bool) {
	switch e.Kind {
	case lifecycle.KindItem:
		it, ok := tx.Item(e.Subject)
		return it.State, ok
	case lifecycle.KindProposal:
		p, ok := tx.Proposal(e.Subject)
		return p.Status, ok
	case lifecycle.KindIncident, lifecycle.KindRelease:
		_, ok := tx.Item(e.Subject)
		return e.Incident.State, ok
	}
	return "", false
}

// mayHold reports whether the tables give this worker a step of this
// kind on a subject in this state.
func mayHold(worker string, e logbook.Entry, state lifecycle.State) bool {
	switch e.Kind {
	case lifecycle.KindIncident:
		return worker == IncidentWorker
	case lifecycle.KindRelease:
		return worker == ReleaseWorker
	}
	return lifecycle.Holds(worker, e.Kind, state)
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
