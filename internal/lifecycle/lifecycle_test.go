package lifecycle

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"regexp"
	"testing"

	"github.com/ClearedStack/muster/internal/registry"
)

var update = flag.Bool("update", false, "rewrite the saved tables")

// The saved copy of every table. A change to a state, a move, a lane or
// a route fails this test until the saved copy is changed to match, so
// no rule changes by accident.
func TestSavedTables(t *testing.T) {
	type tables struct {
		ItemStates     []State `json:"item_states"`
		ItemMoves      []Move  `json:"item_moves"`
		ProposalStates []State `json:"proposal_states"`
		ProposalMoves  []Move  `json:"proposal_moves"`
		Lanes          []Lane  `json:"lanes"`
		Routes         []Route `json:"routes"`
	}
	got, err := json.MarshalIndent(tables{
		States(KindItem), Moves(KindItem), States(KindProposal), Moves(KindProposal), Lanes(), Routes(),
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	const path = "testdata/tables.json"
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
		t.Errorf("the tables no longer match %s\n got:\n%s", path, got)
	}
}

func TestFourteenStatesThreeLanes(t *testing.T) {
	if n := len(States(KindItem)); n != 14 {
		t.Errorf("there are %d item states, not fourteen", n)
	}
	if n := len(Lanes()); n != 3 {
		t.Errorf("there are %d lanes, not three", n)
	}
	seen := map[State]bool{}
	for _, s := range States(KindItem) {
		if seen[s] {
			t.Errorf("state %q is listed twice", s)
		}
		seen[s] = true
	}
}

func TestMovesStayInsideTheStates(t *testing.T) {
	for _, k := range []Kind{KindItem, KindProposal} {
		seen := map[Move]bool{}
		for _, m := range Moves(k) {
			if !Known(k, m.From) || !Known(k, m.To) {
				t.Errorf("%s move %v uses a state that does not exist", k, m)
			}
			if m.From == m.To {
				t.Errorf("%s move %v goes nowhere", k, m)
			}
			if seen[m] {
				t.Errorf("%s move %v is listed twice", k, m)
			}
			seen[m] = true
		}
	}
}

func TestEndStates(t *testing.T) {
	for _, s := range []State{Rejected, Retired} {
		for _, to := range States(KindItem) {
			if Allowed(KindItem, s, to) {
				t.Errorf("%s is an end state and may not move to %s", s, to)
			}
		}
	}
	for _, s := range []State{Declined, Live} {
		for _, to := range States(KindProposal) {
			if Allowed(KindProposal, s, to) {
				t.Errorf("a %s proposal may not move to %s", s, to)
			}
		}
	}
	// Every state is spelled in lower case with underscores.
	spelled := regexp.MustCompile(`^[a-z]+(_[a-z]+)*$`)
	for _, k := range []Kind{KindItem, KindProposal} {
		for _, s := range States(k) {
			if !spelled.MatchString(string(s)) {
				t.Errorf("the state %q is not spelled in lower case with underscores", s)
			}
		}
	}
}

func TestPausable(t *testing.T) {
	want := map[State]bool{Recorded: true, Observed: true, Approved: true}
	for _, s := range States(KindItem) {
		if Pausable(s) != want[s] {
			t.Errorf("Pausable(%s) = %v", s, Pausable(s))
		}
		// An item can return from a pause only to a state it can be
		// paused from.
		if Allowed(KindItem, NeedsSubtool, s) != want[s] {
			t.Errorf("needs_subtool to %s: allowed = %v", s, !want[s])
		}
	}
}

func TestRoutesAgreeWithTheOtherTables(t *testing.T) {
	type key struct {
		worker string
		kind   Kind
		from   State
		to     State
	}
	seen := map[key]bool{}
	for _, r := range Routes() {
		if !registry.IsWorker(r.Worker) {
			t.Errorf("route %+v: %q is not a worker", r, r.Worker)
		}
		if r.Next != "" && !registry.IsWorker(r.Next) {
			t.Errorf("route %+v: %q is not a worker", r, r.Next)
		}
		if !Known(r.Kind, r.From) || !Known(r.Kind, r.To) {
			t.Errorf("route %+v uses a state that does not exist", r)
		}
		if r.From != r.To && !Allowed(r.Kind, r.From, r.To) {
			t.Errorf("route %+v makes a move the state table does not allow", r)
		}
		if r.Lane != "" && Rank(r.Lane) < 0 {
			t.Errorf("route %+v names a lane that does not exist", r)
		}
		if r.Next != "" && !Holds(r.Next, r.Kind, r.To) {
			t.Errorf("route %+v hands over to %s, which has no step for %s", r, r.Next, r.To)
		}
		k := key{r.Worker, r.Kind, r.From, r.To}
		if seen[k] {
			t.Errorf("route %+v is listed twice", r)
		}
		seen[k] = true
	}
}

func TestIdentifierForms(t *testing.T) {
	good := []string{"PA-0000", "PA-0001", "PA-9999", "PA-10000", "PA-999999999"}
	bad := []string{"", "PA-", "PA-1", "PA-001", "PA-00001", "PA-01234", "PA-1000000000", "pa-0001", "PA-00a1", " PA-0001", "PA-0001 ", "EP-0001", "PA-0001\n"}
	for _, id := range good {
		if !IsItemID(id) {
			t.Errorf("%q is refused as an item identifier", id)
		}
		if p := "EP" + id[2:]; !IsProposalID(p) {
			t.Errorf("%q is refused as a proposal identifier", p)
		}
	}
	for _, id := range bad {
		if IsItemID(id) {
			t.Errorf("%q is accepted as an item identifier", id)
		}
	}
	if IsProposalID("PA-0001") {
		t.Error("an item identifier is accepted as a proposal identifier")
	}
	// The identifiers Muster writes are ones it accepts.
	for _, n := range []uint64{1, 42, 9999, 10000, 123456, MaxID} {
		if id := ItemID(n); !IsItemID(id) {
			t.Errorf("item %d is written %q", n, id)
		}
		if id := ProposalID(n); !IsProposalID(id) {
			t.Errorf("proposal %d is written %q", n, id)
		}
	}
	if ItemID(7) != "PA-0007" || ItemID(10000) != "PA-10000" || ProposalID(7) != "EP-0007" {
		t.Error("an identifier is not written as expected")
	}
}

func TestRank(t *testing.T) {
	if !(Rank(Urgent) < Rank(Normal) && Rank(Normal) < Rank(Backfill)) {
		t.Error("the lanes are not in the order urgent, normal, backfill")
	}
	if Rank("made-up") != -1 {
		t.Error("a lane that does not exist has a rank")
	}
}
