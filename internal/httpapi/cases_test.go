package httpapi_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"math/bits"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClearedStack/muster/internal/auth"
	"github.com/ClearedStack/muster/internal/feed"
	"github.com/ClearedStack/muster/internal/health"
	"github.com/ClearedStack/muster/internal/httpapi"
	"github.com/ClearedStack/muster/internal/intake"
	"github.com/ClearedStack/muster/internal/logbook"
	"github.com/ClearedStack/muster/internal/logbook/memory"
	"github.com/ClearedStack/muster/internal/logbook/sql/sqltest"
	"github.com/ClearedStack/muster/internal/queue"
)

// The saved examples in testdata/cases are the record of how Muster
// behaves. Each file is a short story: an optional set of made-up items
// to start from, then calls in order, each with the exact status and
// body expected back.
//
// Run "go test ./internal/httpapi -update" to rewrite the expected
// answers from what Muster does now, then read the difference: every
// changed line is a change in behaviour.
var update = flag.Bool("update", false, "rewrite the expected answers in the saved examples")

// Every example starts at this made-up moment.
var start = time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)

// Settings every example runs with: the defaults a deployment gets.
const (
	leaseFor   = 60 * time.Second
	powBits    = 20
	dailyCap   = 5
	storeWaits = 5 * time.Second
)

type caseFile struct {
	About string       `json:"about"`
	Seed  *memory.Seed `json:"seed,omitempty"`
	Steps []step       `json:"steps"`
}

