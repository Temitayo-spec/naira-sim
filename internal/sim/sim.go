// Package sim is the simulator itself: the wallet, transfers, the failure
// rules, events and webhook deliveries (spec sections 2 to 4).
//
// All state sits behind one lock. Everything timed is a task on the scenario's
// clock, and each task remembers which scenario it belongs to, so loading a new
// scenario can never be disturbed by a task left over from the old one.
package sim

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Temitayo-spec/naira-sim/internal/clock"
	"github.com/Temitayo-spec/naira-sim/internal/scenario"
)

// Transfer statuses (S8).
const (
	Pending    = "pending"
	Successful = "successful"
	Failed     = "failed"
	Reversed   = "reversed"
)

// Webhook retry delays after a failed attempt (S14): attempts 2 to 5.
var retryAfter = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour}

// Transfer is a transfer as the API shows it (S7).
type Transfer struct {
	ID            string    `json:"id"`
	Reference     string    `json:"reference"`
	Amount        int64     `json:"amount"`
	Currency      string    `json:"currency"`
	BankCode      string    `json:"bank_code"`
	AccountNumber string    `json:"account_number"`
	AccountName   string    `json:"account_name"`
	Narration     string    `json:"narration"`
	Status        string    `json:"status"`
	FailureReason *string   `json:"failure_reason"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`

	settledAt time.Time
	plan      map[string]scenario.Rule // the failures picked for it at creation (S16)
	request   TransferRequest
}

// TransferRequest is the body of POST /v1/transfers (S6).
type TransferRequest struct {
	Reference     string `json:"reference"`
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
	BankCode      string `json:"bank_code"`
	AccountNumber string `json:"account_number"`
	Narration     string `json:"narration"`
}

// Event is a webhook event (S12).
type Event struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
	Data      Transfer  `json:"data"`
}

// Delivery is one attempt at sending an event.
type Delivery struct {
	EventID string    `json:"event_id"`
	Attempt int       `json:"attempt"`
	Copy    int       `json:"copy"`
	At      time.Time `json:"at"`
	Status  int       `json:"status"`
	Error   string    `json:"error,omitempty"`
}

// SettlementEntry is one line of the settlement report (S11).
type SettlementEntry struct {
	TransferID string    `json:"transfer_id"`
	Reference  string    `json:"reference"`
	Amount     int64     `json:"amount"`
	Status     string    `json:"status"`
	SettledAt  time.Time `json:"settled_at"`
}

// Error is an API error: an HTTP status, a code and plain words.
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func fail(status int, code, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Sender delivers a webhook. The real one posts over HTTP; tests record calls.
type Sender interface {
	Send(url string, body []byte, headers map[string]string) (status int, err error)
}

// Sim is the simulator. Create it with New; it's safe for concurrent use.
type Sim struct {
	mu     sync.Mutex
	loaded scenario.Scenario // as loaded, for reset
	sc     scenario.Scenario // with the rules as they are now
	clk    *clock.Scheduler
	stop   context.CancelFunc
	gen    int
	rng    *rand.Rand
	sender Sender

	balance    int64
	banks      map[string]scenario.Bank
	accounts   map[string]scenario.Account
	transfers  []*Transfer
	byID       map[string]*Transfer
	byRef      map[string]*Transfer
	events     []Event
	deliveries []Delivery
	nextRule   int

	recipients  []*Recipient
	recipientAt map[string]*Recipient // by bank code + "/" + account number
	autoRefs    int
}

// New makes a simulator running sc, sending webhooks with sender.
func New(sc scenario.Scenario, sender Sender) *Sim {
	s := &Sim{sender: sender}
	s.Load(sc)
	return s
}

// Load resets everything and runs sc (S25).
func (s *Sim) Load(sc scenario.Scenario) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != nil {
		s.stop()
		s.stop = nil
	}
	s.gen++
	s.loaded = sc
	s.sc = sc
	s.sc.Rules = nil
	s.clk = clock.New(sc.Start)
	s.rng = rand.New(rand.NewPCG(sc.Seed, sc.Seed^0x9e3779b97f4a7c15))
	s.balance = sc.Balance
	s.banks = map[string]scenario.Bank{}
	for _, b := range sc.Banks {
		s.banks[b.Code] = b
	}
	s.accounts = map[string]scenario.Account{}
	for _, a := range sc.Accounts {
		s.accounts[a.BankCode+"/"+a.AccountNumber] = a
	}
	s.transfers, s.events, s.deliveries = nil, nil, nil
	s.byID, s.byRef = map[string]*Transfer{}, map[string]*Transfer{}
	s.recipients, s.recipientAt, s.autoRefs = nil, map[string]*Recipient{}, 0
	s.nextRule = 0
	for _, r := range sc.Rules {
		s.addRuleLocked(r)
	}
	if sc.Clock == "real" {
		ctx, cancel := context.WithCancel(context.Background())
		s.stop = cancel
		go s.clk.Follow(ctx, 50*time.Millisecond)
	}
}

