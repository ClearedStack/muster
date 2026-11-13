package auth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTable(t *testing.T) {
	table, err := ParseTable([]byte(`{"callers":{"made-up-token-for-sextant":"sextant","made-up-token-for-conn":"conn"}}`))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if who, err := table.Identify(ctx, "made-up-token-for-sextant"); err != nil || who != "sextant" {
		t.Errorf("got %q, %v", who, err)
	}
	for _, token := range []string{"", "made-up-token-for-nobody", "made-up-token-for-sextan", "sextant"} {
		if who, err := table.Identify(ctx, token); err != ErrNoCredential || who != "" {
			t.Errorf("token %q: got %q, %v", token, who, err)
		}
	}
}

func TestTableRefusals(t *testing.T) {
	bad := map[string]string{
		"not JSON":                       `callers`,
		"no callers":                     `{"callers":{}}`,
		"an unknown field":               `{"callers":{"made-up-token-for-sextant":"sextant"},"note":"x"}`,
		"a short token":                  `{"callers":{"short":"sextant"}}`,
		"a name that is not in the list": `{"callers":{"made-up-token-for-nobody":"made-up"}}`,
		"a component that does not call": `{"callers":{"made-up-token-for-logbook":"logbook"}}`,
		"Muster may not call itself":     `{"callers":{"made-up-token-for-muster":"muster"}}`,
		"two values":                     `{"callers":{"made-up-token-for-sextant":"sextant"}}{}`,
	}
	for name, text := range bad {
		if _, err := ParseTable([]byte(text)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCallerOf(t *testing.T) {
	cases := map[string]string{
		"system:serviceaccount:cs-workers:sextant":      "sextant",
		"system:serviceaccount:cs-workers:bellbuoy":     "bellbuoy",
		"system:serviceaccount:clearedstack:conn":       "conn",
		"system:serviceaccount:clearedstack:shipkeeper": "shipkeeper",
		// A worker's name in the control-plane namespace, and the other
		// way round, is not that component.
		"system:serviceaccount:clearedstack:sextant": "",
		"system:serviceaccount:cs-workers:conn":      "",
		// Components that never call Muster.
		"system:serviceaccount:clearedstack:logbook": "",
		"system:serviceaccount:clearedstack:muster":  "",
		// Not a ServiceAccount of this platform, or not one at all.
		"system:serviceaccount:default:sextant":         "",
		"system:serviceaccount:cs-workers:made-up":      "",
		"system:serviceaccount:cs-workers:sextant:more": "",
		"system:node:sextant":                           "",
		"sextant":                                       "",
		"":                                              "",
	}
	for username, want := range cases {
		got, ok := callerOf(username)
		if got != want || ok != (want != "") {
			t.Errorf("callerOf(%q) = %q, %v; want %q", username, got, ok, want)
		}
	}
}

// fakeAPI stands in for the cluster's API. It answers from saved files
// and keeps what it was sent.
type fakeAPI struct {
	t      *testing.T
	answer string // file under testdata to answer with
	status int
	calls  atomic.Int32
	sent   atomic.Value // last request body
	auth   atomic.Value // last Authorization header
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	if r.Method != http.MethodPost || r.URL.Path != "/apis/authentication.k8s.io/v1/tokenreviews" {
		f.t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
	}
	body, _ := io.ReadAll(r.Body)
	f.sent.Store(string(body))
	f.auth.Store(r.Header.Get("Authorization"))
	data, err := os.ReadFile(filepath.Join("testdata", f.answer))
	if err != nil {
		f.t.Error(err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(f.status)
	w.Write(data)
}

func newCluster(t *testing.T, api *fakeAPI) (*Cluster, *time.Time) {
	t.Helper()
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("made-up-token-of-muster-itself\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)
	return &Cluster{
		URL:       srv.URL,
		TokenFile: tokenFile,
		Client:    &http.Client{Timeout: 2 * time.Second},
		Clock:     func() time.Time { return now },
	}, &now
}

func TestClusterSavedExamples(t *testing.T) {
	cases := []struct {
		name   string
		answer string
		status int
		who    string
		err    error
	}{
		{"a worker's token", "review_worker.json", http.StatusCreated, "sextant", nil},
		{"Conn's token", "review_conn.json", http.StatusCreated, "conn", nil},
		{"a token the cluster does not accept", "review_rejected.json", http.StatusCreated, "", ErrNoCredential},
		{"a real ServiceAccount that is not a caller", "review_stranger.json", http.StatusCreated, "", ErrNoCredential},
		{"the cluster refuses Muster's own token", "review_forbidden.json", http.StatusForbidden, "", ErrUnavailable},
		{"an answer that is not JSON", "review_garbled.txt", http.StatusCreated, "", ErrUnavailable},
	}
	want, err := os.ReadFile("testdata/review_request.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		api := &fakeAPI{t: t, answer: c.answer, status: c.status}
		cluster, _ := newCluster(t, api)
		who, err := cluster.Identify(context.Background(), "made-up-token-presented-by-a-caller")
		if who != c.who || err != c.err {
			t.Errorf("%s: got %q, %v; want %q, %v", c.name, who, err, c.who, c.err)
		}
		if got := api.sent.Load().(string); got != strings.TrimSpace(string(want)) {
			t.Errorf("%s: sent %s\nwant %s", c.name, got, want)
		}
		if got := api.auth.Load().(string); got != "Bearer made-up-token-of-muster-itself" {
			t.Errorf("%s: Muster presented %q to the cluster", c.name, got)
		}
	}
}

func TestClusterRemembersOnlyGoodAnswers(t *testing.T) {
	api := &fakeAPI{t: t, answer: "review_worker.json", status: http.StatusCreated}
	cluster, now := newCluster(t, api)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if who, err := cluster.Identify(ctx, "made-up-token-presented-by-a-caller"); who != "sextant" || err != nil {
			t.Fatalf("got %q, %v", who, err)
		}
	}
	if n := api.calls.Load(); n != 1 {
		t.Errorf("the cluster was asked %d times in one minute; want 1", n)
	}
	*now = now.Add(61 * time.Second)
	cluster.Identify(ctx, "made-up-token-presented-by-a-caller")
	if n := api.calls.Load(); n != 2 {
		t.Errorf("after a minute the cluster was asked %d times in all; want 2", n)
	}

	api = &fakeAPI{t: t, answer: "review_rejected.json", status: http.StatusCreated}
	cluster, _ = newCluster(t, api)
	for i := 0; i < 2; i++ {
		cluster.Identify(ctx, "made-up-token-nobody-has")
	}
	if n := api.calls.Load(); n != 2 {
		t.Errorf("a refusal was remembered: the cluster was asked %d times; want 2", n)
	}
}

func TestClusterUnreachable(t *testing.T) {
	api := &fakeAPI{t: t, answer: "review_worker.json", status: http.StatusCreated}
	cluster, _ := newCluster(t, api)
	cluster.URL = "http://127.0.0.1:1" // nothing listens here
	if who, err := cluster.Identify(context.Background(), "made-up-token-presented-by-a-caller"); who != "" || err != ErrUnavailable {
		t.Errorf("got %q, %v", who, err)
	}
	if who, err := cluster.Identify(context.Background(), ""); who != "" || err != ErrNoCredential {
		t.Errorf("an empty token: got %q, %v", who, err)
	}
}
