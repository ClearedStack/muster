package intake

import (
	"context"
	"encoding/json"
	"net/netip"
	"unicode"

	"github.com/ClearedStack/muster/internal/contract"
	"github.com/ClearedStack/muster/internal/lifecycle"
	"github.com/ClearedStack/muster/internal/logbook"
	"github.com/ClearedStack/muster/internal/queue"
)

// Limits on a candidate, in bytes, which is how Logbook counts them.
const (
	MaxTitle   = 200
	MaxAddress = 2000
)

// SiteForm is the source of a candidate a visitor sent in through the
// form on the site. It is the one source that needs a proof of work and
// counts against the visitor's daily limit.
const SiteForm = "site-form"

// sources are the places a candidate can come from, and the lane a new
// item from each starts in. A bulk find from an automated sweep starts
// in backfill; everything else starts in normal.
var sources = map[string]lifecycle.Lane{
	"sam-gov":       lifecycle.Normal,
	"github-issues": lifecycle.Normal,
	"web-sweep":     lifecycle.Backfill,
	SiteForm:        lifecycle.Normal,
}

// Candidate is a find to be recorded as an item.
type Candidate struct {
	Source  string          `json:"source"`
	Title   string          `json:"title"`
	Address string          `json:"address"`
	Result  json.RawMessage `json:"result"`

	// Only with the source site-form, and required with it: the puzzle
	// token and the visitor's address.
	Pow     string `json:"pow"`
	Visitor string `json:"visitor"`
}

// Recorded is the answer to a candidate.
type Recorded struct {
	ID        string          `json:"id"`
	State     lifecycle.State `json:"state"`
	Next      string          `json:"next,omitempty"`
	Duplicate bool            `json:"duplicate,omitempty"`
}

// Record makes an item of a candidate, once.
//
// An address not seen before becomes a new item in discovered, with
// Trawl's step waiting. An address already recorded changes nothing but
// the ledger, which notes the sighting; the answer names the item that
// has it and says it is a duplicate. Addresses are compared exactly as
// sent: making equivalent addresses equal is Trawl's rule, not Muster's.
//
// A candidate from the site's form passes the same proof of work as an
// edit proposal and counts against the same daily limit. The puzzle is
// the one in pow.go with the source, title and address in the place of
// the target, change and reason.
func (s *Service) Record(ctx context.Context, caller string, c Candidate) (*Recorded, error) {
	for _, field := range []string{c.Source, c.Title, c.Address, c.Pow, c.Visitor} {
		if !text(field) {
			return nil, contract.BadField
		}
	}
	if c.Source == "" || c.Title == "" || c.Address == "" {
		return nil, contract.MissingField
	}
	if !sourceForm(c.Source) {
		return nil, contract.BadField
	}
	if len(c.Title) > MaxTitle || !plain(c.Title) {
		return nil, contract.BadField
	}
	if len(c.Address) > MaxAddress || !unbroken(c.Address) {
		return nil, contract.BadField
	}
	lane, known := sources[c.Source]
	if !known {
		return nil, contract.UnknownSource
	}
	now := s.Clock()

	fromForm := c.Source == SiteForm
	var addr netip.Addr
	if fromForm {
		if c.Pow == "" || c.Visitor == "" {
			return nil, contract.MissingField
		}
		var ok bool
		if addr, ok = visitor(c.Visitor); !ok {
			return nil, contract.BadField
		}
		if !Proven(c.Pow, c.Source, c.Title, c.Address, s.Difficulty, now) {
			return nil, contract.BadProof
		}
	} else if c.Pow != "" || c.Visitor != "" {
		// These belong to the site's form and to no other source.
		return nil, contract.BadField
	}

	title := Strip(c.Title)

	var answer *Recorded
	var refusal *contract.Problem
	err := s.Store.Update(ctx, func(tx logbook.Tx) error {
		answer, refusal = nil, nil
		if fromForm && !s.admit(tx, addr, now) {
			refusal = contract.RateLimited
			return nil
		}
		if title == "" {
			refusal = contract.NothingLeft
			return nil
		}
		it := logbook.Item{
			State: lifecycle.Discovered, Lane: lane,
			Source: c.Source, Title: title, Address: c.Address,
		}
		if c.Result != nil {
			it.Results = map[string]json.RawMessage{caller: append(json.RawMessage(nil), c.Result...)}
		}
		id, existing := tx.AddItem(it)
		if id == "" {
			refusal = contract.Unexpected
			return nil
		}
		if existing {
			known, _ := tx.Item(id)
			queue.Record(tx, now, caller, logbook.ActSighting, id, known.State, known.State, "")
			answer = &Recorded{ID: id, State: known.State, Duplicate: true}
			return nil
		}
		tx.PutEntry(logbook.Entry{
			Key:     id,
			Subject: id,
			Kind:    lifecycle.KindItem,
			Worker:  "trawl",
			Lane:    lane,
		})
		queue.Record(tx, now, caller, logbook.ActRecord, id, "", lifecycle.Discovered, "")
		answer = &Recorded{ID: id, State: lifecycle.Discovered, Next: "trawl"}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return nil, refusal
	}
	return answer, nil
}

// sourceForm reports whether s has the shape of a source name: lower
// case letters, digits and hyphens.
func sourceForm(s string) bool {
	if len(s) > 32 {
		return false
	}
	for _, c := range []byte(s) {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// unbroken reports whether s has no white space and no control
// characters, as an address must.
func unbroken(s string) bool {
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}
