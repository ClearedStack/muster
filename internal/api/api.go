// Package api serves the five worker calls over HTTP.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/clearedstack/muster/internal/logbook"
	"github.com/clearedstack/muster/internal/queue"
	"github.com/clearedstack/muster/internal/state"
)

// MaxBody is the largest request body accepted, in bytes.
const MaxBody = 64 << 10

// The three public error codes. Nothing else is ever sent as a code.
const (
	ETimeout      = "E_TIMEOUT"
	EParse        = "E_PARSE"
	ENeedsSubtool = "E_NEEDS_SUBTOOL"
)

// workers are the ten workers named in the registry. A call from any other
// name is refused.
var workers = map[string]bool{
	"trawl": true, "sextant": true, "assay": true, "countersign": true, "waybill": true,
	"moldloft": true, "shakedown": true, "chartroom": true, "tidegauge": true, "bellbuoy": true,
}

// Server holds what the handlers need.
type Server struct {
	Gate    *queue.Gatekeeper
	Now     func() time.Time // the clock; replaced in tests
	Timeout time.Duration    // how long one Logbook update may take
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/lease", s.post(s.lease))
	mux.HandleFunc("/v1/heartbeat", s.post(s.heartbeat))
	mux.HandleFunc("/v1/complete", s.post(s.complete))
	mux.HandleFunc("/v1/fail", s.post(s.fail))
	mux.HandleFunc("/v1/needs_subtool", s.post(s.needsSubtool))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		refuse(w, http.StatusNotFound, EParse, "There is no such call.")
	})
	return mux
}

type request struct {
	Worker  string          `json:"worker"`
	State   *string         `json:"state"`
	Item    *string         `json:"item"`
	LeaseID *string         `json:"lease_id"`
	To      *string         `json:"to"`
	Code    *string         `json:"code"`
	Result  json.RawMessage `json:"result"`
}

type leaseBody struct {
	Item      string `json:"item"`
	State     string `json:"state"`
	Lane      string `json:"lane"`
	LeaseID   string `json:"lease_id"`
	ExpiresAt string `json:"expires_at"`
}

type outcomeBody struct {
	Item  string `json:"item"`
	State string `json:"state"`
	Queue string `json:"queue"`
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// refusal is a request turned away before it reaches the gatekeeper.
type refusal struct {
	status  int
	message string
}

func bad(message string) *refusal { return &refusal{http.StatusBadRequest, message} }

type call func(ctx context.Context, req request, now time.Time) (any, *refusal, error)

// post wraps a call with everything the five have in common: the method,
// the size limit, strict reading of the body, the worker's name, the time
// limit and the translation of every failure into one of the three codes.
func (s *Server) post(fn call) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			refuse(w, http.StatusMethodNotAllowed, EParse, "This call accepts POST only.")
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				refuse(w, http.StatusRequestEntityTooLarge, EParse, "The request is larger than 64 KiB.")
				return
			}
			refuse(w, http.StatusBadRequest, EParse, "The request could not be read.")
			return
		}
		var req request
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			refuse(w, http.StatusBadRequest, EParse, "The request is not valid JSON with the expected fields.")
			return
		}
		if _, err := dec.Token(); err != io.EOF {
			refuse(w, http.StatusBadRequest, EParse, "The request has something after the JSON object.")
			return
		}
		if !workers[req.Worker] {
			refuse(w, http.StatusBadRequest, EParse, "The worker is not one of the ten in the registry.")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), s.Timeout)
		defer cancel()
		body, ref, err := fn(ctx, req, s.Now().UTC())
		switch {
		case ref != nil:
			refuse(w, ref.status, EParse, ref.message)
		case err == nil && body == nil:
			w.WriteHeader(http.StatusNoContent)
		case err == nil:
			send(w, http.StatusOK, body)
		case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
			refuse(w, http.StatusGatewayTimeout, ETimeout, "The record of state took too long to answer. Nothing was changed.")
		case errors.Is(err, queue.ErrNoItem):
			refuse(w, http.StatusNotFound, EParse, "There is no item with that identifier.")
		case errors.Is(err, queue.ErrLeaseNotHeld):
			refuse(w, http.StatusConflict, EParse, "This worker does not hold that lease on the item.")
		case errors.Is(err, queue.ErrMoveNotAllowed):
			refuse(w, http.StatusConflict, EParse, "The item may not make that move from its present state.")
		case errors.Is(err, queue.ErrRepeatDiffers):
			refuse(w, http.StatusConflict, EParse, "A different report was already recorded for that lease.")
		default:
			refuse(w, http.StatusInternalServerError, EParse, "The request could not be recorded. Nothing was changed.")
		}
	}
}

