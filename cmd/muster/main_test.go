package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests build the real binary and run it as a deployment would:
// settings from the environment, a directory it cannot write to, made-up
// callers. They check what the unit tests cannot: that it starts, stays
// inside its memory budget under load, keeps a feed open past the time
// allowed for reading a request, and stops cleanly when told to.

const budget = 48 << 20 // 48 MiB

type running struct {
	t    *testing.T
	cmd  *exec.Cmd
	base string
	out  *bytes.Buffer
}

func build(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "muster")
	cmd := exec.Command("go", "build", "-trimpath", "-o", bin, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func start(t *testing.T, bin string, env map[string]string) *running {
	t.Helper()
	addr := freePort(t)
	// A working directory the process cannot write to.
	cwd := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(cwd, 0o555); err != nil {
		t.Fatal(err)
	}
	testdata, err := filepath.Abs("../../internal/httpapi/testdata")
	if err != nil {
		t.Fatal(err)
	}
	full := map[string]string{
		"MUSTER_LISTEN":        addr,
		"MUSTER_AUTH":          "file",
		"MUSTER_AUTH_FILE":     filepath.Join(testdata, "callers.json"),
		"MUSTER_OWNERSHIP_MAP": filepath.Join(testdata, "ownership_map.json"),
		"MUSTER_SALT_FILE":     filepath.Join(testdata, "salt.txt"),
		"MUSTER_FEED_SECONDS":  "1",
	}
	for k, v := range env {
		full[k] = v
	}
	cmd := exec.Command(bin)
	cmd.Dir = cwd
	for k, v := range full {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out := &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r := &running{t: t, cmd: cmd, base: "http://" + addr, out: out}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(r.base + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return r
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("muster did not come up:\n%s", out)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (r *running) call(as, path string, body any) (int, map[string]any) {
	r.t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, r.base+path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer made-up-token-for-"+as)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatalf("%s: %v", path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var answer map[string]any
	if len(data) > 0 {
		if err := json.Unmarshal(data, &answer); err != nil {
			r.t.Fatalf("%s: answer is not JSON: %s", path, data)
		}
	}
	return resp.StatusCode, answer
}

func (r *running) feed(ctx context.Context) *http.Response {
	r.t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, r.base+"/v1/feed", nil)
	req.Header.Set("Authorization", "Bearer made-up-token-for-conn")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		r.t.Fatalf("feed: status %d", resp.StatusCode)
	}
	return resp
}

// peak returns the most memory the process has ever held, in bytes.
func (r *running) peak() int {
	r.t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", r.cmd.Process.Pid))
	if err != nil {
		r.t.Fatal(err)
	}
	m := regexp.MustCompile(`VmHWM:\s+(\d+) kB`).FindSubmatch(data)
	if m == nil {
		r.t.Fatal("no peak memory figure")
	}
	kb, _ := strconv.Atoi(string(m[1]))
	return kb << 10
}

func (r *running) stop() {
	r.t.Helper()
	if err := r.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		r.t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- r.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			r.t.Errorf("muster did not stop cleanly: %v\n%s", err, r.out)
		}
	case <-time.After(15 * time.Second):
		r.t.Fatalf("muster did not stop:\n%s", r.out)
	}
}

func writeSeed(t *testing.T, items int) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(`{"items":[`)
	lanes := []string{"urgent", "normal", "backfill"}
	for i := 1; i <= items; i++ {
		if i > 1 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":"PA-%04d","state":"discovered","lane":%q,"next":"trawl"}`, i, lanes[i%3])
	}
	b.WriteString(`]}`)
	path := filepath.Join(t.TempDir(), "seed.json")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Two thousand made-up items, each carried through four workers with a
// two-kilobyte result at every step, while every feed slot is taken and
// health reports keep arriving. The most memory the process ever holds
// must fit in the budget.
func TestMemoryBudgetUnderLoad(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("peak memory is read from /proc")
	}
	if testing.Short() {
		t.Skip("builds and loads the real binary")
	}
	const items = 2000
	r := start(t, build(t), map[string]string{"MUSTER_SEED_FILE": writeSeed(t, items)})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < 8; i++ {
		resp := r.feed(ctx)
		defer resp.Body.Close()
		go io.Copy(io.Discard, resp.Body)
	}

	result := map[string]string{"text": strings.Repeat("made-up result text. ", 100)}
	steps := []struct{ worker, to string }{
		{"trawl", "recorded"}, {"sextant", "observed"}, {"assay", "scored"}, {"countersign", "approved"},
	}
	v1 := map[string]string{"contract": "v1"}
	for _, s := range steps {
		for n := 0; ; n++ {
			status, grant := r.call(s.worker, "/v1/lease", v1)
			if status == http.StatusNoContent {
				if n != items {
					t.Fatalf("%s was offered %d items, want %d", s.worker, n, items)
				}
				break
			}
			if status != http.StatusOK {
				t.Fatalf("%s lease: %d %v", s.worker, status, grant)
			}
			status, ack := r.call(s.worker, "/v1/complete", map[string]any{
				"contract": "v1", "lease": grant["lease"], "state": s.to, "result": result,
			})
			if status != http.StatusOK {
				t.Fatalf("%s complete: %d %v", s.worker, status, ack)
			}
			if n%100 == 0 {
				r.call("shipkeeper", "/v1/health", map[string]any{"contract": "v1", "reports": []map[string]string{
					{"component": s.worker, "state": "healthy"},
				}})
			}
		}
	}

	peak := r.peak()
	t.Logf("peak memory with %d items and 8 feed subscribers: %.1f MiB of %d", items, float64(peak)/(1<<20), budget>>20)
	if peak > budget {
		t.Errorf("peak memory %d bytes is over the %d byte budget", peak, budget)
	}
	cancel()
	r.stop()
}

