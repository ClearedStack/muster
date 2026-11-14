//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// reaches is the condition a stage waits for: the item in a state, with a
// step queued for exactly one worker.
func (e *env) reaches(state, next string) func() (bool, string) {
	return func() (bool, string) {
		s, q, err := e.item(e.itemID)
		if err != nil {
			return false, err.Error()
		}
		return s == state && q == next, fmt.Sprintf("state=%s queued_for=[%s]", s, q)
	}
}

// drive runs one worker until the item is where the stage says it should be.
// start begins the worker (and may be called again if a one-shot worker ends
// with the item not yet there). A worker that ends with a failure status, or
// a worker that has not got the item there in time, fails the stage with the
// evidence.
func (e *env) drive(t *testing.T, name string, limit time.Duration, start func() *proc, until func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(limit)
	p := start()
	runs := 1
	for {
		ok, where := until()
		if ok {
			p.stop()
			e.showLog(t, p)
			return
		}
		if p.exited() {
			// One more look: the worker may have finished the last call just
			// before it ended.
			if ok, where = until(); ok {
				e.showLog(t, p)
				return
			}
			if st, _, err := e.item(e.itemID); err == nil && (st == "rejected" || st == "retired") {
				t.Fatalf("%s took the item to %q, an end state, so it will go no further.\n%s\n--- %s output (last lines) ---\n%s",
					name, st, e.evidence(e.itemID), name, e.tail(p, 20))
			}
			if p.exit != nil || runs >= 6 {
				t.Fatalf("%s stopped before the item got where it should (exit: %v, runs: %d).\nwhere the item is: %s\n%s\n--- %s output (last lines) ---\n%s",
					name, p.exit, runs, where, e.evidence(e.itemID), name, e.tail(p, 20))
			}
			time.Sleep(500 * time.Millisecond)
			p = start()
			runs++
			continue
		}
		if time.Now().After(deadline) {
			p.stop()
			t.Fatalf("%s did not get the item where it should in %s.\nwhere the item is: %s\n%s\n--- %s output (last lines) ---\n%s\n--- muster output (last lines) ---\n%s",
				name, limit, where, e.evidence(e.itemID), name, e.tail(p, 20), e.tail(e.muster, 8))
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// showLog prints a finished worker's last output when E2E_SHOW_LOGS is set,
// for looking at what a passing stage said.
func (e *env) showLog(t *testing.T, p *proc) {
	if os.Getenv("E2E_SHOW_LOGS") != "" {
		t.Logf("--- %s output (last lines) ---\n%s", p.name, e.tail(p, 12))
	}
}

func (e *env) tokenFile(name string) string { return filepath.Join(e.root, "tokens", name) }

// spool is a worker's spool directory. It exists before the worker starts,
// as the mounted volume does in the cluster (Chartroom will not make it).
func (e *env) spool(name string) string {
	dir := filepath.Join(e.root, "spool", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		e.t.Fatal(err)
	}
	return dir
}

// stageTrawl records the candidate and has Trawl record it.
//
// Trawl does not sweep a local source (its fetcher refuses loopback and
// private addresses, trawl internal/fetch/fetch.go:121), so the harness makes
// the find itself, with POST /v1/items as the trawl caller, which is the call
// Trawl's own sweep makes. The real Trawl then leases the item and checks it.
// The address is on the local page server, because that is the only kind of
// address Sextant will fetch with SEXTANT_LOCAL_TEST_ORIGIN, and Trawl takes
// it only with its own test-only TRAWL_LOCAL_TEST_ORIGIN (trawl PR #4); a
// Trawl without that setting refuses it (host_not_public) and ends the item
// as rejected. E2E_STAND_IN=trawl lets a run go on past that.
func stageTrawl(e *env, t *testing.T) {
	status, body, err := e.call("trawl", "POST", "/v1/items", map[string]any{
		"contract": "v1",
		"source":   "web-sweep",
		"title":    "Wrap rate calculator",
		"address":  e.itemAddress(),
	})
	if err != nil || status != 201 {
		t.Fatalf("POST /v1/items as trawl with a local address: status %d, body %v, error %v", status, body, err)
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("POST /v1/items gave no id: %v", body)
	}
	e.itemID = id
	t.Logf("item %s recorded through POST /v1/items as the trawl caller; Muster accepted the local address", id)
	e.notes["trawl"] = "intake by harness"

	if e.standIn("trawl") {
		e.standInTrawl(t)
		return
	}
	defer func() {
		if t.Failed() {
			e.mu.Lock()
			p := e.lastProc["trawl"]
			e.mu.Unlock()
			if p != nil {
				t.Log("OBSERVED: " + describeEnv(p.env, "TRAWL_LOCAL_TEST_ORIGIN") + " (the page server is " + e.pageURL + ").")
			}
		}
	}()
	e.drive(t, "trawl", 40*time.Second, e.startTrawl, e.reaches("recorded", "sextant"))
}

// startTrawl starts the real Trawl on the item, told the page server's origin
// in TRAWL_LOCAL_TEST_ORIGIN (the same origin Sextant gets), without which it
// refuses the local address. E2E_TRAWL_LOCAL_TEST_ORIGIN replaces the value,
// even with an empty one: that is the control run, which must fail here.
func (e *env) startTrawl() *proc {
	origin := e.pageURL
	if e.trawlOriginOverride != nil {
		origin = *e.trawlOriginOverride
	}
	return e.start("trawl", filepath.Join(e.bin, "trawl"), []string{
		"work", "-muster-url", e.musterURL,
		"-token-file", e.tokenFile("trawl"),
		"-spool", e.spool("trawl"), "-drain",
	}, "TRAWL_LOCAL_TEST_ORIGIN="+origin)
}

// standInTrawl does what Trawl's work loop does to Muster for a clean item,
// by hand. It is a diagnostic only: nothing in it is Trawl.
func (e *env) standInTrawl(t *testing.T) {
	status, lease, err := e.call("trawl", "POST", "/v1/lease", map[string]any{"contract": "v1"})
	if err != nil || status != 200 {
		t.Fatalf("stand-in: lease as trawl: status %d, %v, %v", status, lease, err)
	}
	status, body, err := e.call("trawl", "POST", "/v1/complete", map[string]any{
		"contract": "v1", "lease": lease["lease"], "state": "recorded",
		"result": map[string]any{"source": "web-sweep", "address": e.itemAddress(), "title": "Wrap rate calculator"},
	})
	if err != nil || status != 200 {
		t.Fatalf("stand-in: complete as trawl: status %d, %v, %v", status, body, err)
	}
	e.standIns = append(e.standIns, "trawl")
	t.Logf("STAND-IN: Trawl did not record the item. The harness completed its step.")
}

// standIn reports whether the run was asked to stand in for a stage's worker
// (E2E_STAND_IN=trawl, sextant or trawl,sextant). Only these two can be stood
// in for, and only to see how far the later workers get when one of them
// cannot run. A run that stands in for any worker is a diagnostic and never
// passes: see the end of the test.
func (e *env) standIn(worker string) bool {
	for _, w := range strings.Split(e.standInEnv, ",") {
		if strings.TrimSpace(w) == worker {
			return true
		}
	}
	return false
}

// stageSextant has Sextant read the page and report what job it does.
//
// Sextant fetches only public https pages. For a test run it has one
// exception, SEXTANT_LOCAL_TEST_ORIGIN, which names one http://127.0.0.1:port
// origin; the harness names the page server's. A Sextant built without that
// setting refuses the page and this stage fails with what it observed.
func stageSextant(e *env, t *testing.T) {
	if e.standIn("sextant") {
		e.standInSextant(t)
		return
	}
	defer func() {
		if t.Failed() {
			t.Log(e.sextantObservation())
		}
	}()
	e.drive(t, "sextant", 25*time.Second, func() *proc {
		return e.start("sextant", filepath.Join(e.bin, "sextant"), []string{
			"work", "-muster-url", e.musterURL,
			"-token-file", e.tokenFile("sextant"),
			"-spool", e.spool("sextant"),
			"-poll-every", "300ms", "-fetch-timeout", "5s", "-drain",
		}, "SEXTANT_LOCAL_TEST_ORIGIN="+e.pageURL)
	}, e.reaches("observed", "assay"))
}

// sextantObservation says what the ledger and Sextant's own output show about
// a failed Sextant stage, and nothing else: the cause is not assumed.
func (e *env) sextantObservation() string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var n int
	var code, note string
	if err := e.db.Admin.QueryRow(ctx, `
		SELECT count(*), coalesce(max(error_code), ''), coalesce(max(note), '')
		FROM core.ledger WHERE item_id = $1 AND actor = 'sextant'`, e.itemID).Scan(&n, &code, &note); err != nil {
		return "OBSERVED: the ledger could not be read: " + e.scrub(err.Error())
	}
	// The reason is in the output of whichever run met the failure: look
	// through every Sextant run, latest first.
	reason := ""
	logs, _ := filepath.Glob(filepath.Join(e.root, "logs", "sextant-*.log"))
	for i := len(logs) - 1; i >= 0 && reason == ""; i-- {
		raw, err := os.ReadFile(logs[i])
		if err != nil {
			continue
		}
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		for j := len(lines) - 1; j >= 0 && reason == ""; j-- {
			var ev struct {
				Reason string `json:"reason"`
			}
			if json.Unmarshal([]byte(lines[j]), &ev) == nil {
				reason = ev.Reason
			}
		}
	}
	if reason == "" {
		reason = "none in its output"
	}
	origin := "SEXTANT_LOCAL_TEST_ORIGIN was not set (no run started)"
	e.mu.Lock()
	if p := e.lastProc["sextant"]; p != nil {
		origin = describeOrigin(p.env)
	}
	e.mu.Unlock()
	return e.scrub(fmt.Sprintf("OBSERVED: Sextant made %d ledger rows for %s; error code %q, note %q; its own reason %q; %s.",
		n, e.itemID, code, note, reason, origin))
}

// standInSextant does what Sextant's work loop does to Muster, by hand, with
// the result Sextant's own tests derive from the same page. It is a
// diagnostic only: nothing in it is Sextant.
func (e *env) standInSextant(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "wrap-rate", "results.sextant.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	status, lease, err := e.call("sextant", "POST", "/v1/lease", map[string]any{"contract": "v1"})
	if err != nil || status != 200 {
		t.Fatalf("stand-in: lease as sextant: status %d, %v, %v", status, lease, err)
	}
	status, body, err := e.call("sextant", "POST", "/v1/complete", map[string]any{
		"contract": "v1", "lease": lease["lease"], "state": "observed", "result": result,
	})
	if err != nil || status != 200 {
		t.Fatalf("stand-in: complete as sextant: status %d, %v, %v", status, body, err)
	}
	e.standIns = append(e.standIns, "sextant")
	t.Logf("STAND-IN: Sextant did not run. The harness completed its step with Sextant's own derived result for the same page.")
}

func stageAssay(e *env, t *testing.T) {
	e.drive(t, "assay", 30*time.Second, func() *proc {
		return e.start("assay", filepath.Join(e.bin, "assay"), []string{"run"},
			"MUSTER_URL="+e.musterURL,
			"ASSAY_TOKEN_FILE="+e.tokenFile("assay"),
			"ASSAY_SPOOL_DIR="+e.spool("assay"),
			"ASSAY_POLL_INTERVAL=300ms",
		)
	}, e.reaches("scored", "countersign"))
	// Reported, not asserted: what Assay made of the item.
	var rec string
	var score int
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.db.Admin.QueryRow(ctx, `SELECT recommendation, score FROM pub.evaluation WHERE item_id = $1`, e.itemID).Scan(&rec, &score); err == nil {
		e.notes["assay"] = fmt.Sprintf("recommendation %q, score %d", rec, score)
	} else {
		e.notes["assay"] = "no evaluation on record"
	}
}

// stageCountersign leases the scored item with the real Countersign, then
// decides it.
//
// In a real run the decision is the owner's, made with `countersign decide`.
// The harness is the owner here and only here: it runs that same command with
// `approve -waybill FILE`, giving the invented Wrap Rate specification in
// testdata, and it says so in the reason it records.
func stageCountersign(e *env, t *testing.T) {
	bin := filepath.Join(e.bin, "countersign")
	spool := e.spool("countersign")
	p := e.start("countersign", bin, []string{
		"-spool", spool, "run", "-muster", e.musterURL,
		"-token-file", e.tokenFile("countersign"), "-interval", "300ms",
	})
	listed := waitUntil(25*time.Second, func() bool {
		out, _ := e.run(bin, []string{"-spool", spool, "list"})
		return strings.Contains(out, e.itemID)
	})
	if !listed {
		out, _ := e.run(bin, []string{"-spool", spool, "list"})
		t.Fatalf("countersign never held the item for a decision.\n%s\n--- countersign list ---\n%s\n--- countersign output (last lines) ---\n%s",
			e.evidence(e.itemID), out, e.tail(p, 20))
	}
	out, err := e.run(bin, []string{
		"-spool", spool, "decide", e.itemID, "approve",
		"-reason", "stood in for the owner by the end-to-end harness; invented item",
		"-waybill", filepath.Join("testdata", "wrap-rate-waybill.json"),
	})
	if err != nil {
		t.Fatalf("countersign decide %s approve -waybill ...: %v\n%s\n%s", e.itemID, err, out, e.evidence(e.itemID))
	}
	e.drive(t, "countersign", 25*time.Second, func() *proc { return p }, e.reaches("approved", "waybill"))
	e.notes["countersign"] = "owner decision by harness"
}

func stageWaybill(e *env, t *testing.T) {
	e.drive(t, "waybill", 30*time.Second, func() *proc {
		return e.start("waybill", filepath.Join(e.bin, "waybill"),
			[]string{"run", "-once", "-spool", e.spool("waybill")},
			"MUSTER_URL="+e.musterURL,
			"WAYBILL_TOKEN_FILE="+e.tokenFile("waybill"),
			"WAYBILL_POLL_INTERVAL=300ms",
		)
	}, e.reaches("approved", "moldloft"))
}

func stageMoldloft(e *env, t *testing.T) {
	e.drive(t, "moldloft", 40*time.Second, func() *proc {
		return e.start("moldloft", filepath.Join(e.bin, "moldloft"), []string{
			"run", "-once",
			"-muster", e.musterURL,
			"-plainforge", e.gitURL + "/" + org,
			"-token-file", e.tokenFile("moldloft"),
			"-spool", e.spool("moldloft"),
		}, "MOLDLOFT_PLAINFORGE_CREDENTIALS_DIR="+e.credDir)
	}, e.reaches("building", "shakedown"))
}

func stageShakedown(e *env, t *testing.T) {
	e.drive(t, "shakedown", 60*time.Second, func() *proc {
		return e.start("shakedown", filepath.Join(e.bin, "shakedown"), []string{
			"run", "-once", "-listen", "off",
			"-muster-url", e.musterURL,
			"-token-file", e.tokenFile("shakedown"),
			"-spool-dir", e.spool("shakedown"),
		})
	}, e.reaches("building", "chartroom"))
}

func stageChartroom(e *env, t *testing.T) {
	e.drive(t, "chartroom", 40*time.Second, func() *proc {
		return e.start("chartroom", filepath.Join(e.bin, "chartroom"), []string{
			"run", "-once",
			"-muster-url", e.musterURL,
			"-plainforge-url", e.gitURL + "/" + org + "/" + hubRepo,
			"-token-file", e.tokenFile("chartroom"),
			"-spool", e.spool("chartroom"),
		}, "CHARTROOM_PLAINFORGE_CREDENTIALS_DIR="+e.credDir)
	}, e.reaches("live", "tidegauge"))
}

// canary is the string the invented page carries. The clean-room rule says
// nothing of a page may reach a generated tool, so it must not appear in it.
const canary = "CANARY-wrap-9f3e51c2"

// wantHistory is every change of state the item must have in the ledger, in
// order, with who made it. It follows the queue-step table in the contract.
var wantHistory = []struct{ actor, from, to string }{
	{"trawl", "", "discovered"},
	{"trawl", "discovered", "recorded"},
	{"sextant", "recorded", "observed"},
	{"assay", "observed", "scored"},
	{"countersign", "scored", "approved"},
	{"waybill", "approved", "approved"},
	{"moldloft", "approved", "building"},
	{"shakedown", "building", "building"},
	{"chartroom", "building", "live"},
}

// stageOutcome checks what the run was for.
func stageOutcome(e *env, t *testing.T) {
	var problems []string
	bad := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }

	// The item is live, and Tidegauge's first check is queued.
	if state, queued, err := e.item(e.itemID); err != nil || state != "live" || queued != "tidegauge" {
		bad("item %s: state=%q queued_for=[%s] err=%v, want live with a step for tidegauge", e.itemID, state, queued, err)
	}

	// A git failure is a failure of the check, never a clean result.
	mustGit := func(repo string, args ...string) string {
		out, err := e.git(repo, args...)
		if err != nil {
			bad("git %s in %s failed: %v", args[0], repo, err)
		}
		return out
	}

	// The hub: the catalog branch holds the entry, main is untouched.
	if _, err := e.git(hubRepo, "rev-parse", "--verify", "refs/heads/catalog/"+toolRepo); err != nil {
		bad("the hub has no refs/heads/catalog/%s: %v", toolRepo, err)
	} else if tools, err := e.git(hubRepo, "show", "refs/heads/catalog/"+toolRepo+":catalog/tools.json"); err != nil {
		bad("the catalog branch has no catalog/tools.json: %v", err)
	} else if !strings.Contains(tools, `"`+toolRepo+`"`) {
		bad("catalog/tools.json on the catalog branch does not name %s", toolRepo)
	}
	if out := mustGit(hubRepo, "rev-parse", "refs/heads/main"); strings.TrimSpace(out) != e.hubMain {
		bad("main in the hub moved: was %s, is %s", e.hubMain, strings.TrimSpace(out))
	}
	if hubRefs := strings.Fields(mustGit(hubRepo, "for-each-ref", "--format=%(refname)")); len(hubRefs) != 2 {
		bad("the hub should have exactly main and the catalog branch, it has: %s", strings.Join(hubRefs, " "))
	}

	// Moldloft pushed the generated tool repository, and nothing of the page
	// is in it: not in any file of any revision (binary or not), not in a
	// name, not in a commit.
	refs := strings.Fields(mustGit(toolRepo, "for-each-ref", "--format=%(refname)"))
	if len(refs) == 0 {
		bad("Moldloft pushed nothing to %s/%s", org, toolRepo)
	}
	files := 0
	for _, ref := range refs {
		files += len(strings.Fields(mustGit(toolRepo, "ls-tree", "-r", "--name-only", ref)))
	}
	if len(refs) > 0 && files == 0 {
		bad("the tool repository holds no files, so the canary check proved nothing")
	}
	if hits, err := scanCanary(e.bare(toolRepo), canary); err != nil {
		bad("the canary scan of the tool repository failed: %v", err)
	} else if len(hits) > 0 {
		bad("the canary is in the tool repository: %s", strings.Join(hits, "; "))
	}
	if page, err := os.ReadFile(filepath.Join("testdata", "wrap-rate", "page.html")); err != nil || !strings.Contains(string(page), canary) {
		bad("the invented page does not carry the canary, so the canary check proves nothing")
	}

	// The ledger holds every change of state, in order, by the right worker.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	type row struct{ actor, from, to string }
	var got []row
	rows, err := e.db.Admin.Query(ctx, `SELECT actor, coalesce(from_state, ''), to_state FROM pub.item_history WHERE item_id = $1 ORDER BY entry_id`, e.itemID)
	if err != nil {
		bad("pub.item_history: %v", err)
	} else {
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.actor, &r.from, &r.to); err != nil {
				bad("pub.item_history: %v", err)
				break
			}
			got = append(got, r)
		}
		rows.Close()
	}
	if len(got) != len(wantHistory) {
		bad("pub.item_history has %d rows for %s, want %d", len(got), e.itemID, len(wantHistory))
	}
	for i := 0; i < len(got) && i < len(wantHistory); i++ {
		if w := wantHistory[i]; got[i] != (row{w.actor, w.from, w.to}) {
			bad("pub.item_history row %d: got %s %q -> %q, want %s %q -> %q", i+1, got[i].actor, got[i].from, got[i].to, w.actor, w.from, w.to)
		}
	}

	// Logbook's own integrity checks find nothing wrong.
	checks := 0
	rows, err = e.db.Admin.Query(ctx, `SELECT check_name, failures FROM core.integrity_check()`)
	if err != nil {
		bad("core.integrity_check(): %v", err)
	} else {
		for rows.Next() {
			var name string
			var failures int64
			if err := rows.Scan(&name, &failures); err != nil {
				bad("core.integrity_check(): %v", err)
				break
			}
			checks++
			if failures != 0 {
				bad("core.integrity_check(): %q found %d", name, failures)
			}
		}
		rows.Close()
		if checks == 0 {
			bad("core.integrity_check() returned no checks")
		}
	}

	if len(problems) > 0 {
		t.Fatalf("the run did not end as it should:\n  - %s\n%s", strings.Join(problems, "\n  - "), e.evidence(e.itemID))
	}
	var recommendation string
	var score int
	_ = e.db.Admin.QueryRow(ctx, `SELECT recommendation, score FROM pub.evaluation WHERE item_id = $1`, e.itemID).Scan(&recommendation, &score)
	t.Logf("Assay's evaluation on record: recommendation %q, score %d", recommendation, score)
	t.Logf("%s is live; the catalog branch holds the entry and main is untouched; the tool repository has %d files and no canary; %d ledger rows; %d integrity checks, none failing", e.itemID, files, len(got), checks)
}
