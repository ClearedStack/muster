// Package state holds the fourteen item states and the moves allowed
// between them. It is the one place the table is written down in code;
// testdata/transitions.json is the saved copy the tests compare it with.
package state

// State is one of the fourteen item states.
type State string

const (
	Discovered   State = "discovered"
	Recorded     State = "recorded"
	Observed     State = "observed"
	ScreenedOut  State = "screened-out"
	Scored       State = "scored"
	Approved     State = "approved"
	Building     State = "building"
	Live         State = "live"
	Parked       State = "parked"
	Linked       State = "linked"
	Merged       State = "merged"
	Rejected     State = "rejected"
	Retired      State = "retired"
	NeedsSubtool State = "needs_subtool"
)

// all lists the states in a fixed order.
var all = []State{
	Discovered, Recorded, Observed, ScreenedOut, Scored, Approved, Building,
	Live, Parked, Linked, Merged, Rejected, Retired, NeedsSubtool,
}

// moves lists, for each state, the states an item may move to next.
// A move into needs_subtool is made only by the needs_subtool call, and a
// move out of it only by a replay, which returns the item to the state it
// was paused in. Both are in the table so that it is complete.
var moves = map[State][]State{
	Discovered:   {Recorded, Rejected},
	Recorded:     {Observed, NeedsSubtool},
	Observed:     {Scored, ScreenedOut, NeedsSubtool},
	ScreenedOut:  {Observed},
	Scored:       {Approved, Parked, Linked, Merged, Rejected},
	Approved:     {Building, NeedsSubtool},
	Building:     {Live, Parked},
	Live:         {Building, Retired},
	Parked:       {Observed},
	Linked:       {Observed},
	Merged:       {Observed},
	Rejected:     {},
	Retired:      {},
	NeedsSubtool: {Recorded, Observed, Approved},
}

// All returns the fourteen states in a fixed order.
func All() []State {
	out := make([]State, len(all))
	copy(out, all)
	return out
}

// Valid reports whether s is one of the fourteen states.
func Valid(s State) bool {
	_, ok := moves[s]
	return ok
}

// Next returns the states an item in state s may move to, in a fixed order.
func Next(s State) []State {
	out := make([]State, len(moves[s]))
	copy(out, moves[s])
	return out
}

// Allowed reports whether an item may move from one state to another.
func Allowed(from, to State) bool {
	for _, n := range moves[from] {
		if n == to {
			return true
		}
	}
	return false
}

// End reports whether s is a state nothing moves out of.
func End(s State) bool {
	return Valid(s) && len(moves[s]) == 0
}

// CanPause reports whether an item in state s may be paused for a missing
// sub-tool.
func CanPause(s State) bool {
	return Allowed(s, NeedsSubtool)
}

// Leasable reports whether a worker may ask for items waiting in state s.
// End states have no work left, and a paused item is not handed out: it
// waits for a replay.
func Leasable(s State) bool {
	return Valid(s) && !End(s) && s != NeedsSubtool
}
