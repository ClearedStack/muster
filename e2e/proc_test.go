//go:build e2e

package e2e

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// proc is one child process of the harness. Every proc is killed when the
// test ends, whatever state the test is in.
type proc struct {
	name    string
	cmd     *exec.Cmd
	logPath string
	done    chan struct{}
	exit    error // valid once done is closed
	once    sync.Once
}

// freePort asks the kernel for a port nothing is listening on. The port is
// released again before the caller uses it, so a caller that starts a child
// on it must be ready to try again if the child cannot bind.
func freePort(t testing.TB) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("no free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// baseEnv is all a child inherits: a path to find tools by, and a home and
// temporary directory of its own. Nothing else from the developer's shell
// reaches a worker, in particular no proxy setting and no credential.
func (e *env) baseEnv() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + filepath.Join(e.root, "home"),
		"TMPDIR=" + filepath.Join(e.root, "tmp"),
		"LC_ALL=en_US.UTF-8", // PostgreSQL on macOS refuses to start without a valid locale
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
	}
}

// start runs bin with args and extra environment, with output going to a log
// file. The process and any child of it are killed at the end of the test.
func (e *env) start(name, bin string, args []string, extra ...string) *proc {
	e.t.Helper()
	e.mu.Lock()
	e.serial++
	logPath := filepath.Join(e.root, "logs", fmt.Sprintf("%s-%d.log", name, e.serial))
	e.mu.Unlock()
	logFile, err := os.Create(logPath)
	if err != nil {
		e.t.Fatalf("log file: %v", err)
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = append(e.baseEnv(), extra...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		e.t.Fatalf("starting %s: %v", name, err)
	}
	p := &proc{name: name, cmd: cmd, logPath: logPath, done: make(chan struct{})}
	go func() {
		p.exit = cmd.Wait()
		logFile.Close()
		close(p.done)
	}()
	e.cleanup(func() { p.stop() })
	return p
}

// stop ends the process group: politely, then for certain.
func (p *proc) stop() {
	p.once.Do(func() {
		select {
		case <-p.done:
			return
		default:
		}
		pgid := p.cmd.Process.Pid
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		select {
		case <-p.done:
		case <-time.After(3 * time.Second):
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			<-p.done
		}
	})
}

func (p *proc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// tail is the last n lines of the process's output, with every secret of the
// run removed and long lines cut.
func (e *env) tail(p *proc, n int) string {
	raw, err := os.ReadFile(p.logPath)
	if err != nil {
		return "(no log)"
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	// Identical lines in a row (a worker repeating one failure) are shown once.
	var out []string
	repeats := 0
	for i, l := range lines {
		if len(l) > 400 {
			l = l[:400] + "..."
		}
		if i > 0 && l == out[len(out)-1] {
			repeats++
			continue
		}
		if repeats > 0 {
			out[len(out)-1] += fmt.Sprintf("   [last line repeated %d more times]", repeats)
			repeats = 0
		}
		out = append(out, l)
	}
	if repeats > 0 {
		out[len(out)-1] += fmt.Sprintf("   [last line repeated %d more times]", repeats)
	}
	return e.scrub(strings.Join(out, "\n"))
}

// run executes a short command to the end and returns its combined output,
// scrubbed. A command that does not end in two minutes is killed.
func (e *env) run(name string, args []string, extra ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = append(e.baseEnv(), extra...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return e.scrub(out.String()), err
	case <-time.After(2 * time.Minute):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return e.scrub(out.String()), fmt.Errorf("%s did not finish in two minutes", name)
	}
}

// scrub removes every generated secret from text that is about to be shown.
func (e *env) scrub(s string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, secret := range e.secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	return s
}
