// Package intake screens anonymous edit proposals.
//
// A submission passes five layers. Muster runs the first four and keeps
// what passes as pending_approval, queued for Countersign. The fifth, a
// person's approval, happens there.
//
//  1. Proof of work: the puzzle token must be valid for this submission.
//  2. Honeypot: a submission with the hidden field filled in is dropped
//     without a trace.
//  3. Rate limit: submissions are counted against a hash of the source
//     address, made with a salt that changes every day. The address is
//     used for that one hash and is never stored or logged.
//  4. Shape and size: the target must be in the ownership map, the
//     request must fit in 4 KB, and markup and scripts are stripped.
//  5. Review gate: the proposal waits in pending_approval.
//
// The same proof of work and the same daily count guard a candidate sent
// in through the form on the site; see Record.
package intake

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/netip"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ClearedStack/muster/internal/contract"
	"github.com/ClearedStack/muster/internal/lifecycle"
	"github.com/ClearedStack/muster/internal/logbook"
	"github.com/ClearedStack/muster/internal/queue"
	"github.com/ClearedStack/muster/internal/registry"
)

// MaxBytes is the most an edit submission may weigh, as sent.
const MaxBytes = 4096

// MinSecretBytes is the least the salt secret may weigh.
const MinSecretBytes = 32

const saltLabel = "muster-salt-v1"

// Submission is an edit proposal as Conn forwards it.
type Submission struct {
	Target   string `json:"target"`
	Change   string `json:"change"`
	Reason   string `json:"reason"`
	Pow      string `json:"pow"`
	Honeypot string `json:"honeypot"`
	Source   string `json:"source"`
}

// Receipt is the answer to a submission that was kept.
type Receipt struct {
	ID     string          `json:"id"`
	Status lifecycle.State `json:"status"`
}

// Service screens submissions.
type Service struct {
	Store      logbook.Store
	Clock      func() time.Time
	Notify     queue.Notifier
	Owners     Ownership
	Secret     []byte
	Difficulty int
	DailyCap   int
}

// Receive screens one submission. size is the weight of the request as
// it was sent. Receive returns a receipt when the proposal was kept, and
// nil with no error when the honeypot dropped it.
func (s *Service) Receive(ctx context.Context, sub Submission, size int) (*Receipt, error) {
	for _, field := range []string{sub.Target, sub.Change, sub.Reason, sub.Pow, sub.Honeypot, sub.Source} {
		if !text(field) {
			return nil, contract.BadField
		}
	}
	if sub.Target == "" || sub.Change == "" || sub.Pow == "" || sub.Source == "" {
		return nil, contract.MissingField
	}
	addr, ok := visitor(sub.Source)
	if !ok {
		return nil, contract.BadField
	}
	now := s.Clock()

	// Layer 1.
	if !Proven(sub.Pow, sub.Target, sub.Change, sub.Reason, s.Difficulty, now) {
		return nil, contract.BadProof
	}
	// Layer 2.
	if sub.Honeypot != "" {
		return nil, nil
	}

	// Layer 4, the parts that need no record. Layer 3 has to count the
	// submission whether or not these pass, so their verdict is held
	// until the count is taken.
	owner, known := s.Owners[sub.Target]
	change, reason := Strip(sub.Change), Strip(sub.Reason)
	var verdict *contract.Problem
	switch {
	case size > MaxBytes:
		verdict = contract.TooLarge
	case !known:
		verdict = contract.UnknownTarget
	case change == "":
		verdict = contract.NothingLeft
	}

	var receipt *Receipt
	var refusal *contract.Problem
	err := s.Store.Update(ctx, func(tx logbook.Tx) error {
		receipt, refusal = nil, nil
		// Layer 3.
		if !s.admit(tx, addr, now) {
			refusal = contract.RateLimited
			return nil
		}
		if verdict != nil {
			refusal = verdict
			return nil
		}
		// Layer 5.
		n := tx.Next(logbook.CounterProposal)
		if n > lifecycle.MaxID {
			refusal = contract.Unexpected
			return nil
		}
		id := lifecycle.ProposalID(n)
		tx.PutProposal(logbook.Proposal{
			ID:         id,
			Target:     sub.Target,
			Repository: owner.Repository,
			File:       owner.File,
			EditKind:   owner.Kind,
			Change:     change,
			Reason:     reason,
			Status:     lifecycle.PendingApproval,
			Received:   now.UTC(),
		})
		tx.PutEntry(logbook.Entry{
			Key:     id,
			Subject: id,
			Kind:    lifecycle.KindProposal,
			Worker:  "countersign",
			Lane:    lifecycle.Normal,
			Arrival: tx.Next(logbook.CounterArrival),
		})
		queue.Record(tx, now, registry.Conn, logbook.ActPropose, id, "", lifecycle.PendingApproval, "")
		receipt = &Receipt{ID: id, Status: lifecycle.PendingApproval}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return nil, refusal
	}
	if s.Notify != nil {
		s.Notify.Edit(receipt.ID, receipt.Status, now)
	}
	return receipt, nil
}

// admit counts one submission against a visitor's address for the day
// and reports whether it is within the daily limit. Edit proposals and
// candidates sent through the site's form share the one count.
func (s *Service) admit(tx logbook.Tx, addr netip.Addr, now time.Time) bool {
	day := now.UTC().Format(dayForm)
	count, ok := tx.Bump(day, SourceKey(s.Secret, day, addr))
	return ok && count <= s.DailyCap
}

// text reports whether a field is well-formed text with no zero byte.
func text(field string) bool {
	return utf8.ValidString(field) && strings.IndexByte(field, 0) < 0
}

// visitor reads a visitor's address.
func visitor(s string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(s)
	if err != nil || addr.Zone() != "" {
		return netip.Addr{}, false
	}
	return addr, true
}

// SourceKey is the only thing ever kept about where a submission came
// from: a one-way hash of the address, keyed with that day's salt. The
// salt is itself derived from the secret and the date, so the same
// address gives an unrelated key every day and the keys of one day say
// nothing about another.
func SourceKey(secret []byte, day string, addr netip.Addr) string {
	salt := hmac.New(sha256.New, secret)
	salt.Write([]byte(saltLabel + "\n" + day))
	mac := hmac.New(sha256.New, salt.Sum(nil))
	mac.Write([]byte(addr.Unmap().String()))
	return hex.EncodeToString(mac.Sum(nil))
}
