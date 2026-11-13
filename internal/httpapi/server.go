// Package httpapi is Muster's cluster-private HTTP surface.
//
//	POST /v1/lease           a worker asks for its next step
//	POST /v1/heartbeat       a worker keeps a lease
//	POST /v1/complete        a worker reports a finished step
//	POST /v1/fail            a worker reports E_TIMEOUT or E_PARSE
//	POST /v1/needs_subtool   a worker pauses an item it has no rule for
//	POST /v1/health          Shipkeeper reports component health
//	POST /v1/edits           Conn forwards an edit proposal
//	GET  /v1/feed            Conn subscribes to the live feed
//	GET  /healthz            the cluster asks whether Muster is up
//
// Every call but the last needs a bearer token, and each caller may make
// only its own calls. Every refusal is one of the fixed refusals in the
// contract package.
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/ClearedStack/muster/internal/auth"
	"github.com/ClearedStack/muster/internal/contract"
	"github.com/ClearedStack/muster/internal/feed"
	"github.com/ClearedStack/muster/internal/health"
	"github.com/ClearedStack/muster/internal/intake"
	"github.com/ClearedStack/muster/internal/lifecycle"
	"github.com/ClearedStack/muster/internal/queue"
	"github.com/ClearedStack/muster/internal/registry"
)

// Limits.
const (
	MaxWorkerBody = 64 << 10 // a worker call, result included
	MaxHealthBody = 16 << 10
	MaxInFlight   = 64 // calls being answered at once, the feed apart
	writeWithin   = 10 * time.Second
)

// Server answers HTTP calls.
type Server struct {
	Auth         auth.Authenticator
	Queue        *queue.Service
	Health       *health.Service
	Intake       *intake.Service
	Feed         *feed.Hub
	StoreTimeout time.Duration

	busy chan struct{}
}

// New returns a server ready to serve.
func New(s Server) *Server {
	s.busy = make(chan struct{}, MaxInFlight)
	return &s
}

type role int

const (
	open role = iota
	worker
	conn
	shipkeeper
)

type call struct {
	method string
	role   role
	stream bool
	do     func(s *Server, w http.ResponseWriter, r *http.Request, caller string)
}

var calls = map[string]call{
	"/healthz":          {method: http.MethodGet, role: open, do: (*Server).alive},
	"/v1/lease":         {method: http.MethodPost, role: worker, do: (*Server).lease},
	"/v1/heartbeat":     {method: http.MethodPost, role: worker, do: (*Server).heartbeat},
	"/v1/complete":      {method: http.MethodPost, role: worker, do: (*Server).complete},
	"/v1/fail":          {method: http.MethodPost, role: worker, do: (*Server).fail},
	"/v1/needs_subtool": {method: http.MethodPost, role: worker, do: (*Server).needsSubtool},
	"/v1/health":        {method: http.MethodPost, role: shipkeeper, do: (*Server).health},
	"/v1/edits":         {method: http.MethodPost, role: conn, do: (*Server).edits},
	"/v1/feed":          {method: http.MethodGet, role: conn, stream: true, do: (*Server).feed},
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c, ok := calls[r.URL.Path]
	if !ok {
		refuse(w, contract.NoSuchCall)
		return
	}
	if r.Method != c.method {
		w.Header().Set("Allow", c.method)
		refuse(w, contract.WrongMethod)
		return
	}
	if c.role == open {
		// The cluster's own check is always answered, so that a rush of
		// calls is never mistaken for Muster being down.
		c.do(s, w, r, "")
		return
	}
	if !c.stream {
		select {
		case s.busy <- struct{}{}:
			defer func() { <-s.busy }()
		default:
			refuse(w, contract.Busy)
			return
		}
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(writeWithin))
	}
	caller, p := s.identify(r, c.role)
	if p != nil {
		refuse(w, p)
		return
	}
	c.do(s, w, r, caller)
}

