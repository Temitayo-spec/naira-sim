package scenario

import (
	"strings"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	sc := Default()
	if sc.Seed != 42 || sc.Clock != "manual" || sc.Balance != 100_000_000 || sc.Latency.D() != 2*time.Second ||
		sc.SecretKey != "sk_test_sim" || sc.Webhook.Secret != "whsec_test" || len(sc.Banks) != 5 || len(sc.Accounts) != 3 {
		t.Errorf("defaults wrong: %+v", sc)
	}
}

func TestSpecExampleParses(t *testing.T) {
	sc, err := Parse([]byte(`{
	  "seed": 42, "clock": "manual", "start": "2026-10-16T09:00:00Z", "balance": 100000000,
	  "latency": "2s", "secret_key": "sk_test_sim",
	  "webhook": { "url": "http://localhost:3000/webhooks", "secret": "whsec_test" },
	  "banks": [{ "code": "990001", "name": "Sandbox Bank One" }],
	  "accounts": [{ "bank_code": "990001", "account_number": "0123456789", "name": "Ada Obi" }],
	  "rules": [{ "type": "stuck_pending", "match": { "bank_code": "990001" }, "rate": 0.2, "params": { "hold": "6h" } }]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	r := sc.Rules[0]
	if r.Params.Hold.D() != 6*time.Hour || r.Params.Outcome != "successful" || r.Rate != 0.2 {
		t.Errorf("rule %+v", r)
	}
}

func TestRuleDefaults(t *testing.T) {
	for typ, check := range map[string]func(Rule) bool{
		StuckPending:       func(r Rule) bool { return r.Params.Hold.D() == 2*time.Hour && r.Params.Outcome == "successful" },
		BankDelay:          func(r Rule) bool { return r.Params.Delay.D() == 30*time.Minute },
		DuplicateWebhook:   func(r Rule) bool { return r.Params.Times == 3 && r.Params.Gap.D() == 10*time.Second },
		SettlementMismatch: func(r Rule) bool { return r.Params.Field == "amount" && r.Params.Delta == -100 },
		LateReversal:       func(r Rule) bool { return r.Params.After.D() == 48*time.Hour },
		NameEnquiryFailure: func(r Rule) bool { return r.Params.Mode == "error" },
		TimeoutThenSuccess: func(r Rule) bool { return r.Rate == 1 },
	} {
		r := Rule{Type: typ}
		if err := r.Fill(); err != nil || !check(r) {
			t.Errorf("%s: %v, %+v", typ, err, r)
		}
	}
}

func TestRefused(t *testing.T) {
	for name, raw := range map[string]string{
		"unknown field":             `{"sead": 1}`,
		"bad clock":                 `{"clock": "fast"}`,
		"negative balance":          `{"balance": -5}`,
		"bad duration":              `{"latency": "2 days"}`,
		"duration as number":        `{"latency": 2}`,
		"bad webhook url":           `{"webhook": {"url": "localhost:3000"}}`,
		"account at unknown bank":   `{"accounts": [{"bank_code": "123", "account_number": "0123456789", "name": "X"}]}`,
		"short account number":      `{"accounts": [{"bank_code": "990001", "account_number": "123", "name": "X"}]}`,
		"duplicate bank":            `{"banks": [{"code": "990001", "name": "A"}, {"code": "990001", "name": "B"}]}`,
		"unknown rule type":         `{"rules": [{"type": "explode"}]}`,
		"rate above 1":              `{"rules": [{"type": "missing_webhook", "rate": 1.5}]}`,
		"bad outcome":               `{"rules": [{"type": "stuck_pending", "params": {"outcome": "maybe"}}]}`,
		"one duplicate isn't a dup": `{"rules": [{"type": "duplicate_webhook", "params": {"times": 1}}]}`,
		"bad mismatch field":        `{"rules": [{"type": "settlement_mismatch", "params": {"field": "date"}}]}`,
		"name rule by reference":    `{"rules": [{"type": "name_enquiry_failure", "match": {"reference_prefix": "x"}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(raw)); err == nil {
				t.Errorf("accepted %s", raw)
			}
		})
	}
}

func TestUnknownTypeListsTheTypes(t *testing.T) {
	_, err := Parse([]byte(`{"rules": [{"type": "explode"}]}`))
	if err == nil || !strings.Contains(err.Error(), "stuck_pending") {
		t.Errorf("error should list the types: %v", err)
	}
}
