// Package sql is Logbook itself: the store kept in PostgreSQL, reached
// with the muster role.
//
// Everything Muster keeps lives in Logbook's tables (migrations 0001 to
// 0010), so nothing is held in process memory beside the database and
// nothing is lost when Muster restarts:
//
//	core.item              an item's state, title, address and source
//	core.queue             each step, its lane, whether it is held, and its lease
//	core.ledger            every change of state, failure, pause and release
//	core.pause             which worker paused an item, and from which state
//	core.edit_proposal     a proposal's element, repository, class, text, reason, file and status
//	core.result            the latest JSON object each worker returned for an item or proposal
//	core.lease_outcome     how the last lease on each subject ended, for repeated calls
//	core.evaluation        what Assay found, when its result is an evaluation
//	core.decision          what the owner decided about an evaluated item
//	core.component_health  each component's latest health state, error code and image digest
//	core.submission_count  daily salted visitor submission counts
//
// Logbook gives out every identifier. A new item is inserted without
// one, and an address already on record returns the item that has it.
//
// Every transaction runs alone: one at a time, on one connection. Muster
// is one pod and the only writer, so nothing is lost by it, and a change
// of state, its ledger row and the next queue step are one PostgreSQL
// transaction.
//
// Times come from Muster's clock, never from the database's, so that the
// same calls give the same record on any machine.
package sql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ClearedStack/muster/internal/contract"
	"github.com/ClearedStack/muster/internal/lifecycle"
	"github.com/ClearedStack/muster/internal/logbook"
)

// incidentWorker is the worker whose steps on an item are incident
// steps. Logbook's queue has no column for the kind of a step, and
// Bellbuoy has no other step on an item, so the worker tells the kind.
const incidentWorker = "bellbuoy"

// pausePattern is what Muster writes where Logbook asks for the pattern
// of an unfamiliar case. The contract's needs_subtool call carries none.
const pausePattern = "not given"

// keysPerDay caps how many distinct visitor hashes one UTC day keeps in
// core.submission_count, matching the in-memory stand-in's bound.
const keysPerDay = 8192

// Store is Logbook.
type Store struct {
	mu     sync.Mutex
	config *pgx.ConnConfig
	clock  func() time.Time
	conn   *pgx.Conn
}

var _ logbook.Store = (*Store)(nil)

// Open reads the address of the database. It does not connect; the first
// transaction does. An address that cannot be read is refused without
// being repeated, since it can hold a password.
func Open(url string, clock func() time.Time) (*Store, error) {
	return OpenWithPassword(url, "", clock)
}

// OpenWithPassword is Open, with a password kept separate from the
// address so that characters in the secret need not be escaped for a
// URL. An empty password leaves whatever the address or environment gave.
func OpenWithPassword(url, password string, clock func() time.Time) (*Store, error) {
	config, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, errors.New("MUSTER_DATABASE_URL is not a database address")
	}
	if password != "" {
		config.Password = password
	}
	// Each statement goes in one round trip with no prepared statements
	// kept on the connection across reconnections.
	config.DefaultQueryExecMode = pgx.QueryExecModeExec
	return &Store{config: config, clock: clock}, nil
}

// Close ends the connection.
func (s *Store) Close(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drop(ctx)
}

func (s *Store) drop(ctx context.Context) {
	if s.conn != nil {
		_ = s.conn.Close(ctx)
		s.conn = nil
	}
}

// Update runs fn in one PostgreSQL transaction.
func (s *Store) Update(ctx context.Context, fn func(logbook.Tx) error) error {
	return s.run(ctx, fn, true)
}

// View runs fn in a transaction that is always rolled back.
func (s *Store) View(ctx context.Context, fn func(logbook.Tx) error) error {
	return s.run(ctx, fn, false)
}

func (s *Store) run(ctx context.Context, fn func(logbook.Tx) error, keep bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.conn == nil || s.conn.IsClosed() {
		conn, err := pgx.ConnectConfig(ctx, s.config)
		if err != nil {
			return translate(ctx, err)
		}
		s.conn = conn
	}
	pg, err := s.conn.Begin(ctx)
	if err != nil {
		s.drop(ctx)
		return translate(ctx, err)
	}
	t := &tx{ctx: ctx, pg: pg, now: s.clock().UTC()}
	err = fn(t)
	if err == nil {
		err = t.err
	}
	if err != nil || !keep {
		// The context may be the reason; roll back regardless of it.
		undo, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if pg.Rollback(undo) != nil {
			s.drop(undo)
		}
		if err == nil {
			return nil
		}
		return translate(ctx, err)
	}
	if err := pg.Commit(ctx); err != nil {
		s.drop(context.WithoutCancel(ctx))
		return translate(ctx, err)
	}
	return nil
}

// translate turns an error from the database into one of the fixed
// refusals, or passes on a refusal Muster made itself. Nothing the
// database said is passed on: it can name a host, a role or a value.
func translate(ctx context.Context, err error) error {
	var problem *contract.Problem
	if errors.As(err, &problem) {
		return problem
	}
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return context.DeadlineExceeded
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// Logbook's own checks refuse a row with one of the public
		// codes at the start of the message.
		if pgErr.Code == "P0001" {
			switch {
			case strings.HasPrefix(pgErr.Message, string(contract.Parse)+":"):
				return contract.LogbookRefused
			case strings.HasPrefix(pgErr.Message, string(contract.NeedsSubtool)+":"):
				return contract.LogbookUnfamiliar
			}
		}
		return contract.Unexpected
	}
	// Anything else is the connection: the database is not answering.
	return contract.StoreSlow
}

type tx struct {
	ctx context.Context
	pg  pgx.Tx
	now time.Time
	err error // the first failure; nothing is done after it
}

func (t *tx) fail(err error) {
	if t.err == nil && err != nil {
		t.err = err
	}
}

func (t *tx) exec(query string, args ...any) {
	if t.err != nil {
		return
	}
	_, err := t.pg.Exec(t.ctx, query, args...)
	t.fail(err)
}

// one runs a query that returns at most one row and reports whether it
// returned one.
func (t *tx) one(dest []any, query string, args ...any) bool {
	if t.err != nil {
		return false
	}
	err := t.pg.QueryRow(t.ctx, query, args...).Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	t.fail(err)
	return err == nil
}

