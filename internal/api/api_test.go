package api

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/clearedstack/muster/internal/logbook"
	"github.com/clearedstack/muster/internal/queue"
	"github.com/clearedstack/muster/internal/state"
)

var update = flag.Bool("update", false, "rewrite the expected output in testdata/cases from what the code does now")

// start is the made-up moment every saved example begins at.
var start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// A savedCase is one saved example: made-up items, the calls made in
// order, and the exact answers, ledger and records expected.
type savedCase struct {
	Rule         string            `json:"rule"`
	LeaseSeconds int               `json:"lease_seconds"`
	Items        []logbook.Seed    `json:"items"`
	Steps        []step            `json:"steps"`
	Ledger       []json.RawMessage `json:"ledger"`
	ItemsAfter   []json.RawMessage `json:"items_after"`
}

type step struct {
	AfterSeconds int             `json:"after_seconds,omitempty"` // time that passes before this call
	Method       string          `json:"method,omitempty"`        // POST unless given
	Path         string          `json:"path"`
	Body         json.RawMessage `json:"body,omitempty"` // sent as written
	Raw          *string         `json:"raw,omitempty"`  // sent instead of body, for broken input
	Status       int             `json:"status"`
	Response     json.RawMessage `json:"response,omitempty"` // absent when the answer has no body
}

type itemAfter struct {
	ID          string `json:"id"`
	State       string `json:"state"`
	Lane        string `json:"lane"`
	Queue       string `json:"queue"`
	LeaseID     string `json:"lease_id,omitempty"`
	LeaseWorker string `json:"lease_worker,omitempty"`
	LeaseUntil  string `json:"lease_until,omitempty"`
	PausedFrom  string `json:"paused_from,omitempty"`
	PausedBy    string `json:"paused_by,omitempty"`
}

type fixedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fixedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fixedClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newServer(t *testing.T, seeds []logbook.Seed, ttl time.Duration) (*Server, *logbook.Memory, *fixedClock) {
	t.Helper()
	book := logbook.NewMemory()
	if err := book.Load(seeds); err != nil {
		t.Fatal(err)
	}
	clock := &fixedClock{t: start}
	return &Server{Gate: &queue.Gatekeeper{Book: book, TTL: ttl}, Now: clock.now, Timeout: time.Second}, book, clock
}

func do(h http.Handler, method, path, body string) (int, string) {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec.Code, strings.TrimSuffix(rec.Body.String(), "\n")
}

func compact(t *testing.T, raw []byte) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("saved example holds broken JSON: %v", err)
	}
	return buf.String()
}

func after(book *logbook.Memory) []itemAfter {
	var out []itemAfter
	for _, it := range book.Items() {
		a := itemAfter{ID: it.ID, State: string(it.State), Lane: string(it.Lane), Queue: string(it.Queue),
			LeaseID: it.LeaseID, LeaseWorker: it.LeaseWorker, PausedFrom: string(it.PausedFrom), PausedBy: it.PausedBy}
		if !it.LeaseExpires.IsZero() {
			a.LeaseUntil = it.LeaseExpires.UTC().Format(time.RFC3339)
		}
		out = append(out, a)
	}
	return out
}

// TestSavedExamples replays every file in testdata/cases and compares each
// answer, the ledger and the records, byte for byte, with what was saved.
func TestSavedExamples(t *testing.T) {
	files, err := filepath.Glob("testdata/cases/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no saved examples found")
	}
	for _, file := range files {
		t.Run(strings.TrimSuffix(filepath.Base(file), ".json"), func(t *testing.T) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var c savedCase
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&c); err != nil {
				t.Fatal(err)
			}
			if c.Rule == "" || c.LeaseSeconds < 1 {
				t.Fatal("a saved example needs a rule and a lease length")
			}
			srv, book, clock := newServer(t, c.Items, time.Duration(c.LeaseSeconds)*time.Second)
			h := srv.Handler()
			for i := range c.Steps {
				s := &c.Steps[i]
				clock.add(time.Duration(s.AfterSeconds) * time.Second)
				method := s.Method
				if method == "" {
					method = http.MethodPost
				}
				body := string(s.Body)
				if s.Raw != nil {
					body = *s.Raw
				}
				status, got := do(h, method, s.Path, body)
				if got != "" {
					var e errorBody
					if json.Unmarshal([]byte(got), &e) == nil && e.Error.Code != "" {
						if e.Error.Code != ETimeout && e.Error.Code != EParse && e.Error.Code != ENeedsSubtool {
							t.Errorf("step %d: %q is not one of the three public error codes", i+1, e.Error.Code)
						}
					}
				}
				if *update {
					s.Status, s.Response = status, nil
					if got != "" {
						s.Response = json.RawMessage(got)
					}
					continue
				}
				want := ""
				if s.Response != nil {
					want = compact(t, s.Response)
				}
				if status != s.Status || got != want {
					t.Errorf("step %d: %s %s %s\n got %d %s\nwant %d %s", i+1, method, s.Path, body, status, got, s.Status, want)
				}
			}
			var ledger, items []json.RawMessage
			for _, e := range book.Ledger() {
				b, _ := json.Marshal(e)
				ledger = append(ledger, b)
			}
			for _, a := range after(book) {
				b, _ := json.Marshal(a)
				items = append(items, b)
			}
			if *update {
				c.Ledger, c.ItemsAfter = ledger, items
				if err := os.WriteFile(file, render(c), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			compare := func(what string, got, want []json.RawMessage) {
				if len(got) != len(want) {
					t.Errorf("%s: got %d lines, want %d", what, len(got), len(want))
					return
				}
				for i := range got {
					if string(got[i]) != compact(t, want[i]) {
						t.Errorf("%s line %d\n got %s\nwant %s", what, i+1, got[i], compact(t, want[i]))
					}
				}
			}
			compare("ledger", ledger, c.Ledger)
			compare("items_after", items, c.ItemsAfter)
		})
	}
}

