// Package auth works out who is calling.
//
// Every caller presents a bearer token. A worker, Conn or Shipkeeper
// presents the token of its Kubernetes ServiceAccount, and Muster asks
// the cluster whose token it is. The answer is always a name from the
// registry, or a refusal.
package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/ClearedStack/muster/internal/registry"
)

// ErrNoCredential means the token is missing or is not the token of a
// component that may call Muster.
var ErrNoCredential = errors.New("auth: no valid credential")

// ErrUnavailable means the identity check could not be made in time.
var ErrUnavailable = errors.New("auth: identity check unavailable")

// Authenticator turns a bearer token into the registry name of the
// caller.
type Authenticator interface {
	Identify(ctx context.Context, token string) (string, error)
}

// MayCall reports whether slug names a component that calls Muster: one
// of the ten workers, Conn or Shipkeeper.
func MayCall(slug string) bool {
	return registry.IsWorker(slug) || slug == registry.Conn || slug == registry.Shipkeeper
}

// Table is a fixed list of made-up tokens, for running Muster outside a
// cluster. It keeps a hash of each token, not the token.
type Table struct {
	callers map[[sha256.Size]byte]string
}

// MinTokenBytes is the shortest token a table will accept.
const MinTokenBytes = 16

// ParseTable reads a table of the form {"callers":{"<token>":"<name>"}}.
func ParseTable(data []byte) (*Table, error) {
	var file struct {
		Callers map[string]string `json:"callers"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		return nil, errors.New("the caller table could not be read")
	}
	if dec.More() {
		return nil, errors.New("the caller table has more than one value")
	}
	if len(file.Callers) == 0 {
		return nil, errors.New("the caller table lists no callers")
	}
	t := &Table{callers: make(map[[sha256.Size]byte]string, len(file.Callers))}
	for token, slug := range file.Callers {
		if len(token) < MinTokenBytes {
			return nil, errors.New("the caller table has a token that is too short")
		}
		if !MayCall(slug) {
			return nil, errors.New("the caller table names a component that does not call Muster")
		}
		t.callers[sha256.Sum256([]byte(token))] = slug
	}
	return t, nil
}

// Identify looks the token up.
func (t *Table) Identify(_ context.Context, token string) (string, error) {
	if slug, ok := t.callers[sha256.Sum256([]byte(token))]; ok {
		return slug, nil
	}
	return "", ErrNoCredential
}
