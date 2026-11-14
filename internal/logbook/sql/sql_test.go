package sql_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ClearedStack/muster/internal/contract"
	"github.com/ClearedStack/muster/internal/intake"
	"github.com/ClearedStack/muster/internal/lifecycle"
	"github.com/ClearedStack/muster/internal/logbook"
	"github.com/ClearedStack/muster/internal/logbook/memory"
	"github.com/ClearedStack/muster/internal/logbook/sql/sqltest"
	"github.com/ClearedStack/muster/internal/queue"
)

// These tests run Muster's own calls against a real Logbook: a
// PostgreSQL database with every Logbook migration applied, written to
// through the muster role. They are skipped when no test database is
// given; see the sqltest package.

var ctx = context.Background()

type clock struct{ t time.Time }

func (c *clock) Now() time.Time { return c.t }

type fixture struct {
	t     *testing.T
	book  *sqltest.Logbook
	clock *clock
	queue *queue.Service
	take  *intake.Service
}

func setup(t *testing.T, items ...memory.SeedItem) *fixture {
	t.Helper()
	clk := &clock{t: time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)}
	book := sqltest.New(t, clk.Now)
	book.Seed(t, memory.Seed{Items: items}, clk.t)
	return &fixture{
		t: t, book: book, clock: clk,
		queue: &queue.Service{Store: book.Store, Clock: clk.Now, LeaseFor: time.Minute},
		take: &intake.Service{
			Store: book.Store, Clock: clk.Now,
			Secret: []byte("made-up-salt-secret-for-tests-only-0123456789abcdef"), Difficulty: 20, DailyCap: 5,
		},
	}
}

func recorded(id string) memory.SeedItem {
	return memory.SeedItem{ID: id, State: lifecycle.Recorded, Lane: lifecycle.Normal, Next: "sextant"}
}

// ask runs a query as the owner of the database and returns its rows,
// each as its columns joined by "|".
func (f *fixture) ask(query string, args ...any) []string {
	f.t.Helper()
	rows, err := f.book.Admin.Query(ctx, query, args...)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			f.t.Fatal(err)
		}
		parts := make([]string, len(values))
		for i, v := range values {
			if v == nil {
				parts[i] = ""
			} else {
				parts[i] = fmt.Sprint(v)
			}
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *fixture) want(what string, got []string, want ...string) {
	f.t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		f.t.Errorf("%s:\n got: %q\nwant: %q", what, got, want)
	}
}

