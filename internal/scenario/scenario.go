// Package scenario reads and checks a scenario file (spec section 5): the
// setup, the clock and the failure rules. A scenario that can't be read is
// refused with a plain message; nothing is guessed.
package scenario

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Scenario is a parsed, checked scenario with every default filled in.
type Scenario struct {
	Seed      uint64    `json:"seed"`
	Clock     string    `json:"clock"`
	Start     time.Time `json:"start"`
	Balance   int64     `json:"balance"`
	Latency   Duration  `json:"latency"`
	SecretKey string    `json:"secret_key"`
	Webhook   Webhook   `json:"webhook"`
	Banks     []Bank    `json:"banks"`
	Accounts  []Account `json:"accounts"`
	Rules     []Rule    `json:"rules"`
}

type Webhook struct {
	URL    string `json:"url"`
	Secret string `json:"secret"`
}

type Bank struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

type Account struct {
	BankCode      string `json:"bank_code"`
	AccountNumber string `json:"account_number"`
	Name          string `json:"name"`
}

// Rule is one failure, switched on for the transfers it matches (S16).
type Rule struct {
	ID     string  `json:"id,omitempty"`
	Type   string  `json:"type"`
	Match  Match   `json:"match"`
	Rate   float64 `json:"rate"`
	Params Params  `json:"params"`
}

type Match struct {
	BankCode        string `json:"bank_code,omitempty"`
	AccountNumber   string `json:"account_number,omitempty"`
	ReferencePrefix string `json:"reference_prefix,omitempty"`
}

// Params holds every failure's settings; each type reads only its own.
type Params struct {
	Hold    Duration `json:"hold,omitempty"`
	Outcome string   `json:"outcome,omitempty"`
	Delay   Duration `json:"delay,omitempty"`
	Times   int      `json:"times,omitempty"`
	Gap     Duration `json:"gap,omitempty"`
	Field   string   `json:"field,omitempty"`
	Delta   int64    `json:"delta,omitempty"`
	After   Duration `json:"after,omitempty"`
	Mode    string   `json:"mode,omitempty"`
}

// The nine failure types (S17).
const (
	StuckPending       = "stuck_pending"
	BankDelay          = "bank_delay"
	DuplicateWebhook   = "duplicate_webhook"
	SettlementMismatch = "settlement_mismatch"
	LateReversal       = "late_reversal"
	TimeoutThenSuccess = "timeout_then_success"
	MissingWebhook     = "missing_webhook"
	OutOfOrderWebhooks = "out_of_order_webhooks"
	NameEnquiryFailure = "name_enquiry_failure"
)

// Types lists every failure type, in the spec's order.
var Types = []string{StuckPending, BankDelay, DuplicateWebhook, SettlementMismatch, LateReversal,
	TimeoutThenSuccess, MissingWebhook, OutOfOrderWebhooks, NameEnquiryFailure}

var (
	bankCode      = regexp.MustCompile(`^\d{3,6}$`)
	accountNumber = regexp.MustCompile(`^\d{10}$`)
)

// DefaultStart is when a scenario's clock starts unless it says otherwise.
var DefaultStart = time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)

// DefaultBanks are made up: no bank in naira-sim is a real bank (S3).
var DefaultBanks = []Bank{
	{Code: "990001", Name: "Sandbox Bank One"},
	{Code: "990002", Name: "Sandbox Bank Two"},
	{Code: "990003", Name: "Sandbox Bank Three"},
	{Code: "990004", Name: "Sandbox Bank Four"},
	{Code: "990005", Name: "Sandbox Bank Five"},
}

var DefaultAccounts = []Account{
	{BankCode: "990001", AccountNumber: "0123456789", Name: "Ada Obi"},
	{BankCode: "990002", AccountNumber: "1234567890", Name: "Tunde Bakare"},
	{BankCode: "990003", AccountNumber: "2345678901", Name: "Ngozi Eze"},
}

// Parse reads a scenario, applies defaults (S24) and checks it.
func Parse(raw []byte) (Scenario, error) {
	sc := Scenario{}
	if len(bytes.TrimSpace(raw)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields() // a typo in a field name is an error, not a silent default
		if err := dec.Decode(&sc); err != nil {
			return Scenario{}, fmt.Errorf("scenario: %w", err)
		}
	}
	if err := sc.fill(); err != nil {
		return Scenario{}, err
	}
	return sc, nil
}

// Default is the scenario with nothing set.
func Default() Scenario {
	sc, _ := Parse(nil)
	return sc
}

