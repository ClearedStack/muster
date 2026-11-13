package httpapi_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ClearedStack/muster/internal/auth"
	"github.com/ClearedStack/muster/internal/contract"
	"github.com/ClearedStack/muster/internal/httpapi"
	"github.com/ClearedStack/muster/internal/lifecycle"
	"github.com/ClearedStack/muster/internal/logbook"
	"github.com/ClearedStack/muster/internal/logbook/memory"
)

// contract/v1/examples holds one saved example for each call and each
// refusal of the contract: the request, without the token, and the exact
// status and body expected back. This test replays every one of them
// against Muster running on the in-memory stand-in for Logbook.
//
// Every example starts at the same made-up moment as the saved stories,
// 2026-01-05T09:00:00Z, with the default settings.
//
// Run "go test ./internal/httpapi -run TestContractExamples -update" to
// rewrite the expected answers from what Muster does now.
const examplesDir = "../../contract/v1/examples"

type example struct {
	About   string  `json:"about"`
	Given   *given  `json:"given,omitempty"`
	Request request `json:"request"`
	Expect  expect  `json:"expect"`
}

// given sets the scene before the request is made.
type given struct {
	Items     []memory.SeedItem `json:"items,omitempty"`     // made-up items on record
	Calls     []request         `json:"calls,omitempty"`     // earlier calls, each of which must succeed
	Wait      string            `json:"wait,omitempty"`      // time that passes after them
	Condition string            `json:"condition,omitempty"` // something wrong outside the request; see arrange
}

// request is a call without its token. As names the ServiceAccount whose
// token is presented; left out, no token is presented at all.
type request struct {
	As          string          `json:"as,omitempty"`
	Method      string          `json:"method"`
	Path        string          `json:"path"`
	ContentType *string         `json:"content_type,omitempty"` // when not application/json
	Body        json.RawMessage `json:"body,omitempty"`
	Raw         *string         `json:"raw,omitempty"` // a body that is not JSON, as written
}

type expect struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body,omitempty"`   // the exact body; none when left out
	Stream *string         `json:"stream,omitempty"` // for the feed: the events a new subscriber is sent at once
}

func (r request) step() step {
	return step{As: r.As, Method: r.Method, Path: r.Path, Type: r.ContentType, Send: r.Body, SendRaw: r.Raw}
}

type slowAuth struct{}

func (slowAuth) Identify(context.Context, string) (string, error) { return "", auth.ErrUnavailable }

// broken is a Logbook that fails in a way nothing provides for.
type broken struct{ logbook.Store }

func (broken) Update(context.Context, func(logbook.Tx) error) error {
	return errors.New("made-up failure")
}
func (broken) View(context.Context, func(logbook.Tx) error) error {
	return errors.New("made-up failure")
}

