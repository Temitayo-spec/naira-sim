package api

import (
	"encoding/json"
	"testing"

	"github.com/Temitayo-spec/naira-sim/internal/sim"
)

// psTransfer sends 5,000 naira to Ada Obi's recipient; the reference meets
// Paystack's rule (16 to 50 lowercase letters, digits, - or _).
func psTransfer(ref string) string {
	return `{"source":"balance","amount":500000,"recipient":"RCP_sim000000001","reference":"` + ref + `","reason":"Rent"}`
}

func (h *harness) adaRecipient() map[string]any {
	h.t.Helper()
	code, body := h.do("POST", "/transferrecipient", `{"type":"nuban","name":"Ada Obi","account_number":"0123456789","bank_code":"990001","currency":"NGN","metadata":{"job":"tenant"}}`, true)
	if code != 200 || body["status"] != true {
		h.t.Fatalf("create recipient %d %v", code, body)
	}
	return body["data"].(map[string]any)
}

func TestPaystackEndToEnd(t *testing.T) {
	h := newHarnessFormat(t, "paystack", `[]`)

	if code, body := h.do("GET", "/balance", "", false); code != 401 || body["status"] != false || body["message"] != "No Authorization header was found" {
		t.Errorf("no key: %d %v", code, body)
	}
	if _, body := h.do("GET", "/bank", "", true); len(body["data"].([]any)) != 5 {
		t.Errorf("banks %v", body)
	}
	if code, body := h.do("GET", "/bank/resolve?account_number=0123456789&bank_code=990001", "", true); code != 200 || body["data"].(map[string]any)["account_name"] != "Ada Obi" {
		t.Errorf("resolve %d %v", code, body)
	}
	if code, body := h.do("GET", "/bank/resolve?account_number=0000000000&bank_code=990001", "", true); code != 422 || body["status"] != false {
		t.Errorf("resolve an unknown account: %d %v", code, body)
	}

	rcp := h.adaRecipient()
	if rcp["recipient_code"] != "RCP_sim000000001" || rcp["details"].(map[string]any)["bank_name"] != "Sandbox Bank One" {
		t.Errorf("recipient %v", rcp)
	}
	if again := h.adaRecipient(); again["recipient_code"] != "RCP_sim000000001" {
		t.Errorf("the same account should give the same recipient, as Paystack does: %v", again)
	}

	code, body := h.do("POST", "/transfer", psTransfer("rent-october-0001"), true)
	data, _ := body["data"].(map[string]any)
	if code != 200 || body["message"] != "Transfer has been queued" || data["status"] != "pending" || data["transfer_code"] != "TRF_sim000000001" || data["recipient"].(float64) != 1 {
		t.Fatalf("initiate %d %v", code, body)
	}
	if code, body := h.do("POST", "/transfer", psTransfer("rent-october-0001"), true); code != 200 || body["data"].(map[string]any)["id"].(float64) != 1 {
		t.Errorf("a retry with the same reference must not pay twice: %d %v", code, body)
	}

	h.do("POST", "/_sim/clock/advance", `{"by":"2s"}`, false)

	for _, path := range []string{"/transfer/verify/rent-october-0001", "/transfer/1", "/transfer/TRF_sim000000001"} {
		code, body := h.do("GET", path, "", true)
		data, _ := body["data"].(map[string]any)
		if code != 200 || data["status"] != "success" || data["transferred_at"] == nil || data["recipient"].(map[string]any)["recipient_code"] != "RCP_sim000000001" {
			t.Errorf("%s: %d %v", path, code, body)
		}
	}
	if _, body := h.do("GET", "/transfer", "", true); len(body["data"].([]any)) != 1 || body["meta"].(map[string]any)["total"].(float64) != 1 {
		t.Errorf("list %v", body)
	}
	if _, body := h.do("GET", "/balance", "", true); body["data"].([]any)[0].(map[string]any)["balance"].(float64) != 100_000_000-500_000 {
		t.Errorf("balance %v", body)
	}

	// The webhook is Paystack's: its event name, its signature, no sim headers (S34).
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.received) != 1 {
		t.Fatalf("%d webhooks received", len(h.received))
	}
	if got := h.received[0].Get("X-Paystack-Signature"); got != sim.SignPaystack("sk_test_sim", h.bodies[0]) {
		t.Errorf("signature %q", got)
	}
	if h.received[0].Get("X-Sim-Signature") != "" || h.received[0].Get("X-Sim-Event-Id") != "" {
		t.Error("a Paystack-format webhook carries no naira-sim headers")
	}
	var ev struct {
		Event string `json:"event"`
		Data  struct {
			Status      string         `json:"status"`
			Reference   string         `json:"reference"`
			Integration map[string]any `json:"integration"`
			Recipient   map[string]any `json:"recipient"`
		} `json:"data"`
	}
	if err := json.Unmarshal(h.bodies[0], &ev); err != nil || ev.Event != "transfer.success" || ev.Data.Status != "success" ||
		ev.Data.Reference != "rent-october-0001" || ev.Data.Integration["business_name"] != "naira-sim" || ev.Data.Recipient["name"] != "Ada Obi" {
		t.Errorf("webhook %s", h.bodies[0])
	}
}

