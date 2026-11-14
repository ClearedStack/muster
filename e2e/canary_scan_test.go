//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scratch makes a work tree and a bare repository to push to, and returns a
// function that runs git in the work tree.
func scratch(t *testing.T) (work func(args ...string), bare string) {
	t.Helper()
	dir := t.TempDir()
	wt, bare := filepath.Join(dir, "wt"), filepath.Join(dir, "bare.git")
	run := func(d string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = d
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	run(wt, "init", "-q", "-b", "main")
	run(dir, "init", "--bare", "-q", bare)
	return func(args ...string) { t.Helper(); run(wt, args...) }, bare
}

func put(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanCanary(t *testing.T) {
	const canary = "CANARY-scan-test-0001"
	cases := []struct {
		name  string
		build func(t *testing.T, git func(...string), wt string)
		want  bool
	}{
		{"clean repository", func(t *testing.T, git func(...string), wt string) {
			put(t, filepath.Join(wt, "a.txt"), []byte("nothing here\n"))
			git("add", ".")
			git("commit", "-qm", "clean")
		}, false},
		{"canary in a file at the tip", func(t *testing.T, git func(...string), wt string) {
			put(t, filepath.Join(wt, "a.txt"), []byte("x "+canary+" y\n"))
			git("add", ".")
			git("commit", "-qm", "tip")
		}, true},
		{"canary added then deleted", func(t *testing.T, git func(...string), wt string) {
			put(t, filepath.Join(wt, "a.txt"), []byte("x "+canary+" y\n"))
			git("add", ".")
			git("commit", "-qm", "add")
			git("rm", "-q", "a.txt")
			put(t, filepath.Join(wt, "b.txt"), []byte("clean\n"))
			git("add", ".")
			git("commit", "-qm", "delete")
		}, true},
		{"canary in a binary file", func(t *testing.T, git func(...string), wt string) {
			put(t, filepath.Join(wt, "a.bin"), append([]byte{0, 1, 2, 0, 0xff}, []byte(canary+"\x00\x00")...))
			git("add", ".")
			git("commit", "-qm", "binary")
		}, true},
		{"canary only in a file name", func(t *testing.T, git func(...string), wt string) {
			put(t, filepath.Join(wt, canary+".txt"), []byte("clean\n"))
			git("add", ".")
			git("commit", "-qm", "name")
		}, true},
		{"canary only in a commit message", func(t *testing.T, git func(...string), wt string) {
			put(t, filepath.Join(wt, "a.txt"), []byte("clean\n"))
			git("add", ".")
			git("commit", "-qm", "mentions "+canary)
		}, true},
		{"canary only on a side branch", func(t *testing.T, git func(...string), wt string) {
			put(t, filepath.Join(wt, "a.txt"), []byte("clean\n"))
			git("add", ".")
			git("commit", "-qm", "main")
			git("checkout", "-qb", "side")
			put(t, filepath.Join(wt, "s.txt"), []byte(canary+"\n"))
			git("add", ".")
			git("commit", "-qm", "side")
			git("checkout", "-q", "main")
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			git, bare := scratch(t)
			wt := filepath.Join(filepath.Dir(bare), "wt")
			c.build(t, git, wt)
			git("push", "-q", bare, "--all")
			hits, err := scanCanary(bare, canary)
			if err != nil {
				t.Fatalf("scanCanary: %v", err)
			}
			if got := len(hits) > 0; got != c.want {
				t.Errorf("found=%v (%s), want %v", got, strings.Join(hits, "; "), c.want)
			}
		})
	}
}

// A git failure must never read as "not found".
func TestScanCanaryGitFailureIsAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.git")
	if hits, err := scanCanary(missing, "x"); err == nil {
		t.Errorf("a repository that does not exist scanned clean (hits %v), want an error", hits)
	}
	notRepo := t.TempDir()
	if _, err := scanCanary(notRepo, "x"); err == nil {
		t.Errorf("a directory that is not a repository scanned clean, want an error")
	}
}

// shimGit puts a git on PATH that behaves as git except for the subcommand
// named, which it handles with body (a shell fragment).
func shimGit(t *testing.T, subcommand, body string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nfor a in \"$@\"; do\n  if [ \"$a\" = " + subcommand + " ]; then\n" + body + "\n  fi\ndone\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A cat-file that stops early and says nothing wrong must not read as clean:
// every object listed has to be read.
func TestScanCanaryEveryListedObjectIsRead(t *testing.T) {
	git, bare := scratch(t)
	wt := filepath.Join(filepath.Dir(bare), "wt")
	put(t, filepath.Join(wt, "a.txt"), []byte("clean\n"))
	git("add", ".")
	git("commit", "-qm", "c")
	git("push", "-q", bare, "--all")

	for name, body := range map[string]string{
		"exits at once with status 0": "    exit 0",
		"exits with status 1":         "    exit 1",
	} {
		t.Run(name, func(t *testing.T) {
			shimGit(t, "cat-file", body)
			if hits, err := scanCanary(bare, "CANARY"); err == nil {
				t.Errorf("a cat-file that did not deliver every object scanned clean (hits %v), want an error", hits)
			}
		})
	}
}
