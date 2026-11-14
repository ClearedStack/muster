//go:build e2e

package e2e

import (
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// hubRepo and toolRepo are the two repositories the run needs, under the
// organisation the workers are told about (decision D3: the tool repository
// exists, empty, before Moldloft runs).
const (
	org      = "ClearedStack"
	hubRepo  = "clearedstack.com"
	toolRepo = "wrap-rate-calculator"
)

// startGit serves bare repositories over Git's smart HTTP protocol, as
// `git http-backend` does, behind Basic auth on the run's invented
// credentials. It returns the address of the server, with no path.
func (e *env) startGit() string {
	e.t.Helper()
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		e.t.Fatalf("git --exec-path: %v", err)
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		e.t.Fatalf("git http-backend not found at %s", backend)
	}
	e.gitRoot = filepath.Join(e.root, "git")
	gitLog, err := os.Create(filepath.Join(e.root, "logs", "git-server.log"))
	if err != nil {
		e.t.Fatal(err)
	}
	e.gitLog = gitLog
	e.cleanup(func() { gitLog.Close() })
	e.seedHub()
	e.initBare(toolRepo)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(user), []byte(e.gitUser)) != 1 ||
			subtle.ConstantTimeCompare([]byte(pass), []byte(e.gitPass)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="e2e"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h := &cgi.Handler{
			Path: backend,
			Dir:  e.gitRoot,
			Env: []string{
				"GIT_PROJECT_ROOT=" + e.gitRoot,
				"GIT_HTTP_EXPORT_ALL=1",
				"REMOTE_USER=" + user, // lets http-backend accept pushes
				"GIT_CONFIG_GLOBAL=/dev/null",
				"GIT_CONFIG_NOSYSTEM=1",
			},
			InheritEnv: []string{"PATH"},
			Stderr:     e.gitLog,
		}
		h.ServeHTTP(w, r)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		e.t.Fatalf("git server: %v", err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	e.cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}

func (e *env) bare(repo string) string {
	return filepath.Join(e.gitRoot, org, repo+".git")
}

// git runs git against one bare repository.
func (e *env) git(repo string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"--git-dir", e.bare(repo)}, args...)...).CombinedOutput()
	return string(out), err
}

func (e *env) initBare(repo string) {
	e.t.Helper()
	dir := e.bare(repo)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "--bare", "-q", "-b", "main", dir},
		{"--git-dir", dir, "config", "http.receivepack", "true"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			e.t.Fatalf("git %v: %v\n%s", args[0], err, out)
		}
	}
}

// seedHub makes the hub repository with one commit on main, the way the real
// hub has a main branch for Chartroom to branch from. The test later checks
// that main is still this commit. It uses plumbing only, so no work tree.
func (e *env) seedHub() {
	e.t.Helper()
	e.initBare(hubRepo)
	run := func(stdin string, args ...string) string {
		cmd := exec.Command("git", append([]string{"--git-dir", e.bare(hubRepo)}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=e2e", "GIT_AUTHOR_EMAIL=e2e@example.invalid",
			"GIT_COMMITTER_NAME=e2e", "GIT_COMMITTER_EMAIL=e2e@example.invalid",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err != nil {
			e.t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	blob := run("An invented hub repository for the end-to-end harness.\n", "hash-object", "-w", "--stdin")
	tree := run(fmt.Sprintf("100644 blob %s\tREADME.md\n", blob), "mktree")
	commit := run("", "commit-tree", tree, "-m", "seed the hub")
	run("", "update-ref", "refs/heads/main", commit)
	e.hubMain = commit
}