// strings runs a query that returns one text column.
func (t *tx) strings(query string, args ...any) []string {
	if t.err != nil {
		return nil
	}
	rows, err := t.pg.Query(t.ctx, query, args...)
	if err != nil {
		t.fail(err)
		return nil
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	t.fail(err)
	return out
}

func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func compactJSON(s string) json.RawMessage {
	var buf bytes.Buffer
	if json.Compact(&buf, []byte(s)) != nil {
		return json.RawMessage(s)
	}
	return json.RawMessage(buf.Bytes())
}

// ---------------------------------------------------------------------
// Items
// ---------------------------------------------------------------------

func (t *tx) Item(id string) (logbook.Item, bool) {
	if !lifecycle.IsItemID(id) {
		return logbook.Item{}, false
	}
	var state, title, address, source string
	var lane, pausedBy, pausedFrom *string
	var pauseID *int64
	if !t.one([]any{&state, &title, &address, &source, &lane, &pausedBy, &pausedFrom, &pauseID}, `
		SELECT i.state, i.title, coalesce(i.address, ''), coalesce(i.source, ''),
		       q.lane, p.worker, p.paused_from, p.pause_id
		FROM core.item i
		LEFT JOIN core.queue q ON q.item_id = i.item_id AND q.worker <> $2
		LEFT JOIN core.pause p ON p.item_id = i.item_id AND p.replayed_at IS NULL
		WHERE i.item_id = $1
		ORDER BY q.queue_id
		LIMIT 1`, id, incidentWorker) {
		return logbook.Item{}, false
	}
	it := logbook.Item{
		ID:      id,
		State:   lifecycle.State(state),
		Title:   title,
		Address: address,
		Source:  source,
	}
	if lane != nil {
		it.Lane = lifecycle.Lane(*lane)
	}
	if pauseID != nil {
		it.PausedBy, it.PausedFrom, it.PausedSeq = *pausedBy, lifecycle.State(*pausedFrom), uint64(*pauseID)
	}
	it.Results = t.itemResults(id)
	it.Done, it.IncidentDone = t.itemOutcomes(id)
	return it, true
}

func (t *tx) AddItem(v logbook.Item) (string, bool) {
	address := optionalString(v.Address)
	if address != nil {
		var id string
		if t.one([]any{&id}, `SELECT item_id FROM core.item WHERE address_sha256 = core.address_sha256($1)`, v.Address) {
			return id, true
		}
	}
	// No identifier is sent: Logbook's default gives it. If another
	// writer recorded the address in the meantime, nothing is inserted
	// and the row that has it is returned.
	var id string
	if !t.one([]any{&id}, `
		INSERT INTO core.item (state, title, address, source) VALUES ($1, $2, $3, $4)
		ON CONFLICT (address_sha256) DO NOTHING
		RETURNING item_id`, string(v.State), v.Title, address, optionalString(v.Source)) {
		if address == nil || !t.one([]any{&id}, `SELECT item_id FROM core.item WHERE address_sha256 = core.address_sha256($1)`, v.Address) {
			return "", false
		}
		return id, true
	}
	if t.err != nil {
		return "", false
	}
	t.putItemResults(id, v.Results)
	t.putItemOutcomes(id, v.Done, v.IncidentDone)
	return id, false
}

func (t *tx) PutItem(v logbook.Item) {
	var wasState, wasSource string
	if !t.one([]any{&wasState, &wasSource}, `SELECT state, coalesce(source, '') FROM core.item WHERE item_id = $1`, v.ID) {
		return
	}
	paused := string(lifecycle.NeedsSubtool)
	switch {
	case string(v.State) == paused && wasState != paused:
		t.exec(`INSERT INTO core.pause (item_id, worker, pattern, paused_from, paused_at) VALUES ($1, $2, $3, $4, $5)`,
			v.ID, v.PausedBy, pausePattern, string(v.PausedFrom), t.now)
	case string(v.State) != paused && wasState == paused:
		t.exec(`UPDATE core.pause SET replayed_at = $2 WHERE item_id = $1 AND replayed_at IS NULL`, v.ID, t.now)
	}
	if string(v.State) != wasState || v.Source != wasSource {
		t.exec(`UPDATE core.item SET state = $2, source = $3 WHERE item_id = $1`,
			v.ID, string(v.State), optionalString(v.Source))
	}
	t.putItemResults(v.ID, v.Results)
	t.putItemOutcomes(v.ID, v.Done, v.IncidentDone)
}

func (t *tx) Paused(worker string) []logbook.Item {
	var out []logbook.Item
	for _, id := range t.strings(`SELECT item_id FROM core.pause WHERE worker = $1 AND replayed_at IS NULL ORDER BY pause_id`, worker) {
		if it, ok := t.Item(id); ok {
			out = append(out, it)
		}
	}
	return out
}

// ---------------------------------------------------------------------
// Proposals. Logbook numbers them; on the wire a proposal is EP- and the
// number, padded to four digits.
// ---------------------------------------------------------------------

func proposalNumber(id string) (int64, bool) {
	if !lifecycle.IsProposalID(id) {
		return 0, false
	}
	n, err := strconv.ParseInt(id[len("EP-"):], 10, 64)
	return n, err == nil
}

func (t *tx) Proposal(id string) (logbook.Proposal, bool) {
	n, ok := proposalNumber(id)
	if !ok {
		return logbook.Proposal{}, false
	}
	var element, repository, class, payload, reason, filePath, status string
	var submitted time.Time
	if !t.one([]any{&element, &repository, &class, &payload, &reason, &filePath, &status, &submitted}, `
		SELECT element_id, owner_repo, edit_class, payload, coalesce(reason, ''), coalesce(file_path, ''), status, submitted_at
		FROM core.edit_proposal WHERE proposal_id = $1`, n) {
		return logbook.Proposal{}, false
	}
	p := logbook.Proposal{
		ID:         id,
		Target:     element,
		Repository: repository,
		File:       filePath,
		EditKind:   class,
		Change:     payload,
		Reason:     reason,
		Status:     lifecycle.State(status),
		Received:   submitted.UTC(),
		Results:    t.proposalResults(n),
		Done:       t.proposalOutcome(id, n),
	}
	return p, true
}

func (t *tx) AddProposal(v logbook.Proposal) string {
	var n int64
	if !t.one([]any{&n}, `
		INSERT INTO core.edit_proposal (element_id, owner_repo, edit_class, payload, reason, file_path, status, submitted_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING proposal_id`,
		v.Target, v.Repository, v.EditKind, v.Change, optionalString(v.Reason), optionalString(v.File), string(v.Status), v.Received) {
		return ""
	}
	if n < 1 || n > lifecycle.MaxID {
		t.fail(contract.Unexpected)
		return ""
	}
	t.putProposalResults(n, v.Results)
	t.putProposalOutcome(n, v.Done)
	return lifecycle.ProposalID(uint64(n))
}

func (t *tx) PutProposal(v logbook.Proposal) {
	n, ok := proposalNumber(v.ID)
	if !ok {
		return
	}
	// decided_at is set the first time a proposal leaves pending_approval.
	t.exec(`
		UPDATE core.edit_proposal
		SET status = $2,
		    decided_at = CASE WHEN $2 = 'pending_approval' THEN NULL ELSE coalesce(decided_at, $3) END
		WHERE proposal_id = $1 AND status IS DISTINCT FROM $2`, n, string(v.Status), t.now)
	t.putProposalResults(n, v.Results)
	t.putProposalOutcome(n, v.Done)
}

func (t *tx) Building(repository string) []logbook.Proposal {
	var out []logbook.Proposal
	for _, n := range t.strings(`
		SELECT proposal_id::text FROM core.edit_proposal
		WHERE status = 'building' AND owner_repo = $1 ORDER BY proposal_id`, repository) {
		number, err := strconv.ParseUint(n, 10, 64)
		if err != nil {
			continue
		}
		if p, ok := t.Proposal(lifecycle.ProposalID(number)); ok {
			out = append(out, p)
		}
	}
	return out
}

// ---------------------------------------------------------------------
// Worker results and lease outcomes
// ---------------------------------------------------------------------

func (t *tx) itemResults(itemID string) map[string]json.RawMessage {
	if t.err != nil {
		return nil
	}
	rows, err := t.pg.Query(t.ctx, `SELECT worker, payload::text FROM core.result WHERE item_id = $1`, itemID)
	if err != nil {
		t.fail(err)
		return nil
	}
	var out map[string]json.RawMessage
	var worker, payload string
	_, err = pgx.ForEachRow(rows, []any{&worker, &payload}, func() error {
		if out == nil {
			out = map[string]json.RawMessage{}
		}
		out[worker] = compactJSON(payload)
		return nil
	})
	t.fail(err)
	return out
}

func (t *tx) proposalResults(proposalID int64) map[string]json.RawMessage {
	if t.err != nil {
		return nil
	}
	rows, err := t.pg.Query(t.ctx, `SELECT worker, payload::text FROM core.result WHERE proposal_id = $1`, proposalID)
	if err != nil {
		t.fail(err)
		return nil
	}
	var out map[string]json.RawMessage
	var worker, payload string
	_, err = pgx.ForEachRow(rows, []any{&worker, &payload}, func() error {
		if out == nil {
			out = map[string]json.RawMessage{}
		}
		out[worker] = compactJSON(payload)
		return nil
	})
	t.fail(err)
	return out
}

func sortedWorkers(results map[string]json.RawMessage) []string {
	workers := make([]string, 0, len(results))
	for w := range results {
		workers = append(workers, w)
	}
	sort.Strings(workers)
	return workers
}

func (t *tx) putItemResults(itemID string, results map[string]json.RawMessage) {
	for _, worker := range sortedWorkers(results) {
		t.exec(`
			INSERT INTO core.result (item_id, worker, payload, recorded_at)
			VALUES ($1, $2, $3::jsonb, $4)
			ON CONFLICT (item_id, worker) WHERE item_id IS NOT NULL
			DO UPDATE SET payload = EXCLUDED.payload, recorded_at = EXCLUDED.recorded_at
			WHERE core.result.payload IS DISTINCT FROM EXCLUDED.payload`,
			itemID, worker, string(results[worker]), t.now)
	}
}

func (t *tx) putProposalResults(proposalID int64, results map[string]json.RawMessage) {
	for _, worker := range sortedWorkers(results) {
		t.exec(`
			INSERT INTO core.result (proposal_id, worker, payload, recorded_at)
			VALUES ($1, $2, $3::jsonb, $4)
			ON CONFLICT (proposal_id, worker) WHERE proposal_id IS NOT NULL
			DO UPDATE SET payload = EXCLUDED.payload, recorded_at = EXCLUDED.recorded_at
			WHERE core.result.payload IS DISTINCT FROM EXCLUDED.payload`,
			proposalID, worker, string(results[worker]), t.now)
	}
}

func (t *tx) itemOutcomes(itemID string) (done, incident logbook.Finished) {
	if t.err != nil {
		return done, incident
	}
	rows, err := t.pg.Query(t.ctx, `
		SELECT is_incident, lease, worker, call_name, coalesce(call_arg, ''),
		       ack_state, coalesce(ack_next, ''), ack_held
		FROM core.lease_outcome WHERE item_id = $1`, itemID)
	if err != nil {
		t.fail(err)
		return done, incident
	}
	var isIncident, ackHeld bool
	var lease, worker, callName, callArg, ackState, ackNext string
	_, err = pgx.ForEachRow(rows, []any{&isIncident, &lease, &worker, &callName, &callArg, &ackState, &ackNext, &ackHeld}, func() error {
		f := logbook.Finished{
			Lease:  lease,
			Worker: worker,
			Call:   callName,
			Arg:    callArg,
			Ack: logbook.Ack{
				ID:    itemID,
				State: lifecycle.State(ackState),
				Next:  ackNext,
				Held:  ackHeld,
			},
		}
		if isIncident {
			incident = f
		} else {
			done = f
		}
		return nil
	})
	t.fail(err)
	return done, incident
}

func (t *tx) putItemOutcomes(itemID string, done, incident logbook.Finished) {
	t.putItemOutcome(itemID, false, done)
	t.putItemOutcome(itemID, true, incident)
}

func (t *tx) putItemOutcome(itemID string, isIncident bool, d logbook.Finished) {
	if d.Lease == "" {
		t.exec(`DELETE FROM core.lease_outcome WHERE item_id = $1 AND is_incident = $2`, itemID, isIncident)
		return
	}
	t.exec(`
		INSERT INTO core.lease_outcome (item_id, is_incident, lease, worker, call_name, call_arg, ack_state, ack_next, ack_held, recorded_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (item_id, is_incident) WHERE item_id IS NOT NULL
		DO UPDATE SET lease = EXCLUDED.lease, worker = EXCLUDED.worker, call_name = EXCLUDED.call_name,
		              call_arg = EXCLUDED.call_arg, ack_state = EXCLUDED.ack_state, ack_next = EXCLUDED.ack_next,
		              ack_held = EXCLUDED.ack_held, recorded_at = EXCLUDED.recorded_at
		WHERE (core.lease_outcome.lease, core.lease_outcome.worker, core.lease_outcome.call_name,
		       core.lease_outcome.call_arg, core.lease_outcome.ack_state, core.lease_outcome.ack_next,
		       core.lease_outcome.ack_held)
		   IS DISTINCT FROM
		      (EXCLUDED.lease, EXCLUDED.worker, EXCLUDED.call_name,
		       EXCLUDED.call_arg, EXCLUDED.ack_state, EXCLUDED.ack_next,
		       EXCLUDED.ack_held)`,
		itemID, isIncident, d.Lease, d.Worker, d.Call, optionalString(d.Arg),
		string(d.Ack.State), optionalString(d.Ack.Next), d.Ack.Held, t.now)
}

func (t *tx) proposalOutcome(proposalID string, n int64) logbook.Finished {
	var lease, worker, callName, callArg, ackState, ackNext string
	var ackHeld bool
	if !t.one([]any{&lease, &worker, &callName, &callArg, &ackState, &ackNext, &ackHeld}, `
		SELECT lease, worker, call_name, coalesce(call_arg, ''),
		       ack_state, coalesce(ack_next, ''), ack_held
		FROM core.lease_outcome WHERE proposal_id = $1`, n) {
		return logbook.Finished{}
	}
	return logbook.Finished{
		Lease:  lease,
		Worker: worker,
		Call:   callName,
		Arg:    callArg,
		Ack: logbook.Ack{
			ID:    proposalID,
			State: lifecycle.State(ackState),
			Next:  ackNext,
			Held:  ackHeld,
		},
	}
}

func (t *tx) putProposalOutcome(n int64, d logbook.Finished) {
	if d.Lease == "" {
		t.exec(`DELETE FROM core.lease_outcome WHERE proposal_id = $1`, n)
		return
	}
	t.exec(`
		INSERT INTO core.lease_outcome (proposal_id, is_incident, lease, worker, call_name, call_arg, ack_state, ack_next, ack_held, recorded_at)
		VALUES ($1, false, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (proposal_id) WHERE proposal_id IS NOT NULL
		DO UPDATE SET lease = EXCLUDED.lease, worker = EXCLUDED.worker, call_name = EXCLUDED.call_name,
		              call_arg = EXCLUDED.call_arg, ack_state = EXCLUDED.ack_state, ack_next = EXCLUDED.ack_next,
		              ack_held = EXCLUDED.ack_held, recorded_at = EXCLUDED.recorded_at
		WHERE (core.lease_outcome.lease, core.lease_outcome.worker, core.lease_outcome.call_name,
		       core.lease_outcome.call_arg, core.lease_outcome.ack_state, core.lease_outcome.ack_next,
		       core.lease_outcome.ack_held)
		   IS DISTINCT FROM
		      (EXCLUDED.lease, EXCLUDED.worker, EXCLUDED.call_name,
		       EXCLUDED.call_arg, EXCLUDED.ack_state, EXCLUDED.ack_next,
		       EXCLUDED.ack_held)`,
		n, d.Lease, d.Worker, d.Call, optionalString(d.Arg),
		string(d.Ack.State), optionalString(d.Ack.Next), d.Ack.Held, t.now)
}

// ---------------------------------------------------------------------
// The queue
// ---------------------------------------------------------------------

// where says which row of core.queue a key names.
func where(key string) (clause string, arg any, kind lifecycle.Kind, subject string, ok bool) {
	if id, isIncident := strings.CutPrefix(key, logbook.IncidentKey("")); isIncident {
		if !lifecycle.IsItemID(id) {
			return "", nil, "", "", false
		}
		return "item_id = $1 AND worker = '" + incidentWorker + "'", id, lifecycle.KindIncident, id, true
	}
	if lifecycle.IsItemID(key) {
		return "item_id = $1 AND worker <> '" + incidentWorker + "'", key, lifecycle.KindItem, key, true
	}
	if n, isProposal := proposalNumber(key); isProposal {
		return "proposal_id = $1", n, lifecycle.KindProposal, key, true
	}
	return "", nil, "", "", false
}

type row struct {
	id       int64
	worker   string
	lane     string
	item     *string
	proposal *int64
	queued   time.Time
	attempt  int
	held     bool
	token    *string
	until    *time.Time
}

const rowColumns = `queue_id, worker, lane, item_id, proposal_id, queued_at, attempt, held, lease_token::text, leased_until`

func (r *row) dest() []any {
	return []any{&r.id, &r.worker, &r.lane, &r.item, &r.proposal, &r.queued, &r.attempt, &r.held, &r.token, &r.until}
}

// leaseToken is the token Logbook holds for a live lease. It is made
// from the step's place in the order of arrival and the attempt, the two
// numbers the lease itself names, so the same lease always has the same
// token.
func leaseToken(arrival uint64, attempt int) string {
	return fmt.Sprintf("%08x-0000-0000-%04x-%012x", uint32(attempt), arrival>>48, arrival&0xffffffffffff)
}

func (t *tx) entry(r row) logbook.Entry {
	e := logbook.Entry{
		Worker:    r.worker,
		Lane:      lifecycle.Lane(r.lane),
		Arrival:   uint64(r.id),
		Attempt:   r.attempt,
		NotBefore: r.queued.UTC(),
	}
	switch {
	case r.proposal != nil:
		e.Kind, e.Subject = lifecycle.KindProposal, lifecycle.ProposalID(uint64(*r.proposal))
		e.Key = e.Subject
	case r.item != nil && r.worker == incidentWorker:
		e.Kind, e.Subject = lifecycle.KindIncident, *r.item
		e.Key = logbook.IncidentKey(e.Subject)
		// What the incident reports is in the ledger: the last time a
		// worker stopped or paused the item.
		var actor, code string
		var from *string
		if t.one([]any{&actor, &code, &from}, `
			SELECT actor, error_code, from_state FROM core.ledger
			WHERE item_id = $1 AND error_code IS NOT NULL AND note IN ('fail', 'needs_subtool', 'set_aside')
			ORDER BY entry_id DESC LIMIT 1`, e.Subject) && from != nil {
			e.Incident = logbook.Incident{Worker: actor, Code: code, State: lifecycle.State(*from)}
		}
	case r.item != nil:
		e.Kind, e.Subject = lifecycle.KindItem, *r.item
		e.Key = e.Subject
	}
	switch {
	case r.held:
		e.Held = true
	case r.token != nil:
		e.Lease = e.Key + "." + strconv.FormatUint(e.Arrival, 10) + "." + strconv.Itoa(e.Attempt)
		if r.until != nil {
			e.Expires = r.until.UTC()
		}
	}
	return e
}

func (t *tx) Entry(key string) (logbook.Entry, bool) {
	clause, arg, _, _, ok := where(key)
	if !ok {
		return logbook.Entry{}, false
	}
	var r row
	if !t.one(r.dest(), `SELECT `+rowColumns+` FROM core.queue WHERE `+clause+` ORDER BY queue_id LIMIT 1`, arg) {
		return logbook.Entry{}, false
	}
	return t.entry(r), true
}

func (t *tx) NextEntry(worker string, now time.Time) (logbook.Entry, bool) {
	var r row
	if !t.one(r.dest(), `
		SELECT `+rowColumns+` FROM core.queue
		WHERE worker = $1 AND NOT held AND queued_at <= $2 AND (lease_token IS NULL OR leased_until <= $2)
		ORDER BY lane_rank, queue_id
		LIMIT 1`, worker, now.UTC()) {
		return logbook.Entry{}, false
	}
	return t.entry(r), true
}

func (t *tx) PutEntry(e logbook.Entry) {
	clause, arg, kind, subject, ok := where(e.Key)
	if !ok {
		t.fail(contract.Unexpected)
		return
	}
	var token *string
	var until, beat *time.Time
	if !e.Held && e.Lease != "" {
		made := leaseToken(e.Arrival, e.Attempt)
		expires := e.Expires.UTC()
		token, until, beat = &made, &expires, &t.now
	}
	if e.Arrival != 0 {
		// The step keeps its place. Only its attempt count, held flag and
		// lease can change.
		t.exec(`
			UPDATE core.queue
			SET attempt = $2, held = $3, lease_token = $4::uuid, leased_until = $5, heartbeat_at = $6
			WHERE queue_id = $1`, int64(e.Arrival), e.Attempt, e.Held, token, until, beat)
		return
	}
	// A new step takes the place of any step with the same key and joins
	// the end of its lane.
	t.exec(`DELETE FROM core.queue WHERE `+clause, arg)
	var item *string
	var proposal *int64
	if kind == lifecycle.KindProposal {
		n, _ := proposalNumber(subject)
		proposal = &n
	} else {
		item = &subject
	}
	queued := t.now
	if !e.NotBefore.IsZero() {
		queued = e.NotBefore.UTC()
	}
	t.exec(`
		INSERT INTO core.queue (worker, lane, item_id, proposal_id, queued_at, attempt, held, lease_token, leased_until, heartbeat_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::uuid, $9, $10)`,
		e.Worker, string(e.Lane), item, proposal, queued, e.Attempt, e.Held, token, until, beat)
}

func (t *tx) DeleteEntry(key string) {
	if clause, arg, _, _, ok := where(key); ok {
		t.exec(`DELETE FROM core.queue WHERE `+clause, arg)
	}
}

func (t *tx) Depths(now time.Time) logbook.Depths {
	var d logbook.Depths
	if t.err != nil {
		return d
	}
	rows, err := t.pg.Query(t.ctx, `
		SELECT lane, count(*) FROM core.queue
		WHERE NOT held AND queued_at <= $1 AND (lease_token IS NULL OR leased_until <= $1)
		GROUP BY lane`, now.UTC())
	if err != nil {
		t.fail(err)
		return d
	}
	var lane string
	var n int
	_, err = pgx.ForEachRow(rows, []any{&lane, &n}, func() error {
		switch lifecycle.Lane(lane) {
		case lifecycle.Urgent:
			d.Urgent = n
		case lifecycle.Normal:
			d.Normal = n
		case lifecycle.Backfill:
			d.Backfill = n
		}
		return nil
	})
	t.fail(err)
	t.one([]any{&d.NeedsSubtool}, `SELECT count(*) FROM core.item WHERE state = 'needs_subtool'`)
	return d
}

// ---------------------------------------------------------------------
// The ledger
// ---------------------------------------------------------------------

func (t *tx) Append(r logbook.Record) uint64 {
	// Logbook's ledger is the history of each item and proposal: its
	// changes of state, its failures, its pauses. A lease changes
	// nothing, and an incident only repeats the failure or pause before
	// it, so neither has a row.
	if r.Action == logbook.ActLease || r.Action == logbook.ActIncident {
		return 0
	}
	note := r.Action
	subject := r.Subject
	if id, isIncident := strings.CutPrefix(subject, logbook.IncidentKey("")); isIncident {
		subject, note = id, "incident "+r.Action
	}
	var item *string
	var proposal *int64
	var current string
	if n, isProposal := proposalNumber(subject); isProposal {
		proposal = &n
		if !t.one([]any{&current}, `SELECT status FROM core.edit_proposal WHERE proposal_id = $1`, n) {
			return 0
		}
	} else {
		item = &subject
		if !t.one([]any{&current}, `SELECT state FROM core.item WHERE item_id = $1`, subject) {
			return 0 // nothing on record to write a history for
		}
	}
	// An entry that changes nothing starts and ends in the state the
	// subject is in now, so that each entry starts where the one before
	// it ended.
	from, to := string(r.From), string(r.To)
	if from == to {
		from, to = current, current
	}
	var seq int64
	t.one([]any{&seq}, `
		INSERT INTO core.ledger (item_id, proposal_id, actor, from_state, to_state, error_code, note, recorded_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING entry_id`, item, proposal, r.Actor, optionalString(from), to, optionalString(r.Code), note, r.At.UTC())
	return uint64(seq)
}

// ---------------------------------------------------------------------
// Evaluations and decisions
// ---------------------------------------------------------------------

// Evaluation keeps what Assay found as a row of core.evaluation, when
// the result is an evaluation: that is, when it has gates. Logbook
// checks the row and refuses one that is not well formed.
func (t *tx) Evaluation(itemID string, result json.RawMessage) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(result, &fields) != nil {
		return
	}
	if _, ok := fields["gates"]; !ok {
		return
	}
	var e struct {
		Profile        string          `json:"profile"`
		Gates          json.RawMessage `json:"gates"`
		Factors        json.RawMessage `json:"factors"`
		Score          *int            `json:"score"`
		Recommendation *string         `json:"recommendation"`
		SplitFlag      bool            `json:"split_flag"`
	}
	if json.Unmarshal(result, &e) != nil {
		t.fail(contract.LogbookRefused)
		return
	}
	if e.Factors == nil {
		e.Factors = json.RawMessage(`{}`)
	}
	t.exec(`
		INSERT INTO core.evaluation (item_id, profile, gates, factors, score, recommendation, split_flag, evaluated_at)
		VALUES ($1, $2, $3::jsonb, $4::jsonb, $5, $6, $7, $8)`,
		itemID, e.Profile, string(e.Gates), string(e.Factors), e.Score, e.Recommendation, e.SplitFlag, t.now)
}

