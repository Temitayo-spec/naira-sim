// Package api is naira-sim's HTTP side: the public API a team's backend calls
// (/v1, spec section 2) and the control API for tests and simctl (/_sim,
// section 7).
package api

import (
	"bytes"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Temitayo-spec/naira-sim/internal/scenario"
	"github.com/Temitayo-spec/naira-sim/internal/sim"
)

// maxBody caps a request body; scenarios are the biggest thing sent.
const maxBody = 1 << 20

// The dashboard: one page that reads the control API (milestone 4).
//
//go:embed dashboard.html
var dashboard []byte

// New returns the handler for both APIs.
func New(s *sim.Sim) http.Handler {
	mux := http.NewServeMux()
	v1 := func(pattern string, h func(w http.ResponseWriter, r *http.Request)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if !authorised(s, r) {
				writeError(w, &sim.Error{Status: 401, Code: "unauthorized", Message: "send Authorization: Bearer <secret key>"})
				return
			}
			h(w, r)
		})
	}

	// --- public API ---

	v1("GET /v1/banks", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"banks": s.Banks()})
	})

	v1("POST /v1/name-enquiry", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			BankCode      string `json:"bank_code"`
			AccountNumber string `json:"account_number"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		acct, err := s.NameEnquiry(req.BankCode, req.AccountNumber)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, 200, map[string]string{"bank_code": acct.BankCode, "account_number": acct.AccountNumber, "account_name": acct.Name})
	})

	v1("GET /v1/balance", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"balance": s.Balance(), "currency": "NGN"})
	})

	v1("POST /v1/transfers", func(w http.ResponseWriter, r *http.Request) {
		var req sim.TransferRequest
		if !readJSON(w, r, &req) {
			return
		}
		tr, created, timedOut, err := s.CreateTransfer(req)
		switch {
		case err != nil:
			writeError(w, err)
		case timedOut:
			// The transfer exists and goes on; only the answer is lost (S21).
			writeError(w, &sim.Error{Status: 504, Code: "upstream_timeout", Message: "the bank didn't answer in time; look the transfer up by reference before retrying"})
		case created:
			writeJSON(w, 201, tr)
		default:
			writeJSON(w, 200, tr)
		}
	})

	v1("GET /v1/transfers/{id}", func(w http.ResponseWriter, r *http.Request) {
		tr, err := s.Transfer(r.PathValue("id"))
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, 200, tr)
	})

	v1("GET /v1/transfers", func(w http.ResponseWriter, r *http.Request) {
		ref := r.URL.Query().Get("reference")
		if ref == "" {
			writeError(w, &sim.Error{Status: 400, Code: "invalid_request", Message: "look a transfer up with ?reference=<your reference>"})
			return
		}
		tr, err := s.TransferByReference(ref)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, 200, tr)
	})

	v1("GET /v1/settlements", func(w http.ResponseWriter, r *http.Request) {
		date := r.URL.Query().Get("date")
		entries, err := s.Settlements(date)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"date": date, "entries": entries})
	})

	// --- control API ---

	mux.HandleFunc("GET /_sim/{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// The page talks only to this server, and loads nothing from anywhere else.
		w.Header().Set("Content-Security-Policy", "default-src 'none'; connect-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		w.Write(dashboard)
	})

	mux.HandleFunc("GET /_sim", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/_sim/", http.StatusFound)
	})

	mux.HandleFunc("GET /_sim/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, s.State())
	})

	mux.HandleFunc("GET /_sim/scenario", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, s.Scenario())
	})

	mux.HandleFunc("POST /_sim/scenario", func(w http.ResponseWriter, r *http.Request) {
		raw, ok := readBody(w, r)
		if !ok {
			return
		}
		sc, err := scenario.Parse(raw)
		if err != nil {
			writeError(w, &sim.Error{Status: 400, Code: "invalid_scenario", Message: err.Error()})
			return
		}
		s.Load(sc)
		writeJSON(w, 200, s.State())
	})

	mux.HandleFunc("POST /_sim/reset", func(w http.ResponseWriter, r *http.Request) {
		s.Reset()
		writeJSON(w, 200, s.State())
	})

	mux.HandleFunc("POST /_sim/clock/advance", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			By scenario.Duration `json:"by"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		if err := s.Advance(time.Duration(req.By)); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, 200, s.State())
	})

	mux.HandleFunc("GET /_sim/rules", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"rules": s.Rules()})
	})

	mux.HandleFunc("POST /_sim/rules", func(w http.ResponseWriter, r *http.Request) {
		var rule scenario.Rule
		if !readJSON(w, r, &rule) {
			return
		}
		added, err := s.AddRule(rule)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, 201, added)
	})

	mux.HandleFunc("DELETE /_sim/rules/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.RemoveRule(r.PathValue("id")); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(204)
	})

	mux.HandleFunc("GET /_sim/transfers", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"transfers": s.Transfers()})
	})

	mux.HandleFunc("GET /_sim/events", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"events": s.Events()})
	})

	mux.HandleFunc("GET /_sim/deliveries", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"deliveries": s.Deliveries()})
	})

	registerPaystack(mux, s)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, &sim.Error{Status: 404, Code: "not_found", Message: "no such endpoint; see spec/sim-v1.md"})
	})
	return mux
}

func authorised(s *sim.Sim, r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	want := s.Scenario().SecretKey
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeError(w, &sim.Error{Status: 400, Code: "invalid_request", Message: "the body is too large or couldn't be read"})
		return nil, false
	}
	return raw, true
}

// readJSON decodes the body strictly: an unknown field is an error, so a typo
// in a request is caught rather than silently ignored.
func readJSON(w http.ResponseWriter, r *http.Request, into any) bool {
	raw, ok := readBody(w, r)
	if !ok {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		writeError(w, &sim.Error{Status: 400, Code: "invalid_request", Message: "the body isn't valid JSON for this endpoint: " + err.Error()})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}

func writeError(w http.ResponseWriter, err *sim.Error) {
	writeJSON(w, err.Status, map[string]*sim.Error{"error": err})
}

// HTTPSender delivers webhooks over HTTP (S12), giving up after 5 seconds.
type HTTPSender struct{ Client *http.Client }

func (h HTTPSender) Send(url string, body []byte, headers map[string]string) (int, error) {
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("User-Agent", "naira-sim")
	res, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	return res.StatusCode, nil
}
