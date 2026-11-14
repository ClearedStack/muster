//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The Trawl the stage starts must be told the page server's origin, or it
// refuses the local address and ends the item as rejected. This starts the
// stage's own Trawl command with a stand-in binary that prints what it was
// given.
func TestTrawlIsStartedWithTheLocalOrigin(t *testing.T) {
	e := newBareEnv()
	e.trawlOriginOverride = nil // whatever the control run put in the environment
	e.cur = t
	e.t = tb{t, e}
	e.root = t.TempDir()
	e.bin = t.TempDir()
	for _, d := range []string{"home", "tmp", "logs", "spool"} {
		if err := os.MkdirAll(filepath.Join(e.root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(e.bin, "trawl"), []byte("#!/bin/sh\necho \"origin=$TRAWL_LOCAL_TEST_ORIGIN\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.musterURL = "http://127.0.0.1:1"
	e.pageURL = "http://127.0.0.1:4242"

	run := func() *proc {
		p := e.startTrawl()
		select {
		case <-p.done:
		case <-time.After(10 * time.Second):
			t.Fatal("the stand-in trawl did not end")
		}
		return p
	}

	p := run()
	if out, _ := os.ReadFile(p.logPath); !strings.Contains(string(out), "origin=http://127.0.0.1:4242\n") {
		t.Errorf("Trawl was not started with the page server's origin; it printed %q", out)
	}
	if got := describeEnv(p.env, "TRAWL_LOCAL_TEST_ORIGIN"); got != "TRAWL_LOCAL_TEST_ORIGIN was http://127.0.0.1:4242" {
		t.Errorf("describeEnv = %q", got)
	}

	// The control: an override, as the control run uses, replaces the origin
	// even with an empty one, and the report says so.
	empty := ""
	e.trawlOriginOverride = &empty
	p = run()
	if got := describeEnv(p.env, "TRAWL_LOCAL_TEST_ORIGIN"); got != "TRAWL_LOCAL_TEST_ORIGIN was set to an empty value" {
		t.Errorf("with the origin forced empty, describeEnv = %q", got)
	}
}