func must[T any](v T, err error) func(*testing.T) T {
	return func(t *testing.T) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

const history = `SELECT actor, coalesce(from_state, ''), to_state, coalesce(error_code, ''), note FROM core.ledger WHERE item_id = $1 ORDER BY entry_id`

func TestLease(t *testing.T) {
	f := setup(t, recorded("PA-0001"), recorded("PA-0002"))
	g := must(f.queue.Lease(ctx, "sextant"))(t)
	if g == nil || g.Lease != "PA-0001.1.1" || g.ID != "PA-0001" || g.State != lifecycle.Recorded || g.Attempt != 1 || g.Expires != "2026-01-05T09:01:00Z" {
		t.Fatalf("the lease: %+v", g)
	}
	f.want("the queue row", f.ask(`SELECT worker, lane, attempt, lease_token IS NOT NULL, leased_until = '2026-01-05 09:01:00+00', heartbeat_at = '2026-01-05 09:00:00+00' FROM core.queue WHERE item_id = 'PA-0001'`),
		"sextant|normal|1|true|true|true")
	// The item that is under lease is not offered again; the next one is.
	g2 := must(f.queue.Lease(ctx, "sextant"))(t)
	if g2 == nil || g2.ID != "PA-0002" {
		t.Fatalf("the second lease: %+v", g2)
	}
	if g3 := must(f.queue.Lease(ctx, "sextant"))(t); g3 != nil {
		t.Fatalf("a third lease: %+v", g3)
	}
	if g4 := must(f.queue.Lease(ctx, "assay"))(t); g4 != nil {
		t.Fatalf("another worker's lease: %+v", g4)
	}
}

func TestHeartbeat(t *testing.T) {
	f := setup(t, recorded("PA-0001"))
	g := must(f.queue.Lease(ctx, "sextant"))(t)
	f.clock.t = f.clock.t.Add(45 * time.Second)
	beat := must(f.queue.Heartbeat(ctx, "sextant", g.Lease))(t)
	if beat.Expires != "2026-01-05T09:01:45Z" {
		t.Errorf("the heartbeat: %+v", beat)
	}
	f.want("the queue row", f.ask(`SELECT leased_until = '2026-01-05 09:01:45+00', heartbeat_at = '2026-01-05 09:00:45+00' FROM core.queue`), "true|true")
	if _, err := f.queue.Heartbeat(ctx, "assay", g.Lease); err != contract.LeaseNotHeld {
		t.Errorf("another worker's heartbeat: %v", err)
	}
	if _, err := f.queue.Heartbeat(ctx, "sextant", "PA-0001.1.2"); err != contract.LeaseNotHeld {
		t.Errorf("a heartbeat on a lease never given: %v", err)
	}
}

// The change of state, its ledger row and the next queue step are one
// transaction: all three are there afterwards, and when the transaction
// is lost none of them is.
func TestCompleteIsOneTransaction(t *testing.T) {
	f := setup(t, recorded("PA-0001"))
	g := must(f.queue.Lease(ctx, "sextant"))(t)

	lost := *f.queue
	lost.Store = failing{f.book.Store}
	if _, err := lost.Complete(ctx, "sextant", g.Lease, lifecycle.Observed, json.RawMessage(`{"job":"made-up job"}`)); err == nil {
		t.Fatal("a lost transaction was reported as kept")
	}
	f.want("the item after a lost transaction", f.ask(`SELECT state FROM core.item`), "recorded")
	f.want("the queue after a lost transaction", f.ask(`SELECT worker, lease_token IS NOT NULL FROM core.queue`), "sextant|true")
	f.want("the ledger after a lost transaction", f.ask(history, "PA-0001"), "trawl||recorded||seed")

	ack := must(f.queue.Complete(ctx, "sextant", g.Lease, lifecycle.Observed, json.RawMessage(`{"job":"made-up job"}`)))(t)
	if ack.ID != "PA-0001" || ack.State != lifecycle.Observed || ack.Next != "assay" {
		t.Errorf("the answer: %+v", ack)
	}
	f.want("the item", f.ask(`SELECT state FROM core.item`), "observed")
	f.want("the queue", f.ask(`SELECT worker, lane, attempt, lease_token IS NULL FROM core.queue`), "assay|normal|0|true")
	f.want("the ledger", f.ask(history, "PA-0001"), "trawl||recorded||seed", "sextant|recorded|observed||complete")

	next := must(f.queue.Lease(ctx, "assay"))(t)
	if next == nil || string(next.Results["sextant"]) != `{"job":"made-up job"}` {
		t.Errorf("what Assay is given: %+v", next)
	}
}

// failing loses every transaction at the last moment.
type failing struct{ logbook.Store }

func (f failing) Update(ctx context.Context, fn func(logbook.Tx) error) error {
	return f.Store.Update(ctx, func(tx logbook.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		return contract.Unexpected
	})
}

func TestFail(t *testing.T) {
	f := setup(t, recorded("PA-0001"), recorded("PA-0002"))
	g := must(f.queue.Lease(ctx, "sextant"))(t)
	ack := must(f.queue.Fail(ctx, "sextant", g.Lease, contract.Timeout))(t)
	if ack.Next != "sextant" || ack.Held {
		t.Errorf("after E_TIMEOUT: %+v", ack)
	}
	// It is offered again, in its place, as a second attempt.
	g = must(f.queue.Lease(ctx, "sextant"))(t)
	if g == nil || g.Lease != "PA-0001.1.2" || g.Attempt != 2 {
		t.Fatalf("the second attempt: %+v", g)
	}
	ack = must(f.queue.Fail(ctx, "sextant", g.Lease, contract.Parse))(t)
	if !ack.Held || ack.State != lifecycle.Recorded {
		t.Errorf("after E_PARSE: %+v", ack)
	}
	// The stopped item is not offered, however long anyone waits; the
	// next item is. Bellbuoy has an incident step about it.
	f.clock.t = f.clock.t.Add(30 * 24 * time.Hour)
	if g := must(f.queue.Lease(ctx, "sextant"))(t); g == nil || g.ID != "PA-0002" {
		t.Errorf("after the stop: %+v", g)
	}
	inc := must(f.queue.Lease(ctx, "bellbuoy"))(t)
	if inc == nil || inc.Kind != lifecycle.KindIncident || *inc.Incident != (queue.IncidentView{ID: "PA-0001", Worker: "sextant", Code: "E_PARSE", State: lifecycle.Recorded}) {
		t.Errorf("the incident: %+v", inc)
	}
	f.want("the item", f.ask(`SELECT state FROM core.item WHERE item_id = 'PA-0001'`), "recorded")
	f.want("the ledger", f.ask(history, "PA-0001"),
		"trawl||recorded||seed", "sextant|recorded|recorded|E_TIMEOUT|fail", "sextant|recorded|recorded|E_PARSE|fail")

	// The owner's release puts it back, in the urgent lane.
	rel := must(f.queue.Release(ctx, "countersign", "PA-0001"))(t)
	if rel.Next != "sextant" || rel.State != lifecycle.Recorded {
		t.Errorf("the release: %+v", rel)
	}
	f.want("the queue row after release", f.ask(`SELECT worker, lane, attempt, lease_token IS NULL FROM core.queue WHERE item_id = 'PA-0001' AND worker = 'sextant'`), "sextant|urgent|2|true")
}

func TestNeedsSubtoolAndReplay(t *testing.T) {
	f := setup(t, recorded("PA-0001"))
	g := must(f.queue.Lease(ctx, "sextant"))(t)
	ack := must(f.queue.NeedsSubtool(ctx, "sextant", g.Lease))(t)
	if ack.State != lifecycle.NeedsSubtool {
		t.Errorf("the answer: %+v", ack)
	}
	f.want("the item", f.ask(`SELECT state FROM core.item`), "needs_subtool")
	f.want("the pause", f.ask(`SELECT item_id, worker, paused_from, replayed_at IS NULL FROM core.pause`), "PA-0001|sextant|recorded|true")
	f.want("the queue", f.ask(`SELECT worker FROM core.queue ORDER BY queue_id`), "bellbuoy")
	if g := must(f.queue.Lease(ctx, "sextant"))(t); g != nil {
		t.Errorf("a paused item was offered: %+v", g)
	}

	f.clock.t = f.clock.t.Add(time.Hour)
	if err := f.book.Store.Update(ctx, func(tx logbook.Tx) error {
		if n := queue.Replay(tx, "sextant", f.clock.Now()); n != 1 {
			t.Errorf("replayed %d items, want 1", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.want("the item after replay", f.ask(`SELECT state FROM core.item`), "recorded")
	f.want("the pause after replay", f.ask(`SELECT replayed_at = '2026-01-05 10:00:00+00' FROM core.pause`), "true")
	f.want("the ledger", f.ask(history, "PA-0001"),
		"trawl||recorded||seed", "sextant|recorded|needs_subtool|E_NEEDS_SUBTOOL|needs_subtool", "sextant|needs_subtool|recorded||replay")
	g = must(f.queue.Lease(ctx, "sextant"))(t)
	if g == nil || g.Lane != lifecycle.Urgent || g.State != lifecycle.Recorded {
		t.Errorf("the replayed item: %+v", g)
	}
}

func TestExpiredLeaseIsOfferedAgain(t *testing.T) {
	f := setup(t, recorded("PA-0001"))
	first := must(f.queue.Lease(ctx, "sextant"))(t)
	f.clock.t = f.clock.t.Add(59 * time.Second)
	if g := must(f.queue.Lease(ctx, "sextant"))(t); g != nil {
		t.Fatalf("offered while the lease still held: %+v", g)
	}
	f.clock.t = f.clock.t.Add(time.Second)
	second := must(f.queue.Lease(ctx, "sextant"))(t)
	if second == nil || second.ID != "PA-0001" || second.Attempt != 2 || second.Lease == first.Lease {
		t.Fatalf("after the lease ran out: %+v", second)
	}
	if _, err := f.queue.Complete(ctx, "sextant", first.Lease, lifecycle.Observed, nil); err != contract.LeaseNotHeld {
		t.Errorf("the worker that lost the lease: %v", err)
	}
	must(f.queue.Complete(ctx, "sextant", second.Lease, lifecycle.Observed, nil))(t)
}

func TestDuplicateReportIsRecordedOnce(t *testing.T) {
	f := setup(t, recorded("PA-0001"))
	g := must(f.queue.Lease(ctx, "sextant"))(t)
	first := must(f.queue.Complete(ctx, "sextant", g.Lease, lifecycle.Observed, json.RawMessage(`{"job":"made-up job"}`)))(t)
	again := must(f.queue.Complete(ctx, "sextant", g.Lease, lifecycle.Observed, json.RawMessage(`{"job":"made-up job"}`)))(t)
	if *again != *first {
		t.Errorf("the second answer %+v differs from the first %+v", again, first)
	}
	f.want("the ledger", f.ask(history, "PA-0001"), "trawl||recorded||seed", "sextant|recorded|observed||complete")
	f.want("the queue", f.ask(`SELECT worker, count(*) FROM core.queue GROUP BY worker`), "assay|1")
}

func TestDuplicateAddressReturnsTheSameItem(t *testing.T) {
	f := setup(t)
	find := intake.Candidate{Source: "sam-gov", Title: "A made-up notice", Address: "https://example.org/made-up/notice-1"}
	first := must(f.take.Record(ctx, "trawl", find))(t)
	if first.ID != "PA-0001" || first.State != lifecycle.Discovered || first.Next != "trawl" || first.Duplicate {
		t.Fatalf("the new item: %+v", first)
	}
	again := must(f.take.Record(ctx, "conn", intake.Candidate{Source: "github-issues", Title: "Another made-up title", Address: find.Address}))(t)
	if again.ID != first.ID || !again.Duplicate {
		t.Errorf("the same address again: %+v", again)
	}
	other := must(f.take.Record(ctx, "trawl", intake.Candidate{Source: "web-sweep", Title: "A made-up page", Address: "https://example.org/made-up/swept-1"}))(t)
	// No number was used up by the duplicate.
	if other.ID != "PA-0002" || other.Duplicate {
		t.Errorf("another address: %+v", other)
	}
	f.want("the items", f.ask(`SELECT item_id, state, title, address FROM core.item ORDER BY item_id`),
		"PA-0001|discovered|A made-up notice|https://example.org/made-up/notice-1",
		"PA-0002|discovered|A made-up page|https://example.org/made-up/swept-1")
	f.want("the queue", f.ask(`SELECT item_id, worker, lane FROM core.queue ORDER BY queue_id`), "PA-0001|trawl|normal", "PA-0002|trawl|backfill")
	f.want("the ledger", f.ask(history, "PA-0001"), "trawl||discovered||record", "conn|discovered|discovered||sighting")

	// What the address was found through reaches Trawl with the lease.
	g := must(f.queue.Lease(ctx, "trawl"))(t)
	if g == nil || g.Found == nil || *g.Found != (queue.FoundView{Source: "sam-gov", Title: "A made-up notice", Address: find.Address}) {
		t.Errorf("what Trawl is given: %+v", g)
	}
}

// Logbook's ledger can be added to and nothing else. Muster's role may
// not change or remove a row, and Logbook refuses even its owner.
func TestLedgerRefusesUpdateAndDelete(t *testing.T) {
	f := setup(t, recorded("PA-0001"))
	g := must(f.queue.Lease(ctx, "sextant"))(t)
	must(f.queue.Complete(ctx, "sextant", g.Lease, lifecycle.Observed, nil))(t)
	before := f.ask(history, "PA-0001")

	for _, statement := range []string{
		`UPDATE core.ledger SET note = 'made-up'`,
		`DELETE FROM core.ledger`,
		`TRUNCATE core.ledger`,
	} {
		for _, role := range []string{"muster", ""} {
			tx, err := f.book.Admin.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if role != "" {
				if _, err := tx.Exec(ctx, `SET LOCAL ROLE `+role); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := tx.Exec(ctx, statement); err == nil {
				t.Errorf("%q was allowed (role %q)", statement, role)
			}
			tx.Rollback(ctx)
		}
	}
	f.want("the ledger afterwards", f.ask(history, "PA-0001"), before...)
}

// Assay's result, when it is an evaluation, is kept as one, and the
// owner's decision joins its decision trail. Logbook checks both.
func TestEvaluationAndDecision(t *testing.T) {
	f := setup(t,
		memory.SeedItem{ID: "PA-0001", State: lifecycle.Observed, Lane: lifecycle.Normal, Next: "assay"},
		memory.SeedItem{ID: "PA-0002", State: lifecycle.Observed, Lane: lifecycle.Normal, Next: "assay"})
	evaluation := json.RawMessage(`{"profile":"sample-profile",
		"gates":{"public_source":{"passed":true},"one_job":{"passed":true}},
		"factors":{"need":{"rating":4,"weight":4},"gap":{"rating":3,"weight":4}},
		"score":28,"recommendation":"build"}`)
	g := must(f.queue.Lease(ctx, "assay"))(t)
	must(f.queue.Complete(ctx, "assay", g.Lease, lifecycle.Scored, evaluation))(t)
	f.want("the evaluation", f.ask(`SELECT item_id, profile, score, recommendation FROM core.evaluation`), "PA-0001|sample-profile|28|build")

	// An evaluation Logbook's own check refuses is refused whole: the
	// item does not move.
	g2 := must(f.queue.Lease(ctx, "assay"))(t)
	wrong := json.RawMessage(`{"profile":"sample-profile","gates":{"one_job":{"passed":true}},"factors":{"need":{"rating":4,"weight":4}},"score":99,"recommendation":"build"}`)
	if _, err := f.queue.Complete(ctx, "assay", g2.Lease, lifecycle.Scored, wrong); err != contract.LogbookRefused {
		t.Errorf("an evaluation whose score does not add up: %v", err)
	}
	malformed := json.RawMessage(`{"profile":"sample-profile","gates":{"one_job":{"passed":true}},"factors":{"need":{"rating":4,"weight":4}},"score":28.5,"recommendation":"build"}`)
	if _, err := f.queue.Complete(ctx, "assay", g2.Lease, lifecycle.Scored, malformed); err != contract.LogbookRefused {
		t.Errorf("an evaluation whose score is not an integer: %v", err)
	}
	f.want("the second item", f.ask(`SELECT state FROM core.item WHERE item_id = 'PA-0002'`), "observed")

	// The owner goes against the recommendation. Logbook wants a reason.
	d := must(f.queue.Lease(ctx, "countersign"))(t)
	if _, err := f.queue.Complete(ctx, "countersign", d.Lease, lifecycle.Parked, nil); err != contract.LogbookRefused {
		t.Errorf("an override with no reason: %v", err)
	}
	must(f.queue.Complete(ctx, "countersign", d.Lease, lifecycle.Parked, json.RawMessage(`{"reason":"Held until the made-up rule is final."}`)))(t)
	f.want("the decision", f.ask(`SELECT outcome, reason FROM core.decision`), "park|Held until the made-up rule is final.")
	f.want("the item", f.ask(`SELECT state FROM core.item WHERE item_id = 'PA-0001'`), "parked")
}

// Evaluations and decisions live in Logbook's own tables, so later
// workers still receive results.assay and results.countersign after
// Muster restarts with an empty memory store.
func TestEvaluationAndDecisionSurviveRestart(t *testing.T) {
	f := setup(t,
		memory.SeedItem{ID: "PA-0001", State: lifecycle.Observed, Lane: lifecycle.Normal, Next: "assay"},
		memory.SeedItem{ID: "PA-0002", State: lifecycle.Observed, Lane: lifecycle.Normal, Next: "assay"})

	scored := json.RawMessage(`{"profile":"sample-profile",
		"gates":{"public_source":{"passed":true},"one_job":{"passed":true}},
		"factors":{"need":{"rating":4,"weight":4},"gap":{"rating":3,"weight":4}},
		"score":28,"recommendation":"build","split_flag":true}`)
	g1 := must(f.queue.Lease(ctx, "assay"))(t)
	must(f.queue.Complete(ctx, "assay", g1.Lease, lifecycle.Scored, scored))(t)

	screened := json.RawMessage(`{"profile":"sample-profile",
		"gates":{"public_source":{"passed":true},"one_job":{"passed":false,"reason":"More than one made-up job."}}}`)
	g2 := must(f.queue.Lease(ctx, "assay"))(t)
	must(f.queue.Complete(ctx, "assay", g2.Lease, lifecycle.ScreenedOut, screened))(t)

	restarted := &queue.Service{Store: f.book.Reopen(t, f.clock.Now), Clock: f.clock.Now, LeaseFor: time.Minute}

	d1 := must(restarted.Lease(ctx, "countersign"))(t)
	if d1 == nil || d1.ID != "PA-0001" {
		t.Fatalf("first countersign lease after restart: %+v", d1)
	}
	var gotEval struct {
		Profile        string         `json:"profile"`
		Gates          map[string]any `json:"gates"`
		Factors        map[string]any `json:"factors"`
		Score          *int           `json:"score"`
		Recommendation *string        `json:"recommendation"`
		SplitFlag      bool           `json:"split_flag"`
	}
	if err := json.Unmarshal(d1.Results["assay"], &gotEval); err != nil {
		t.Fatalf("results.assay on PA-0001 after restart (%q): %v", d1.Results["assay"], err)
	}
	if gotEval.Profile != "sample-profile" || len(gotEval.Gates) != 2 || len(gotEval.Factors) != 2 ||
		gotEval.Score == nil || *gotEval.Score != 28 ||
		gotEval.Recommendation == nil || *gotEval.Recommendation != "build" || !gotEval.SplitFlag {
		t.Errorf("reconstructed scored evaluation: %+v", gotEval)
	}

	d2 := must(restarted.Lease(ctx, "countersign"))(t)
	if d2 == nil || d2.ID != "PA-0002" {
		t.Fatalf("second countersign lease after restart: %+v", d2)
	}
	var gotScreened struct {
		Profile        string         `json:"profile"`
		Gates          map[string]any `json:"gates"`
		Factors        map[string]any `json:"factors"`
		Score          *int           `json:"score"`
		Recommendation *string        `json:"recommendation"`
		SplitFlag      bool           `json:"split_flag"`
	}
	if err := json.Unmarshal(d2.Results["assay"], &gotScreened); err != nil {
		t.Fatalf("results.assay on PA-0002 after restart (%q): %v", d2.Results["assay"], err)
	}
	if gotScreened.Profile != "sample-profile" || len(gotScreened.Gates) != 2 ||
		len(gotScreened.Factors) != 0 || gotScreened.Score != nil || gotScreened.Recommendation != nil || gotScreened.SplitFlag {
		t.Errorf("reconstructed screened_out evaluation: %+v", gotScreened)
	}

	must(restarted.Complete(ctx, "countersign", d1.Lease, lifecycle.Approved,
		json.RawMessage(`{"reason":"Approved from the made-up rule."}`)))(t)

	restartedAgain := &queue.Service{Store: f.book.Reopen(t, f.clock.Now), Clock: f.clock.Now, LeaseFor: time.Minute}
	w := must(restartedAgain.Lease(ctx, "waybill"))(t)
	if w == nil || w.ID != "PA-0001" {
		t.Fatalf("waybill lease after second restart: %+v", w)
	}
	if len(w.Results["assay"]) == 0 {
		t.Errorf("waybill missing results.assay after second restart: %+v", w.Results)
	}
	var gotDecision struct {
		Outcome string `json:"outcome"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(w.Results["countersign"], &gotDecision); err != nil {
		t.Fatalf("results.countersign after second restart (%q): %v", w.Results["countersign"], err)
	}
	if gotDecision.Outcome != "build" || gotDecision.Reason != "Approved from the made-up rule." {
		t.Errorf("reconstructed decision: %+v", gotDecision)
	}
}

// In the cluster Muster logs in directly as the muster role using the
// password from Secret logbook-credentials, without superuser SET ROLE
// and without interpolating special characters into MUSTER_DATABASE_URL.
func TestDirectMusterRoleLoginWithPassword(t *testing.T) {
	f := setup(t)
	store := f.book.OpenAsMuster(t, "made-up-pass-@:/?#%-secret", f.clock.Now)
	q := &queue.Service{Store: store, Clock: f.clock.Now, LeaseFor: time.Minute}
	take := &intake.Service{
		Store: store, Clock: f.clock.Now,
		Secret: []byte("made-up-salt-secret-for-tests-only-0123456789abcdef"), Difficulty: 20, DailyCap: 5,
	}

	rec := must(take.Record(ctx, "trawl", intake.Candidate{
		Source:  "sam-gov",
		Title:   "A made-up notice",
		Address: "https://example.org/made-up/direct-login-1",
	}))(t)
	if rec.ID != "PA-0001" {
		t.Fatalf("recorded item: %+v", rec)
	}
	tr := must(q.Lease(ctx, "trawl"))(t)
	must(q.Complete(ctx, "trawl", tr.Lease, lifecycle.Recorded, json.RawMessage(`{"source":"sam-gov"}`)))(t)
	sx := must(q.Lease(ctx, "sextant"))(t)
	must(q.NeedsSubtool(ctx, "sextant", sx.Lease))(t)
	if err := store.Update(ctx, func(tx logbook.Tx) error {
		if n := queue.Replay(tx, "sextant", f.clock.Now()); n != 1 {
			t.Errorf("replayed %d items, want 1", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sx = must(q.Lease(ctx, "sextant"))(t)
	must(q.Complete(ctx, "sextant", sx.Lease, lifecycle.Observed, json.RawMessage(`{"job":"made-up job"}`)))(t)
	as := must(q.Lease(ctx, "assay"))(t)
	must(q.Complete(ctx, "assay", as.Lease, lifecycle.Scored, json.RawMessage(`{"profile":"sample-profile",
		"gates":{"one_job":{"passed":true}},
		"factors":{"need":{"rating":4,"weight":4}},
		"score":16,"recommendation":"build"}`)))(t)
	cs := must(q.Lease(ctx, "countersign"))(t)
	must(q.Complete(ctx, "countersign", cs.Lease, lifecycle.Approved, nil))(t)

	for _, row := range f.ask(`SELECT check_name, failures FROM core.integrity_check()`) {
		if !strings.HasSuffix(row, "|0") {
			t.Errorf("integrity check: %s", row)
		}
	}
}

// After a story of every kind of call, Logbook's own integrity check
// finds nothing wrong with what Muster wrote.
func TestIntegrityCheckPasses(t *testing.T) {
	f := setup(t, recorded("PA-0001"), recorded("PA-0002"), recorded("PA-0003"),
		memory.SeedItem{ID: "PA-0004", State: lifecycle.Live, Lane: lifecycle.Normal})
	a := must(f.queue.Lease(ctx, "sextant"))(t)
	must(f.queue.Complete(ctx, "sextant", a.Lease, lifecycle.Observed, json.RawMessage(`{}`)))(t)
	b := must(f.queue.Lease(ctx, "sextant"))(t)
	must(f.queue.Fail(ctx, "sextant", b.Lease, contract.Parse))(t)
	c := must(f.queue.Lease(ctx, "sextant"))(t)
	must(f.queue.NeedsSubtool(ctx, "sextant", c.Lease))(t)
	inc := must(f.queue.Lease(ctx, "bellbuoy"))(t)
	must(f.queue.Complete(ctx, "bellbuoy", inc.Lease, inc.State, nil))(t)
	inc = must(f.queue.Lease(ctx, "bellbuoy"))(t)
	must(f.queue.Complete(ctx, "bellbuoy", inc.Lease, inc.State, nil))(t)
	must(f.queue.Release(ctx, "countersign", "PA-0002"))(t)
	check := must(f.queue.Lease(ctx, "tidegauge"))(t)
	must(f.queue.Complete(ctx, "tidegauge", check.Lease, lifecycle.Live, nil))(t)
	must(f.take.Record(ctx, "trawl", intake.Candidate{Source: "sam-gov", Title: "A made-up notice", Address: "https://example.org/made-up/1"}))(t)
	must(f.take.Record(ctx, "trawl", intake.Candidate{Source: "sam-gov", Title: "A made-up notice", Address: "https://example.org/made-up/1"}))(t)

	for _, row := range f.ask(`SELECT check_name, failures FROM core.integrity_check()`) {
		if !strings.HasSuffix(row, "|0") {
			t.Errorf("integrity check: %s", row)
		}
	}
	// The next check on the live item is seven days out, and until then
	// it is not waiting.
	f.want("the next check", f.ask(`SELECT worker, lane, queued_at = '2026-01-12 09:00:00+00' FROM core.queue WHERE item_id = 'PA-0004'`), "tidegauge|backfill|true")
}
