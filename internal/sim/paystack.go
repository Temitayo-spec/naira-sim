package sim

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Paystack's shapes (spec section 8). naira-sim speaks Paystack's transfer API
// so a backend built on it needs only a new base URL; it is not Paystack, and
// these are Paystack's documented shapes, filled with simulated values.

// Recipient is a transfer recipient (S31). Paystack keeps one per account, so
// naira-sim does too; every transfer has one, however it was created.
type Recipient struct {
	ID            int64
	Code          string
	Name          string
	Description   string
	BankCode      string
	AccountNumber string
	AccountName   string // the name the bank gave, which may not be Name
	CreatedAt     time.Time
}

// paystackIntegration is the made-up business every object belongs to.
const paystackIntegration = 100001

// CreateRecipient makes a recipient, or returns the one for that account (S31).
// A new one is checked with a name enquiry, so name_enquiry_failure applies.
func (s *Sim) CreateRecipient(name, description, bankCode, accountNumber string) (Recipient, *Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.recipientAt[bankCode+"/"+accountNumber]; ok {
		return *r, nil
	}
	acct, err := s.nameEnquiryLocked(bankCode, accountNumber)
	if err != nil {
		return Recipient{}, err
	}
	r := s.newRecipientLocked(bankCode, accountNumber, s.clk.Now())
	r.Name, r.Description, r.AccountName = name, description, acct.Name
	return *r, nil
}

// recipientLocked is the account's recipient, made without a name enquiry if
// it has none: a transfer sent through /v1 still needs one to be shown.
func (s *Sim) recipientLocked(bankCode, accountNumber string, now time.Time) *Recipient {
	if r, ok := s.recipientAt[bankCode+"/"+accountNumber]; ok {
		return r
	}
	r := s.newRecipientLocked(bankCode, accountNumber, now)
	r.Name = s.accounts[bankCode+"/"+accountNumber].Name
	r.AccountName = r.Name
	return r
}

func (s *Sim) newRecipientLocked(bankCode, accountNumber string, now time.Time) *Recipient {
	n := int64(len(s.recipients) + 1)
	r := &Recipient{ID: n, Code: fmt.Sprintf("RCP_sim%09d", n), BankCode: bankCode, AccountNumber: accountNumber, CreatedAt: now}
	s.recipients = append(s.recipients, r)
	s.recipientAt[bankCode+"/"+accountNumber] = r
	return r
}

// Recipient finds a recipient by its ID or code.
func (s *Sim) Recipient(idOrCode string) (Recipient, *Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.findRecipientLocked(idOrCode); r != nil {
		return *r, nil
	}
	return Recipient{}, fail(404, "recipient_not_found", "no recipient %q", idOrCode)
}

func (s *Sim) findRecipientLocked(idOrCode string) *Recipient {
	n, ok := paystackNumber(idOrCode, "RCP_sim")
	if !ok || n < 1 || n > int64(len(s.recipients)) {
		return nil
	}
	return s.recipients[n-1]
}

// GenerateReference is a reference for a transfer sent without one, as
// Paystack makes one up: unique, and the same on a replay (S33).
func (s *Sim) GenerateReference() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.autoRefs++
	return fmt.Sprintf("sim_generated_%06d", s.autoRefs)
}

// PaystackTransferID turns a Paystack transfer ID or code into naira-sim's ID.
func PaystackTransferID(idOrCode string) (string, bool) {
	n, ok := paystackNumber(idOrCode, "TRF_sim")
	if !ok || n < 1 {
		return "", false
	}
	return fmt.Sprintf("trf_%06d", n), true
}

// paystackNumber reads "42" or prefix+"000000042".
func paystackNumber(v, prefix string) (int64, bool) {
	v = strings.TrimPrefix(v, prefix)
	n, err := strconv.ParseInt(v, 10, 64)
	return n, err == nil && v != "" && v[0] != '+' && v[0] != '-'
}

// PaystackTransfer is a transfer in Paystack's shape (S32).
type PaystackTransfer struct {
	Amount          int64           `json:"amount"`
	CreatedAt       string          `json:"createdAt"`
	Currency        string          `json:"currency"`
	Domain          string          `json:"domain"`
	Failures        any             `json:"failures"`
	ID              int64           `json:"id"`
	Integration     any             `json:"integration"`
	Reason          string          `json:"reason"`
	Reference       string          `json:"reference"`
	Source          string          `json:"source"`
	SourceDetails   any             `json:"source_details"`
	Status          string          `json:"status"`
	TitanCode       any             `json:"titan_code"`
	TransferCode    string          `json:"transfer_code"`
	TransferredAt   *string         `json:"transferred_at"`
	UpdatedAt       string          `json:"updatedAt"`
	Recipient       any             `json:"recipient"`
	Session         paystackSession `json:"session"`
	FeeCharged      int64           `json:"fee_charged"`
	FeesBreakdown   any             `json:"fees_breakdown"`
	GatewayResponse any             `json:"gateway_response"`
}

