// Package registry holds the names Muster is allowed to use.
//
// The one place names are written down is the ClearedStack naming
// registry. This table is Muster's copy of the parts it needs: each
// component's identifier, its kind, its namespace and its repository.
// If the registry and this table disagree, the registry is right and
// this table is the thing to fix.
package registry

// Kind says whether a component is part of the control plane or a worker.
type Kind string

const (
	ControlPlane Kind = "control-plane"
	Worker       Kind = "worker"
)

// Self is this component's identifier everywhere.
const Self = "muster"

// The two peers that are not workers and still call Muster.
const (
	Conn       = "conn"
	Shipkeeper = "shipkeeper"
)

// Component is one entry of the registry, cut down to what Muster uses.
type Component struct {
	Slug       string
	Kind       Kind
	Namespace  string
	Repository string
	Stage      int // 1 to 10 for workers, 0 otherwise
}

const (
	controlNamespace = "clearedstack"
	workerNamespace  = "cs-workers"
)

var components = []Component{
	{"conn", ControlPlane, controlNamespace, "clearedstack.com", 0},
	{"muster", ControlPlane, controlNamespace, "muster", 0},
	{"logbook", ControlPlane, controlNamespace, "logbook", 0},
	{"plainforge", ControlPlane, controlNamespace, "plainforge", 0},
	{"lazarette", ControlPlane, controlNamespace, "lazarette", 0},
	{"keelworks", ControlPlane, controlNamespace, "keelworks", 0},
	{"shipkeeper", ControlPlane, controlNamespace, "shipkeeper", 0},
	{"trawl", Worker, workerNamespace, "trawl", 1},
	{"sextant", Worker, workerNamespace, "sextant", 2},
	{"assay", Worker, workerNamespace, "assay", 3},
	{"countersign", Worker, workerNamespace, "countersign", 4},
	{"waybill", Worker, workerNamespace, "waybill", 5},
	{"moldloft", Worker, workerNamespace, "moldloft", 6},
	{"shakedown", Worker, workerNamespace, "shakedown", 7},
	{"chartroom", Worker, workerNamespace, "chartroom", 8},
	{"tidegauge", Worker, workerNamespace, "tidegauge", 9},
	{"bellbuoy", Worker, workerNamespace, "bellbuoy", 10},
}

var bySlug = func() map[string]Component {
	m := make(map[string]Component, len(components))
	for _, c := range components {
		m[c.Slug] = c
	}
	return m
}()

// All returns every component, control plane first, then workers by stage.
func All() []Component {
	out := make([]Component, len(components))
	copy(out, components)
	return out
}

// Lookup finds a component by its identifier.
func Lookup(slug string) (Component, bool) {
	c, ok := bySlug[slug]
	return c, ok
}

// IsWorker reports whether slug names one of the ten workers.
func IsWorker(slug string) bool {
	c, ok := bySlug[slug]
	return ok && c.Kind == Worker
}

// IsRepository reports whether name is the repository of some component.
func IsRepository(name string) bool {
	for _, c := range components {
		if c.Repository == name {
			return true
		}
	}
	return false
}
