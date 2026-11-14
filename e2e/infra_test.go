//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ClearedStack/muster/internal/logbook/sql/sqltest"
)

// callers is every name Muster accepts a token for. Each gets its own
// invented token, made fresh for the run.
var callers = []string{
	"trawl", "sextant", "assay", "countersign", "waybill", "moldloft",
	"shakedown", "chartroom", "tidegauge", "bellbuoy", "conn",
}

// secret makes an invented credential and remembers it so that no output
// of the harness ever shows it.
func (e *env) secret(prefix string) string {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		e.t.Fatal(err)
	}
	s := prefix + "-" + hex.EncodeToString(b)
	e.mu.Lock()
	e.secrets = append(e.secrets, s)
	e.mu.Unlock()
	return s
}

func (e *env) write(rel, content string, mode os.FileMode) string {
	e.t.Helper()
	path := filepath.Join(e.root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		e.t.Fatal(err)
	}
	return path
}

// startPostgres makes a throwaway PostgreSQL cluster in the run's directory
// on a free port, applies Logbook's migrations to a database of its own
// (with the helper Muster's own tests use), and returns that database's
// address as the muster role.
func (e *env) startPostgres() string {
	e.t.Helper()
	data := filepath.Join(e.root, "pgdata")
	if out, err := e.run("initdb", []string{"-D", data, "-U", "postgres", "-A", "trust", "-E", "UTF8", "--no-sync"}); err != nil {
		e.t.Fatalf("initdb: %v\n%s", err, out)
	}
	var port int
	var lastOut string
	started := false
	for try := 0; try < 3 && !started; try++ {
		port = freePort(e.t)
		opts := fmt.Sprintf("-p %d -c listen_addresses=127.0.0.1 -c unix_socket_directories='' "+
			"-c fsync=off -c synchronous_commit=off -c full_page_writes=off -c max_connections=60", port)
		out, err := e.run("pg_ctl", []string{"start", "-w", "-t", "30", "-D", data, "-l", filepath.Join(e.root, "logs", "postgres.log"), "-o", opts})
		lastOut = out
		started = err == nil
	}
	if !started {
		e.t.Fatalf("pg_ctl start failed three times:\n%s", lastOut)
	}
	e.cleanup(func() {
		if out, err := e.run("pg_ctl", []string{"stop", "-m", "immediate", "-w", "-t", "30", "-D", data}); err != nil {
			e.t.Logf("pg_ctl stop: %v\n%s", err, out)
		}
	})

	// sqltest reads these two variables. t.Setenv puts them back afterwards.
	e.t.Setenv("MUSTER_TEST_DATABASE_URL", fmt.Sprintf("postgres://postgres@127.0.0.1:%d/postgres?sslmode=disable", port))
	e.t.Setenv("MUSTER_LOGBOOK_MIGRATIONS", e.migrations)
	e.db = sqltest.New(e.t, time.Now)
	name := e.db.Admin.Config().Database
	return fmt.Sprintf("postgres://postgres@127.0.0.1:%d/%s?role=muster&sslmode=disable", port, name)
}

// startMuster writes the caller table, the ownership map and the salt, and
// starts the Muster binary built from this checkout.
func (e *env) startMuster(databaseURL string) {
	e.t.Helper()
	table := map[string]map[string]string{"callers": {}}
	e.tokens = map[string]string{}
	for _, name := range callers {
		tok := e.secret("tok")
		e.tokens[name] = tok
		table["callers"][tok] = name
		e.write(filepath.Join("tokens", name), tok, 0o600)
	}
	raw, _ := json.Marshal(table)
	authFile := e.write("muster/callers.json", string(raw), 0o600)
	ownership := e.write("muster/ownership_map.json", `{"elements":{"catalog.wrap-rate-calculator.description":{"repository":"clearedstack.com","file":"src/data/catalog.json","kind":"DATA"}}}`, 0o644)
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		e.t.Fatal(err)
	}
	saltFile := e.write("muster/salt", hex.EncodeToString(salt), 0o600)

	e.musterBin = filepath.Join(e.bin, "muster")
	var lastLog string
	for try := 0; try < 3; try++ {
		port := freePort(e.t)
		p := e.start("muster", e.musterBin, nil,
			"MUSTER_AUTH=file",
			"MUSTER_AUTH_FILE="+authFile,
			"MUSTER_OWNERSHIP_MAP="+ownership,
			"MUSTER_SALT_FILE="+saltFile,
			"MUSTER_DATABASE_URL="+databaseURL,
			"MUSTER_LISTEN=127.0.0.1:"+strconv.Itoa(port),
			"MUSTER_LEASE_SECONDS=30",
		)
		url := "http://127.0.0.1:" + strconv.Itoa(port)
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) && !p.exited() {
			resp, err := http.Get(url + "/healthz")
			if err == nil {
				ok := resp.StatusCode == 200
				resp.Body.Close()
				if ok {
					e.musterURL, e.muster = url, p
					return
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		lastLog = e.tail(p, 20)
		p.stop()
	}
	e.t.Fatalf("muster did not become healthy:\n%s", lastLog)
}

// startPage serves the invented Wrap Rate page, and a robots.txt that allows
// everything, as a public page would.
func (e *env) startPage(page []byte) string {
	e.t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "User-agent: *\nAllow: /\n")
	})
	mux.HandleFunc("/calculator", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(page)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		e.t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	e.cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}

