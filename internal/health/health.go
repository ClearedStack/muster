// Package health receives Shipkeeper's health reports.
//
// A report is a closed form: a component from the registry, one of four
// health states, optionally one of the three error codes and optionally
// an image digest. There is no free-text field, so a report has nowhere
// to carry a node name or an address, and a report with any other field
// is refused before it gets here.
//
// A report is also how Muster learns that a worker was redeployed. When
// the digest reported for a worker differs from the last one recorded,
// every item that worker paused is replayed.
package health

import (
	"context"
	"regexp"
	"time"

	"github.com/ClearedStack/muster/internal/contract"
	"github.com/ClearedStack/muster/internal/logbook"
	"github.com/ClearedStack/muster/internal/queue"
	"github.com/ClearedStack/muster/internal/registry"
)

// MaxReports is the most reports one call may carry.
const MaxReports = 64

// Report is the health of one component.
type Report struct {
	Component string `json:"component"`
	State     string `json:"state"`
	Code      string `json:"code,omitempty"`
	Digest    string `json:"digest,omitempty"`
}

// Summary is the answer to a batch of reports.
type Summary struct {
	Received int `json:"received"`
	Replayed int `json:"replayed"`
}

// Notifier is told about each report after it has been kept.
type Notifier interface {
	Health(component, state, code string, at time.Time)
}

// Service receives reports.
type Service struct {
	Store  logbook.Store
	Clock  func() time.Time
	Notify Notifier
}

// The health states Shipkeeper sends.
var states = map[string]bool{"healthy": true, "idle": true, "degraded": true, "paused": true}

var digestForm = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Receive checks a batch of reports and keeps all of them, or none.
func (s *Service) Receive(ctx context.Context, reports []Report) (*Summary, error) {
	if len(reports) == 0 || len(reports) > MaxReports {
		return nil, contract.Malformed
	}
	seen := make(map[string]bool, len(reports))
	for _, r := range reports {
		if _, ok := registry.Lookup(r.Component); !ok || seen[r.Component] {
			return nil, contract.Malformed
		}
		seen[r.Component] = true
		if r.State == "" {
			return nil, contract.Malformed
		}
		switch contract.Code(r.Code) {
		case "", contract.Timeout, contract.Parse, contract.NeedsSubtool:
		default:
			return nil, contract.Malformed
		}
		if r.Digest != "" && !digestForm.MatchString(r.Digest) {
			return nil, contract.Malformed
		}
	}
	// Only once every report is well formed: a state that is a plain word
	// and not one of the four is valid and unfamiliar.
	for _, r := range reports {
		if !states[r.State] {
			return nil, contract.UnknownHealth
		}
	}

	var sum Summary
	var now time.Time
	err := s.Store.Update(ctx, func(tx logbook.Tx) error {
		sum = Summary{Received: len(reports)}
		now = s.Clock()
		for _, r := range reports {
			c, _ := tx.Component(r.Component)
			if r.Digest != "" {
				if registry.IsWorker(r.Component) && c.Digest != "" && c.Digest != r.Digest {
					sum.Replayed += queue.Replay(tx, r.Component, now)
				}
				c.Digest = r.Digest
			}
			c.Slug, c.State, c.Code, c.At = r.Component, r.State, r.Code, now.UTC()
			tx.PutComponent(c)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if s.Notify != nil {
		for _, r := range reports {
			s.Notify.Health(r.Component, r.State, r.Code, now)
		}
	}
	return &sum, nil
}
