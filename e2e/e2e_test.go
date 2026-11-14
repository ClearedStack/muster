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
	"fmt"
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

// itemAddress is where the invented candidate "was found". It has to look
// public: Trawl refuses the reserved suffixes (.test, .example, .invalid) and
// every IP address, and Sextant refuses anything but https on a public host.
// The name is invented and does not resolve to anything.
const itemAddress = "https://wrap-rate.e2e-agency.gov/calculator"

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

	type result struct {
		name, status, note string
	}
	var report []result
	t.Cleanup(func() {
		var b strings.Builder
		b.WriteString("\nSTAGE REPORT\n")
		for i, r := range report {
			fmt.Fprintf(&b, "  %2d  %-16s %s", i+1, r.name, r.status)
			if r.note != "" {
				fmt.Fprintf(&b, "  (%s)", r.note)
			}
			b.WriteString("\n")
		}
		t.Log(b.String())
	})

	failed := ""
	for _, s := range stages {
		if failed != "" {
			report = append(report, result{s.name, "NOT REACHED", "stage " + failed + " failed"})
			t.Run(s.name, func(t *testing.T) { t.Skipf("not reached: stage %s failed", failed) })
			continue
		}
		ok := t.Run(s.name, func(t *testing.T) { e.cur = t; s.run(e, t) })
		if ok {
			if slices.Contains(e.standIns, s.name) {
				report = append(report, result{s.name, "STAND-IN", "the worker did not run"})
				continue
			}
			report = append(report, result{s.name, "PASSED", ""})
		} else {
			report = append(report, result{s.name, "FAILED", "see the failure above"})
			failed = s.name
		}
	}
	if len(e.standIns) > 0 {
		t.Errorf("DIAGNOSTIC RUN, NOT AN ACCEPTANCE PASS: the harness stood in for %s (E2E_STAND_IN)", strings.Join(e.standIns, ", "))
	}
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

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{cur: t}
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