// only refuses a request that carries a field the call does not take, or
// lacks one it needs. The names are those of the JSON fields.
func only(req request, need ...string) *refusal {
	has := map[string]bool{
		"state": req.State != nil, "item": req.Item != nil, "lease_id": req.LeaseID != nil,
		"to": req.To != nil, "code": req.Code != nil, "result": req.Result != nil,
	}
	wanted := map[string]bool{}
	for _, n := range need {
		wanted[n] = true
		if !has[n] && n != "result" {
			return bad("The request lacks a field this call needs.")
		}
	}
	for n, present := range has {
		if present && !wanted[n] {
			return bad("The request has a field this call does not take.")
		}
	}
	return nil
}

func held(req request) *refusal {
	if !logbook.ValidID(*req.Item) {
		return bad("The item identifier is not of the form PA-0001.")
	}
	if *req.LeaseID == "" {
		return bad("The lease identifier is empty.")
	}
	return nil
}

func (s *Server) lease(ctx context.Context, req request, now time.Time) (any, *refusal, error) {
	if ref := only(req, "state"); ref != nil {
		return nil, ref, nil
	}
	st := state.State(*req.State)
	if !state.Valid(st) {
		return nil, bad("The state is not one of the fourteen."), nil
	}
	if !state.Leasable(st) {
		return nil, bad("Items in that state are not handed out."), nil
	}
	l, err := s.Gate.Lease(ctx, req.Worker, st, now)
	if err != nil || l == nil {
		return nil, nil, err
	}
	return leaseOut(l), nil, nil
}

func (s *Server) heartbeat(ctx context.Context, req request, now time.Time) (any, *refusal, error) {
	if ref := only(req, "item", "lease_id"); ref != nil {
		return nil, ref, nil
	}
	if ref := held(req); ref != nil {
		return nil, ref, nil
	}
	l, err := s.Gate.Heartbeat(ctx, req.Worker, *req.Item, *req.LeaseID, now)
	if err != nil {
		return nil, nil, err
	}
	return leaseOut(l), nil, nil
}

func (s *Server) complete(ctx context.Context, req request, now time.Time) (any, *refusal, error) {
	if ref := only(req, "item", "lease_id", "to", "result"); ref != nil {
		return nil, ref, nil
	}
	if ref := held(req); ref != nil {
		return nil, ref, nil
	}
	to := state.State(*req.To)
	if !state.Valid(to) {
		return nil, bad("The state is not one of the fourteen."), nil
	}
	var result json.RawMessage
	if req.Result != nil {
		var buf bytes.Buffer
		if err := json.Compact(&buf, req.Result); err != nil || buf.Len() == 0 || buf.Bytes()[0] != '{' {
			return nil, bad("The result is not a JSON object."), nil
		}
		result = buf.Bytes()
	}
	o, err := s.Gate.Complete(ctx, req.Worker, *req.Item, *req.LeaseID, to, result, now)
	return outcomeOut(o), nil, err
}

func (s *Server) fail(ctx context.Context, req request, now time.Time) (any, *refusal, error) {
	if ref := only(req, "item", "lease_id", "code"); ref != nil {
		return nil, ref, nil
	}
	if ref := held(req); ref != nil {
		return nil, ref, nil
	}
	switch *req.Code {
	case queue.CodeTimeout, queue.CodeParse:
	case ENeedsSubtool:
		return nil, bad("E_NEEDS_SUBTOOL is reported with the needs_subtool call."), nil
	default:
		return nil, bad("The code is not one of the three public error codes."), nil
	}
	o, err := s.Gate.Fail(ctx, req.Worker, *req.Item, *req.LeaseID, *req.Code, now)
	return outcomeOut(o), nil, err
}

func (s *Server) needsSubtool(ctx context.Context, req request, now time.Time) (any, *refusal, error) {
	if ref := only(req, "item", "lease_id"); ref != nil {
		return nil, ref, nil
	}
	if ref := held(req); ref != nil {
		return nil, ref, nil
	}
	o, err := s.Gate.NeedsSubtool(ctx, req.Worker, *req.Item, *req.LeaseID, now)
	return outcomeOut(o), nil, err
}

func leaseOut(l *queue.Lease) any {
	return leaseBody{Item: l.Item, State: string(l.State), Lane: string(l.Lane), LeaseID: l.LeaseID,
		ExpiresAt: l.ExpiresAt.UTC().Format(time.RFC3339)}
}

func outcomeOut(o *queue.Outcome) any {
	if o == nil {
		return nil
	}
	return outcomeBody{Item: o.Item, State: string(o.State), Queue: string(o.Queue)}
}

func send(w http.ResponseWriter, status int, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		status, raw = http.StatusInternalServerError, []byte(`{"error":{"code":"E_PARSE","message":"The answer could not be written."}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(append(raw, '\n'))
}

func refuse(w http.ResponseWriter, status int, code, message string) {
	send(w, status, errorBody{Error: errorDetail{Code: code, Message: message}})
}
