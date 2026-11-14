//go:build e2e

// Package e2e is the Phase 1 acceptance test: one invented item, the Wrap
// Rate Calculator, travels from intake to live through the real worker
// binaries, against a real Muster and a real local PostgreSQL 17 holding
// Logbook's schema, and every change of state lands in the ledger.
//
// It runs only with the build tag e2e, so plain `go test ./...` is
// unchanged. It execs binaries and never imports worker code. Read the
// README section "End-to-end harness" before running it.
//
// A worker defect, or a mismatch between a worker and contract v1, is a
// finding: the stage that meets it fails with the evidence, the later stages
// are reported as not reached, and no worker is changed from here.
package e2e

import (
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ClearedStack/muster/internal/logbook/sql/sqltest"
)

// itemAddress is where the invented candidate "was found": the invented page
// on the local page server. Sextant reads it only because the harness names
// that server's origin in SEXTANT_LOCAL_TEST_ORIGIN.
func (e *env) itemAddress() string { return e.pageURL + "/calculator" }

type env struct {
	t    testing.TB // cleanups and the environment belong to the whole run; a failure belongs to the stage running
	cur  testing.TB // the stage running
	root string     // the run's own directory, removed when the test ends
	bin  string     // E2E_BIN_DIR: the built binaries
	// migrations is Logbook's migrations directory.
	migrations string

	mu      sync.Mutex
	serial  int
	secrets []string
	undo    []func() // what to run, last first, if the run is interrupted

	db        *sqltest.Logbook
	musterBin string
	musterURL string
	muster    *proc
	tokens    map[string]string

	gitURL           string // http://127.0.0.1:port, no path
	gitRoot          string
	gitLog           *os.File // what git http-backend says on stderr
	gitUser, gitPass string
	credDir          string
	hubMain          string // the commit main must still be at when the run ends
	pageURL          string

	itemID string
	// standIns names the workers that were stood in for; see E2E_STAND_IN.
	standIns []string
	// standInEnv is E2E_STAND_IN, read once at the start.
	standInEnv string
	// notes is what a stage adds to its line in the stage report.
	notes map[string]string
	// lastProc is the latest process started under each name.
	lastProc map[string]*proc
}

// tb sends failures to the stage that is running and everything else (cleanup,
// environment) to the whole run, so that a stage may start something that
// outlives it and still fail cleanly.
type tb struct {
	testing.TB
	e *env
}

func (x tb) Fatalf(f string, a ...any) { x.e.cur.Helper(); x.e.cur.Fatalf(f, a...) }
func (x tb) Fatal(a ...any)            { x.e.cur.Helper(); x.e.cur.Fatal(a...) }
func (x tb) FailNow()                  { x.e.cur.FailNow() }
func (x tb) Errorf(f string, a ...any) { x.e.cur.Helper(); x.e.cur.Errorf(f, a...) }

type stage struct {
	name string
	run  func(e *env, t *testing.T)
}

func TestEndToEnd(t *testing.T) {
	e := newEnv(t)

	stages := []stage{
		{"infrastructure", stageInfrastructure},
		{"trawl", stageTrawl},
		{"sextant", stageSextant},
		{"assay", stageAssay},
		{"countersign", stageCountersign},
		{"waybill", stageWaybill},
		{"moldloft", stageMoldloft},
		{"shakedown", stageShakedown},
		{"chartroom", stageChartroom},
		{"outcome", stageOutcome},
	}

	runStages(t, e, stages)
}

// runStages runs the stages in order and reports what the run amounts to.
//
// A stage ran only if its body reached its last statement: a subtest that a
// -run filter skipped makes t.Run return true too, so that result alone
// proves nothing. What the run amounts to is decided by judge.
func runStages(t *testing.T, e *env, stages []stage) verdict {
	ran := map[string]bool{}
	var runs []stageRun
	blocked := false
	for _, s := range stages {
		r := stageRun{Name: s.name}
		if blocked {
			t.Run(s.name, func(t *testing.T) { t.Skip("an earlier stage failed or did not run") })
			runs = append(runs, r)
			continue
		}
		ok := t.Run(s.name, func(t *testing.T) {
			e.cur = t
			s.run(e, t)
			ran[s.name] = true // the last statement of the subtest
		})
		r.Failed, r.Ran, r.Note = !ok, ran[s.name], e.notes[s.name]
		blocked = r.Failed || !r.Ran
		runs = append(runs, r)
	}
	v := judge(runs, e.standInEnv, e.standIns)
	t.Log(renderReport(e.versions(), v.Lines))
	for _, p := range v.Problems {
		t.Error(p)
	}
	return v
}

