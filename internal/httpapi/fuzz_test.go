package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ClearedStack/muster/internal/contract"
	"github.com/ClearedStack/muster/internal/lifecycle"
	"github.com/ClearedStack/muster/internal/logbook/memory"
)

type flushCancelRecorder struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (r *flushCancelRecorder) Flush() {
	r.ResponseRecorder.Flush()
	if r.cancel != nil {
		r.cancel()
	}
}

func FuzzHTTPAPI(f *testing.F) {
	allowedRefusals := map[int]map[string]bool{}
	for _, p := range contract.All() {
		raw, err := json.Marshal(struct {
			Error  contract.Code `json:"error"`
			Cause  string        `json:"cause"`
			Reason string        `json:"reason"`
		}{
			Error:  p.Code,
			Cause:  p.Cause,
			Reason: p.Reason,
		})
		if err != nil {
			f.Fatal(err)
		}
		if allowedRefusals[p.Status] == nil {
			allowedRefusals[p.Status] = map[string]bool{}
		}
		allowedRefusals[p.Status][string(raw)+"\n"] = true
	}

	f.Add("GET", "/healthz", "", "application/json", []byte(nil))
	f.Add("POST", "/v1/lease", "trawl", "application/json", []byte(`{"contract":"v1"}`))
	f.Add("POST", "/v1/lease", "trawl", "text/plain", []byte(`{"contract":"v1"}`))
	f.Add("POST", "/v1/complete", "sextant", "application/json", []byte(`{"contract":"v1","lease":"PA-0001.1.1","state":"observed","result":{"job":"made-up"}}`))
	f.Add("POST", "/v1/fail", "assay", "application/json", []byte(`{"contract":"v1","lease":"PA-0001.1.1","code":"E_PARSE"}`))
	f.Add("POST", "/v1/needs_subtool", "sextant", "application/json", []byte(`{"contract":"v1","lease":"PA-0001.1.1"}`))
	f.Add("POST", "/v1/release", "countersign", "application/json", []byte(`{"contract":"v1","id":"PA-0001","reason":"fixed"}`))
	f.Add("POST", "/v1/items", "trawl", "application/json", []byte(`{"contract":"v1","source":"sam-gov","title":"Notice","address":"https://example.org/1"}`))
	f.Add("POST", "/v1/health", "shipkeeper", "application/json", []byte(`{"contract":"v1","reports":[{"component":"muster","state":"healthy"}]}`))
	f.Add("POST", "/v1/edits", "conn", "application/json", []byte(`{"contract":"v1","target":"a.b","change":"<b>hi</b>","reason":"fix","pow":"2026-01-05.00","honeypot":"","visitor":"198.51.100.1"}`))
	f.Add("GET", "/v1/feed", "conn", "", []byte(nil))
	f.Add("DELETE", "/v1/unknown", "bad-token", "application/json", []byte(`{"secret":"never-echo-me"}`))

	f.Fuzz(func(t *testing.T, method, path, caller, contentType string, body []byte) {
		if method == "" {
			method = http.MethodPost
		}
		if len(path) == 0 || path[0] != '/' {
			path = "/" + path
		}

		seed := &memory.Seed{
			Items: []memory.SeedItem{
				{ID: "PA-0001", State: lifecycle.Recorded, Lane: lifecycle.Normal, Next: "sextant"},
			},
		}
		st := newStack(t, seed)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, method, "http://muster.internal"+path, bytes.NewReader(body))
		if err != nil {
			// Invalid HTTP method token or control bytes in URL: nothing reaches ServeHTTP.
			return
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if caller != "" {
			if tok, ok := st.tokens[caller]; ok {
				req.Header.Set("Authorization", "Bearer "+tok)
			} else {
				req.Header.Set("Authorization", "Bearer "+caller)
			}
		}

		rec := &flushCancelRecorder{
			ResponseRecorder: httptest.NewRecorder(),
			cancel:           cancel,
		}
		st.server.ServeHTTP(rec, req)

		res := rec.Result()
		defer res.Body.Close()
		gotBody := rec.Body.String()

		if res.StatusCode >= 400 {
			byStatus := allowedRefusals[res.StatusCode]
			if !byStatus[gotBody] {
				t.Fatalf("status %d returned non-catalog refusal body %q", res.StatusCode, gotBody)
			}
			if got := res.Header.Get("Content-Type"); got != "application/json" {
				t.Fatalf("status %d Content-Type = %q, want application/json", res.StatusCode, got)
			}
			if got := res.Header.Get("Cache-Control"); got != "no-store" {
				t.Fatalf("status %d Cache-Control = %q, want no-store", res.StatusCode, got)
			}
		}
	})
}
