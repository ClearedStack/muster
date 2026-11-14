// Package sql is Logbook itself: the store kept in PostgreSQL, reached
// with the muster role.
//
// It keeps in Logbook everything Logbook has a place for:
//
//	core.item           an item's state, title and address
//	core.queue          the queue: each step, its lane, its lease
//	core.ledger         every change of state, failure, pause and release
//	core.pause          which worker paused an item, and from which state
//	core.edit_proposal  a proposal's element, repository, class, text and status
//	core.evaluation     what Assay found, when its result is an evaluation
//	core.decision       what the owner decided about an evaluated item
//
// Logbook gives out every identifier. A new item is inserted without
// one, and an address already on record returns the item that has it.
//
// A few things Muster needs have no column in Logbook yet. They are kept
// beside it, in this process, and are lost when it stops:
//
//   - the result each worker gave, other than an evaluation or a decision
//   - the source an item was found through
//   - a proposal's reason and the file its element lives in
//   - the answer to the call that ended each subject's last lease
//   - the latest health and image digest of each component
//   - the day's count of submissions for each visitor
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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ClearedStack/muster/internal/contract"
	"github.com/ClearedStack/muster/internal/lifecycle"
	"github.com/ClearedStack/muster/internal/logbook"
	"github.com/ClearedStack/muster/internal/logbook/memory"
)

// incidentWorker is the worker whose steps on an item are incident
// steps. Logbook's queue has no column for the kind of a step, and
// Bellbuoy has no other step on an item, so the worker tells the kind.
const incidentWorker = "bellbuoy"

// heldToken is the lease a stopped step is given. With an end that never
// comes, it keeps the step from being offered until the owner releases
// it, and keeps the step's place and its count of attempts on record.
const heldToken = "ffffffff-ffff-ffff-ffff-ffffffffffff"

// pausePattern is what Muster writes where Logbook asks for the pattern
// of an unfamiliar case. The contract's needs_subtool call carries none.
const pausePattern = "not given"

// Store is Logbook.
type Store struct {
	config *pgx.ConnConfig
	clock  func() time.Time
	beside *memory.Store
	conn   *pgx.Conn // used only inside a transaction, which holds beside's lock
}

var _ logbook.Store = (*Store)(nil)

// Open reads the address of the database. It does not connect; the first
// transaction does. An address that cannot be read is refused without
// being repeated, since it can hold a password.
func Open(url string, clock func() time.Time) (*Store, error) {
	config, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, errors.New("MUSTER_DATABASE_URL is not a database address")
	}
	// The simple protocol sends each statement whole, with no prepared
	// statements to keep track of across reconnections.
	config.DefaultQueryExecMode = pgx.QueryExecModeExec
	return &Store{config: config, clock: clock, beside: memory.New()}, nil
}

// Close ends the connection.
func (s *Store) Close(ctx context.Context) {
	_ = s.beside.Update(ctx, func(logbook.Tx) error {
		s.drop(ctx)
		return nil
	})
}

func (s *Store) drop(ctx context.Context) {
	if s.conn != nil {
		_ = s.conn.Close(ctx)
		s.conn = nil
	}
}

// Update runs fn in one PostgreSQL transaction.
func (s *Store) Update(ctx context.Context, fn func(logbook.Tx) error) error {
	return s.beside.Update(ctx, func(beside logbook.Tx) error { return s.run(ctx, beside, fn, true) })
}

// View runs fn in a transaction that is always rolled back.
func (s *Store) View(ctx context.Context, fn func(logbook.Tx) error) error {
	return s.beside.View(ctx, func(beside logbook.Tx) error { return s.run(ctx, beside, fn, false) })
}