type paystackSession struct {
	Provider any `json:"provider"`
	ID       any `json:"id"`
}

// PaystackRecipient is a recipient in Paystack's shape (S31).
type PaystackRecipient struct {
	Active        bool                     `json:"active"`
	CreatedAt     string                   `json:"createdAt"`
	Currency      string                   `json:"currency"`
	Description   string                   `json:"description"`
	Domain        string                   `json:"domain"`
	Email         any                      `json:"email"`
	ID            int64                    `json:"id"`
	Integration   int                      `json:"integration"`
	Metadata      any                      `json:"metadata"`
	Name          string                   `json:"name"`
	RecipientCode string                   `json:"recipient_code"`
	Type          string                   `json:"type"`
	UpdatedAt     string                   `json:"updatedAt"`
	IsDeleted     bool                     `json:"is_deleted"`
	Details       PaystackRecipientDetails `json:"details"`
}

type PaystackRecipientDetails struct {
	AuthorizationCode any    `json:"authorization_code"`
	AccountNumber     string `json:"account_number"`
	AccountName       string `json:"account_name"`
	BankCode          string `json:"bank_code"`
	BankName          string `json:"bank_name"`
}

// PaystackEvent is a webhook body in Paystack's format (S34).
type PaystackEvent struct {
	Event string           `json:"event"`
	Data  PaystackTransfer `json:"data"`
}

// PaystackTransfer shows t as Paystack's initiate answers: the recipient and
// the integration as IDs.
func (s *Sim) PaystackTransfer(t Transfer) PaystackTransfer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paystackTransferLocked(t, false)
}

// paystackTransferLocked shows t in Paystack's shape; a webhook carries the
// recipient and the integration as objects.

func (s *Sim) paystackTransferLocked(t Transfer, webhook bool) PaystackTransfer {
	n, _ := strconv.ParseInt(strings.TrimPrefix(t.ID, "trf_"), 10, 64)
	r := s.recipientLocked(t.BankCode, t.AccountNumber, t.CreatedAt)
	out := PaystackTransfer{
		Amount:       t.Amount,
		CreatedAt:    paystackTime(t.CreatedAt),
		Currency:     t.Currency,
		Domain:       "test",
		ID:           n,
		Integration:  paystackIntegration,
		Reason:       t.Narration,
		Reference:    t.Reference,
		Source:       "balance",
		Status:       PaystackStatus(t.Status),
		TransferCode: fmt.Sprintf("TRF_sim%09d", n),
		UpdatedAt:    paystackTime(t.UpdatedAt),
		Recipient:    r.ID,
	}
	if tr, ok := s.byID[t.ID]; ok && !tr.settledAt.IsZero() && t.Status != Pending {
		at := paystackTime(tr.settledAt)
		out.TransferredAt = &at
	}
	if webhook {
		out.Integration = map[string]any{"id": paystackIntegration, "is_live": false, "business_name": "naira-sim"}
		out.Recipient = s.paystackRecipientLocked(*r)
	}
	return out
}

// PaystackTransferFull shows t as fetch, verify and list do: the recipient as
// an object, the integration as an ID.
func (s *Sim) PaystackTransferFull(t Transfer) PaystackTransfer {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.paystackTransferLocked(t, false)
	out.Recipient = s.paystackRecipientLocked(*s.recipientLocked(t.BankCode, t.AccountNumber, t.CreatedAt))
	return out
}

func (s *Sim) PaystackRecipient(r Recipient) PaystackRecipient {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paystackRecipientLocked(r)
}

func (s *Sim) paystackRecipientLocked(r Recipient) PaystackRecipient {
	return PaystackRecipient{
		Active:        true,
		CreatedAt:     paystackTime(r.CreatedAt),
		Currency:      "NGN",
		Description:   r.Description,
		Domain:        "test",
		ID:            r.ID,
		Integration:   paystackIntegration,
		Name:          r.Name,
		RecipientCode: r.Code,
		Type:          "nuban",
		UpdatedAt:     paystackTime(r.CreatedAt),
		Details: PaystackRecipientDetails{
			AccountNumber: r.AccountNumber,
			AccountName:   r.AccountName,
			BankCode:      r.BankCode,
			BankName:      s.banks[r.BankCode].Name,
		},
	}
}

// PaystackStatus is a transfer status as Paystack writes it.
func PaystackStatus(status string) string {
	if status == Successful {
		return "success"
	}
	return status
}

func paystackEventName(typ string) string {
	if typ == "transfer.successful" {
		return "transfer.success"
	}
	return typ
}

// paystackTime is Paystack's timestamp: UTC with milliseconds.
func paystackTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// SignPaystack is the X-Paystack-Signature value: hex HMAC-SHA512 of the body
// under the secret key, as Paystack signs (S34).
func SignPaystack(secretKey string, body []byte) string {
	mac := hmac.New(sha512.New, []byte(secretKey))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