func TestPaystackErrors(t *testing.T) {
	h := newHarnessFormat(t, "paystack", `[]`)
	h.adaRecipient()
	h.do("POST", "/transfer", psTransfer("rent-october-0001"), true)

	for name, tc := range map[string]struct {
		method, path, body string
		code               int
		message            string
	}{
		"wrong key":           {"GET", "/balance", "", 401, "Invalid key"},
		"short reference":     {"POST", "/transfer", psTransfer("rent-1"), 400, "Invalid reference: use 16 to 50 lowercase letters, digits, - or _"},
		"uppercase reference": {"POST", "/transfer", psTransfer("RENT-OCTOBER-0002"), 400, "Invalid reference: use 16 to 50 lowercase letters, digits, - or _"},
		"unknown recipient":   {"POST", "/transfer", `{"source":"balance","amount":100,"recipient":"RCP_nope","reference":"rent-october-0003"}`, 400, "Recipient specified is invalid"},
		"no source":           {"POST", "/transfer", `{"amount":100,"recipient":"RCP_sim000000001","reference":"rent-october-0004"}`, 400, `source must be "balance"`},
		"too much":            {"POST", "/transfer", `{"source":"balance","amount":999999999999,"recipient":"RCP_sim000000001","reference":"rent-october-0005"}`, 400, "Your balance is not enough to fulfil this request"},
		"reference reused":    {"POST", "/transfer", `{"source":"balance","amount":1,"recipient":"RCP_sim000000001","reference":"rent-october-0001"}`, 400, "Duplicate reference(s) found: rent-october-0001"},
		"unknown transfer":    {"GET", "/transfer/verify/never-sent-this-one", "", 404, "Transfer not found"},
		"unknown recipient 2": {"GET", "/transferrecipient/RCP_sim000000099", "", 404, "Recipient not found"},
		"unresolvable":        {"POST", "/transferrecipient", `{"type":"nuban","name":"Nobody","account_number":"0000000000","bank_code":"990001"}`, 400, "Cannot resolve account"},
		"not json":            {"POST", "/transfer", `{`, 400, "The request body isn't valid JSON for this endpoint"},
	} {
		t.Run(name, func(t *testing.T) {
			key := "sk_test_sim"
			if name == "wrong key" {
				key = "sk_test_wrong"
			}
			code, body := h.doAs(tc.method, tc.path, tc.body, key)
			if code != tc.code || body["status"] != false || body["message"] != tc.message {
				t.Errorf("got %d %v, want %d %q", code, body, tc.code, tc.message)
			}
		})
	}

	// An amount sent as a string is read, and a missing reference is made up.
	code, body := h.do("POST", "/transfer", `{"source":"balance","amount":"25000","recipient":"RCP_sim000000001"}`, true)
	if data, _ := body["data"].(map[string]any); code != 200 || data["amount"].(float64) != 25000 || data["reference"] != "sim_generated_000001" {
		t.Errorf("string amount, no reference: %d %v", code, body)
	}
}

// S21 in Paystack's shape: a 504, then verify finds the transfer.
func TestPaystackTimeoutThenVerify(t *testing.T) {
	h := newHarnessFormat(t, "paystack", `[{"type":"timeout_then_success"}]`)
	h.adaRecipient()
	if code, body := h.do("POST", "/transfer", psTransfer("rent-october-0001"), true); code != 504 || body["status"] != false {
		t.Fatalf("initiate %d %v", code, body)
	}
	if code, body := h.do("GET", "/transfer/verify/rent-october-0001", "", true); code != 200 || body["data"].(map[string]any)["status"] != "pending" {
		t.Errorf("verify must find the transfer the 504 hid: %d %v", code, body)
	}
}

func TestPaystackReversalAndWrongName(t *testing.T) {
	h := newHarnessFormat(t, "paystack", `[{"type":"late_reversal"},{"type":"name_enquiry_failure","params":{"mode":"different_name"}}]`)
	rcp := h.adaRecipient()
	if got := rcp["details"].(map[string]any)["account_name"]; got == "Ada Obi" || rcp["name"] != "Ada Obi" {
		t.Errorf("the bank's name should differ from the name given: %v", rcp)
	}
	h.do("POST", "/transfer", psTransfer("rent-october-0001"), true)
	h.do("POST", "/_sim/clock/advance", `{"by":"49h"}`, false)

	h.mu.Lock()
	defer h.mu.Unlock()
	var events []string
	for _, b := range h.bodies {
		var ev struct {
			Event string `json:"event"`
			Data  struct {
				Status string `json:"status"`
			} `json:"data"`
		}
		_ = json.Unmarshal(b, &ev)
		events = append(events, ev.Event+"/"+ev.Data.Status)
	}
	if len(events) != 2 || events[0] != "transfer.success/success" || events[1] != "transfer.reversed/reversed" {
		t.Errorf("events %v", events)
	}
}

func TestPaystackRefusesOutOfOrder(t *testing.T) {
	h := newHarnessFormat(t, "paystack", `[]`)
	if code, body := h.do("POST", "/_sim/rules", `{"type":"out_of_order_webhooks"}`, false); code != 400 {
		t.Errorf("Paystack never sends a pending event, so the rule can't run: %d %v", code, body)
	}
	if code, _ := h.do("POST", "/_sim/scenario", `{"webhook":{"format":"paystack"},"rules":[{"type":"out_of_order_webhooks"}]}`, false); code != 400 {
		t.Errorf("a scenario with both: %d", code)
	}
	if code, _ := h.do("POST", "/_sim/scenario", `{"webhook":{"format":"stripe"}}`, false); code != 400 {
		t.Errorf("an unknown format: %d", code)
	}
}