func (sc *Scenario) fill() error {
	if sc.Seed == 0 {
		sc.Seed = 42
	}
	switch sc.Clock {
	case "":
		sc.Clock = "manual"
	case "manual", "real":
	default:
		return fmt.Errorf(`scenario: clock must be "manual" or "real", not %q`, sc.Clock)
	}
	if sc.Start.IsZero() {
		sc.Start = DefaultStart
	}
	sc.Start = sc.Start.UTC()
	if sc.Balance == 0 {
		sc.Balance = 100_000_000 // ₦1,000,000
	}
	if sc.Balance < 0 {
		return errors.New("scenario: balance can't be negative")
	}
	if sc.Latency == 0 {
		sc.Latency = Duration(2 * time.Second)
	}
	if sc.Latency < 0 {
		return errors.New("scenario: latency can't be negative")
	}
	if sc.SecretKey == "" {
		sc.SecretKey = "sk_test_sim"
	}
	if sc.Webhook.URL != "" && !strings.HasPrefix(sc.Webhook.URL, "http://") && !strings.HasPrefix(sc.Webhook.URL, "https://") {
		return fmt.Errorf("scenario: webhook.url must start with http:// or https://, not %q", sc.Webhook.URL)
	}
	if sc.Webhook.Secret == "" {
		sc.Webhook.Secret = "whsec_test"
	}
	if sc.Banks == nil {
		sc.Banks = append([]Bank(nil), DefaultBanks...)
	}
	if sc.Accounts == nil {
		sc.Accounts = append([]Account(nil), DefaultAccounts...)
	}
	known := map[string]bool{}
	for _, b := range sc.Banks {
		if !bankCode.MatchString(b.Code) || b.Name == "" {
			return fmt.Errorf("scenario: bank %q needs a code of 3 to 6 digits and a name", b.Code)
		}
		if known[b.Code] {
			return fmt.Errorf("scenario: bank %s is listed twice", b.Code)
		}
		known[b.Code] = true
	}
	seen := map[string]bool{}
	for _, a := range sc.Accounts {
		if !known[a.BankCode] {
			return fmt.Errorf("scenario: account %s is at bank %q, which isn't in the bank list", a.AccountNumber, a.BankCode)
		}
		if !accountNumber.MatchString(a.AccountNumber) || a.Name == "" {
			return fmt.Errorf("scenario: account %q needs a 10-digit number and a name", a.AccountNumber)
		}
		if seen[a.BankCode+"/"+a.AccountNumber] {
			return fmt.Errorf("scenario: account %s at %s is listed twice", a.AccountNumber, a.BankCode)
		}
		seen[a.BankCode+"/"+a.AccountNumber] = true
	}
	for i := range sc.Rules {
		if err := sc.Rules[i].Fill(); err != nil {
			return fmt.Errorf("scenario: rule %d: %w", i+1, err)
		}
	}
	return nil
}

// Fill applies a rule's defaults (S17) and checks it.
func (r *Rule) Fill() error {
	if r.Rate == 0 {
		r.Rate = 1
	}
	if r.Rate < 0 || r.Rate > 1 {
		return fmt.Errorf("rate must be from 0 to 1, not %v", r.Rate)
	}
	p := &r.Params
	switch r.Type {
	case StuckPending:
		if p.Hold == 0 {
			p.Hold = Duration(2 * time.Hour)
		}
		if p.Outcome == "" {
			p.Outcome = "successful"
		}
		if p.Outcome != "successful" && p.Outcome != "failed" {
			return fmt.Errorf(`stuck_pending's outcome must be "successful" or "failed", not %q`, p.Outcome)
		}
	case BankDelay:
		if p.Delay == 0 {
			p.Delay = Duration(30 * time.Minute)
		}
	case DuplicateWebhook:
		if p.Times == 0 {
			p.Times = 3
		}
		if p.Times < 2 || p.Times > 10 {
			return fmt.Errorf("duplicate_webhook's times must be from 2 to 10, not %d", p.Times)
		}
		if p.Gap == 0 {
			p.Gap = Duration(10 * time.Second)
		}
	case SettlementMismatch:
		if p.Field == "" {
			p.Field = "amount"
		}
		if p.Field != "amount" && p.Field != "status" {
			return fmt.Errorf(`settlement_mismatch's field must be "amount" or "status", not %q`, p.Field)
		}
		if p.Field == "amount" && p.Delta == 0 {
			p.Delta = -100
		}
	case LateReversal:
		if p.After == 0 {
			p.After = Duration(48 * time.Hour)
		}
	case NameEnquiryFailure:
		if p.Mode == "" {
			p.Mode = "error"
		}
		if p.Mode != "error" && p.Mode != "different_name" {
			return fmt.Errorf(`name_enquiry_failure's mode must be "error" or "different_name", not %q`, p.Mode)
		}
		if r.Match.ReferencePrefix != "" {
			return errors.New("name_enquiry_failure matches by bank_code and account_number only")
		}
	case TimeoutThenSuccess, MissingWebhook, OutOfOrderWebhooks:
	default:
		return fmt.Errorf("unknown failure type %q; use one of %s", r.Type, strings.Join(Types, ", "))
	}
	for _, d := range []Duration{p.Hold, p.Delay, p.Gap, p.After} {
		if d < 0 {
			return errors.New("durations can't be negative")
		}
	}
	return nil
}

// Duration is a time.Duration written as "2h", "30m" or "10s" in JSON.
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) MarshalJSON() ([]byte, error) {
	if d == 0 {
		return []byte(`""`), nil
	}
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New(`a duration is a string like "2h", "30m" or "10s"`)
	}
	if s == "" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf(`%q isn't a duration; write it like "48h", "30m" or "10s"`, s)
	}
	*d = Duration(v)
	return nil
}