func (s *Server) identify(r *http.Request, want role) (string, *contract.Problem) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return "", contract.NoCredential
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.StoreTimeout)
	defer cancel()
	caller, err := s.Auth.Identify(ctx, token)
	switch {
	case err == nil:
	case errors.Is(err, auth.ErrNoCredential):
		return "", contract.NoCredential
	default:
		return "", contract.IdentitySlow
	}
	var may bool
	switch want {
	case worker:
		may = registry.IsWorker(caller)
	case conn:
		may = caller == registry.Conn
	case shipkeeper:
		may = caller == registry.Shipkeeper
	}
	if !may {
		return "", contract.NotPermitted
	}
	return caller, nil
}

func (s *Server) alive(w http.ResponseWriter, _ *http.Request, _ string) {
	answer(w, http.StatusOK, struct {
		Status string `json:"status"`
	}{"ok"})
}

type leaseRequest struct {
	Contract string `json:"contract"`
}

func (s *Server) lease(w http.ResponseWriter, r *http.Request, caller string) {
	var req leaseRequest
	if p := read(w, r, MaxWorkerBody, &req, &req.Contract); p != nil {
		refuse(w, p)
		return
	}
	ctx, cancel := s.within(r)
	defer cancel()
	grant, err := s.Queue.Lease(ctx, caller)
	if err != nil {
		refuse(w, problemOf(err))
		return
	}
	if grant == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	answer(w, http.StatusOK, grant)
}

type heartbeatRequest struct {
	Contract string `json:"contract"`
	Lease    string `json:"lease"`
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request, caller string) {
	var req heartbeatRequest
	if p := read(w, r, MaxWorkerBody, &req, &req.Contract); p != nil {
		refuse(w, p)
		return
	}
	if req.Lease == "" {
		refuse(w, contract.Malformed)
		return
	}
	ctx, cancel := s.within(r)
	defer cancel()
	beat, err := s.Queue.Heartbeat(ctx, caller, req.Lease)
	if err != nil {
		refuse(w, problemOf(err))
		return
	}
	answer(w, http.StatusOK, beat)
}

type completeRequest struct {
	Contract string          `json:"contract"`
	Lease    string          `json:"lease"`
	State    string          `json:"state"`
	Result   json.RawMessage `json:"result"`
}

func (s *Server) complete(w http.ResponseWriter, r *http.Request, caller string) {
	var req completeRequest
	if p := read(w, r, MaxWorkerBody, &req, &req.Contract); p != nil {
		refuse(w, p)
		return
	}
	if req.Lease == "" || req.State == "" {
		refuse(w, contract.Malformed)
		return
	}
	result := req.Result
	switch {
	case len(result) == 0 || string(result) == "null":
		result = nil
	case result[0] != '{':
		// A result is a JSON object or it is left out.
		refuse(w, contract.Malformed)
		return
	}
	ctx, cancel := s.within(r)
	defer cancel()
	ack, err := s.Queue.Complete(ctx, caller, req.Lease, lifecycle.State(req.State), result)
	if err != nil {
		refuse(w, problemOf(err))
		return
	}
	answer(w, http.StatusOK, ack)
}

type failRequest struct {
	Contract string `json:"contract"`
	Lease    string `json:"lease"`
	Code     string `json:"code"`
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, caller string) {
	var req failRequest
	if p := read(w, r, MaxWorkerBody, &req, &req.Contract); p != nil {
		refuse(w, p)
		return
	}
	if req.Lease == "" || req.Code == "" {
		refuse(w, contract.Malformed)
		return
	}
	ctx, cancel := s.within(r)
	defer cancel()
	ack, err := s.Queue.Fail(ctx, caller, req.Lease, contract.Code(req.Code))
	if err != nil {
		refuse(w, problemOf(err))
		return
	}
	answer(w, http.StatusOK, ack)
}

func (s *Server) needsSubtool(w http.ResponseWriter, r *http.Request, caller string) {
	var req heartbeatRequest
	if p := read(w, r, MaxWorkerBody, &req, &req.Contract); p != nil {
		refuse(w, p)
		return
	}
	if req.Lease == "" {
		refuse(w, contract.Malformed)
		return
	}
	ctx, cancel := s.within(r)
	defer cancel()
	ack, err := s.Queue.NeedsSubtool(ctx, caller, req.Lease)
	if err != nil {
		refuse(w, problemOf(err))
		return
	}
	answer(w, http.StatusOK, ack)
}