// render writes a saved example with one step, ledger line or record to a
// line, so that a change shows up as a small diff.
func render(c savedCase) []byte {
	var b bytes.Buffer
	line := func(v any) string {
		raw, _ := json.Marshal(v)
		return string(raw)
	}
	list := func(name string, rows []string, last bool) {
		fmt.Fprintf(&b, "  %q: [", name)
		for i, r := range rows {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString("\n    " + r)
		}
		if len(rows) > 0 {
			b.WriteString("\n  ")
		}
		b.WriteString("]")
		if !last {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "{\n  \"rule\": %s,\n  \"lease_seconds\": %d,\n", line(c.Rule), c.LeaseSeconds)
	var items, steps, ledger, afterRows []string
	for _, it := range c.Items {
		items = append(items, line(it))
	}
	for _, s := range c.Steps {
		steps = append(steps, line(s))
	}
	for _, e := range c.Ledger {
		ledger = append(ledger, string(e))
	}
	for _, a := range c.ItemsAfter {
		afterRows = append(afterRows, string(a))
	}
	list("items", items, false)
	list("steps", steps, false)
	list("ledger", ledger, false)
	list("items_after", afterRows, true)
	b.WriteString("}\n")
	return b.Bytes()
}

// TestEveryMoveOverHTTP tries all 14 x 14 moves through lease and complete,
// and expects exactly the allowed ones to be accepted.
func TestEveryMoveOverHTTP(t *testing.T) {
	accepted := 0
	for _, from := range state.All() {
		for _, to := range state.All() {
			seeds := []logbook.Seed{{ID: "PA-0001", State: from, Lane: logbook.Normal}}
			if from == state.NeedsSubtool {
				// An item cannot be seeded as paused, and paused items are
				// not handed out: the lease itself must be refused.
				srv, _, _ := newServer(t, nil, time.Minute)
				if status, _ := do(srv.Handler(), "POST", "/v1/lease", `{"worker":"assay","state":"needs_subtool"}`); status != 400 {
					t.Fatalf("lease on needs_subtool: got %d, want 400", status)
				}
				continue
			}
			srv, book, _ := newServer(t, seeds, time.Minute)
			h := srv.Handler()
			status, body := do(h, "POST", "/v1/lease", fmt.Sprintf(`{"worker":"assay","state":%q}`, from))
			if !state.Leasable(from) {
				if status != 400 {
					t.Fatalf("lease on %s: got %d, want 400", from, status)
				}
				continue
			}
			if status != 200 {
				t.Fatalf("lease on %s: got %d %s", from, status, body)
			}
			status, body = do(h, "POST", "/v1/complete",
				fmt.Sprintf(`{"worker":"assay","item":"PA-0001","lease_id":"L-00000001","to":%q}`, to))
			ok := state.Allowed(from, to) && to != state.NeedsSubtool
			it := book.Items()[0]
			if ok {
				accepted++
				if status != 200 || it.State != to {
					t.Errorf("%s -> %s: got %d %s, item now %s; want it accepted", from, to, status, body, it.State)
				}
			} else if status != 409 || it.State != from || it.Queue != logbook.Leased || len(book.Ledger()) != 1 {
				t.Errorf("%s -> %s: got %d %s, item now %s %s; want 409 and nothing changed", from, to, status, body, it.State, it.Queue)
			}
		}
	}
	// 25 allowed moves, less the 3 into needs_subtool and the 3 out of it.
	if accepted != 19 {
		t.Errorf("complete accepted %d moves, want 19", accepted)
	}
}

// TestPauseFromEveryState tries needs_subtool from each state an item can
// be leased in.
func TestPauseFromEveryState(t *testing.T) {
	for _, from := range state.All() {
		if !state.Leasable(from) {
			continue
		}
		srv, book, _ := newServer(t, []logbook.Seed{{ID: "PA-0001", State: from, Lane: logbook.Normal}}, time.Minute)
		h := srv.Handler()
		do(h, "POST", "/v1/lease", fmt.Sprintf(`{"worker":"sextant","state":%q}`, from))
		status, body := do(h, "POST", "/v1/needs_subtool", `{"worker":"sextant","item":"PA-0001","lease_id":"L-00000001"}`)
		it := book.Items()[0]
		if state.CanPause(from) {
			if status != 200 || it.State != state.NeedsSubtool || it.PausedFrom != from || it.PausedBy != "sextant" || it.Queue != logbook.Idle {
				t.Errorf("pause from %s: got %d %s, item %+v", from, status, body, it)
			}
		} else if status != 409 || it.State != from || it.Queue != logbook.Leased {
			t.Errorf("pause from %s: got %d %s, want 409 and nothing changed", from, status, body)
		}
	}
}

func TestBodyOverLimitIsRefused(t *testing.T) {
	srv, book, _ := newServer(t, []logbook.Seed{{ID: "PA-0001", State: state.Recorded, Lane: logbook.Normal}}, time.Minute)
	h := srv.Handler()
	do(h, "POST", "/v1/lease", `{"worker":"sextant","state":"recorded"}`)
	pad := strings.Repeat("x", MaxBody)
	status, body := do(h, "POST", "/v1/complete",
		`{"worker":"sextant","item":"PA-0001","lease_id":"L-00000001","to":"observed","result":{"note":"`+pad+`"}}`)
	want := `{"error":{"code":"E_PARSE","message":"The request is larger than 64 KiB."}}`
	if status != 413 || body != want {
		t.Errorf("got %d %s\nwant 413 %s", status, body, want)
	}
	if it := book.Items()[0]; it.State != state.Recorded || len(book.Ledger()) != 1 {
		t.Errorf("an oversized request changed something: %+v", it)
	}
}

// slowBook stands for a Logbook that does not answer: it waits until the
// caller gives up.
type slowBook struct{}

func (slowBook) Update(ctx context.Context, fn func(logbook.Tx) error) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestSlowLogbookGivesTimeoutCode(t *testing.T) {
	srv := &Server{Gate: &queue.Gatekeeper{Book: slowBook{}, TTL: time.Minute},
		Now: func() time.Time { return start }, Timeout: 20 * time.Millisecond}
	want := `{"error":{"code":"E_TIMEOUT","message":"The record of state took too long to answer. Nothing was changed."}}`
	for _, c := range [][2]string{
		{"/v1/lease", `{"worker":"trawl","state":"discovered"}`},
		{"/v1/heartbeat", `{"worker":"trawl","item":"PA-0001","lease_id":"L-00000001"}`},
		{"/v1/complete", `{"worker":"trawl","item":"PA-0001","lease_id":"L-00000001","to":"recorded"}`},
		{"/v1/fail", `{"worker":"trawl","item":"PA-0001","lease_id":"L-00000001","code":"E_TIMEOUT"}`},
		{"/v1/needs_subtool", `{"worker":"trawl","item":"PA-0001","lease_id":"L-00000001"}`},
	} {
		if status, body := do(srv.Handler(), "POST", c[0], c[1]); status != 504 || body != want {
			t.Errorf("%s: got %d %s\nwant 504 %s", c[0], status, body, want)
		}
	}
}

// TestNoItemIsHandedOutTwice has many workers ask at once and checks each
// item goes to exactly one of them.
func TestNoItemIsHandedOutTwice(t *testing.T) {
	var seeds []logbook.Seed
	for i := 1; i <= 50; i++ {
		seeds = append(seeds, logbook.Seed{ID: fmt.Sprintf("PA-%04d", i), State: state.Recorded, Lane: logbook.Normal})
	}
	srv, _, _ := newServer(t, seeds, time.Minute)
	h := srv.Handler()
	var mu sync.Mutex
	got := map[string]int{}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				status, body := do(h, "POST", "/v1/lease", `{"worker":"sextant","state":"recorded"}`)
				if status != 200 {
					return
				}
				var l leaseBody
				json.Unmarshal([]byte(body), &l)
				mu.Lock()
				got[l.Item]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(got) != 50 {
		t.Fatalf("%d items were handed out, want 50", len(got))
	}
	for id, n := range got {
		if n != 1 {
			t.Errorf("%s was handed out %d times", id, n)
		}
	}
}
