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
		Reason string `json:"reason"`
	}
	var rows []row
	for _, p := range All() {
		rows = append(rows, row{p.Status, p.Code, p.Reason})
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
		if seen[p.Reason] {
			t.Errorf("reason %q is used twice", p.Reason)
		}
		seen[p.Reason] = true
	}
}
