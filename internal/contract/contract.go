// Package contract holds the parts of worker contract v1 that every other
// package needs: the version, the three public error codes and the fixed
// list of refusals.
//
// A refusal carries an HTTP status, one of the three codes, a cause and
// a reason. Every cause and reason is fixed text written here. None is
// built from request data, a file path, a host name or an error from
// another system, so nothing that names a machine, an address or a
// credential can reach a caller through one.
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
//
// Status is the HTTP status. Code is one of the three public codes.
// Cause is a short fixed word a program can act on; each status has its
// own few. Reason is the same thing said in a sentence for a person.
type Problem struct {
	Status int
	Code   Code
	Cause  string
	Reason string
}

func (p *Problem) Error() string { return string(p.Code) + " " + p.Cause + ": " + p.Reason }

// The complete list of refusals.
var (
	// 400: the request itself is wrong.
	NotJSON         = &Problem{http.StatusBadRequest, Parse, "not_json", "the request is not one json value"}
	UnknownField    = &Problem{http.StatusBadRequest, Parse, "unknown_field", "the request has a field this call does not define"}
	MissingField    = &Problem{http.StatusBadRequest, Parse, "missing_field", "a required field is missing"}
	BadField        = &Problem{http.StatusBadRequest, Parse, "bad_field", "a field does not have the form this call requires"}
	UseNeedsSubtool = &Problem{http.StatusBadRequest, Parse, "bad_field", "report a missing rule with the needs_subtool call"}
	BadProof        = &Problem{http.StatusBadRequest, Parse, "bad_field", "the proof of work is not valid"}
	UnknownTarget   = &Problem{http.StatusBadRequest, Parse, "bad_field", "the target is not in the ownership map"}
	NothingLeft     = &Problem{http.StatusBadRequest, Parse, "bad_field", "nothing is left of the text once markup is removed"}
	LogbookRefused  = &Problem{http.StatusBadRequest, Parse, "bad_field", "the record store refused a field"}

	NoCredential = &Problem{http.StatusUnauthorized, Parse, "no_credential", "no valid credential was presented"}
	NotYourCall  = &Problem{http.StatusForbidden, Parse, "not_your_call", "this caller may not make this call"}
	NoSuchCall   = &Problem{http.StatusNotFound, Parse, "no_such_call", "no such call"}
	WrongMethod  = &Problem{http.StatusMethodNotAllowed, Parse, "wrong_method", "this call does not accept that method"}
	TooLarge     = &Problem{http.StatusRequestEntityTooLarge, Parse, "too_large", "the request is too large"}
	WrongType    = &Problem{http.StatusUnsupportedMediaType, Parse, "wrong_type", "the content type must be application/json"}

	// 409: the request is well formed and the records do not allow it.
	LeaseNotHeld   = &Problem{http.StatusConflict, Parse, "lease_not_held", "the lease is not held"}
	MoveNotAllowed = &Problem{http.StatusConflict, Parse, "move_not_allowed", "that move is not allowed"}
	Duplicate      = &Problem{http.StatusConflict, Parse, "duplicate", "the same component is reported twice in one batch"}

	// 422: valid and unfamiliar.
	UnknownContract = &Problem{http.StatusUnprocessableEntity, NeedsSubtool, "unknown_contract", "this contract version is not known"}
	UnknownHealth   = &Problem{http.StatusUnprocessableEntity, NeedsSubtool, "unknown_value", "this health state is not known"}
	UnknownSource   = &Problem{http.StatusUnprocessableEntity, NeedsSubtool, "unknown_value", "this source is not known"}

	// Logbook checks every row it is given and refuses one with a public
	// code. LogbookRefused, above, is its E_PARSE; this is its
	// E_NEEDS_SUBTOOL.
	LogbookUnfamiliar = &Problem{http.StatusUnprocessableEntity, NeedsSubtool, "unknown_value", "the record store has no rule for a value"}

	RateLimited = &Problem{http.StatusTooManyRequests, Timeout, "rate_limited", "too many submissions from this visitor today; try again tomorrow"}

	// 503: try again.
	StoreSlow    = &Problem{http.StatusServiceUnavailable, Timeout, "store_slow", "the record store took too long"}
	Busy         = &Problem{http.StatusServiceUnavailable, Timeout, "busy", "too busy; try again"}
	IdentitySlow = &Problem{http.StatusServiceUnavailable, Timeout, "busy", "the identity check took too long; try again"}

	BadRecord  = &Problem{http.StatusInternalServerError, Parse, "bad_record", "a stored record was not consistent and has been set aside"}
	Unexpected = &Problem{http.StatusInternalServerError, Parse, "unexpected", "an unexpected condition; the request was refused"}
)

// All lists every refusal, for the saved example that pins them.
func All() []*Problem {
	return []*Problem{
		NotJSON, UnknownField, MissingField, BadField, UseNeedsSubtool, BadProof,
		UnknownTarget, NothingLeft, LogbookRefused, NoCredential, NotYourCall, NoSuchCall,
		WrongMethod, TooLarge, WrongType, LeaseNotHeld, MoveNotAllowed, Duplicate,
		UnknownContract, UnknownHealth, UnknownSource, LogbookUnfamiliar, RateLimited, StoreSlow,
		Busy, IdentitySlow, BadRecord, Unexpected,
	}
}

// Causes lists, for each status, the causes a refusal with that status
// may carry.
func Causes() map[int][]string {
	return map[int][]string{
		http.StatusBadRequest:            {"not_json", "unknown_field", "missing_field", "bad_field"},
		http.StatusUnauthorized:          {"no_credential"},
		http.StatusForbidden:             {"not_your_call"},
		http.StatusNotFound:              {"no_such_call"},
		http.StatusMethodNotAllowed:      {"wrong_method"},
		http.StatusRequestEntityTooLarge: {"too_large"},
		http.StatusUnsupportedMediaType:  {"wrong_type"},
		http.StatusConflict:              {"lease_not_held", "move_not_allowed", "duplicate"},
		http.StatusUnprocessableEntity:   {"unknown_contract", "unknown_value"},
		http.StatusTooManyRequests:       {"rate_limited"},
		http.StatusServiceUnavailable:    {"store_slow", "busy"},
		http.StatusInternalServerError:   {"bad_record", "unexpected"},
	}
}
