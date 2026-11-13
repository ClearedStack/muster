// Package httpapi is Muster's cluster-private HTTP surface.
//
//	POST /v1/lease           a worker asks for its next step
//	POST /v1/heartbeat       a worker keeps a lease
//	POST /v1/complete        a worker reports a finished step
//	POST /v1/fail            a worker reports E_TIMEOUT or E_PARSE
//	POST /v1/needs_subtool   a worker pauses an item it has no rule for
//	POST /v1/items           Trawl or Conn records a candidate
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
	MaxBody        = 64 << 10  // any request body
	MaxWaybillBody = 256 << 10 // Waybill's complete, which carries a specification
	MaxInFlight    = 64        // calls being answered at once, the feed apart
	writeWithin    = 10 * time.Second
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

// who says which callers may make a call.
type who func(caller string) bool

func only(names ...string) who {
	return func(caller string) bool {
		for _, n := range names {
			if n == caller {
				return true
			}
		}
		return false
	}
}

type call struct {
	method string
	may    who // nil for the one call that needs no token
	stream bool
	do     func(s *Server, w http.ResponseWriter, r *http.Request, caller string)
}

var calls = map[string]call{
	"/healthz":          {method: http.MethodGet, do: (*Server).alive},
	"/v1/lease":         {method: http.MethodPost, may: registry.IsWorker, do: (*Server).lease},
	"/v1/heartbeat":     {method: http.MethodPost, may: registry.IsWorker, do: (*Server).heartbeat},
	"/v1/complete":      {method: http.MethodPost, may: registry.IsWorker, do: (*Server).complete},
	"/v1/fail":          {method: http.MethodPost, may: registry.IsWorker, do: (*Server).fail},
	"/v1/needs_subtool": {method: http.MethodPost, may: registry.IsWorker, do: (*Server).needsSubtool},
	"/v1/items":         {method: http.MethodPost, may: only("trawl", registry.Conn), do: (*Server).items},
	"/v1/health":        {method: http.MethodPost, may: only(registry.Shipkeeper), do: (*Server).health},
	"/v1/edits":         {method: http.MethodPost, may: only(registry.Conn), do: (*Server).edits},
	"/v1/feed":          {method: http.MethodGet, may: only(registry.Conn), stream: true, do: (*Server).feed},
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
	if c.may == nil {
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
	caller, p := s.identify(r, c.may)
	if p != nil {
		refuse(w, p)
		return
	}
	c.do(s, w, r, caller)
}

func (s *Server) identify(r *http.Request, may who) (string, *contract.Problem) {
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
	if !may(caller) {
		return "", contract.NotYourCall
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
	if p := read(w, r, MaxBody, &req, &req.Contract); p != nil {
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
	if p := read(w, r, MaxBody, &req, &req.Contract); p != nil {
		refuse(w, p)
		return
	}
	if req.Lease == "" {
		refuse(w, contract.MissingField)
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
	limit := int64(MaxBody)
	if caller == "waybill" {
		limit = MaxWaybillBody
	}
	if p := read(w, r, limit, &req, &req.Contract); p != nil {
		refuse(w, p)
		return
	}
	if req.Lease == "" || req.State == "" {
		refuse(w, contract.MissingField)
		return
	}
	result, ok := object(req.Result)
	if !ok {
		refuse(w, contract.BadField)
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
	if p := read(w, r, MaxBody, &req, &req.Contract); p != nil {
		refuse(w, p)
		return
	}
	if req.Lease == "" || req.Code == "" {
		refuse(w, contract.MissingField)
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
	if p := read(w, r, MaxBody, &req, &req.Contract); p != nil {
		refuse(w, p)
		return
	}
	if req.Lease == "" {
		refuse(w, contract.MissingField)
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

type itemRequest struct {
	Contract string `json:"contract"`
	intake.Candidate
}

func (s *Server) items(w http.ResponseWriter, r *http.Request, caller string) {
	var req itemRequest
	if p := read(w, r, MaxBody, &req, &req.Contract); p != nil {
		refuse(w, p)
		return
	}
	result, ok := object(req.Result)
	if !ok {
		refuse(w, contract.BadField)
		return
	}
	req.Result = result
	ctx, cancel := s.within(r)
	defer cancel()
	recorded, err := s.Intake.Record(ctx, caller, req.Candidate)
	if err != nil {
		refuse(w, problemOf(err))
		return
	}
	status := http.StatusCreated
	if recorded.Duplicate {
		status = http.StatusOK
	}
	answer(w, status, recorded)
}

type healthRequest struct {
	Contract string          `json:"contract"`
	Reports  []health.Report `json:"reports"`
}

func (s *Server) health(w http.ResponseWriter, r *http.Request, _ string) {
	var req healthRequest
	if p := read(w, r, MaxBody, &req, &req.Contract); p != nil {
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
	size, p := readSized(w, r, MaxBody, &req, &req.Contract)
	if p != nil {
		refuse(w, p)
		return
	}
	ctx, cancel := s.within(r)
	defer cancel()
	receipt, err := s.Intake.Receive(ctx, req.Submission, size)
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
// one JSON value, has a field the call does not define, or has a field
// of the wrong type.
func read(w http.ResponseWriter, r *http.Request, limit int64, dst any, version *string) *contract.Problem {
	_, p := readSized(w, r, limit, dst, version)
	return p
}

// readSized is read, and also returns the weight of the body as sent.
func readSized(w http.ResponseWriter, r *http.Request, limit int64, dst any, version *string) (int, *contract.Problem) {
	kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || kind != "application/json" {
		return 0, contract.WrongType
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return 0, contract.TooLarge
		}
		return 0, contract.NotJSON
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var wrongType *json.UnmarshalTypeError
		switch {
		case errors.As(err, &wrongType):
			// The request is JSON, and something in it is not the kind
			// of value the call takes there.
			return 0, contract.BadField
		case strings.HasPrefix(err.Error(), "json: unknown field "):
			return 0, contract.UnknownField
		default:
			return 0, contract.NotJSON
		}
	}
	if _, err := dec.Token(); err != io.EOF {
		return 0, contract.NotJSON
	}
	switch *version {
	case contract.Version:
		return len(body), nil
	case "":
		return 0, contract.MissingField
	default:
		return 0, contract.UnknownContract
	}
}

// object reads an optional JSON object. It returns nil for one that was
// left out, and false for a value that is not an object.
func object(raw json.RawMessage) (json.RawMessage, bool) {
	switch {
	case len(raw) == 0 || string(raw) == "null":
		return nil, true
	case raw[0] != '{':
		return nil, false
	}
	return raw, true
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
	Cause  string        `json:"cause"`
	Reason string        `json:"reason"`
}

func refuse(w http.ResponseWriter, p *contract.Problem) {
	answer(w, p.Status, refusal{Error: p.Code, Cause: p.Cause, Reason: p.Reason})
}

func answer(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		status = http.StatusInternalServerError
		p := contract.Unexpected
		body, _ = json.Marshal(refusal{Error: p.Code, Cause: p.Cause, Reason: p.Reason})
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}