// arrange brings about a condition an example names: something wrong
// that no request can cause by itself.
func arrange(t *testing.T, s *stack, condition string) {
	t.Helper()
	setStore := func(store logbook.Store) {
		s.server.Queue.Store, s.server.Health.Store, s.server.Intake.Store, s.hub.Store = store, store, store, store
	}
	switch condition {
	case "":
	case "logbook takes too long":
		setStore(slow{})
	case "logbook fails in a way nothing provides for":
		setStore(broken{})
	case "every place for a call in flight is taken":
		t.Cleanup(s.server.Occupy())
	case "the cluster does not answer the identity check in time":
		s.server.Auth = slowAuth{}
	case "the queue says sextant is next for an item that is live":
		if err := s.store.Update(context.Background(), func(tx logbook.Tx) error {
			tx.PutItem(logbook.Item{ID: "PA-0001", State: lifecycle.Live, Lane: lifecycle.Normal})
			tx.PutEntry(logbook.Entry{Key: "PA-0001", Subject: "PA-0001", Kind: lifecycle.KindItem, Worker: "sextant", Lane: lifecycle.Normal, Arrival: 1})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown condition %q", condition)
	}
}

func TestContractExamples(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(examplesDir, "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no contract examples found (%v)", err)
	}
	seenPath := map[string]bool{}
	seenRefusal := map[*contract.Problem]bool{}
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".json")
		t.Run(name, func(t *testing.T) {
			var ex example
			dec := json.NewDecoder(bytes.NewReader(readFile(t, path)))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&ex); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			if ex.About == "" {
				t.Fatal("every example says what it is about")
			}
			var seed *memory.Seed
			if ex.Given != nil && ex.Given.Items != nil {
				seed = &memory.Seed{Items: ex.Given.Items}
			}
			s := newStack(t, seed)
			var granted []string
			if g := ex.Given; g != nil {
				for i := range g.Calls {
					st := g.Calls[i].step()
					if *update {
						solve(t, &st, s.clock.Now())
						st.Send = realLeases(st.Send, granted)
						g.Calls[i].Body = st.Send
					}
					rec := s.do(t, st)
					if rec.Code < 200 || rec.Code > 299 {
						t.Fatalf("given call %d (%s) did not succeed: %d %s", i+1, st.Path, rec.Code, rec.Body)
					}
					granted = noteLease(granted, st.Path, rec)
				}
				if g.Wait != "" {
					d, err := time.ParseDuration(g.Wait)
					if err != nil {
						t.Fatal(err)
					}
					s.clock.Advance(d)
				}
				arrange(t, s, g.Condition)
			}
			st := ex.Request.step()
			if *update {
				solve(t, &st, s.clock.Now())
				st.Send = realLeases(st.Send, granted)
				ex.Request.Body = st.Send
			}
			seenPath[ex.Request.Path] = true

			var status int
			var body []byte
			if ex.Expect.Stream != nil || (*update && ex.Request.Path == "/v1/feed" && ex.Request.As == "conn") {
				status, body = openFeed(t, s, st)
				if *update {
					text := string(body)
					ex.Expect = expect{Status: status, Stream: &text}
				} else if status != ex.Expect.Status || string(body) != *ex.Expect.Stream {
					t.Errorf("got %d\n%s\nwant %d\n%s", status, body, ex.Expect.Status, *ex.Expect.Stream)
				}
			} else {
				rec := s.do(t, st)
				status, body = rec.Code, rec.Body.Bytes()
				if *update {
					ex.Expect = expect{Status: status}
					if len(body) > 0 {
						ex.Expect.Body = json.RawMessage(bytes.TrimSuffix(body, []byte("\n")))
					}
				} else {
					var want []byte
					if ex.Expect.Body != nil {
						var buf bytes.Buffer
						if err := json.Compact(&buf, ex.Expect.Body); err != nil {
							t.Fatalf("expect.body is not JSON: %v", err)
						}
						want = append(buf.Bytes(), '\n')
					}
					if status != ex.Expect.Status || !bytes.Equal(body, want) {
						t.Errorf("\n got: %d %s\nwant: %d %s", status, body, ex.Expect.Status, want)
					}
				}
			}
			if status >= 400 {
				var r struct {
					Error  contract.Code `json:"error"`
					Cause  string        `json:"cause"`
					Reason string        `json:"reason"`
				}
				if err := json.Unmarshal(body, &r); err != nil {
					t.Fatalf("a refusal that is not JSON: %s", body)
				}
				for _, p := range contract.All() {
					if p.Status == status && p.Code == r.Error && p.Cause == r.Cause && p.Reason == r.Reason {
						seenRefusal[p] = true
					}
				}
			}
			if *update {
				var buf bytes.Buffer
				enc := json.NewEncoder(&buf)
				enc.SetEscapeHTML(false)
				enc.SetIndent("", "  ")
				if err := enc.Encode(ex); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		})
	}

	// One example at least for each call, and one for each refusal.
	all := httpapi.Paths()
	sort.Strings(all)
	for _, path := range all {
		if !seenPath[path] {
			t.Errorf("no example calls %s", path)
		}
	}
	for _, p := range contract.All() {
		if !seenRefusal[p] {
			t.Errorf("no example is refused with %d %s %q", p.Status, p.Cause, p.Reason)
		}
	}
}

// openFeed subscribes over a real connection and returns the events a
// new subscriber is sent at once: the current picture.
func openFeed(t *testing.T, s *stack, st step) (int, []byte) {
	t.Helper()
	srv := httptest.NewServer(s.server)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, st.Method, srv.URL+st.Path, nil)
	req.Header.Set("Authorization", "Bearer "+s.tokens[st.As])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, body
	}
	// The picture is one depths event and one health event for each
	// component with a record.
	var components int
	if err := s.store.View(ctx, func(tx logbook.Tx) error {
		components = len(tx.Components())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	lines := bufio.NewReader(resp.Body)
	for events := 0; events < 1+components; {
		line, err := lines.ReadString('\n')
		if err != nil {
			t.Fatalf("the feed ended early: %v", err)
		}
		out.WriteString(line)
		if line == "\n" {
			events++
		}
	}
	return http.StatusOK, out.Bytes()
}