// Decision adds the owner's decision to the decision trail of the item's
// latest evaluation. An item with no evaluation on record has no trail
// to add to. Logbook refuses a decision that goes against the
// recommendation without a reason.
func (t *tx) Decision(itemID, outcome, reason string) {
	var evaluation int64
	if !t.one([]any{&evaluation}, `
		SELECT evaluation_id FROM core.evaluation WHERE item_id = $1 ORDER BY evaluation_id DESC LIMIT 1`, itemID) {
		return
	}
	t.exec(`INSERT INTO core.decision (evaluation_id, outcome, reason, decided_at) VALUES ($1, $2, $3, $4)`,
		evaluation, outcome, optionalString(reason), t.now)
}

// ---------------------------------------------------------------------
// Component health and daily submission counts
// ---------------------------------------------------------------------

func (t *tx) Component(slug string) (logbook.Component, bool) {
	var state, code, digest string
	var reported time.Time
	if !t.one([]any{&state, &code, &digest, &reported}, `
		SELECT state, coalesce(error_code, ''), coalesce(digest, ''), reported_at
		FROM core.component_health WHERE component = $1`, slug) {
		return logbook.Component{}, false
	}
	return logbook.Component{
		Slug:   slug,
		State:  state,
		Code:   code,
		Digest: digest,
		At:     reported.UTC(),
	}, true
}

