package contract

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"regexp"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the saved list of refusals")

// The saved copy of every refusal Muster can give: status, code and
// reason. This is the whole of what a caller can ever be told went
// wrong.
func TestSavedRefusals(t *testing.T) {
	type row struct {
		Status int    `json:"status"`
		Error  Code   `json:"error"`
		Cause  string `json:"cause"`
		Reason string `json:"reason"`
	}
	var rows []row
	for _, p := range All() {
		rows = append(rows, row{p.Status, p.Code, p.Cause, p.Reason})
	}
	got, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	const path = "testdata/refusals.json"
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the refusals no longer match %s\n got:\n%s", path, got)
	}
}

// Only three codes exist, and a reason is a plain sentence: lower-case
// words and light punctuation, with nothing that could be a number, an
// address, a path or a name.
func TestRefusalsSayNothingTheyShouldNot(t *testing.T) {
	plain := regexp.MustCompile(`^[a-z][a-z ,;_/]*[a-z]$`)
	word := regexp.MustCompile(`^[a-z]+(_[a-z]+)*$`)
	seen := map[string]bool{}
	for _, p := range All() {
		switch p.Code {
		case Timeout, Parse, NeedsSubtool:
		default:
			t.Errorf("%q is not one of the three codes", p.Code)
		}
		if p.Status < 400 || p.Status > 599 {
			t.Errorf("%q has status %d", p.Reason, p.Status)
		}
		if !plain.MatchString(p.Reason) {
			t.Errorf("reason %q is not a plain sentence", p.Reason)
		}
		if !word.MatchString(p.Cause) {
			t.Errorf("cause %q is not a plain word", p.Cause)
		}
		if seen[p.Reason] {
			t.Errorf("reason %q is used twice", p.Reason)
		}
		seen[p.Reason] = true
	}
}

// Each status has its own few causes, every refusal carries one of the
// causes of its status, and every cause is used.
func TestCausesByStatus(t *testing.T) {
	used := map[string]bool{}
	for _, p := range All() {
		ok := false
		for _, c := range Causes()[p.Status] {
			if c == p.Cause {
				ok = true
			}
		}
		if !ok {
			t.Errorf("%d may not carry the cause %q", p.Status, p.Cause)
		}
		used[p.Cause] = true
	}
	owner := map[string]int{}
	for status, causes := range Causes() {
		for _, c := range causes {
			if other, dup := owner[c]; dup {
				t.Errorf("cause %q belongs to both %d and %d", c, other, status)
			}
			owner[c] = status
			if !used[c] {
				t.Errorf("no refusal carries the cause %q", c)
			}
		}
	}
	if len(owner) != 20 {
		t.Errorf("there are %d causes, not twenty", len(owner))
	}
}