type healthRequest struct {
	Contract string          `json:"contract"`
	Reports  []health.Report `json:"reports"`
}

func (s *Server) health(w http.ResponseWriter, r *http.Request, _ string) {
	var req healthRequest
	if p := read(w, r, MaxHealthBody, &req, &req.Contract); p != nil {
		refuse(w, p)
		return
	}
	ctx, cancel := s.within(r)
	defer cancel()
	sum, err := s.Health.Receive(ctx, req.Reports)
	if err != nil {
		refuse(w, problemOf(err))
		return
	}
	answer(w, http.StatusOK, sum)
}

type editRequest struct {
	Contract string `json:"contract"`
	intake.Submission
}

func (s *Server) edits(w http.ResponseWriter, r *http.Request, _ string) {
	var req editRequest
	if p := read(w, r, intake.MaxBytes, &req, &req.Contract); p != nil {
		refuse(w, p)
		return
	}
	ctx, cancel := s.within(r)
	defer cancel()
	receipt, err := s.Intake.Receive(ctx, req.Submission)
	if err != nil {
		refuse(w, problemOf(err))
		return
	}
	if receipt == nil {
		// The honeypot: no error, no record, nothing to say.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	answer(w, http.StatusAccepted, receipt)
}

func (s *Server) feed(w http.ResponseWriter, r *http.Request, _ string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		refuse(w, contract.Unexpected)
		return
	}
	sub, err := s.Feed.Subscribe(r.Context())
	if err != nil {
		if errors.Is(err, feed.ErrFull) {
			refuse(w, contract.Busy)
		} else {
			refuse(w, problemOf(err))
		}
		return
	}
	defer s.Feed.Unsubscribe(sub)

	rc := http.NewResponseController(w)
	// The stream outlives the time allowed for reading a request.
	_ = rc.SetReadDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case frame, ok := <-sub.C:
			if !ok {
				return
			}
			_ = rc.SetWriteDeadline(time.Now().Add(writeWithin))
			if _, err := w.Write(frame); err != nil {
				return
			}
			// Send what else is already waiting before flushing, so a
			// batch leaves as one write.
			for more := true; more; {
				select {
				case frame, ok := <-sub.C:
					if !ok {
						flusher.Flush()
						return
					}
					if _, err := w.Write(frame); err != nil {
						return
					}
				default:
					more = false
				}
			}
			flusher.Flush()
		}
	}
}

func (s *Server) within(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), s.StoreTimeout)
}

// read decodes a request body into dst and checks the contract version,
// which every body carries. It refuses a body that is too large, is not
// JSON, has a field the call does not define, or holds anything after
// its one value.
func read(w http.ResponseWriter, r *http.Request, limit int64, dst any, version *string) *contract.Problem {
	kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || kind != "application/json" {
		return contract.WrongType
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return contract.TooLarge
		}
		return contract.Unreadable
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return contract.Unreadable
	}
	if _, err := dec.Token(); err != io.EOF {
		return contract.Unreadable
	}
	switch *version {
	case contract.Version:
		return nil
	case "":
		return contract.Malformed
	default:
		return contract.UnknownVersion
	}
}

// problemOf turns any error into one of the fixed refusals. An error
// from another system is never passed on.
func problemOf(err error) *contract.Problem {
	var p *contract.Problem
	switch {
	case errors.As(err, &p):
		return p
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return contract.StoreSlow
	default:
		return contract.Unexpected
	}
}

type refusal struct {
	Error  contract.Code `json:"error"`
	Reason string        `json:"reason"`
}

func refuse(w http.ResponseWriter, p *contract.Problem) {
	answer(w, p.Status, refusal{Error: p.Code, Reason: p.Reason})
}

func answer(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		status = http.StatusInternalServerError
		body, _ = json.Marshal(refusal{Error: contract.Unexpected.Code, Reason: contract.Unexpected.Reason})
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}
