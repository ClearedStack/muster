package httpapi_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ClearedStack/muster/internal/contract"
	"github.com/ClearedStack/muster/internal/httpapi"
	"github.com/ClearedStack/muster/internal/logbook"
	"github.com/ClearedStack/muster/internal/logbook/memory"
)

func oneItem() *memory.Seed {
	return &memory.Seed{Items: []memory.SeedItem{{ID: "PA-0001", State: "recorded", Lane: "normal", Next: "sextant"}}}
}

func post(path, as string, body any) step {
	raw, _ := json.Marshal(body)
	return step{Method: http.MethodPost, Path: path, As: as, Send: raw}
}

func refusalOf(t *testing.T, rec *httptest.ResponseRecorder) *contract.Problem {
	t.Helper()
	var body struct {
		Error  contract.Code `json:"error"`
		Cause  string        `json:"cause"`
		Reason string        `json:"reason"`
	}
	dec := json.NewDecoder(rec.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		t.Fatalf("not a refusal: %v", err)
	}
	for _, p := range contract.All() {
		if p.Status == rec.Code && p.Code == body.Error && p.Cause == body.Cause && p.Reason == body.Reason {
			return p
		}
	}
	t.Fatalf("%d %s %s %q is not one of the fixed refusals", rec.Code, body.Error, body.Cause, body.Reason)
	return nil
}

// Any body may weigh 64 KiB. Waybill's complete, which carries a whole
// specification, may weigh 256 KiB.
func TestBodySizeLimits(t *testing.T) {
	seed := &memory.Seed{Items: []memory.SeedItem{
		{ID: "PA-0001", State: "recorded", Lane: "normal", Next: "sextant"},
		{ID: "PA-0002", State: "approved", Lane: "normal", Next: "waybill"},
	}}
	s := newStack(t, seed)
	s.do(t, post("/v1/lease", "sextant", map[string]string{"contract": "v1"}))
	s.do(t, post("/v1/lease", "waybill", map[string]string{"contract": "v1"}))
	complete := func(as, lease, state string, pad int) *httptest.ResponseRecorder {
		return s.do(t, post("/v1/complete", as, map[string]any{
			"contract": "v1", "lease": lease, "state": state,
			"result": map[string]string{"text": strings.Repeat("x", pad)},
		}))
	}
	if p := refusalOf(t, complete("sextant", "PA-0001.1.1", "observed", httpapi.MaxBody)); p != contract.TooLarge {
		t.Errorf("an oversized call from Sextant: %v", p)
	}
	if rec := complete("sextant", "PA-0001.1.1", "observed", httpapi.MaxBody-200); rec.Code != http.StatusOK {
		t.Errorf("a call from Sextant just inside the limit: %d %s", rec.Code, rec.Body)
	}
	if p := refusalOf(t, complete("waybill", "PA-0002.2.1", "approved", httpapi.MaxWaybillBody)); p != contract.TooLarge {
		t.Errorf("an oversized call from Waybill: %v", p)
	}
	if rec := complete("waybill", "PA-0002.2.1", "approved", httpapi.MaxWaybillBody-200); rec.Code != http.StatusOK {
		t.Errorf("a call from Waybill just inside its limit: %d %s", rec.Code, rec.Body)
	}
	// The larger limit is for Waybill's complete and nothing else.
	big := map[string]any{"contract": "v1", "pad": strings.Repeat("x", httpapi.MaxBody)}
	for _, path := range []string{"/v1/lease", "/v1/heartbeat", "/v1/fail", "/v1/needs_subtool"} {
		if p := refusalOf(t, s.do(t, post(path, "waybill", big))); p != contract.TooLarge {
			t.Errorf("%s from Waybill: %v", path, p)
		}
	}
	for path, as := range map[string]string{"/v1/items": "trawl", "/v1/release": "countersign", "/v1/health": "shipkeeper", "/v1/edits": "conn"} {
		if p := refusalOf(t, s.do(t, post(path, as, big))); p != contract.TooLarge {
			t.Errorf("%s: %v", path, p)
		}
	}
}

