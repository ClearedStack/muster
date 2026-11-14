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

// rawOwner is a row as written: Contract v1 names (repository, file, kind)
// or the names Conn ships (ownerRepo, filePath, editClass), not both.
type rawOwner struct {
	Repository string `json:"repository"`
	File       string `json:"file"`
	Kind       string `json:"kind"`
	OwnerRepo  string `json:"ownerRepo"`
	FilePath   string `json:"filePath"`
	EditClass  string `json:"editClass"`
}

// ParseOwnership reads ownership_map.json and refuses anything in it that
// it does not recognise: an unknown field, a repository that is not in
// the registry, a kind of edit that is not DATA, CONFIG or ISSUE.
//
// Besides the contract's {"elements": ...} form it takes the map Conn
// ships, which adds "version": "v1", "maxBytes" (1 to MaxBytes) and the
// row names ownerRepo, filePath and editClass.
func ParseOwnership(data []byte) (Ownership, error) {
	if len(data) > MaxOwnershipBytes {
		return nil, errors.New("the ownership map is too large")
	}
	var file struct {
		Version  *string             `json:"version"`
		MaxBytes *int                `json:"maxBytes"`
		Elements map[string]rawOwner `json:"elements"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		return nil, errors.New("the ownership map could not be read")
	}
	if dec.More() {
		return nil, errors.New("the ownership map has more than one value")
	}
	if file.Version != nil && *file.Version != "v1" {
		return nil, errors.New("the ownership map version is not known")
	}
	if file.MaxBytes != nil && (*file.MaxBytes < 1 || *file.MaxBytes > MaxBytes) {
		return nil, errors.New("the ownership map maxBytes is out of range")
	}
	if file.Elements == nil {
		return nil, errors.New("the ownership map has no elements")
	}
	out := make(Ownership, len(file.Elements))
	for target, raw := range file.Elements {
		if len(target) > MaxTargetBytes || !targetForm.MatchString(target) {
			return nil, errors.New("the ownership map has a target name that is too long or not a well-formed name")
		}
		o, err := resolveOwner(raw)
		if err != nil {
			return nil, fmt.Errorf("ownership map, target %q: %w", target, err)
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
		out[target] = o
	}
	return out, nil
}

// resolveOwner folds the two sets of row names into one, refusing a row
// that uses both names for the same thing.
func resolveOwner(r rawOwner) (Owner, error) {
	if (r.Repository != "" && r.OwnerRepo != "") ||
		(r.File != "" && r.FilePath != "") ||
		(r.Kind != "" && r.EditClass != "") {
		return Owner{}, errors.New("a field is given under both of its names")
	}
	return Owner{
		Repository: cmpOr(r.Repository, r.OwnerRepo),
		File:       cmpOr(r.File, r.FilePath),
		Kind:       cmpOr(r.Kind, r.EditClass),
	}, nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
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