type step struct {
	Note    string          `json:"note,omitempty"`
	Wait    string          `json:"wait,omitempty"`  // let this much time pass first
	As      string          `json:"as,omitempty"`    // which caller's made-up token to present
	Token   string          `json:"token,omitempty"` // or a token to present as written
	Method  string          `json:"method"`
	Path    string          `json:"path"`
	Type    *string         `json:"type,omitempty"`     // content type, when not application/json
	Send    json.RawMessage `json:"send,omitempty"`     // the body, as JSON
	SendRaw *string         `json:"send_raw,omitempty"` // or the body, as written
	Status  int             `json:"status"`
	Want    json.RawMessage `json:"want,omitempty"` // the exact body expected; none when left out
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type stack struct {
	clock  *clock
	store  logbook.Store
	mem    *memory.Store    // the stand-in, when that is the store
	book   *sqltest.Logbook // a real Logbook, when that is the store
	hub    *feed.Hub
	server *httpapi.Server
	tokens map[string]string // caller name to its made-up token
}

// newStack builds Muster on the in-memory stand-in for Logbook.
func newStack(t *testing.T, seed *memory.Seed) *stack {
	t.Helper()
	clk := &clock{t: start}
	mem := memory.New()
	if seed != nil {
		if err := mem.Load(*seed); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	s := build(t, clk, mem)
	s.mem = mem
	return s
}

// newStackOnLogbook builds Muster on a real Logbook: a PostgreSQL
// database of the test's own, with Logbook's migrations applied. The
// test is skipped when no test database is given.
func newStackOnLogbook(t *testing.T, seed *memory.Seed) *stack {
	t.Helper()
	clk := &clock{t: start}
	book := sqltest.New(t, clk.Now)
	if seed != nil {
		book.Seed(t, *seed, start)
	}
	s := build(t, clk, book.Store)
	s.book = book
	return s
}

func build(t *testing.T, clk *clock, store logbook.Store) *stack {
	t.Helper()
	callers := readFile(t, "testdata/callers.json")
	table, err := auth.ParseTable(callers)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Callers map[string]string `json:"callers"`
	}
	if err := json.Unmarshal(callers, &file); err != nil {
		t.Fatal(err)
	}
	tokens := map[string]string{}
	for token, name := range file.Callers {
		tokens[name] = token
	}
	owners, err := intake.ParseOwnership(readFile(t, "testdata/ownership_map.json"))
	if err != nil {
		t.Fatal(err)
	}
	hub := &feed.Hub{Store: store, Clock: clk.Now, Timeout: storeWaits}
	server := httpapi.New(httpapi.Server{
		Auth:   table,
		Queue:  &queue.Service{Store: store, Clock: clk.Now, LeaseFor: leaseFor, Notify: hub},
		Health: &health.Service{Store: store, Clock: clk.Now, Notify: hub},
		Intake: &intake.Service{
			Store: store, Clock: clk.Now, Notify: hub, Owners: owners,
			Secret:     bytes.TrimSpace(readFile(t, "testdata/salt.txt")),
			Difficulty: powBits, DailyCap: dailyCap,
		},
		Feed:         hub,
		StoreTimeout: storeWaits,
	})
	return &stack{clock: clk, store: store, hub: hub, server: server, tokens: tokens}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func (s *stack) do(t *testing.T, st step) *httptest.ResponseRecorder {
	t.Helper()
	var body []byte
	switch {
	case st.SendRaw != nil:
		body = []byte(*st.SendRaw)
	case st.Send != nil:
		var buf bytes.Buffer
		if err := json.Compact(&buf, st.Send); err != nil {
			t.Fatalf("send is not JSON: %v", err)
		}
		body = buf.Bytes()
	}
	req := httptest.NewRequest(st.Method, st.Path, bytes.NewReader(body))
	if body != nil {
		kind := "application/json"
		if st.Type != nil {
			kind = *st.Type
		}
		if kind != "" {
			req.Header.Set("Content-Type", kind)
		}
	}
	token := st.Token
	if st.As != "" {
		var ok bool
		if token, ok = s.tokens[st.As]; !ok {
			t.Fatalf("no made-up token for %q", st.As)
		}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.server.ServeHTTP(rec, req)
	return rec
}

func TestSavedExamples(t *testing.T) {
	replayCases(t, newStack)
}

func TestSavedExamplesOnLogbook(t *testing.T) {
	if !sqltest.Available() {
		t.Skip("no test Logbook: set MUSTER_TEST_DATABASE_URL and MUSTER_LOGBOOK_MIGRATIONS")
	}
	if *update {
		t.Skip("the expected answers are written from the stand-in")
	}
	replayCases(t, newStackOnLogbook)
}

func replayCases(t *testing.T, newStack func(*testing.T, *memory.Seed) *stack) {
	paths, err := filepath.Glob("testdata/cases/*.json")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no saved examples found (%v)", err)
	}
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".json")
		t.Run(name, func(t *testing.T) {
			var cf caseFile
			dec := json.NewDecoder(bytes.NewReader(readFile(t, path)))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&cf); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			if cf.About == "" {
				t.Fatalf("%s: every example says what it is about", path)
			}
			s := newStack(t, cf.Seed)
			var granted []string
			for i := range cf.Steps {
				st := &cf.Steps[i]
				if st.Wait != "" {
					d, err := time.ParseDuration(st.Wait)
					if err != nil {
						t.Fatalf("step %d: wait: %v", i+1, err)
					}
					s.clock.Advance(d)
				}
				if *update {
					solve(t, st, s.clock.Now())
					st.Send = realLeases(st.Send, granted)
				}
				rec := s.do(t, *st)
				got := rec.Body.Bytes()
				granted = noteLease(granted, st.Path, rec)
				if *update {
					st.Status = rec.Code
					st.Want = nil
					if len(got) > 0 {
						st.Want = json.RawMessage(bytes.TrimSuffix(got, []byte("\n")))
					}
					continue
				}
				var want []byte
				if st.Want != nil {
					var buf bytes.Buffer
					if err := json.Compact(&buf, st.Want); err != nil {
						t.Fatalf("step %d: want is not JSON: %v", i+1, err)
					}
					want = append(buf.Bytes(), '\n')
				}
				if rec.Code != st.Status || !bytes.Equal(got, want) {
					t.Errorf("step %d (%s %s as %q, %s)\n got: %d %s\nwant: %d %s",
						i+1, st.Method, st.Path, st.As, st.Note, rec.Code, got, st.Status, want)
				}
				if len(got) > 0 && rec.Header().Get("Content-Type") != "application/json" {
					t.Errorf("step %d: an answer with a body must be application/json", i+1)
				}
			}
			if *update {
				var buf bytes.Buffer
				enc := json.NewEncoder(&buf)
				enc.SetEscapeHTML(false)
				enc.SetIndent("", "  ")
				if err := enc.Encode(cf); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// solve fills in a proof-of-work token where an example being written
// has "pow": "?". It runs only with -update; after that the token is
// part of the saved input like everything else.
//
// It is written from the description of the puzzle, not by calling
// Muster's own code, so that it checks that description too.
//
// "pow": "COPY" takes the token of the step before it that was solved
// this way, for examples about a token used a second time.
var (
	unsolved = regexp.MustCompile(`"pow":\s*"\?"`)
	copied   = regexp.MustCompile(`"pow":\s*"COPY"`)
	lastPow  []byte
)

func solve(t *testing.T, st *step, now time.Time) {
	t.Helper()
	if st.Send != nil && copied.Match(st.Send) {
		st.Send = copied.ReplaceAllLiteral(st.Send, lastPow)
		return
	}
	if st.Send == nil || !unsolved.Match(st.Send) {
		return
	}
	// An edit proposal's puzzle covers its target, change and reason. A
	// candidate's covers its source, title and address.
	var sub struct {
		Target  string `json:"target"`
		Change  string `json:"change"`
		Reason  string `json:"reason"`
		Source  string `json:"source"`
		Title   string `json:"title"`
		Address string `json:"address"`
	}
	if err := json.Unmarshal(st.Send, &sub); err != nil {
		t.Fatal(err)
	}
	parts := []string{sub.Target, sub.Change, sub.Reason}
	if st.Path == "/v1/items" {
		parts = []string{sub.Source, sub.Title, sub.Address}
	}
	day := now.UTC().Format("2006-01-02")
	prefix := []byte("muster-pow-v1\x00" + day + "\x00" + parts[0] + "\x00" + parts[1] + "\x00" + parts[2] + "\x00")
	for n := 0; ; n++ {
		nonce := fmt.Sprintf("%x", n)
		sum := sha256.Sum256(append(prefix[:len(prefix):len(prefix)], nonce...))
		zeros := 0
		for _, b := range sum {
			if b != 0 {
				zeros += bits.LeadingZeros8(b)
				break
			}
			zeros += 8
		}
		if zeros >= powBits {
			lastPow = []byte(`"pow": "` + day + "." + nonce + `"`)
			st.Send = unsolved.ReplaceAllLiteral(st.Send, lastPow)
			return
		}
	}
}

// The scripts that draft the saved examples name a lease by its place in
// the story: PA-0001.2 for the second lease granted. When an example is
// written (with -update) that shorthand is replaced by the lease Muster
// really gave, and a lease that was never granted by one that never
// could be. After that the file holds real leases only.
var shortLease = regexp.MustCompile(`"lease":\s*"((?:incident:)?[A-Z]{2}-[0-9]+)\.([0-9]+)"`)

func realLeases(send json.RawMessage, granted []string) json.RawMessage {
	if send == nil {
		return send
	}
	return shortLease.ReplaceAllFunc(send, func(m []byte) []byte {
		parts := shortLease.FindSubmatch(m)
		n := 0
		fmt.Sscanf(string(parts[2]), "%d", &n)
		if n >= 1 && n <= len(granted) && strings.HasPrefix(granted[n-1], string(parts[1])+".") {
			return []byte(`"lease": "` + granted[n-1] + `"`)
		}
		return []byte(`"lease": "` + string(parts[1]) + `.999.1"`)
	})
}

func noteLease(granted []string, path string, rec *httptest.ResponseRecorder) []string {
	if path != "/v1/lease" || rec.Code != 200 {
		return granted
	}
	var g struct {
		Lease string `json:"lease"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &g) == nil && g.Lease != "" {
		granted = append(granted, g.Lease)
	}
	return granted
}
