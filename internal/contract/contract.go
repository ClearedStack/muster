// Package contract holds the parts of worker contract v1 that every other
// package needs: the version, the three public error codes and the fixed
// list of refusals.
//
// A refusal carries an HTTP status, one of the three codes and a reason.
// Every reason is a fixed sentence written here. None is built from
// request data, a file path, a host name or an error from another system,
// so nothing that names a machine, an address or a credential can reach a
// caller through one.
package contract

import "net/http"

// Version is the only contract version this build speaks.
const Version = "v1"

// Code is one of the three public error codes.
type Code string

const (
	// Timeout: something outside took too long.
	Timeout Code = "E_TIMEOUT"
	// Parse: the data was broken.
	Parse Code = "E_PARSE"
	// NeedsSubtool: the data was valid and unfamiliar.
	NeedsSubtool Code = "E_NEEDS_SUBTOOL"
)

// Problem is a refusal. It is the only error a caller ever sees.
type Problem struct {
	Status int
	Code   Code
	Reason string
}

func (p *Problem) Error() string { return string(p.Code) + ": " + p.Reason }

// The complete list of refusals.
var (
	Unreadable      = &Problem{http.StatusBadRequest, Parse, "the request could not be read"}
	Malformed       = &Problem{http.StatusBadRequest, Parse, "a required field is missing or malformed"}
	TooLarge        = &Problem{http.StatusRequestEntityTooLarge, Parse, "the request is too large"}
	WrongType       = &Problem{http.StatusUnsupportedMediaType, Parse, "the content type must be application/json"}
	NoCredential    = &Problem{http.StatusUnauthorized, Parse, "no valid credential was presented"}
	NotPermitted    = &Problem{http.StatusForbidden, Parse, "this caller may not make this call"}
	NoSuchCall      = &Problem{http.StatusNotFound, Parse, "no such call"}
	WrongMethod     = &Problem{http.StatusMethodNotAllowed, Parse, "this call does not accept that method"}
	LeaseNotHeld    = &Problem{http.StatusConflict, Parse, "the lease is not held"}
	MoveNotAllowed  = &Problem{http.StatusConflict, Parse, "that move is not allowed"}
	UseNeedsSubtool = &Problem{http.StatusBadRequest, Parse, "report a missing rule with the needs_subtool call"}
	BadProof        = &Problem{http.StatusBadRequest, Parse, "the proof of work is not valid"}
	UnknownTarget   = &Problem{http.StatusBadRequest, Parse, "the target is not in the ownership map"}
	NothingLeft     = &Problem{http.StatusBadRequest, Parse, "nothing is left of the proposal once markup is removed"}
	Inconsistent    = &Problem{http.StatusInternalServerError, Parse, "a stored record was not consistent and has been set aside"}
	Unexpected      = &Problem{http.StatusInternalServerError, Parse, "an unexpected condition; the request was refused"}

	UnknownVersion = &Problem{http.StatusUnprocessableEntity, NeedsSubtool, "this contract version is not known"}
	UnknownHealth  = &Problem{http.StatusUnprocessableEntity, NeedsSubtool, "this health state is not known"}

	StoreSlow    = &Problem{http.StatusServiceUnavailable, Timeout, "the record store took too long"}
	IdentitySlow = &Problem{http.StatusServiceUnavailable, Timeout, "the identity check took too long"}
	Busy         = &Problem{http.StatusServiceUnavailable, Timeout, "too busy; try again"}
	TooMany      = &Problem{http.StatusTooManyRequests, Timeout, "too many submissions from this source today; try again tomorrow"}
)

// All lists every refusal, for the saved example that pins them.
func All() []*Problem {
	return []*Problem{
		Unreadable, Malformed, TooLarge, WrongType, NoCredential, NotPermitted,
		NoSuchCall, WrongMethod, LeaseNotHeld, MoveNotAllowed, UseNeedsSubtool,
		BadProof, UnknownTarget, NothingLeft, Inconsistent, Unexpected,
		UnknownVersion, UnknownHealth, StoreSlow, IdentitySlow, Busy, TooMany,
	}
}
