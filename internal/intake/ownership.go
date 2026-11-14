package intake

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/ClearedStack/muster/internal/registry"
)

// Owner says where one editable element lives and what kind of change an
// edit to it is.
type Owner struct {
	Repository string `json:"repository"`
	File       string `json:"file"`
	Kind       string `json:"kind"`
}

// Ownership is the ownership map: every editable element, by its target
// name.
type Ownership map[string]Owner

// Limits on the ownership map and on a target name.
const (
	MaxOwnershipBytes = 1 << 20
	MaxTargetBytes    = 100
)

// A target is a name of the form Logbook keeps for an element: lower
// case words joined by full stops, hyphens or underscores, such as
// catalog.sample-tool.description.
var targetForm = regexp.MustCompile(`^[a-z][a-z0-9]*([._-][a-z0-9]+)*$`)

var editKinds = map[string]bool{"DATA": true, "CONFIG": true, "ISSUE": true}

// ParseOwnership reads ownership_map.json and refuses anything in it that
// it does not recognise: an unknown field, a repository that is not in
// the registry, a kind of edit that is not DATA, CONFIG or ISSUE.
func ParseOwnership(data []byte) (Ownership, error) {
	if len(data) > MaxOwnershipBytes {
		return nil, errors.New("the ownership map is too large")
	}
	var file struct {
		Elements map[string]Owner `json:"elements"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		return nil, errors.New("the ownership map could not be read")
	}
	if dec.More() {
		return nil, errors.New("the ownership map has more than one value")
	}
	if file.Elements == nil {
		return nil, errors.New("the ownership map has no elements")
	}
	for target, o := range file.Elements {
		if len(target) > MaxTargetBytes || !targetForm.MatchString(target) {
			return nil, errors.New("the ownership map has a target name that is too long or not a well-formed name")
		}
		if !registry.IsRepository(o.Repository) {
			return nil, fmt.Errorf("ownership map, target %q: the repository is not in the registry", target)
		}
		if o.File == "" || !plain(o.File) {
			return nil, fmt.Errorf("ownership map, target %q: the file is missing or not plain text", target)
		}
		if !editKinds[o.Kind] {
			return nil, fmt.Errorf("ownership map, target %q: the kind must be DATA, CONFIG or ISSUE", target)
		}
	}
	return Ownership(file.Elements), nil
}

// plain reports whether s is free of control characters.
func plain(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
