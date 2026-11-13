// Package lifecycle holds the fixed tables Muster enforces: the fourteen
// item states, the moves allowed between them, the three lanes, the
// statuses of an edit proposal, and which worker takes the next step.
//
// Everything here is data. Nothing is decided at run time.
package lifecycle

import (
	"fmt"
	"regexp"
)

// State is the state of an item or the status of an edit proposal.
type State string

// Kind says what a queue entry is about.
type Kind string

const (
	KindItem     Kind = "item"
	KindProposal Kind = "proposal"
	// KindIncident is a step that tells Bellbuoy an item was stopped or
	// paused. It is about an item and changes no state.
	KindIncident Kind = "incident"
)

// The fourteen item states.
const (
	Discovered   State = "discovered"
	Recorded     State = "recorded"
	Observed     State = "observed"
	ScreenedOut  State = "screened_out"
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

// Proposal statuses that are not also item states. A proposal also uses
// Approved, Building and Live.
const (
	PendingApproval State = "pending_approval"
	Declined        State = "declined"
)

// Move is one allowed change of state.
type Move struct {
	From State `json:"from"`
	To   State `json:"to"`
}

var itemStates = []State{
	Discovered, Recorded, Observed, ScreenedOut, Scored, Approved, Building,
	Live, Parked, Linked, Merged, Rejected, Retired, NeedsSubtool,
}

// itemMoves is the table "Which worker makes each move" from the item
// lifecycle page, one row per move. Rejected and retired have no way out.
//
// A paused item returns to the state it was paused from. The three moves
// out of needs_subtool below are therefore narrowed by one more rule,
// which the queue enforces: the destination must be that state.
var itemMoves = []Move{
	{Discovered, Recorded},
	{Discovered, Rejected},
	{Recorded, Observed},
	{Recorded, NeedsSubtool},
	{Observed, Scored},
	{Observed, ScreenedOut},
	{Observed, NeedsSubtool},
	{ScreenedOut, Observed},
	{Scored, Approved},
	{Scored, Parked},
	{Scored, Linked},
	{Scored, Merged},
	{Scored, Rejected},
	{Approved, Building},
	{Approved, NeedsSubtool},
	{Building, Live},
	{Building, Parked},
	{Live, Building},
	{Live, Retired},
	{Parked, Observed},
	{Linked, Observed},
	{Merged, Observed},
	{NeedsSubtool, Recorded},
	{NeedsSubtool, Observed},
	{NeedsSubtool, Approved},
}

var proposalStates = []State{PendingApproval, Approved, Declined, Building, Live}

// The forms an identifier takes: four digits, or five to nine with no
// leading zero.
const (
	ItemIDForm     = `PA-(?:[0-9]{4}|[1-9][0-9]{4,8})`
	ProposalIDForm = `EP-(?:[0-9]{4}|[1-9][0-9]{4,8})`
)

var (
	itemID     = regexp.MustCompile(`^` + ItemIDForm + `$`)
	proposalID = regexp.MustCompile(`^` + ProposalIDForm + `$`)
)

// IsItemID reports whether id has the form of an item identifier.
func IsItemID(id string) bool { return itemID.MatchString(id) }

// IsProposalID reports whether id has the form of a proposal identifier.
func IsProposalID(id string) bool { return proposalID.MatchString(id) }

// MaxID is the largest number an identifier can carry.
const MaxID = 999999999

// ItemID writes the identifier of the item with this number.
func ItemID(n uint64) string { return fmt.Sprintf("PA-%04d", n) }

// ProposalID writes the identifier of the proposal with this number.
func ProposalID(n uint64) string { return fmt.Sprintf("EP-%04d", n) }

var proposalMoves = []Move{
	{PendingApproval, Approved},
	{PendingApproval, Declined},
	{Approved, Building},
	{Building, Live},
}

// States lists the states a subject of this kind can be in.
func States(k Kind) []State {
	switch k {
	case KindItem:
		return append([]State(nil), itemStates...)
	case KindProposal:
		return append([]State(nil), proposalStates...)
	}
	return nil
}

// Moves lists the allowed moves for a subject of this kind.
func Moves(k Kind) []Move {
	switch k {
	case KindItem:
		return append([]Move(nil), itemMoves...)
	case KindProposal:
		return append([]Move(nil), proposalMoves...)
	}
	return nil
}

// Known reports whether s is a state a subject of this kind can be in.
func Known(k Kind, s State) bool {
	for _, x := range States(k) {
		if x == s {
			return true
		}
	}
	return false
}

// Allowed reports whether the tables permit a move from one state to
// another.
func Allowed(k Kind, from, to State) bool {
	for _, m := range Moves(k) {
		if m.From == from && m.To == to {
			return true
		}
	}
	return false
}

// Pausable reports whether an item in state s may be paused to wait for a
// missing rule.
func Pausable(s State) bool { return Allowed(KindItem, s, NeedsSubtool) }

// Lane is one of the three priority lanes.
type Lane string

const (
	Urgent   Lane = "urgent"
	Normal   Lane = "normal"
	Backfill Lane = "backfill"
)

var lanes = []Lane{Urgent, Normal, Backfill}

// Lanes lists the lanes in the order they are emptied.
func Lanes() []Lane { return append([]Lane(nil), lanes...) }

// Rank gives a lane's place in that order, or -1 for a lane that does not
// exist.
func Rank(l Lane) int {
	for i, x := range lanes {
		if x == l {
			return i
		}
	}
	return -1
}
