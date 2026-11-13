package lifecycle

// Route is one row of the routing table: a worker holding a subject in
// state From may complete it to state To, and the next queue step then
// belongs to Next. An empty Next means no step follows and the subject
// leaves the queue. A non-empty Lane moves the subject to that lane;
// otherwise it stays in the lane it was in.
//
// From and To are equal when a worker finishes its step without changing
// the state, as Waybill does when it writes a specification for an
// approved item.
type Route struct {
	Worker string `json:"worker"`
	Kind   Kind   `json:"kind"`
	From   State  `json:"from"`
	To     State  `json:"to"`
	Next   string `json:"next,omitempty"`
	Lane   Lane   `json:"lane,omitempty"`
}

// routes covers stages 1 to 8 of the pipeline and the two steps of an
// edit proposal. Each row comes from the published description of the
// worker named in it.
//
// Tidegauge has no row. Its moves (live to building or retired; parked,
// linked or merged back to observed) are allowed by the state table, but
// nothing says what a scheduled check is or which worker follows one, so
// no queue step can be built for it without guessing.
var routes = []Route{
	{Worker: "trawl", Kind: KindItem, From: Discovered, To: Recorded, Next: "sextant"},
	{Worker: "trawl", Kind: KindItem, From: Discovered, To: Rejected},
	{Worker: "sextant", Kind: KindItem, From: Recorded, To: Observed, Next: "assay"},
	{Worker: "assay", Kind: KindItem, From: Observed, To: Scored, Next: "countersign"},
	{Worker: "assay", Kind: KindItem, From: Observed, To: ScreenedOut, Next: "countersign"},
	{Worker: "countersign", Kind: KindItem, From: Scored, To: Approved, Next: "waybill"},
	{Worker: "countersign", Kind: KindItem, From: Scored, To: Parked},
	{Worker: "countersign", Kind: KindItem, From: Scored, To: Linked, Next: "chartroom"},
	{Worker: "countersign", Kind: KindItem, From: Scored, To: Merged, Next: "chartroom"},
	{Worker: "countersign", Kind: KindItem, From: Scored, To: Rejected},
	{Worker: "countersign", Kind: KindItem, From: ScreenedOut, To: Observed, Next: "assay"},
	{Worker: "waybill", Kind: KindItem, From: Approved, To: Approved, Next: "moldloft"},
	{Worker: "moldloft", Kind: KindItem, From: Approved, To: Building, Next: "shakedown"},
	{Worker: "shakedown", Kind: KindItem, From: Building, To: Building, Next: "chartroom"},
	{Worker: "shakedown", Kind: KindItem, From: Building, To: Parked},
	{Worker: "chartroom", Kind: KindItem, From: Building, To: Live},
	{Worker: "chartroom", Kind: KindItem, From: Linked, To: Linked},
	{Worker: "chartroom", Kind: KindItem, From: Merged, To: Merged},

	{Worker: "countersign", Kind: KindProposal, From: PendingApproval, To: Approved, Next: "bellbuoy", Lane: Urgent},
	{Worker: "countersign", Kind: KindProposal, From: PendingApproval, To: Declined},
	{Worker: "bellbuoy", Kind: KindProposal, From: Approved, To: Building},
}

// Routes lists every row of the routing table.
func Routes() []Route { return append([]Route(nil), routes...) }

// Find returns the row that lets worker take a subject from one state to
// another.
func Find(worker string, k Kind, from, to State) (Route, bool) {
	for _, r := range routes {
		if r.Worker == worker && r.Kind == k && r.From == from && r.To == to {
			return r, true
		}
	}
	return Route{}, false
}

// Holds reports whether worker has any step to do on a subject of this
// kind in this state.
func Holds(worker string, k Kind, s State) bool {
	for _, r := range routes {
		if r.Worker == worker && r.Kind == k && r.From == s {
			return true
		}
	}
	return false
}