// slow is a Logbook that never answers in time.
type slow struct{ logbook.Store }

func (slow) Update(context.Context, func(logbook.Tx) error) error { return context.DeadlineExceeded }
func (slow) View(context.Context, func(logbook.Tx) error) error   { return context.DeadlineExceeded }

// When Logbook takes too long, every call that needs it answers
// E_TIMEOUT, and nothing about the store reaches the caller.
func TestSlowStoreIsATimeout(t *testing.T) {
	s := newStack(t, oneItem())
	s.server.Queue.Store = slow{}
	s.server.Health.Store = slow{}
	s.server.Intake.Store = slow{}
	s.hub.Store = slow{}

	example := readCase(t, "edits-accepted")
	calls := []step{
		post("/v1/lease", "sextant", map[string]string{"contract": "v1"}),
		post("/v1/heartbeat", "sextant", map[string]string{"contract": "v1", "lease": "PA-0001.1.1"}),
		post("/v1/complete", "sextant", map[string]string{"contract": "v1", "lease": "PA-0001.1.1", "state": "observed"}),
		post("/v1/fail", "sextant", map[string]string{"contract": "v1", "lease": "PA-0001.1.1", "code": "E_PARSE"}),
		post("/v1/needs_subtool", "sextant", map[string]string{"contract": "v1", "lease": "PA-0001.1.1"}),
		post("/v1/health", "shipkeeper", map[string]any{"contract": "v1", "reports": []map[string]string{{"component": "sextant", "state": "healthy"}}}),
		post("/v1/release", "countersign", map[string]string{"contract": "v1", "id": "PA-0001"}),
		post("/v1/items", "trawl", map[string]string{"contract": "v1", "source": "sam-gov", "title": "A made-up title", "address": "https://example.org/made-up"}),
		example.Steps[0],
		{Method: http.MethodGet, Path: "/v1/feed", As: "conn"},
	}
	for _, c := range calls {
		if p := refusalOf(t, s.do(t, c)); p != contract.StoreSlow {
			t.Errorf("%s: %v", c.Path, p)
		}
	}
}

func readCase(t *testing.T, name string) caseFile {
	t.Helper()
	var cf caseFile
	if err := json.Unmarshal(readFile(t, filepath.Join("testdata", "cases", name+".json")), &cf); err != nil {
		t.Fatal(err)
	}
	return cf
}

