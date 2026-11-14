//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestStageRunnerProbe runs runStages over three stages that do nothing. It is
// the probe for TestFilteredRunIsNotAPass, which runs it in a child process
// with and without a -run filter; on its own it does nothing.
func TestStageRunnerProbe(t *testing.T) {
	if os.Getenv("E2E_RUNNER_PROBE") == "" {
		t.Skip("only run by TestFilteredRunIsNotAPass")
	}
	e := newBareEnv()
	e.bin = t.TempDir()
	nothing := func(*env, *testing.T) {}
	runStages(t, e, []stage{{"a", nothing}, {"b", nothing}, {"c", nothing}})
}

// probe runs TestStageRunnerProbe in a child process.
func probe(filter, standIn string) (string, error) {
	cmd := exec.Command(os.Args[0], "-test.v", "-test.count=1", "-test.run="+filter)
	cmd.Env = append(os.Environ(), "E2E_RUNNER_PROBE=1", "E2E_STAND_IN="+standIn)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// A stage that a -run filter kept from running must show as NOT RUN, and the
// run must not exit 0, even though t.Run returns true for such a stage.
func TestFilteredRunIsNotAPass(t *testing.T) {
	out, err := probe("^TestStageRunnerProbe$", "")
	if err != nil || strings.Count(out, "PASSED") != 3 {
		t.Fatalf("control: unfiltered probe should pass all three stages and exit 0, got %v:\n%s", err, out)
	}

	out, err = probe("^TestStageRunnerProbe$/^a$", "")
	if err == nil {
		t.Errorf("a run filtered to one stage exited 0:\n%s", out)
	}
	if strings.Count(out, "NOT RUN") < 2 {
		t.Errorf("stages b and c should be NOT RUN:\n%s", out)
	}
	if strings.Count(out, "PASSED") != 1 {
		t.Errorf("only stage a should be PASSED:\n%s", out)
	}
}

// E2E_STAND_IN makes the run a diagnostic whether or not any stand-in was
// recorded: a run that fails to record one must not read as an acceptance pass.
func TestStandInEnvMakesTheRunADiagnostic(t *testing.T) {
	out, err := probe("^TestStageRunnerProbe$", "trawl")
	if err == nil {
		t.Errorf("a run with E2E_STAND_IN=trawl and no stand-in recorded exited 0:\n%s", out)
	}
	if !strings.Contains(out, "DIAGNOSTIC RUN") {
		t.Errorf("no DIAGNOSTIC line:\n%s", out)
	}
}
