//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

var stageNames = []string{"infrastructure", "trawl", "sextant", "assay", "countersign", "waybill", "moldloft", "shakedown", "chartroom", "outcome"}

// runs makes the stage results: the first stages up to passed ran, the next
// one failed if failed is true, and the rest never started.
func runs(passed int, failed bool) []stageRun {
	var out []stageRun
	for i, n := range stageNames {
		r := stageRun{Name: n}
		switch {
		case i < passed:
			r.Ran = true
		case i == passed && failed:
			r.Failed = true
		}
		out = append(out, r)
	}
	return out
}

func statuses(v verdict) string {
	var s []string
	for _, l := range v.Lines {
		s = append(s, l.Status)
	}
	return strings.Join(s, ",")
}

func TestJudge(t *testing.T) {
	const ten = "PASSED,PASSED,PASSED,PASSED,PASSED,PASSED,PASSED,PASSED,PASSED,PASSED"
	cases := []struct {
		name       string
		runs       []stageRun
		env        string
		standIns   []string
		wantPass   bool
		wantStatus string
		wantProb   string // a substring of some problem, or "" for none
	}{
		{"all ten really ran", runs(10, false), "", nil, true, ten, ""},
		{"a -run filter ran only the first stage", runs(1, false), "", nil, false,
			"PASSED,NOT RUN,NOT RUN,NOT RUN,NOT RUN,NOT RUN,NOT RUN,NOT RUN,NOT RUN,NOT RUN", "NOT ALL STAGES RAN"},
		{"nothing ran", runs(0, false), "", nil, false,
			"NOT RUN,NOT RUN,NOT RUN,NOT RUN,NOT RUN,NOT RUN,NOT RUN,NOT RUN,NOT RUN,NOT RUN", "NOT ALL STAGES RAN"},
		{"a stage failed: the rest are not reached", runs(2, true), "", nil, false,
			"PASSED,PASSED,FAILED,NOT REACHED,NOT REACHED,NOT REACHED,NOT REACHED,NOT REACHED,NOT REACHED,NOT REACHED", ""},
		{"a stand-in is listed", withNote(runs(10, false), "trawl", ""), "trawl", []string{"trawl"}, false,
			"PASSED,STAND-IN,PASSED,PASSED,PASSED,PASSED,PASSED,PASSED,PASSED,PASSED", "DIAGNOSTIC RUN"},
		{"E2E_STAND_IN is set but no stand-in was recorded", runs(10, false), "trawl", nil, false, ten, "DIAGNOSTIC RUN"},
		{"E2E_STAND_IN names a worker that is not in the run", runs(10, false), "nobody", nil, false, ten, "DIAGNOSTIC RUN"},
		{"E2E_STAND_IN is only white space", runs(10, false), "  ", nil, true, ten, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := judge(c.runs, c.env, c.standIns)
			if v.Pass != c.wantPass {
				t.Errorf("Pass = %v, want %v (problems %q)", v.Pass, c.wantPass, v.Problems)
			}
			if got := statuses(v); got != c.wantStatus {
				t.Errorf("statuses:\n got %s\nwant %s", got, c.wantStatus)
			}
			joined := strings.Join(v.Problems, "\n")
			if c.wantProb == "" && joined != "" {
				t.Errorf("unexpected problems: %s", joined)
			}
			if c.wantProb != "" && !strings.Contains(joined, c.wantProb) {
				t.Errorf("problems %q do not mention %q", joined, c.wantProb)
			}
		})
	}
}

func withNote(r []stageRun, name, note string) []stageRun {
	for i := range r {
		if r[i].Name == name {
			r[i].Note = note
		}
	}
	return r
}

func TestRenderReportNamesWhatWasBuilt(t *testing.T) {
	out := renderReport([]string{"trawl sync/x abc1234", "sextant build/y def5678"}, []reportLine{{"trawl", "PASSED", "intake by harness"}})
	for _, want := range []string{"BUILT FROM", "trawl sync/x abc1234", "sextant build/y def5678", "STAGE REPORT", "PASSED (intake by harness)"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(renderReport(nil, nil), "BUILT FROM") {
		t.Errorf("a report with no version list must still say it could not name the build")
	}
}

func TestDescribeOrigin(t *testing.T) {
	cases := []struct {
		env  []string
		want string
	}{
		{[]string{"A=1", "SEXTANT_LOCAL_TEST_ORIGIN=http://127.0.0.1:4242"}, "SEXTANT_LOCAL_TEST_ORIGIN was http://127.0.0.1:4242"},
		{[]string{"A=1"}, "SEXTANT_LOCAL_TEST_ORIGIN was not set"},
		{nil, "SEXTANT_LOCAL_TEST_ORIGIN was not set"},
		{[]string{"SEXTANT_LOCAL_TEST_ORIGIN="}, "SEXTANT_LOCAL_TEST_ORIGIN was set to an empty value"},
	}
	for _, c := range cases {
		if got := describeOrigin(c.env); got != c.want {
			t.Errorf("describeOrigin(%q) = %q, want %q", c.env, got, c.want)
		}
	}
}