// A feed stays open and keeps sending well past the fifteen seconds the
// server allows for reading a request, and no gap between events comes
// near the fifteen seconds after which a feed counts as stale.
func TestFeedStaysOpen(t *testing.T) {
	if testing.Short() {
		t.Skip("holds a feed open for twenty seconds")
	}
	r := start(t, build(t), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp := r.feed(ctx)
	defer resp.Body.Close()

	lines := bufio.NewReader(resp.Body)
	began, last, events := time.Now(), time.Now(), 0
	var longest time.Duration
	for time.Since(began) < 20*time.Second {
		line, err := lines.ReadString('\n')
		if err != nil {
			t.Fatalf("the feed closed after %s: %v", time.Since(began).Round(time.Second), err)
		}
		if strings.HasPrefix(line, "event: depths") {
			events++
			if gap := time.Since(last); gap > longest {
				longest = gap
			}
			last = time.Now()
		}
	}
	if events < 15 {
		t.Errorf("only %d batches in twenty seconds at one a second", events)
	}
	if longest > 5*time.Second {
		t.Errorf("the feed was silent for %s", longest)
	}
	// Stopping lets go of the open feed and still ends cleanly.
	r.stop()
	if !strings.Contains(r.out.String(), "in-memory stand-in") {
		t.Errorf("the log does not say the stand-in is in use:\n%s", r.out)
	}
}

// A setting that is missing or wrong stops Muster before it listens,
// and the reason is said in one plain line.
func TestRefusesToStart(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the real binary")
	}
	bin := build(t)
	dir := t.TempDir()
	write := func(name, text string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	testdata, _ := filepath.Abs("../../internal/httpapi/testdata")
	good := map[string]string{
		"MUSTER_LISTEN":        "127.0.0.1:0",
		"MUSTER_AUTH":          "file",
		"MUSTER_AUTH_FILE":     filepath.Join(testdata, "callers.json"),
		"MUSTER_OWNERSHIP_MAP": filepath.Join(testdata, "ownership_map.json"),
		"MUSTER_SALT_FILE":     filepath.Join(testdata, "salt.txt"),
	}
	cases := []struct {
		name, key, value, says string
	}{
		{"nothing says how to identify callers", "MUSTER_AUTH", "", "MUSTER_AUTH is not set"},
		{"a caller table given in cluster mode", "MUSTER_AUTH", "kubernetes", "MUSTER_AUTH_FILE is set"},
		{"a caller table that is not there", "MUSTER_AUTH_FILE", filepath.Join(dir, "missing.json"), "the caller table could not be read"},
		{"an ownership map with an unknown repository", "MUSTER_OWNERSHIP_MAP", write("map.json", `{"elements":{"a":{"repository":"made-up","file":"f","kind":"DATA"}}}`), "not in the registry"},
		{"a salt that is too short", "MUSTER_SALT_FILE", write("salt.txt", "short"), "the salt secret is too short"},
		{"a seed with an unknown state", "MUSTER_SEED_FILE", write("seed.json", `{"items":[{"id":"PA-0001","state":"finished","lane":"normal"}]}`), "unknown state"},
		{"a lease length that is not a number", "MUSTER_LEASE_SECONDS", "soon", "MUSTER_LEASE_SECONDS must be a whole number"},
	}
	for _, c := range cases {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, bin)
		cmd.Env = cmdEnv(good, c.key, c.value)
		out, err := cmd.CombinedOutput()
		cancel()
		if err == nil {
			t.Errorf("%s: muster started", c.name)
			continue
		}
		if !strings.Contains(string(out), c.says) {
			t.Errorf("%s: the log does not say %q:\n%s", c.name, c.says, out)
		}
	}
}

func cmdEnv(good map[string]string, key, value string) []string {
	var env []string
	for k, v := range good {
		if k != key {
			env = append(env, k+"="+v)
		}
	}
	if value != "" {
		env = append(env, key+"="+value)
	}
	return env
}
