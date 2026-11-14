package intake

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

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

// Limits on the ownership map, a target name and a relative file path.
const (
	MaxOwnershipBytes = 1 << 20
	MaxTargetBytes    = 100
	MaxFileBytes      = 500
)

// A target is a name of the form Logbook keeps for an element: lower
// case words joined by full stops, hyphens or underscores, such as
// catalog.sample-tool.description.
var targetForm = regexp.MustCompile(`^[a-z][a-z0-9]*([._-][a-z0-9]+)*$`)

// relativePathForm matches Logbook 0010's core.require_relative_path rule:
// slash-separated path segments without leading/trailing/empty slashes or
// parent-directory traversal.
var relativePathForm = regexp.MustCompile(`^[A-Za-z0-9._\[\]-]+(/[A-Za-z0-9._\[\]-]+)*$`)

var editKinds = map[string]bool{"DATA": true, "CONFIG": true, "ISSUE": true}

type rawOwner struct {
	Repository string `json:"repository"`
	File       string `json:"file"`
	Kind       string `json:"kind"`
	OwnerRepo  string `json:"ownerRepo"`
	FilePath   string `json:"filePath"`
	EditClass  string `json:"editClass"`
}

type repoRule struct {
	Mirror bool `json:"mirror"`
}

type pathRule struct {
	Prefix string `json:"prefix"`
	Repo   string `json:"repo"`
}

// ParseOwnership reads ownership_map.json and refuses anything in it that
// it does not recognise: an unknown field, a repository that is not in
// the registry, a file path Logbook would reject, or a kind of edit that
// is not DATA, CONFIG or ISSUE.
//
// In addition to Contract v1's {"elements": ...} form, ParseOwnership
// accepts the shared cluster envelopes used by Conn (version, maxBytes
// and ownerRepo/filePath/editClass aliases) and Bellbuoy (repos, workers
// and paths).
func ParseOwnership(data []byte) (Ownership, error) {
	if len(data) > MaxOwnershipBytes {
		return nil, errors.New("the ownership map is too large")
	}
	var file struct {
		Version  *string             `json:"version"`
		MaxBytes *int                `json:"maxBytes"`
		Repos    map[string]repoRule `json:"repos"`
		Workers  map[string]string   `json:"workers"`
		Paths    []pathRule          `json:"paths"`
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
	for repo := range file.Repos {
		if !registry.IsRepository(repo) {
			return nil, fmt.Errorf("ownership map, repo %q: the repository is not in the registry", repo)
		}
	}
	for slug, repo := range file.Workers {
		if _, ok := registry.Lookup(slug); !ok {
			return nil, fmt.Errorf("ownership map, worker %q: the component is not in the registry", slug)
		}
		if !registry.IsRepository(repo) {
			return nil, fmt.Errorf("ownership map, worker %q: the repository is not in the registry", slug)
		}
	}
	for _, p := range file.Paths {
		if p.Prefix == "" || !plain(p.Prefix) || !registry.IsRepository(p.Repo) {
			return nil, errors.New("ownership map: a path rule is malformed")
		}
	}
	if file.Elements == nil {
		return nil, errors.New("the ownership map has no elements")
	}
	out := make(Ownership, len(file.Elements))
	for target, raw := range file.Elements {
		if len(target) > MaxTargetBytes || !targetForm.MatchString(target) {
			return nil, errors.New("the ownership map has a target name that is too long or not a well-formed name")
		}
		o, err := resolveOwner(target, raw)
		if err != nil {
			return nil, err
		}
		out[target] = o
	}
	return out, nil
}

func resolveOwner(target string, raw rawOwner) (Owner, error) {
	if (raw.Repository != "" && raw.OwnerRepo != "") ||
		(raw.File != "" && raw.FilePath != "") ||
		(raw.Kind != "" && raw.EditClass != "") {
		return Owner{}, fmt.Errorf("ownership map, target %q: duplicate field alias", target)
	}
	o := Owner{
		Repository: firstNonEmpty(raw.Repository, raw.OwnerRepo),
		File:       firstNonEmpty(raw.File, raw.FilePath),
		Kind:       firstNonEmpty(raw.Kind, raw.EditClass),
	}
	if !registry.IsRepository(o.Repository) {
		return Owner{}, fmt.Errorf("ownership map, target %q: the repository is not in the registry", target)
	}
	if !ValidRelativePath(o.File) {
		return Owner{}, fmt.Errorf("ownership map, target %q: the file is missing or not a well-formed relative path", target)
	}
	if !editKinds[o.Kind] {
		return Owner{}, fmt.Errorf("ownership map, target %q: the kind must be DATA, CONFIG or ISSUE", target)
	}
	return o, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ValidRelativePath reports whether s satisfies Logbook 0010's
// core.require_relative_path check (1..500 bytes, relative segments only).
func ValidRelativePath(s string) bool {
	if len(s) == 0 || len(s) > MaxFileBytes || !relativePathForm.MatchString(s) {
		return false
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
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