// call makes one call to Muster as a named caller. The body is a JSON value.
func (e *env) call(caller, method, path string, body any) (int, map[string]any, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, e.musterURL+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+e.tokens[caller])
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, e.wrap(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, nil
}

func (e *env) wrap(err error) error { return fmt.Errorf("%s", e.scrub(err.Error())) }

// item is where an item stands: its state and the workers that have a step
// queued for it.
func (e *env) item(id string) (state, queued string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = e.db.Admin.QueryRow(ctx, `
		SELECT i.state, coalesce((SELECT string_agg(q.worker, ',' ORDER BY q.queue_id) FROM core.queue q WHERE q.item_id = i.item_id), '')
		FROM core.item i WHERE i.item_id = $1`, id).Scan(&state, &queued)
	return state, queued, err
}

// evidence says everything useful about an item that did not get where it
// should: where it is, what is queued, what the ledger holds.
func (e *env) evidence(id string) string {
	var b strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	state, queued, err := e.item(id)
	if err != nil {
		fmt.Fprintf(&b, "item %s: %v\n", id, err)
		return b.String()
	}
	fmt.Fprintf(&b, "item %s: state=%s queued_for=[%s]\n", id, state, queued)
	rows, err := e.db.Admin.Query(ctx, `SELECT attempt, lane, worker, coalesce(leased_until > now(), false) FROM core.queue WHERE item_id = $1`, id)
	if err == nil {
		for rows.Next() {
			var attempt int
			var lane, worker string
			var leased bool
			if rows.Scan(&attempt, &lane, &worker, &leased) == nil {
				fmt.Fprintf(&b, "queue: worker=%s lane=%s attempt=%d lease_held=%v\n", worker, lane, attempt, leased)
			}
		}
		rows.Close()
	}
	// Consecutive identical rows (a worker failing the same way over and over)
	// are shown once, with a count.
	rows, err = e.db.Admin.Query(ctx, `SELECT entry_id, actor, coalesce(from_state, ''), to_state, coalesce(error_code, ''), coalesce(note, '') FROM core.ledger WHERE item_id = $1 ORDER BY entry_id`, id)
	if err == nil {
		var last string
		var first, n int64
		flush := func() {
			if n > 0 {
				fmt.Fprintf(&b, "ledger %d..%d (x%d): %s\n", first, first+n-1, n, last)
			}
		}
		for rows.Next() {
			var entry int64
			var actor, from, to, code, note string
			if rows.Scan(&entry, &actor, &from, &to, &code, &note) != nil {
				continue
			}
			line := fmt.Sprintf("%s %q -> %q code=%q note=%q", actor, from, to, code, note)
			if line == last && n > 0 {
				n++
				continue
			}
			flush()
			last, first, n = line, entry, 1
		}
		flush()
		rows.Close()
	}
	rows, err = e.db.Admin.Query(ctx, `SELECT worker, left(payload::text, 300) FROM core.result WHERE item_id = $1 ORDER BY result_id`, id)
	if err == nil {
		for rows.Next() {
			var worker, payload string
			if rows.Scan(&worker, &payload) == nil {
				fmt.Fprintf(&b, "result from %s: %s\n", worker, payload)
			}
		}
		rows.Close()
	}
	return e.scrub(b.String())
}
