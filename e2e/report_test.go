//go:build e2e

package e2e

import (
	"fmt"
	"slices"
	"strings"
)

// stageRun is what the test saw of one stage.
type stageRun struct {
	Name   string
	Failed bool   // its subtest failed
	Ran    bool   // its body reached its last statement
	Note   string // what the stage adds to its report line
}

type reportLine struct{ Name, Status, Note string }

// verdict is what the run amounts to.
type verdict struct {
	Pass     bool // an acceptance pass: every stage really ran and passed
	Lines    []reportLine
	Problems []string // each one fails the test
}

// judge decides the stage report and whether the run is an acceptance pass.
//
// A stage is PASSED only if its body ran to the end; a stage that did not
// run, and every stage after it, is NOT RUN and the run is not a pass. A
// failed stage makes the stages after it NOT REACHED. A run is a diagnostic,
// never a pass, whenever a stand-in was recorded and also whenever
// E2E_STAND_IN was set, whether or not any stand-in was recorded: the
// environment is read once at the start and checked here on its own.
func judge(runs []stageRun, standInEnv string, standIns []string) verdict {
	var v verdict
	blocked, why, status := "", "", ""
	var notRun []string
	failed := false
	for _, r := range runs {
		switch {
		case blocked != "":
			v.Lines = append(v.Lines, reportLine{r.Name, status, why})
			if status == "NOT RUN" {
				notRun = append(notRun, r.Name)
			}
		case r.Failed:
			failed = true
			v.Lines = append(v.Lines, reportLine{r.Name, "FAILED", "see the failure above"})
			blocked, why, status = r.Name, "stage "+r.Name+" failed", "NOT REACHED"
		case !r.Ran:
			v.Lines = append(v.Lines, reportLine{r.Name, "NOT RUN", "a -run filter or a skip kept it from running"})
			notRun = append(notRun, r.Name)
			blocked, why, status = r.Name, "stage "+r.Name+" did not run", "NOT RUN"
		case slices.Contains(standIns, r.Name):
			v.Lines = append(v.Lines, reportLine{r.Name, "STAND-IN", "the worker did not run"})
		default:
			v.Lines = append(v.Lines, reportLine{r.Name, "PASSED", r.Note})
		}
	}
	if len(notRun) > 0 {
		v.Problems = append(v.Problems, "NOT ALL STAGES RAN, so this is not a pass: "+strings.Join(notRun, ", "))
	}
	if len(standIns) > 0 || strings.TrimSpace(standInEnv) != "" {
		v.Problems = append(v.Problems, fmt.Sprintf("DIAGNOSTIC RUN, NOT AN ACCEPTANCE PASS: E2E_STAND_IN=%q, stood in for [%s]",
			strings.TrimSpace(standInEnv), strings.Join(standIns, ", ")))
	}
	v.Pass = !failed && len(v.Problems) == 0
	return v
}

// renderReport is the text of the stage report, headed by what was built.
func renderReport(versions []string, lines []reportLine) string {
	var b strings.Builder
	b.WriteString("\nBUILT FROM (repository, branch, commit)\n")
	if len(versions) == 0 {
		b.WriteString("  (versions.txt could not be read: the commits are not known)\n")
	}
	for _, v := range versions {
		b.WriteString("  " + v + "\n")
	}
	b.WriteString("\nSTAGE REPORT\n")
	for i, r := range lines {
		fmt.Fprintf(&b, "  %2d  %-16s %s", i+1, r.Name, r.Status)
		if r.Note != "" {
			fmt.Fprintf(&b, " (%s)", r.Note)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// describeOrigin says what SEXTANT_LOCAL_TEST_ORIGIN actually was in the
// environment a process was started with.
func describeOrigin(env []string) string {
	const key = "SEXTANT_LOCAL_TEST_ORIGIN"
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], key+"="); ok {
			if v == "" {
				return key + " was set to an empty value"
			}
			return key + " was " + v
		}
	}
	return key + " was not set"
}