// Reset reloads the scenario as it was first loaded.
func (s *Sim) Reset() {
	s.mu.Lock()
	sc := s.loaded
	s.mu.Unlock()
	s.Load(sc)
}

// Close stops a real-mode clock.
func (s *Sim) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != nil {
		s.stop()
		s.stop = nil
	}
}

// Scenario is the current scenario, with the rules as they are now: what
// `simctl export` saves.
func (s *Sim) Scenario() scenario.Scenario {
	s.mu.Lock()
	defer s.mu.Unlock()
	sc := s.sc
	sc.Rules = append([]scenario.Rule(nil), s.sc.Rules...)
	return sc
}

// Advance moves a manual clock forward and runs everything due (S29).
func (s *Sim) Advance(d time.Duration) *Error {
	s.mu.Lock()
	clk, mode := s.clk, s.sc.Clock
	s.mu.Unlock()
	if mode != "manual" {
		return fail(409, "clock_not_manual", "the clock follows real time in this scenario; load one with \"clock\": \"manual\" to move it")
	}
	if d < 0 {
		return fail(400, "invalid_request", "the clock can't go backwards")
	}
	clk.Advance(d) // outside the lock: tasks take it
	return nil
}

// Now is the simulator's clock.
func (s *Sim) Now() time.Time {
	s.mu.Lock()
	clk := s.clk
	s.mu.Unlock()
	return clk.Now()
}

// ---------------------------------------------------------------------------
// Banks, names and balance

func (s *Sim) Banks() []scenario.Bank {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]scenario.Bank(nil), s.sc.Banks...)
}

func (s *Sim) Balance() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.balance
}

var (
	referencePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	accountPattern   = regexp.MustCompile(`^\d{10}$`)
)

// NameEnquiry looks an account up (S4), applying name_enquiry_failure (S23).
func (s *Sim) NameEnquiry(bankCode, accountNumber string) (scenario.Account, *Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nameEnquiryLocked(bankCode, accountNumber)
}

func (s *Sim) nameEnquiryLocked(bankCode, accountNumber string) (scenario.Account, *Error) {
	if !accountPattern.MatchString(accountNumber) {
		return scenario.Account{}, fail(400, "invalid_request", "account_number must be 10 digits")
	}
	if _, ok := s.banks[bankCode]; !ok {
		return scenario.Account{}, fail(400, "unknown_bank", "there's no bank with code %q", bankCode)
	}
	acct, known := s.accounts[bankCode+"/"+accountNumber]
	for _, r := range s.sc.Rules {
		if r.Type != scenario.NameEnquiryFailure || !matches(r.Match, bankCode, accountNumber, "") || !s.picked(r.Rate) {
			continue
		}
		if r.Params.Mode == "error" {
			return scenario.Account{}, fail(502, "upstream_error", "the bank didn't answer the name enquiry")
		}
		if known {
			acct.Name = differentName(acct.Name)
		}
		break
	}
	if !known {
		return scenario.Account{}, fail(404, "account_not_found", "no account %s at bank %s", accountNumber, bankCode)
	}
	return acct, nil
}

// differentName is a plausible name that isn't the account holder's.
func differentName(name string) string {
	words := strings.Fields(name)
	if len(words) < 2 {
		return name + " Ventures"
	}
	for i, j := 0, len(words)-1; i < j; i, j = i+1, j-1 {
		words[i], words[j] = words[j], words[i]
	}
	return strings.Join(words, " ") + " Enterprises"
}

// ---------------------------------------------------------------------------
// Transfers

// CreateTransfer starts a transfer (S6). created is false for an idempotent
// repeat; timedOut is true when timeout_then_success applies (S21).
func (s *Sim) CreateTransfer(req TransferRequest) (t Transfer, created, timedOut bool, err *Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case !referencePattern.MatchString(req.Reference):
		return Transfer{}, false, false, fail(400, "invalid_request", "reference must be 1 to 64 letters, digits, - or _")
	case req.Amount <= 0:
		return Transfer{}, false, false, fail(400, "invalid_request", "amount must be a positive whole number of kobo")
	case req.Currency != "NGN":
		return Transfer{}, false, false, fail(400, "invalid_request", `currency must be "NGN"`)
	case !accountPattern.MatchString(req.AccountNumber):
		return Transfer{}, false, false, fail(400, "invalid_request", "account_number must be 10 digits")
	case len(req.Narration) > 100:
		return Transfer{}, false, false, fail(400, "invalid_request", "narration can be at most 100 characters")
	}
	if prev, ok := s.byRef[req.Reference]; ok {
		if prev.request != req {
			return Transfer{}, false, false, fail(409, "reference_reused", "reference %q was already used for a different transfer", req.Reference)
		}
		return *prev, false, false, nil
	}
	if _, ok := s.banks[req.BankCode]; !ok {
		return Transfer{}, false, false, fail(400, "unknown_bank", "there's no bank with code %q", req.BankCode)
	}
	if req.Amount > s.balance {
		return Transfer{}, false, false, fail(402, "insufficient_balance", "the wallet holds %d kobo; this transfer needs %d", s.balance, req.Amount)
	}

	now := s.clk.Now()
	s.recipientLocked(req.BankCode, req.AccountNumber, now)
	tr := &Transfer{
		ID:            fmt.Sprintf("trf_%06d", len(s.transfers)+1),
		Reference:     req.Reference,
		Amount:        req.Amount,
		Currency:      "NGN",
		BankCode:      req.BankCode,
		AccountNumber: req.AccountNumber,
		AccountName:   s.accounts[req.BankCode+"/"+req.AccountNumber].Name,
		Narration:     req.Narration,
		Status:        Pending,
		CreatedAt:     now,
		UpdatedAt:     now,
		plan:          s.pick(req),
		request:       req,
	}
	s.transfers = append(s.transfers, tr)
	s.byID[tr.ID] = tr
	s.byRef[tr.Reference] = tr
	s.balance -= tr.Amount

	wait := s.sc.Latency.D()
	if r, ok := tr.plan[scenario.StuckPending]; ok {
		wait = r.Params.Hold.D()
	}
	if r, ok := tr.plan[scenario.BankDelay]; ok {
		wait += r.Params.Delay.D()
	}
	id, gen := tr.ID, s.gen
	s.clk.At(now.Add(wait), func() { s.settle(id, gen) })

	_, timedOut = tr.plan[scenario.TimeoutThenSuccess]
	return *tr, true, timedOut, nil
}

// pick decides, once, which failures apply to a new transfer (S16, S17).
func (s *Sim) pick(req TransferRequest) map[string]scenario.Rule {
	plan := map[string]scenario.Rule{}
	for _, r := range s.sc.Rules {
		if r.Type == scenario.NameEnquiryFailure || !matches(r.Match, req.BankCode, req.AccountNumber, req.Reference) {
			continue
		}
		if !s.picked(r.Rate) {
			continue
		}
		if _, taken := plan[r.Type]; !taken {
			plan[r.Type] = r
		}
	}
	return plan
}

// picked draws from the seeded generator only for a partial rate, so a
// replay draws exactly the same numbers in the same order (S26).
func (s *Sim) picked(rate float64) bool {
	if rate >= 1 {
		return true
	}
	return s.rng.Float64() < rate
}

func matches(m scenario.Match, bankCode, accountNumber, reference string) bool {
	return (m.BankCode == "" || m.BankCode == bankCode) &&
		(m.AccountNumber == "" || m.AccountNumber == accountNumber) &&
		(m.ReferencePrefix == "" || strings.HasPrefix(reference, m.ReferencePrefix))
}

func (s *Sim) settle(id string, gen int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen != s.gen {
		return
	}
	tr := s.byID[id]
	if tr.Status != Pending {
		return
	}
	now := s.clk.Now()
	_, known := s.accounts[tr.BankCode+"/"+tr.AccountNumber]
	stuck, isStuck := tr.plan[scenario.StuckPending]
	switch {
	case !known:
		s.failLocked(tr, "account_not_found", now)
		s.emit(tr, "transfer.failed", now)
	case isStuck && stuck.Params.Outcome == "failed":
		s.failLocked(tr, "timed_out", now)
		s.emit(tr, "transfer.failed", now)
	default:
		tr.Status, tr.UpdatedAt, tr.settledAt = Successful, now, now
		s.emit(tr, "transfer.successful", now)
		if r, ok := tr.plan[scenario.LateReversal]; ok {
			s.clk.At(now.Add(r.Params.After.D()), func() { s.reverse(id, gen) })
		}
	}
	if _, ok := tr.plan[scenario.OutOfOrderWebhooks]; ok {
		// Dated when the transfer was created, sent after the final event (S22).
		snap := *tr
		snap.Status, snap.FailureReason, snap.UpdatedAt = Pending, nil, tr.CreatedAt
		s.emitSnapshot(tr, "transfer.pending", tr.CreatedAt, snap)
	}
}

func (s *Sim) failLocked(tr *Transfer, reason string, now time.Time) {
	tr.Status, tr.UpdatedAt = Failed, now
	tr.FailureReason = &reason
	s.balance += tr.Amount
}

func (s *Sim) reverse(id string, gen int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen != s.gen {
		return
	}
	tr := s.byID[id]
	if tr.Status != Successful {
		return
	}
	now := s.clk.Now()
	tr.Status, tr.UpdatedAt = Reversed, now
	s.balance += tr.Amount
	s.emit(tr, "transfer.reversed", now)
}

// ---------------------------------------------------------------------------
// Events and webhooks

func (s *Sim) emit(tr *Transfer, typ string, at time.Time) {
	s.emitSnapshot(tr, typ, at, *tr)
}

func (s *Sim) emitSnapshot(tr *Transfer, typ string, at time.Time, snap Transfer) {
	ev := Event{ID: fmt.Sprintf("evt_%06d", len(s.events)+1), Type: typ, CreatedAt: at, Data: snap}
	s.events = append(s.events, ev)
	if s.sc.Webhook.URL == "" {
		return // recorded, not sent (S24)
	}
	if _, ok := tr.plan[scenario.MissingWebhook]; ok {
		return
	}
	copies, gap := 1, time.Duration(0)
	if r, ok := tr.plan[scenario.DuplicateWebhook]; ok {
		copies, gap = r.Params.Times, r.Params.Gap.D()
	}
	index, gen, now := len(s.events)-1, s.gen, s.clk.Now()
	for c := 1; c <= copies; c++ {

		s.clk.At(now.Add(time.Duration(c-1)*gap), func() { s.deliver(index, c, 1, gen) })
	}
}

func (s *Sim) deliver(index, copy, attempt, gen int) {
	s.mu.Lock()
	if gen != s.gen {
		s.mu.Unlock()
		return
	}
	ev := s.events[index]
	url, sender, clk := s.sc.Webhook.URL, s.sender, s.clk
	at := clk.Now()
	body, headers := s.webhookLocked(ev)
	s.mu.Unlock()

	status, err := sender.Send(url, body, headers)

	s.mu.Lock()
	defer s.mu.Unlock()
	if gen != s.gen {
		return
	}
	d := Delivery{EventID: ev.ID, Attempt: attempt, Copy: copy, At: at, Status: status}
	if err != nil {
		d.Error = err.Error()
	}
	s.deliveries = append(s.deliveries, d)
	if (err != nil || status < 200 || status > 299) && attempt <= len(retryAfter) {
		next := attempt + 1
		clk.At(at.Add(retryAfter[attempt-1]), func() { s.deliver(index, copy, next, gen) })
	}
}

// webhookLocked is an event's body and headers in the scenario's format (S12, S34).
func (s *Sim) webhookLocked(ev Event) ([]byte, map[string]string) {
	if s.sc.Webhook.Format == "paystack" {
		body, _ := json.Marshal(PaystackEvent{Event: paystackEventName(ev.Type), Data: s.paystackTransferLocked(ev.Data, true)})
		return body, map[string]string{
			"Content-Type":         "application/json",
			"X-Paystack-Signature": SignPaystack(s.sc.SecretKey, body),
		}
	}
	body, _ := json.Marshal(ev) // an Event holds only strings, numbers and times
	return body, map[string]string{
		"Content-Type":    "application/json",
		"X-Sim-Signature": Sign(s.sc.Webhook.Secret, body),
		"X-Sim-Event-Id":  ev.ID,
	}
}

// Sign is the X-Sim-Signature value: hex HMAC-SHA256 of body under secret (S13).
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// ---------------------------------------------------------------------------
// Lookups

func (s *Sim) Transfer(id string) (Transfer, *Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tr, ok := s.byID[id]; ok {
		return *tr, nil
	}
	return Transfer{}, fail(404, "transfer_not_found", "no transfer %q", id)
}

func (s *Sim) TransferByReference(ref string) (Transfer, *Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tr, ok := s.byRef[ref]; ok {
		return *tr, nil
	}
	return Transfer{}, fail(404, "transfer_not_found", "no transfer with reference %q", ref)
}

func (s *Sim) Transfers() []Transfer {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Transfer, len(s.transfers))
	for i, tr := range s.transfers {
		out[i] = *tr
	}
	return out
}

func (s *Sim) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.events...)
}

func (s *Sim) Deliveries() []Delivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Delivery(nil), s.deliveries...)
}

// Settlements is the report for one day of the simulator's clock (S11, S20).
func (s *Sim) Settlements(date string) ([]SettlementEntry, *Error) {
	day, err := time.Parse("2006-01-02", date)
	if err != nil {
		return nil, fail(400, "invalid_request", "date must look like 2026-10-16")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := []SettlementEntry{}
	for _, tr := range s.transfers {
		if tr.settledAt.IsZero() || !sameDay(tr.settledAt, day) {
			continue
		}
		e := SettlementEntry{TransferID: tr.ID, Reference: tr.Reference, Amount: tr.Amount, Status: tr.Status, SettledAt: tr.settledAt}
		if r, ok := tr.plan[scenario.SettlementMismatch]; ok {
			if r.Params.Field == "amount" {
				e.Amount += r.Params.Delta
			} else if e.Status == Successful {
				e.Status = Failed
			} else {
				e.Status = Successful
			}
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func sameDay(t, day time.Time) bool {
	y1, m1, d1 := t.UTC().Date()
	y2, m2, d2 := day.Date()
	return y1 == y2 && m1 == m2 && d1 == d2
}

// ---------------------------------------------------------------------------
// Rules

// Rules are the failure rules in force, in the order they apply (S17).
func (s *Sim) Rules() []scenario.Rule {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]scenario.Rule(nil), s.sc.Rules...)
}

// AddRule checks a rule and puts it in force for transfers created from now on.
func (s *Sim) AddRule(r scenario.Rule) (scenario.Rule, *Error) {
	if err := r.Fill(); err != nil {
		return scenario.Rule{}, fail(400, "invalid_rule", "%s", err.Error())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := r.Allowed(s.sc.Webhook.Format); err != nil {
		return scenario.Rule{}, fail(400, "invalid_rule", "%s", err.Error())
	}
	return s.addRuleLocked(r), nil
}

func (s *Sim) addRuleLocked(r scenario.Rule) scenario.Rule {
	s.nextRule++
	r.ID = fmt.Sprintf("rule_%03d", s.nextRule)
	s.sc.Rules = append(s.sc.Rules, r)
	return r
}

// RemoveRule takes a rule out of force. Transfers already created keep their failures.
func (s *Sim) RemoveRule(id string) *Error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, r := range s.sc.Rules {
		if r.ID == id {
			s.sc.Rules = append(s.sc.Rules[:i:i], s.sc.Rules[i+1:]...)
			return nil
		}
	}
	return fail(404, "rule_not_found", "no rule %q", id)
}

// State is a summary for GET /_sim/state.
type State struct {
	Clock     string          `json:"clock"`
	Now       time.Time       `json:"now"`
	Balance   int64           `json:"balance"`
	Transfers int             `json:"transfers"`
	Events    int             `json:"events"`
	Scheduled int             `json:"scheduled"`
	Rules     []scenario.Rule `json:"rules"`
}

func (s *Sim) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return State{
		Clock: s.sc.Clock, Now: s.clk.Now(), Balance: s.balance,
		Transfers: len(s.transfers), Events: len(s.events), Scheduled: s.clk.Pending(),
		Rules: append([]scenario.Rule(nil), s.sc.Rules...),
	}
}