// cleanup runs f when the test ends, or when the run is interrupted with
// Ctrl-C or killed with SIGTERM, whichever comes first. A harness that left a
// PostgreSQL server behind when interrupted would be a nuisance.
func (e *env) cleanup(f func()) {
	f = sync.OnceFunc(f)
	e.mu.Lock()
	e.undo = append(e.undo, f)
	e.mu.Unlock()
	e.t.Cleanup(f)
}

func (e *env) onInterrupt() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		e.mu.Lock()
		undo := slices.Clone(e.undo)
		e.mu.Unlock()
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
		os.Exit(130)
	}()
	e.t.Cleanup(func() { signal.Stop(ch) })
}

// newBareEnv is an env with nothing started. E2E_STAND_IN is read here, once.
func newBareEnv() *env {
	return &env{notes: map[string]string{}, lastProc: map[string]*proc{}, standInEnv: os.Getenv("E2E_STAND_IN")}
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := newBareEnv()
	e.cur = t
	e.t = tb{t, e}
	e.onInterrupt()
	for _, tool := range []string{"initdb", "pg_ctl", "git"} {
		if _, err := lookPath(tool); err != nil {
			t.Fatalf("%s is not on PATH (the harness needs Homebrew postgresql@17 and git)", tool)
		}
	}
	e.bin = os.Getenv("E2E_BIN_DIR")
	if e.bin == "" {
		e.bin = "bin"
	}
	var err error
	if e.bin, err = filepath.Abs(e.bin); err != nil {
		t.Fatal(err)
	}
	for _, name := range append([]string{"muster"}, workerNames...) {
		if _, err := os.Stat(filepath.Join(e.bin, name)); err != nil {
			t.Fatalf("%s is missing: run e2e/build-workers.sh first (E2E_BIN_DIR=%s)", name, e.bin)
		}
	}
	e.migrations = os.Getenv("E2E_LOGBOOK_MIGRATIONS")
	if e.migrations == "" {
		e.migrations = filepath.Join(e.bin, "logbook", "migrations")
	}
	if _, err := os.Stat(filepath.Join(e.migrations, "0010_contract_v1_state.sql")); err != nil {
		t.Fatalf("Logbook's migrations 0001 to 0010 are not in %s: run e2e/build-workers.sh or set E2E_LOGBOOK_MIGRATIONS", e.migrations)
	}

	// A short path: the run's directory holds nothing that needs more, and
	// the test name in t.TempDir would make it long.
	e.root, err = os.MkdirTemp("", "e2e-")
	if err != nil {
		t.Fatal(err)
	}
	e.cleanup(func() { os.RemoveAll(e.root) })
	for _, d := range []string{"home", "tmp", "logs", "spool"} {
		if err := os.MkdirAll(filepath.Join(e.root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

var workerNames = []string{"trawl", "sextant", "assay", "countersign", "waybill", "moldloft", "shakedown", "chartroom"}

func lookPath(name string) (string, error) {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", os.ErrNotExist
}

func stageInfrastructure(e *env, t *testing.T) {
	e.gitUser, e.gitPass = "e2e-"+e.secret("u")[2:], e.secret("pw")
	e.credDir = filepath.Join(e.root, "creds")
	e.write("creds/user", e.gitUser, 0o600)
	e.write("creds/token", e.gitPass, 0o600)

	url := e.startPostgres()
	e.startMuster(url)
	e.gitURL = e.startGit()
	page, err := os.ReadFile(filepath.Join("testdata", "wrap-rate", "page.html"))
	if err != nil {
		t.Fatal(err)
	}
	e.pageURL = e.startPage(page)
	t.Logf("postgres, muster, the git server and the page server are up (muster %s)", e.musterURL)
}

// versions is what build-workers.sh wrote down: repository, branch and commit
// of everything the run executed.
func (e *env) versions() []string {
	raw, err := os.ReadFile(filepath.Join(e.bin, "versions.txt"))
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// waitUntil polls until ok reports true or the time is up.
func waitUntil(limit time.Duration, ok func() bool) bool {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if ok() {
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return ok()
}