func (t *tx) PutComponent(c logbook.Component) {
	at := t.now
	if !c.At.IsZero() {
		at = c.At.UTC()
	}
	t.exec(`
		INSERT INTO core.component_health (component, state, error_code, digest, reported_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (component) DO UPDATE
		SET state = EXCLUDED.state, error_code = EXCLUDED.error_code,
		    digest = EXCLUDED.digest, reported_at = EXCLUDED.reported_at`,
		c.Slug, c.State, optionalString(c.Code), optionalString(c.Digest), at)
}

func (t *tx) Components() []logbook.Component {
	if t.err != nil {
		return nil
	}
	rows, err := t.pg.Query(t.ctx, `
		SELECT component, state, coalesce(error_code, ''), coalesce(digest, ''), reported_at
		FROM core.component_health ORDER BY component`)
	if err != nil {
		t.fail(err)
		return nil
	}
	var out []logbook.Component
	var slug, state, code, digest string
	var reported time.Time
	_, err = pgx.ForEachRow(rows, []any{&slug, &state, &code, &digest, &reported}, func() error {
		out = append(out, logbook.Component{
			Slug:   slug,
			State:  state,
			Code:   code,
			Digest: digest,
			At:     reported.UTC(),
		})
		return nil
	})
	t.fail(err)
	return out
}

func (t *tx) Bump(day, key string) (int, bool) {
	t.exec(`DELETE FROM core.submission_count WHERE submission_day < $1::date`, day)
	var count int
	if t.one([]any{&count}, `
		UPDATE core.submission_count
		SET submissions = submissions + 1
		WHERE submission_day = $1::date AND visitor_key = $2
		RETURNING submissions`, day, key) {
		return count, true
	}
	var distinct int
	if !t.one([]any{&distinct}, `SELECT count(*) FROM core.submission_count WHERE submission_day = $1::date`, day) || distinct >= keysPerDay {
		return 0, false
	}
	if !t.one([]any{&count}, `
		INSERT INTO core.submission_count (submission_day, visitor_key, submissions)
		VALUES ($1::date, $2, 1)
		ON CONFLICT (submission_day, visitor_key)
		DO UPDATE SET submissions = core.submission_count.submissions + 1
		RETURNING submissions`, day, key) {
		return 0, false
	}
	return count, true
}