// The address a proposal came from is used for one hash and kept
// nowhere: not in the proposal, not in the queue, not in the ledger.
func TestSourceAddressIsNeverKept(t *testing.T) {
	s := newStack(t, nil)
	example := readCase(t, "edits-accepted")
	for _, st := range example.Steps[:2] {
		if rec := s.do(t, st); rec.Code != http.StatusAccepted {
			t.Fatalf("the saved submission was not kept: %d %s", rec.Code, rec.Body)
		}
	}
	// Everything the stand-in holds, written out.
	var dump bytes.Buffer
	enc := json.NewEncoder(&dump)
	if err := s.store.View(context.Background(), func(tx logbook.Tx) error {
		for _, id := range []string{"EP-0001", "EP-0002"} {
			p, ok := tx.Proposal(id)
			if !ok {
				t.Fatalf("%s was not kept", id)
			}
			e, _ := tx.Entry(id)
			enc.Encode(p)
			enc.Encode(e)
		}
		enc.Encode(tx.Components())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	enc.Encode(s.store.Ledger())
	for _, address := range []string{"203.0.113.7", "2001:db8::7", "203.0.113", "2001:db8"} {
		if strings.Contains(dump.String(), address) {
			t.Errorf("the records hold the source address %s:\n%s", address, dump.String())
		}
	}
}

// Every refusal a caller can provoke is one of the fixed refusals, with
// no other field. This throws a spread of bad calls at every path.
func TestOnlyFixedRefusals(t *testing.T) {
	s := newStack(t, oneItem())
	paths := []string{"/v1/lease", "/v1/heartbeat", "/v1/complete", "/v1/fail", "/v1/needs_subtool", "/v1/release", "/v1/items", "/v1/health", "/v1/edits", "/v1/feed", "/healthz", "/v1/", "/v1/lease/", "/made-up"}
	bodies := []string{``, `{}`, `[]`, `null`, `"v1"`, `{"contract":"v1"}`, `{"contract":"v9"}`, `{"contract":1}`,
		`{"contract":"v1","lease":"PA-0001.1","state":"observed","code":"E_PARSE"}`,
		`{"contract":"v1","lease":"../../made-up","state":"\u0000"}`,
		`{"contract":"v1","reports":[{"component":"sextant","state":"healthy","digest":"made-up"}]}`,
		`{"contract":"v1","target":"x","change":"y","reason":"","pow":"z","honeypot":"","source":"made-up"}`,
		`{"contract":"v1","id":"PA-0001"}`, `{"contract":"v1","id":"made-up"}`,
		`{"contract":"v1","source":"site-form","title":"t","address":"a","pow":"z","visitor":"made-up"}`,
		`{"contract":"v1","source":"made-up","title":"t","address":"a b"}`,
		`{"contract":"v1"`, "\x00\x01\x02", strings.Repeat("[", 5000)}
	callers := []string{"", "sextant", "conn", "shipkeeper", "tidegauge", "trawl", "countersign"}
	for _, path := range paths {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
			if path == "/v1/feed" && method == http.MethodGet {
				continue // a stream; covered by TestFeedOverHTTP
			}
			for _, as := range callers {
				for _, body := range bodies {
					b := body
					rec := s.do(t, step{Method: method, Path: path, As: as, SendRaw: &b})
					if rec.Code < 400 {
						continue
					}
					refusalOf(t, rec)
				}
			}
		}
	}
}

// The feed over a real connection: Conn subscribes, gets the current
// picture at once, then a batch on the tick with what happened in
// between.
func TestFeedOverHTTP(t *testing.T) {
	s := newStack(t, oneItem())
	srv := httptest.NewServer(s.server)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/feed", nil)
	req.Header.Set("Authorization", "Bearer "+s.tokens["conn"])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d, content type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	events := bufio.NewReader(resp.Body)
	next := func() string {
		var event strings.Builder
		for {
			line, err := events.ReadString('\n')
			if err != nil {
				t.Fatalf("the stream ended: %v (so far %q)", err, event.String())
			}
			if line == "\n" {
				return event.String()
			}
			event.WriteString(line)
		}
	}
	want := func(got, text string) {
		t.Helper()
		if got != text {
			t.Errorf("got event\n%s\nwant\n%s", got, text)
		}
	}
	want(next(), "id: 1\nevent: depths\ndata: {\"at\":\"2026-01-05T09:00:00Z\",\"urgent\":0,\"normal\":1,\"backfill\":0,\"needs_subtool\":0}\n")

	// Shipkeeper reports, a visitor proposes an edit, a worker takes the
	// one waiting item. Five seconds pass and the feed ticks.
	if rec := s.do(t, post("/v1/health", "shipkeeper", map[string]any{"contract": "v1", "reports": []map[string]string{{"component": "sextant", "state": "idle"}}})); rec.Code != http.StatusOK {
		t.Fatal(rec.Body)
	}
	if rec := s.do(t, readCase(t, "edits-accepted").Steps[0]); rec.Code != http.StatusAccepted {
		t.Fatal(rec.Body)
	}
	s.do(t, post("/v1/lease", "sextant", map[string]string{"contract": "v1"}))
	s.clock.Advance(5 * time.Second)
	s.hub.Tick(ctx)

	// One waiting entry: the proposal, in the normal lane. The item is
	// under lease.
	want(next(), "id: 2\nevent: depths\ndata: {\"at\":\"2026-01-05T09:00:05Z\",\"urgent\":0,\"normal\":1,\"backfill\":0,\"needs_subtool\":0}\n")
	want(next(), "id: 3\nevent: health\ndata: {\"at\":\"2026-01-05T09:00:00Z\",\"component\":\"sextant\",\"state\":\"idle\"}\n")
	want(next(), "id: 4\nevent: edit\ndata: {\"at\":\"2026-01-05T09:00:00Z\",\"id\":\"EP-0001\",\"status\":\"pending_approval\"}\n")

	// Countersign approves it: the status change is on the next batch.
	s.do(t, post("/v1/lease", "countersign", map[string]string{"contract": "v1"}))
	if rec := s.do(t, post("/v1/complete", "countersign", map[string]string{"contract": "v1", "lease": "EP-0001.2.1", "state": "approved"})); rec.Code != http.StatusOK {
		t.Fatal(rec.Body)
	}
	s.clock.Advance(5 * time.Second)
	s.hub.Tick(ctx)
	want(next(), "id: 5\nevent: depths\ndata: {\"at\":\"2026-01-05T09:00:10Z\",\"urgent\":1,\"normal\":0,\"backfill\":0,\"needs_subtool\":0}\n")
	want(next(), "id: 6\nevent: edit\ndata: {\"at\":\"2026-01-05T09:00:05Z\",\"id\":\"EP-0001\",\"status\":\"approved\"}\n")

	// When the subscriber goes away the hub lets go of it.
	cancel()
	io.Copy(io.Discard, resp.Body)
}

func TestTooManyFeedSubscribers(t *testing.T) {
	s := newStack(t, nil)
	for {
		if _, err := s.hub.Subscribe(context.Background()); err != nil {
			break
		}
	}
	rec := s.do(t, step{Method: http.MethodGet, Path: "/v1/feed", As: "conn"})
	if p := refusalOf(t, rec); p != contract.Busy {
		t.Errorf("got %v", p)
	}
}

// blocking is a Logbook that holds every transaction until released.
type blocking struct {
	logbook.Store
	entered chan struct{}
	release chan struct{}
}

func (b blocking) Update(ctx context.Context, fn func(logbook.Tx) error) error {
	b.entered <- struct{}{}
	<-b.release
	return b.Store.Update(ctx, fn)
}

// Muster answers a fixed number of calls at once. One more is told to
// try again; it is not queued, so a flood cannot grow memory.
func TestBusyLimit(t *testing.T) {
	s := newStack(t, oneItem())
	gate := blocking{s.store, make(chan struct{}, httpapi.MaxInFlight), make(chan struct{})}
	s.server.Queue.Store = gate
	lease := post("/v1/lease", "tidegauge", map[string]string{"contract": "v1"})

	done := make(chan int, httpapi.MaxInFlight)
	for i := 0; i < httpapi.MaxInFlight; i++ {
		go func() { done <- s.do(t, lease).Code }()
	}
	for i := 0; i < httpapi.MaxInFlight; i++ {
		<-gate.entered
	}
	if p := refusalOf(t, s.do(t, lease)); p != contract.Busy {
		t.Errorf("one call too many: %v", p)
	}
	// The cluster's own check is still answered, so a rush is never
	// mistaken for Muster being down.
	if rec := s.do(t, step{Method: http.MethodGet, Path: "/healthz"}); rec.Code != http.StatusOK {
		t.Errorf("healthz while busy: %d", rec.Code)
	}
	close(gate.release)
	for i := 0; i < httpapi.MaxInFlight; i++ {
		if code := <-done; code != http.StatusNoContent {
			t.Errorf("a held call ended with %d", code)
		}
	}
	if rec := s.do(t, lease); rec.Code != http.StatusNoContent {
		t.Errorf("after the rush: %d", rec.Code)
	}
}
