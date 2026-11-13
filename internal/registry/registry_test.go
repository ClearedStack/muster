package registry

import (
	"encoding/json"
	"os"
	"testing"
)

// component.json is this component's entry in the naming registry. The
// names Muster uses for itself and for what it depends on must be the
// ones written there.
func TestAgreesWithComponentJSON(t *testing.T) {
	data, err := os.ReadFile("../../component.json")
	if err != nil {
		t.Fatal(err)
	}
	var entry struct {
		Slug       string   `json:"slug"`
		Kind       string   `json:"kind"`
		Namespace  string   `json:"namespace"`
		Repository string   `json:"repository"`
		DependsOn  []string `json:"dependsOn"`
	}
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Slug != Self {
		t.Errorf("component.json names this component %q; the code says %q", entry.Slug, Self)
	}
	me, ok := Lookup(Self)
	if !ok {
		t.Fatal("this component is not in its own table")
	}
	if string(me.Kind) != entry.Kind || me.Namespace != entry.Namespace || me.Repository != entry.Repository {
		t.Errorf("the table says %+v; component.json says %+v", me, entry)
	}
	for _, dep := range entry.DependsOn {
		if _, ok := Lookup(dep); !ok {
			t.Errorf("component.json depends on %q, which the table does not know", dep)
		}
	}
}

func TestShape(t *testing.T) {
	workers, control := 0, 0
	stages := map[int]string{}
	for _, c := range All() {
		switch c.Kind {
		case Worker:
			workers++
			if c.Namespace != "cs-workers" {
				t.Errorf("worker %s is in namespace %s", c.Slug, c.Namespace)
			}
			if other, dup := stages[c.Stage]; dup || c.Stage < 1 || c.Stage > 10 {
				t.Errorf("worker %s has stage %d (also %q)", c.Slug, c.Stage, other)
			}
			stages[c.Stage] = c.Slug
		case ControlPlane:
			control++
			if c.Namespace != "clearedstack" {
				t.Errorf("service %s is in namespace %s", c.Slug, c.Namespace)
			}
		}
		if !IsRepository(c.Repository) {
			t.Errorf("%s: repository %q is not found", c.Slug, c.Repository)
		}
	}
	if workers != 10 || control != 7 {
		t.Errorf("%d workers and %d control-plane services; want 10 and 7", workers, control)
	}
	if IsWorker("conn") || !IsWorker("bellbuoy") || IsWorker("made-up") {
		t.Error("IsWorker is wrong")
	}
}
