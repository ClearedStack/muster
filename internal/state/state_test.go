package state

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

type saved struct {
	States   []State           `json:"states"`
	Allowed  map[State][]State `json:"allowed"`
	End      []State           `json:"end"`
	CanPause []State           `json:"can_pause"`
	Leasable []State           `json:"leasable"`
}

func load(t *testing.T) saved {
	t.Helper()
	raw, err := os.ReadFile("testdata/transitions.json")
	if err != nil {
		t.Fatal(err)
	}
	var s saved
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFourteenStates(t *testing.T) {
	want := load(t)
	if len(All()) != 14 {
		t.Fatalf("got %d states, want 14", len(All()))
	}
	if !reflect.DeepEqual(All(), want.States) {
		t.Fatalf("states\n got %v\nwant %v", All(), want.States)
	}
	seen := map[State]bool{}
	for _, s := range All() {
		if seen[s] {
			t.Fatalf("state %q listed twice", s)
		}
		seen[s] = true
		if !Valid(s) {
			t.Fatalf("state %q is listed and not valid", s)
		}
	}
	for _, s := range []State{"", "done", "Recorded", "screened_out", "needs-subtool"} {
		if Valid(s) {
			t.Fatalf("%q must not be a valid state", s)
		}
	}
}

// Every one of the 14 x 14 pairs is checked against the saved table, so a
// move added or removed in code fails here.
func TestEveryPairMatchesSavedTable(t *testing.T) {
	want := load(t)
	if len(want.Allowed) != 14 {
		t.Fatalf("saved table has %d rows, want 14", len(want.Allowed))
	}
	allowed := 0
	for _, from := range All() {
		if !reflect.DeepEqual(Next(from), want.Allowed[from]) {
			t.Errorf("moves from %q\n got %v\nwant %v", from, Next(from), want.Allowed[from])
		}
		for _, to := range All() {
			w := false
			for _, n := range want.Allowed[from] {
				if n == to {
					w = true
				}
			}
			if got := Allowed(from, to); got != w {
				t.Errorf("Allowed(%q, %q) = %v, want %v", from, to, got, w)
			}
			if w {
				allowed++
			}
		}
	}
	if allowed != 25 {
		t.Errorf("got %d allowed moves, want 25", allowed)
	}
	for _, s := range All() {
		if Allowed(s, s) {
			t.Errorf("%q must not move to itself", s)
		}
		if Allowed(s, "nowhere") || Allowed("nowhere", s) {
			t.Errorf("a move to or from an unknown state must not be allowed")
		}
	}
}

func TestStateKinds(t *testing.T) {
	want := load(t)
	var end, pause, lease []State
	for _, s := range All() {
		if End(s) {
			end = append(end, s)
		}
		if CanPause(s) {
			pause = append(pause, s)
		}
		if Leasable(s) {
			lease = append(lease, s)
		}
	}
	if !reflect.DeepEqual(end, want.End) {
		t.Errorf("end states\n got %v\nwant %v", end, want.End)
	}
	if !reflect.DeepEqual(pause, want.CanPause) {
		t.Errorf("states that can pause\n got %v\nwant %v", pause, want.CanPause)
	}
	if !reflect.DeepEqual(lease, want.Leasable) {
		t.Errorf("leasable states\n got %v\nwant %v", lease, want.Leasable)
	}
	// A paused item goes back only to a state it could have been paused in.
	if !reflect.DeepEqual(Next(NeedsSubtool), pause) {
		t.Errorf("moves out of needs_subtool %v do not match states that can pause %v", Next(NeedsSubtool), pause)
	}
}
