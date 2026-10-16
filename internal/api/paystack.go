package api

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Temitayo-spec/naira-sim/internal/sim"
)

// Paystack's transfer API (spec section 8), at the root, so a Paystack client
// works with only its base URL changed. Answers use Paystack's envelope:
// {"status": true, "message", "data"} or {"status": false, "message"}.

// paystackReference is Paystack's rule for references: 16 to 50 lowercase
// letters, digits, - or _ (S33).
var paystackReference = regexp.MustCompile(`^[a-z0-9_-]{16,50}$`)

func registerPaystack(mux *http.ServeMux, s *sim.Sim) {
	ps := func(pattern string, h func(w http.ResponseWriter, r *http.Request)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			switch {
			case r.Header.Get("Authorization") == "":
				paystackFail(w, 401, "No Authorization header was found")
			case !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.Scenario().SecretKey)) != 1:
				paystackFail(w, 401, "Invalid key")
			default:
				h(w, r)
			}
		})
	}

	ps("GET /bank", func(w http.ResponseWriter, r *http.Request) {
		start := s.Scenario().Start
		banks := []map[string]any{}
		for i, b := range s.Banks() {
			banks = append(banks, map[string]any{
				"name": b.Name, "slug": slug(b.Name), "code": b.Code, "longcode": "", "gateway": nil,
				"pay_with_bank": false, "active": true, "is_deleted": false, "country": "Nigeria",
				"currency": "NGN", "type": "nuban", "id": i + 1,
				"createdAt": paystackTime(start), "updatedAt": paystackTime(start),
			})
		}
		paystackOK(w, "Banks retrieved", banks, map[string]any{"next": nil, "previous": nil, "perPage": len(banks)})
	})

	ps("GET /bank/resolve", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		acct, err := s.NameEnquiry(q.Get("bank_code"), q.Get("account_number"))
		if err != nil {
			// Paystack answers every failed resolve the same way, the bank's
			// silence included (S35).
			paystackFail(w, 422, "Could not resolve account name. Check parameters or try again.")
			return
		}
		paystackOK(w, "Account number resolved", map[string]string{"account_number": acct.AccountNumber, "account_name": acct.Name}, nil)
	})

	ps("GET /balance", func(w http.ResponseWriter, r *http.Request) {
		paystackOK(w, "Balances retrieved", []map[string]any{{"currency": "NGN", "balance": s.Balance()}}, nil)
	})

	ps("POST /transferrecipient", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Type          string `json:"type"`
			Name          string `json:"name"`
			AccountNumber string `json:"account_number"`
			BankCode      string `json:"bank_code"`
			Description   string `json:"description"`
			Currency      string `json:"currency"`
		}
		if !readPaystack(w, r, &req) {
			return
		}
		switch {
		case req.Type != "nuban":
			paystackFail(w, 400, `naira-sim simulates "nuban" recipients only`)
			return
		case strings.TrimSpace(req.Name) == "":
			paystackFail(w, 400, "Name is required")
			return
		case req.Currency != "" && req.Currency != "NGN":
			paystackFail(w, 400, "naira-sim simulates NGN only")
			return
		}
		rcp, err := s.CreateRecipient(req.Name, req.Description, req.BankCode, req.AccountNumber)
		if err != nil {
			paystackFail(w, 400, "Cannot resolve account")
			return
		}
		paystackOK(w, "Transfer recipient created successfully", s.PaystackRecipient(rcp), nil)
	})

	ps("GET /transferrecipient/{id_or_code}", func(w http.ResponseWriter, r *http.Request) {
		rcp, err := s.Recipient(r.PathValue("id_or_code"))
		if err != nil {
			paystackFail(w, 404, "Recipient not found")
			return
		}
		paystackOK(w, "Recipient retrieved", s.PaystackRecipient(rcp), nil)
	})

	ps("POST /transfer", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Source    string  `json:"source"`
			Amount    flexInt `json:"amount"`
			Recipient string  `json:"recipient"`
			Reference string  `json:"reference"`
			Reason    string  `json:"reason"`
			Currency  string  `json:"currency"`
		}
		if !readPaystack(w, r, &req) {
			return
		}
		switch {
		case req.Source != "balance":
			paystackFail(w, 400, `source must be "balance"`)
			return
		case req.Amount <= 0:
			paystackFail(w, 400, "Amount must be a positive whole number of kobo")
			return
		case req.Currency != "" && req.Currency != "NGN":
			paystackFail(w, 400, "naira-sim simulates NGN only")
			return
		case req.Reference != "" && !paystackReference.MatchString(req.Reference):
			paystackFail(w, 400, "Invalid reference: use 16 to 50 lowercase letters, digits, - or _")
			return
		}
		rcp, rerr := s.Recipient(req.Recipient)
		if req.Recipient == "" || rerr != nil {
			writeJSON(w, 400, map[string]any{
				"status": false, "message": "Recipient specified is invalid",
				"meta": map[string]string{"nextStep": "Provide a valid recipient. Ensure you are passing the recipient code in the recipient param"},
				"type": "validation_error", "code": "invalid_transfer_recipient",
			})
			return
		}
		ref := req.Reference
		if ref == "" {
			ref = s.GenerateReference() // as Paystack does; a retry then can't be matched (S33)
		}
		tr, _, timedOut, err := s.CreateTransfer(sim.TransferRequest{
			Reference: ref, Amount: int64(req.Amount), Currency: "NGN",
			BankCode: rcp.BankCode, AccountNumber: rcp.AccountNumber, Narration: req.Reason,
		})
		switch {
		case err != nil && err.Code == "insufficient_balance":
			paystackFail(w, 400, "Your balance is not enough to fulfil this request")
		case err != nil && err.Code == "reference_reused":
			paystackFail(w, 400, "Duplicate reference(s) found: "+ref)
		case err != nil:
			paystackFail(w, 400, err.Message)
		case timedOut:
			// The transfer exists and goes on; only the answer is lost (S21).
			paystackFail(w, 504, "Gateway timeout. The transfer may have been queued: verify it by reference before retrying")
		default:
			paystackOK(w, "Transfer has been queued", s.PaystackTransfer(tr), nil)
		}
	})

	ps("GET /transfer", func(w http.ResponseWriter, r *http.Request) {
		perPage, page := atoiOr(r.URL.Query().Get("perPage"), 50), atoiOr(r.URL.Query().Get("page"), 1)
		all := s.Transfers()
		data := []sim.PaystackTransfer{}
		skipped := (page - 1) * perPage
		for i := len(all) - 1 - skipped; i >= 0 && len(data) < perPage; i-- { // newest first
			data = append(data, s.PaystackTransferFull(all[i]))
		}
		paystackOK(w, "Transfers retrieved", data, map[string]any{
			"total": len(all), "skipped": skipped, "perPage": perPage, "page": page,
			"pageCount": (len(all) + perPage - 1) / perPage,
		})
	})

	ps("GET /transfer/{id_or_code}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := sim.PaystackTransferID(r.PathValue("id_or_code"))
		tr, err := s.Transfer(id)
		if !ok || err != nil {
			paystackFail(w, 404, "Transfer not found")
			return
		}
		paystackOK(w, "Transfer retrieved", s.PaystackTransferFull(tr), nil)
	})

	ps("GET /transfer/verify/{reference}", func(w http.ResponseWriter, r *http.Request) {
		tr, err := s.TransferByReference(r.PathValue("reference"))
		if err != nil {
			paystackFail(w, 404, "Transfer not found")
			return
		}
		paystackOK(w, "Transfer retrieved", s.PaystackTransferFull(tr), nil)
	})
}

// readPaystack decodes a body leniently: Paystack clients send fields naira-sim
// doesn't use (metadata, account_reference), and Paystack accepts them.
func readPaystack(w http.ResponseWriter, r *http.Request, into any) bool {
	raw, ok := readBody(w, r)
	if ok {
		if err := json.NewDecoder(bytes.NewReader(raw)).Decode(into); err == nil {
			return true
		}
	}
	paystackFail(w, 400, "The request body isn't valid JSON for this endpoint")
	return false
}

func paystackOK(w http.ResponseWriter, message string, data any, meta map[string]any) {
	body := map[string]any{"status": true, "message": message, "data": data}
	if meta != nil {
		body["meta"] = meta
	}
	writeJSON(w, 200, body)
}

func paystackFail(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"status": false, "message": message})
}

// flexInt is an amount sent as a JSON number or a string of digits.
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		*f = -1 // refused as "not a positive whole number" by the caller
		return nil
	}
	*f = flexInt(n)
	return nil
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n
	}
	return def
}

func slug(name string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), " ", "-")
}

// paystackTime is Paystack's timestamp: UTC with milliseconds.
func paystackTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }
