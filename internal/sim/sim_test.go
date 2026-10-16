package sim

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Temitayo-spec/naira-sim/internal/scenario"
)

// recorder is a webhook receiver: it records every send and answers status.
type recorder struct {
	mu     sync.Mutex
	sent   []sent
	status func(n int) (int, error) // n = 1 for the first send
}

type sent struct {
	body    []byte
	headers map[string]string
	event   Event
}

func (r *recorder) Send(_ string, body []byte, headers map[string]string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ev Event
	_ = json.Unmarshal(body, &ev)
	r.sent = append(r.sent, sent{body: body, headers: headers, event: ev})
	if r.status != nil {
		return r.status(len(r.sent))
	}
	return 200, nil
}

func (r *recorder) types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, s := range r.sent {
		out = append(out, s.event.Type)
	}
	return out
}

// newSim loads a scenario with a webhook URL and the given rules.
func newSim(t *testing.T, rules string) (*Sim, *recorder) {
	t.Helper()
	sc, err := scenario.Parse([]byte(`{"start":"2026-10-16T09:00:00Z","webhook":{"url":"http://receiver.test/hooks"},"rules":` + rules + `}`))
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	s := New(sc, rec)
	t.Cleanup(s.Close)
	return s, rec
}

func send(t *testing.T, s *Sim, ref, bank, acct string, amount int64) Transfer {
	t.Helper()
	tr, _, _, err := s.CreateTransfer(TransferRequest{Reference: ref, Amount: amount, Currency: "NGN", BankCode: bank, AccountNumber: acct})
	if err != nil {
		t.Fatalf("create %s: %v", ref, err)
	}
	return tr
}

func status(t *testing.T, s *Sim, id string) string {
	t.Helper()
	tr, err := s.Transfer(id)
	if err != nil {
		t.Fatal(err)
	}
	return tr.Status
}

func advance(t *testing.T, s *Sim, d time.Duration) {
	t.Helper()
	if err := s.Advance(d); err != nil {
		t.Fatal(err)
	}
}

// S6, S10, S12: the happy path.
func TestHappyPath(t *testing.T) {
	s, rec := newSim(t, `[]`)
	tr := send(t, s, "rent-1", "990001", "0123456789", 500_000)
	if tr.ID != "trf_000001" || tr.Status != Pending || tr.AccountName != "Ada Obi" {
		t.Fatalf("created %+v", tr)
	}
	if s.Balance() != 100_000_000-500_000 {
		t.Errorf("balance %d: the amount should be taken at creation", s.Balance())
	}
	advance(t, s, time.Second)
	if status(t, s, tr.ID) != Pending {
		t.Error("settled before the 2s latency")
	}
	advance(t, s, time.Second)
	if status(t, s, tr.ID) != Successful {
		t.Errorf("status %s after 2s", status(t, s, tr.ID))
	}
	if got := rec.types(); !reflect.DeepEqual(got, []string{"transfer.successful"}) {
		t.Errorf("webhooks %v", got)
	}
}

// S6: idempotency by reference.
func TestIdempotency(t *testing.T) {
	s, _ := newSim(t, `[]`)
	req := TransferRequest{Reference: "r1", Amount: 100, Currency: "NGN", BankCode: "990001", AccountNumber: "0123456789"}
	first, created, _, _ := s.CreateTransfer(req)
	again, createdAgain, _, err := s.CreateTransfer(req)
	if err != nil || !created || createdAgain || again.ID != first.ID {
		t.Fatalf("repeat: %+v %v %v", again, createdAgain, err)
	}
	if s.Balance() != 100_000_000-100 {
		t.Errorf("a repeat must not take the money twice: balance %d", s.Balance())
	}
	req.Amount = 200
	if _, _, _, err := s.CreateTransfer(req); err == nil || err.Code != "reference_reused" || err.Status != 409 {
		t.Errorf("changed body: %v", err)
	}
}

func TestRefusals(t *testing.T) {
	s, _ := newSim(t, `[]`)
	for name, tc := range map[string]struct {
		req  TransferRequest
		code string
	}{
		"bad reference": {TransferRequest{Reference: "has space", Amount: 1, Currency: "NGN", BankCode: "990001", AccountNumber: "0123456789"}, "invalid_request"},
		"zero amount":   {TransferRequest{Reference: "a", Amount: 0, Currency: "NGN", BankCode: "990001", AccountNumber: "0123456789"}, "invalid_request"},
		"dollars":       {TransferRequest{Reference: "a", Amount: 1, Currency: "USD", BankCode: "990001", AccountNumber: "0123456789"}, "invalid_request"},
		"short account": {TransferRequest{Reference: "a", Amount: 1, Currency: "NGN", BankCode: "990001", AccountNumber: "123"}, "invalid_request"},
		"unknown bank":  {TransferRequest{Reference: "a", Amount: 1, Currency: "NGN", BankCode: "123456", AccountNumber: "0123456789"}, "unknown_bank"},
		"too much":      {TransferRequest{Reference: "a", Amount: 200_000_000, Currency: "NGN", BankCode: "990001", AccountNumber: "0123456789"}, "insufficient_balance"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := s.CreateTransfer(tc.req); err == nil || err.Code != tc.code {
				t.Errorf("got %v, want %s", err, tc.code)
			}
		})
	}
	if len(s.Transfers()) != 0 {
		t.Error("a refused transfer was created")
	}
}

// S6: an unknown account is accepted, then fails, and the money comes back.
func TestUnknownAccountFails(t *testing.T) {
	s, rec := newSim(t, `[]`)
	tr := send(t, s, "x", "990004", "9999999999", 1000)
	advance(t, s, 2*time.Second)
	got, _ := s.Transfer(tr.ID)
	if got.Status != Failed || got.FailureReason == nil || *got.FailureReason != "account_not_found" {
		t.Errorf("transfer %+v", got)
	}
	if s.Balance() != 100_000_000 {
		t.Errorf("balance %d: a failed transfer gives the money back", s.Balance())
	}
	if got := rec.types(); !reflect.DeepEqual(got, []string{"transfer.failed"}) {
		t.Errorf("webhooks %v", got)
	}
}

// stuck_pending, both outcomes (S18).
func TestStuckPending(t *testing.T) {
	s, rec := newSim(t, `[{"type":"stuck_pending","match":{"reference_prefix":"ok-"},"params":{"hold":"6h"}},
	                      {"type":"stuck_pending","match":{"reference_prefix":"bad-"},"params":{"hold":"6h","outcome":"failed"}}]`)
	ok := send(t, s, "ok-1", "990001", "0123456789", 1000)
	bad := send(t, s, "bad-1", "990001", "0123456789", 1000)
	advance(t, s, 5*time.Hour)
	if status(t, s, ok.ID) != Pending || status(t, s, bad.ID) != Pending {
		t.Fatal("settled before the 6h hold")
	}
	advance(t, s, time.Hour)
	if status(t, s, ok.ID) != Successful {
		t.Error("ok-1 should succeed after the hold")
	}
	got, _ := s.Transfer(bad.ID)
	if got.Status != Failed || *got.FailureReason != "timed_out" {
		t.Errorf("bad-1: %+v", got)
	}
	if s.Balance() != 100_000_000-1000 {
		t.Errorf("balance %d", s.Balance())
	}
	if len(rec.types()) != 2 {
		t.Errorf("webhooks %v", rec.types())
	}
}

// bank_delay: only the matched bank is slow.
func TestBankDelay(t *testing.T) {
	s, _ := newSim(t, `[{"type":"bank_delay","match":{"bank_code":"990002"},"params":{"delay":"30m"}}]`)
	slow := send(t, s, "slow", "990002", "1234567890", 1000)
	fast := send(t, s, "fast", "990001", "0123456789", 1000)
	advance(t, s, 2*time.Second)
	if status(t, s, fast.ID) != Successful || status(t, s, slow.ID) != Pending {
		t.Fatal("only bank 990002 should be slow")
	}
	advance(t, s, 30*time.Minute)
	if status(t, s, slow.ID) != Successful {
		t.Error("the slow bank should settle after latency + 30m")
	}
}

// duplicate_webhook: copies share one event ID (S19).
func TestDuplicateWebhook(t *testing.T) {
	s, rec := newSim(t, `[{"type":"duplicate_webhook","params":{"times":3,"gap":"10s"}}]`)
	send(t, s, "dup", "990001", "0123456789", 1000)
	advance(t, s, 2*time.Second)
	if len(rec.sent) != 1 {
		t.Fatalf("%d sends before the gap, want 1", len(rec.sent))
	}
	advance(t, s, 20*time.Second)
	if len(rec.sent) != 3 {
		t.Fatalf("%d sends, want 3", len(rec.sent))
	}
	for _, x := range rec.sent {
		if x.event.ID != rec.sent[0].event.ID {
			t.Error("copies must share one event ID")
		}
	}
	if len(s.Events()) != 1 {
		t.Errorf("%d events; duplicates repeat deliveries, not events", len(s.Events()))
	}
}

// settlement_mismatch changes only the report (S20).
func TestSettlementMismatch(t *testing.T) {
	s, _ := newSim(t, `[{"type":"settlement_mismatch","match":{"reference_prefix":"amt-"},"params":{"delta":-100}},
	                      {"type":"settlement_mismatch","match":{"reference_prefix":"st-"},"params":{"field":"status"}}]`)
	a := send(t, s, "amt-1", "990001", "0123456789", 5000)
	b := send(t, s, "st-1", "990001", "0123456789", 5000)
	c := send(t, s, "plain", "990001", "0123456789", 5000)
	advance(t, s, 2*time.Second)
	entries, err := s.Settlements("2026-10-16")
	if err != nil || len(entries) != 3 {
		t.Fatalf("report %v %v", entries, err)
	}
	byID := map[string]SettlementEntry{}
	for _, e := range entries {
		byID[e.TransferID] = e
	}
	if byID[a.ID].Amount != 4900 || byID[b.ID].Status != Failed || byID[c.ID].Amount != 5000 || byID[c.ID].Status != Successful {
		t.Errorf("report %+v", byID)
	}
	if got, _ := s.Transfer(a.ID); got.Amount != 5000 || got.Status != Successful {
		t.Error("the transfer itself must stay true")
	}
	if other, _ := s.Settlements("2026-10-17"); len(other) != 0 {
		t.Error("another day's report should be empty")
	}
}

// late_reversal: success, then reversed, money back.
func TestLateReversal(t *testing.T) {
	s, rec := newSim(t, `[{"type":"late_reversal","params":{"after":"48h"}}]`)
	tr := send(t, s, "rev", "990001", "0123456789", 7000)
	advance(t, s, 2*time.Second)
	if status(t, s, tr.ID) != Successful || s.Balance() != 100_000_000-7000 {
		t.Fatal("should succeed first")
	}
	advance(t, s, 48*time.Hour)
	if status(t, s, tr.ID) != Reversed || s.Balance() != 100_000_000 {
		t.Errorf("status %s, balance %d", status(t, s, tr.ID), s.Balance())
	}
	if got := rec.types(); !reflect.DeepEqual(got, []string{"transfer.successful", "transfer.reversed"}) {
		t.Errorf("webhooks %v", got)
	}
}

// timeout_then_success: told it timed out, but it goes through (S21).
func TestTimeoutThenSuccess(t *testing.T) {
	s, _ := newSim(t, `[{"type":"timeout_then_success","match":{"reference_prefix":"slow-"}}]`)
	_, created, timedOut, err := s.CreateTransfer(TransferRequest{Reference: "slow-1", Amount: 1000, Currency: "NGN", BankCode: "990001", AccountNumber: "0123456789"})
	if err != nil || !created || !timedOut {
		t.Fatalf("created %v timedOut %v err %v", created, timedOut, err)
	}
	advance(t, s, 2*time.Second)
	got, lookupErr := s.TransferByReference("slow-1")
	if lookupErr != nil || got.Status != Successful {
		t.Errorf("lookup %+v %v: the transfer should have gone through", got, lookupErr)
	}
}

// missing_webhook: nothing sent, the lookup still tells the truth (S9).
func TestMissingWebhook(t *testing.T) {
	s, rec := newSim(t, `[{"type":"missing_webhook"}]`)
	tr := send(t, s, "quiet", "990001", "0123456789", 1000)
	advance(t, s, time.Hour)
	if len(rec.sent) != 0 {
		t.Errorf("%d webhooks sent", len(rec.sent))
	}
	if status(t, s, tr.ID) != Successful || len(s.Events()) != 1 {
		t.Error("the event is recorded and the lookup tells the truth")
	}
}

// out_of_order_webhooks: success first, then a pending dated earlier (S22).
func TestOutOfOrderWebhooks(t *testing.T) {
	s, rec := newSim(t, `[{"type":"out_of_order_webhooks"}]`)
	tr := send(t, s, "ooo", "990001", "0123456789", 1000)
	advance(t, s, 2*time.Second)
	if got := rec.types(); !reflect.DeepEqual(got, []string{"transfer.successful", "transfer.pending"}) {
		t.Fatalf("webhooks %v", got)
	}
	pending := rec.sent[1].event
	if !pending.CreatedAt.Equal(tr.CreatedAt) || pending.Data.Status != Pending || !pending.CreatedAt.Before(rec.sent[0].event.CreatedAt) {
		t.Errorf("pending event %+v", pending)
	}
	if status(t, s, tr.ID) != Successful {
		t.Error("the transfer stays successful")
	}
}

// name_enquiry_failure, both modes (S23).
func TestNameEnquiry(t *testing.T) {
	s, _ := newSim(t, `[{"type":"name_enquiry_failure","match":{"account_number":"1234567890"}},
	                      {"type":"name_enquiry_failure","match":{"account_number":"2345678901"},"params":{"mode":"different_name"}}]`)
	if a, err := s.NameEnquiry("990001", "0123456789"); err != nil || a.Name != "Ada Obi" {
		t.Errorf("plain: %+v %v", a, err)
	}
	if _, err := s.NameEnquiry("990002", "1234567890"); err == nil || err.Status != 502 {
		t.Errorf("error mode: %v", err)
	}
	if a, err := s.NameEnquiry("990003", "2345678901"); err != nil || a.Name == "Ngozi Eze" {
		t.Errorf("different_name: %+v %v", a, err)
	}
	if _, err := s.NameEnquiry("990001", "9999999999"); err == nil || err.Status != 404 {
		t.Errorf("unknown account: %v", err)
	}
}

// S13, S14: signed, and retried on failure with the same event ID.
func TestWebhookSignatureAndRetries(t *testing.T) {
	s, rec := newSim(t, `[]`)
	rec.status = func(n int) (int, error) {
		if n == 1 {
			return 0, errors.New("connection refused")
		}
		if n == 2 {
			return 500, nil
		}
		return 200, nil
	}
	send(t, s, "retry", "990001", "0123456789", 1000)
	advance(t, s, 2*time.Second) // attempt 1 fails
	advance(t, s, time.Minute)   // attempt 2 at +1m: 500
	advance(t, s, 5*time.Minute) // attempt 3 at +5m: 200
	advance(t, s, 3*time.Hour)   // nothing more
	ds := s.Deliveries()
	if len(ds) != 3 || ds[0].Error == "" || ds[1].Status != 500 || ds[2].Status != 200 || ds[2].Attempt != 3 {
		t.Fatalf("deliveries %+v", ds)
	}
	first := rec.sent[0]
	if first.headers["X-Sim-Signature"] != Sign("whsec_test", first.body) || first.headers["X-Sim-Event-Id"] != first.event.ID {
		t.Error("headers")
	}
	if rec.sent[2].event.ID != first.event.ID {
		t.Error("a retry keeps the event ID")
	}
}

func TestWebhooksGiveUpAfterFiveAttempts(t *testing.T) {
	s, _ := newSim(t, `[]`)
	s.sender.(*recorder).status = func(int) (int, error) { return 503, nil }
	send(t, s, "dead", "990001", "0123456789", 1000)
	advance(t, s, 24*time.Hour)
	if n := len(s.Deliveries()); n != 5 {
		t.Errorf("%d attempts, want 5", n)
	}
}

// S16: a partial rate picks the same transfers on every replay (S26).
func TestReplayIsIdentical(t *testing.T) {
	run := func() string {
		s, rec := newSim(t, `[{"type":"stuck_pending","rate":0.3,"params":{"hold":"1h","outcome":"failed"}},
		                       {"type":"duplicate_webhook","rate":0.5},
		                       {"type":"late_reversal","rate":0.2,"params":{"after":"3h"}}]`)
		for i := 0; i < 40; i++ {
			send(t, s, "r"+strings.Repeat("x", i%5)+string(rune('a'+i%26))+string(rune('a'+i/26)), "990001", "0123456789", 100)
		}
		advance(t, s, 6*time.Hour)
		out, _ := json.Marshal(struct {
			T []Transfer
			E []Event
			D []Delivery
			B int64
			S int
		}{s.Transfers(), s.Events(), s.Deliveries(), s.Balance(), len(rec.sent)})
		return string(out)
	}
	a, b := run(), run()
	if a != b {
		t.Fatal("the same scenario and requests gave different results")
	}
	if !strings.Contains(a, `"failed"`) || !strings.Contains(a, `"reversed"`) {
		t.Error("with these rates, some transfers should fail and some reverse")
	}
}

// S25, S29: loading resets everything, and a real clock can't be moved.
func TestLoadResetsAndRealClockRefusesAdvance(t *testing.T) {
	s, _ := newSim(t, `[]`)
	send(t, s, "a", "990001", "0123456789", 1000)
	s.Reset()
	if len(s.Transfers()) != 0 || s.Balance() != 100_000_000 {
		t.Error("reset should clear everything")
	}
	sc, _ := scenario.Parse([]byte(`{"clock":"real"}`))
	s.Load(sc)
	if err := s.Advance(time.Hour); err == nil || err.Code != "clock_not_manual" {
		t.Errorf("advance in real mode: %v", err)
	}
}

// Rules added later apply to new transfers only.
func TestRulesAddAndRemove(t *testing.T) {
	s, _ := newSim(t, `[]`)
	before := send(t, s, "before", "990001", "0123456789", 100)
	r, err := s.AddRule(scenario.Rule{Type: scenario.StuckPending})
	if err != nil || r.ID != "rule_001" {
		t.Fatalf("add %+v %v", r, err)
	}
	after := send(t, s, "after", "990001", "0123456789", 100)
	advance(t, s, time.Minute)
	if status(t, s, before.ID) != Successful || status(t, s, after.ID) != Pending {
		t.Error("a new rule applies only to new transfers")
	}
	if err := s.RemoveRule("rule_001"); err != nil || len(s.Rules()) != 0 {
		t.Errorf("remove: %v", err)
	}
	if _, err := s.AddRule(scenario.Rule{Type: "explode"}); err == nil || err.Code != "invalid_rule" {
		t.Errorf("bad rule: %v", err)
	}
}