func (s *Store) run(ctx context.Context, beside logbook.Tx, fn func(logbook.Tx) error, keep bool) error {
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
	t := &tx{ctx: ctx, pg: pg, beside: beside, now: s.clock().UTC()}
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
	ctx    context.Context
	pg     pgx.Tx
	beside logbook.Tx
	now    time.Time
	err    error // the first failure; nothing is done after it
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

// ---------------------------------------------------------------------
// Items
// ---------------------------------------------------------------------

func (t *tx) Item(id string) (logbook.Item, bool) {
	if !lifecycle.IsItemID(id) {
		return logbook.Item{}, false
	}
	var state, title, address string
	var lane, pausedBy, pausedFrom *string
	var pauseID *int64
	if !t.one([]any{&state, &title, &address, &lane, &pausedBy, &pausedFrom, &pauseID}, `
		SELECT i.state, i.title, coalesce(i.address, ''), q.lane, p.worker, p.paused_from, p.pause_id
		FROM core.item i
		LEFT JOIN core.queue q ON q.item_id = i.item_id AND q.worker <> $2
		LEFT JOIN core.pause p ON p.item_id = i.item_id AND p.replayed_at IS NULL
		WHERE i.item_id = $1
		ORDER BY q.queue_id
		LIMIT 1`, id, incidentWorker) {
		return logbook.Item{}, false
	}
	it, _ := t.beside.Item(id)
	it.ID, it.State, it.Title, it.Address = id, lifecycle.State(state), title, address
	if lane != nil {
		it.Lane = lifecycle.Lane(*lane)
	}
	it.PausedBy, it.PausedFrom, it.PausedSeq = "", "", 0
	if pauseID != nil {
		it.PausedBy, it.PausedFrom, it.PausedSeq = *pausedBy, lifecycle.State(*pausedFrom), uint64(*pauseID)
	}
	return it, true
}

func (t *tx) AddItem(v logbook.Item) (string, bool) {
	var address *string
	if v.Address != "" {
		address = &v.Address
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
		INSERT INTO core.item (state, title, address) VALUES ($1, $2, $3)
		ON CONFLICT (address_sha256) DO NOTHING
		RETURNING item_id`, string(v.State), v.Title, address) {
		if address == nil || !t.one([]any{&id}, `SELECT item_id FROM core.item WHERE address_sha256 = core.address_sha256($1)`, v.Address) {
			return "", false
		}
		return id, true
	}
	if t.err != nil {
		return "", false
	}
	t.beside.PutItem(logbook.Item{ID: id, Source: v.Source, Results: v.Results})
	return id, false
}

func (t *tx) PutItem(v logbook.Item) {
	var was string
	if !t.one([]any{&was}, `SELECT state FROM core.item WHERE item_id = $1`, v.ID) {
		return
	}
	paused := string(lifecycle.NeedsSubtool)
	switch {
	case string(v.State) == paused && was != paused:
		t.exec(`INSERT INTO core.pause (item_id, worker, pattern, paused_from, paused_at) VALUES ($1, $2, $3, $4, $5)`,
			v.ID, v.PausedBy, pausePattern, string(v.PausedFrom), t.now)
	case string(v.State) != paused && was == paused:
		t.exec(`UPDATE core.pause SET replayed_at = $2 WHERE item_id = $1 AND replayed_at IS NULL`, v.ID, t.now)
	}
	if string(v.State) != was {
		t.exec(`UPDATE core.item SET state = $2 WHERE item_id = $1`, v.ID, string(v.State))
	}
	t.beside.PutItem(logbook.Item{ID: v.ID, Source: v.Source, Results: v.Results, Done: v.Done, IncidentDone: v.IncidentDone})
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
	var element, repository, class, payload, status string
	var submitted time.Time
	if !t.one([]any{&element, &repository, &class, &payload, &status, &submitted}, `
		SELECT element_id, owner_repo, edit_class, payload, status, submitted_at
		FROM core.edit_proposal WHERE proposal_id = $1`, n) {
		return logbook.Proposal{}, false
	}
	p, _ := t.beside.Proposal(id)
	p.ID, p.Target, p.Repository, p.EditKind, p.Change = id, element, repository, class, payload
	p.Status, p.Received = lifecycle.State(status), submitted.UTC()
	return p, true
}

func (t *tx) AddProposal(v logbook.Proposal) string {
	var n int64
	if !t.one([]any{&n}, `
		INSERT INTO core.edit_proposal (element_id, owner_repo, edit_class, payload, status, submitted_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING proposal_id`, v.Target, v.Repository, v.EditKind, v.Change, string(v.Status), v.Received) {
		return ""
	}
	if n < 1 || n > lifecycle.MaxID {
		t.fail(contract.Unexpected)
		return ""
	}
	id := lifecycle.ProposalID(uint64(n))
	t.beside.PutProposal(logbook.Proposal{ID: id, File: v.File, Reason: v.Reason, Results: v.Results})
	return id
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
	t.beside.PutProposal(logbook.Proposal{ID: v.ID, File: v.File, Reason: v.Reason, Results: v.Results, Done: v.Done})
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
	token    *string
	until    pgtype.Timestamptz
}

const rowColumns = `queue_id, worker, lane, item_id, proposal_id, queued_at, attempt, lease_token::text, leased_until`

func (r *row) dest() []any {
	return []any{&r.id, &r.worker, &r.lane, &r.item, &r.proposal, &r.queued, &r.attempt, &r.token, &r.until}
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
	case r.token == nil:
	case *r.token == heldToken:
		e.Held = true
	default:
		e.Lease = e.Key + "." + strconv.FormatUint(e.Arrival, 10) + "." + strconv.Itoa(e.Attempt)
		if r.until.Valid && r.until.InfinityModifier == pgtype.Finite {
			e.Expires = r.until.Time.UTC()
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
		WHERE worker = $1 AND queued_at <= $2 AND (lease_token IS NULL OR leased_until <= $2)
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
	// The lease as Logbook holds it.
	var token *string
	var until, beat *time.Time
	forever := false
	switch {
	case e.Held:
		held := heldToken
		token, forever = &held, true
	case e.Lease != "":
		made := leaseToken(e.Arrival, e.Attempt)
		expires := e.Expires.UTC()
		token, until, beat = &made, &expires, &t.now
	}
	if e.Arrival != 0 {
		// The step keeps its place. Only its lease and its count of
		// attempts can change.
		t.exec(`
			UPDATE core.queue
			SET attempt = $2, lease_token = $3::uuid,
			    leased_until = CASE WHEN $4 THEN 'infinity'::timestamptz ELSE $5 END,
			    heartbeat_at = $6
			WHERE queue_id = $1`, int64(e.Arrival), e.Attempt, token, forever, until, beat)
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
		INSERT INTO core.queue (worker, lane, item_id, proposal_id, queued_at, attempt, lease_token, leased_until, heartbeat_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7::uuid, CASE WHEN $8 THEN 'infinity'::timestamptz ELSE $9 END, $10)`,
		e.Worker, string(e.Lane), item, proposal, queued, e.Attempt, token, forever, until, beat)
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
		WHERE queued_at <= $1 AND (lease_token IS NULL OR leased_until <= $1)
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
	var fromState, code *string
	if from != "" {
		fromState = &from
	}
	if r.Code != "" {
		code = &r.Code
	}
	var seq int64
	t.one([]any{&seq}, `
		INSERT INTO core.ledger (item_id, proposal_id, actor, from_state, to_state, error_code, note, recorded_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING entry_id`, item, proposal, r.Actor, fromState, to, code, note, r.At.UTC())
	return uint64(seq)
}

// ---------------------------------------------------------------------
// Evaluations and decisions
// ---------------------------------------------------------------------

// Evaluation keeps what Assay found as a row of core.evaluation, when
// the result is an evaluation: that is, when it has gates. Logbook
// checks the row and refuses one that is not well formed.
func (t *tx) Evaluation(itemID string, result json.RawMessage) {
	var e struct {
		Profile        string          `json:"profile"`
		Gates          json.RawMessage `json:"gates"`
		Factors        json.RawMessage `json:"factors"`
		Score          *int            `json:"score"`
		Recommendation *string         `json:"recommendation"`
		SplitFlag      bool            `json:"split_flag"`
	}
	if json.Unmarshal(result, &e) != nil || e.Gates == nil {
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
	var why *string
	if reason != "" {
		why = &reason
	}
	t.exec(`INSERT INTO core.decision (evaluation_id, outcome, reason, decided_at) VALUES ($1, $2, $3, $4)`,
		evaluation, outcome, why, t.now)
}

// ---------------------------------------------------------------------
// Kept beside Logbook, which has no column for these.
// ---------------------------------------------------------------------

func (t *tx) Component(slug string) (logbook.Component, bool) { return t.beside.Component(slug) }
func (t *tx) PutComponent(c logbook.Component)                { t.beside.PutComponent(c) }
func (t *tx) Components() []logbook.Component                 { return t.beside.Components() }
func (t *tx) Bump(day, key string) (int, bool)                { return t.beside.Bump(day, key) }
