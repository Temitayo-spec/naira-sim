package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Temitayo-spec/naira-sim/internal/scenario"
	"github.com/Temitayo-spec/naira-sim/internal/sim"
)

// harness runs naira-sim and a webhook receiver over real HTTP.
type harness struct {
	t        *testing.T
	sim      *httptest.Server
	mu       sync.Mutex
	received []http.Header
	bodies   [][]byte
}

func newHarness(t *testing.T, rules string) *harness {
	h := &harness{t: t}
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.received = append(h.received, r.Header.Clone())
		h.bodies = append(h.bodies, body)
		h.mu.Unlock()
	}))
	t.Cleanup(receiver.Close)
	sc, err := scenario.Parse([]byte(`{"start":"2026-10-16T09:00:00Z","webhook":{"url":"` + receiver.URL + `"},"rules":` + rules + `}`))
	if err != nil {
		t.Fatal(err)
	}
	s := sim.New(sc, HTTPSender{})
	t.Cleanup(s.Close)
	h.sim = httptest.NewServer(New(s))
	t.Cleanup(h.sim.Close)
	return h
}

func (h *harness) do(method, path, body string, auth bool) (int, map[string]any) {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.sim.URL+path, bytes.NewBufferString(body))
	if auth {
		req.Header.Set("Authorization", "Bearer sk_test_sim")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

const transfer = `{"reference":"rent-1","amount":500000,"currency":"NGN","bank_code":"990001","account_number":"0123456789","narration":"Rent"}`

func TestEndToEnd(t *testing.T) {
	h := newHarness(t, `[]`)

	if code, _ := h.do("GET", "/v1/balance", "", false); code != 401 {
		t.Errorf("no key: %d, want 401", code)
	}
	if code, body := h.do("POST", "/v1/name-enquiry", `{"bank_code":"990001","account_number":"0123456789"}`, true); code != 200 || body["account_name"] != "Ada Obi" {
		t.Errorf("name enquiry %d %v", code, body)
	}
	code, body := h.do("POST", "/v1/transfers", transfer, true)
	if code != 201 || body["status"] != "pending" || body["id"] != "trf_000001" {
		t.Fatalf("create %d %v", code, body)
	}
	if code, _ := h.do("POST", "/v1/transfers", transfer, true); code != 200 {
		t.Errorf("same reference again: %d, want 200", code)
	}
	if code, body := h.do("POST", "/_sim/clock/advance", `{"by":"2s"}`, false); code != 200 || body["events"].(float64) != 1 {
		t.Fatalf("advance %d %v", code, body)
	}
	if _, body := h.do("GET", "/v1/transfers?reference=rent-1", "", true); body["status"] != "successful" {
		t.Errorf("lookup %v", body)
	}
	if _, body := h.do("GET", "/v1/transfers/trf_000001", "", true); body["status"] != "successful" {
		t.Errorf("lookup by id %v", body)
	}
	if _, body := h.do("GET", "/v1/balance", "", true); body["balance"].(float64) != 100_000_000-500_000 {
		t.Errorf("balance %v", body)
	}
	_, report := h.do("GET", "/v1/settlements?date=2026-10-16", "", true)
	if entries := report["entries"].([]any); len(entries) != 1 {
		t.Errorf("settlements %v", report)
	}

	// The webhook arrived, signed, before advance answered (S29).
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.received) != 1 {
		t.Fatalf("%d webhooks received", len(h.received))
	}
	if h.received[0].Get("X-Sim-Signature") != sim.Sign("whsec_test", h.bodies[0]) {
		t.Error("bad signature")
	}
}

func TestErrors(t *testing.T) {
	h := newHarness(t, `[]`)
	for name, tc := range map[string]struct {
		method, path, body string
		code               int
		errCode            string
	}{
		"unknown field":    {"POST", "/v1/transfers", `{"referense":"x"}`, 400, "invalid_request"},
		"not json":         {"POST", "/v1/transfers", `{`, 400, "invalid_request"},
		"unknown transfer": {"GET", "/v1/transfers/trf_999999", "", 404, "transfer_not_found"},
		"no reference":     {"GET", "/v1/transfers", "", 400, "invalid_request"},
		"bad date":         {"GET", "/v1/settlements?date=yesterday", "", 400, "invalid_request"},
		"unknown endpoint": {"GET", "/v2/anything", "", 404, "not_found"},
	} {
		t.Run(name, func(t *testing.T) {
			code, body := h.do(tc.method, tc.path, tc.body, true)
			e, _ := body["error"].(map[string]any)
			if code != tc.code || e["code"] != tc.errCode {
				t.Errorf("got %d %v, want %d %s", code, body, tc.code, tc.errCode)
			}
		})
	}
}

// S21 over HTTP: a 504, then the lookup shows the transfer.
func TestTimeoutThenSuccessOverHTTP(t *testing.T) {
	h := newHarness(t, `[{"type":"timeout_then_success"}]`)
	code, body := h.do("POST", "/v1/transfers", transfer, true)
	if e, _ := body["error"].(map[string]any); code != 504 || e["code"] != "upstream_timeout" {
		t.Fatalf("create %d %v", code, body)
	}
	if code, body := h.do("POST", "/v1/transfers", transfer, true); code != 200 || body["id"] != "trf_000001" {
		t.Errorf("a retry with the same reference must find the transfer: %d %v", code, body)
	}
}

func TestControlAPI(t *testing.T) {
	h := newHarness(t, `[]`)
	if code, rule := h.do("POST", "/_sim/rules", `{"type":"missing_webhook","match":{"bank_code":"990001"}}`, false); code != 201 || rule["id"] != "rule_001" {
		t.Fatalf("add rule %d %v", code, rule)
	}
	if code, _ := h.do("POST", "/_sim/rules", `{"type":"explode"}`, false); code != 400 {
		t.Errorf("bad rule: %d", code)
	}
	if _, body := h.do("GET", "/_sim/rules", "", false); len(body["rules"].([]any)) != 1 {
		t.Errorf("rules %v", body)
	}
	if _, sc := h.do("GET", "/_sim/scenario", "", false); len(sc["rules"].([]any)) != 1 {
		t.Errorf("the exported scenario should carry the rule: %v", sc)
	}
	if code, _ := h.do("DELETE", "/_sim/rules/rule_001", "", false); code != 204 {
		t.Errorf("delete: %d", code)
	}
	if code, body := h.do("POST", "/_sim/scenario", `{"clock":"real","balance":5000}`, false); code != 200 || body["clock"] != "real" || body["balance"].(float64) != 5000 {
		t.Errorf("load %d %v", code, body)
	}
	if code, body := h.do("POST", "/_sim/clock/advance", `{"by":"1h"}`, false); code != 409 {
		t.Errorf("advance in real mode: %d %v", code, body)
	}
	if code, body := h.do("POST", "/_sim/scenario", `{"clock":"slow"}`, false); code != 400 {
		t.Errorf("bad scenario: %d %v", code, body)
	}
}

func TestDashboard(t *testing.T) {
	h := newHarness(t, `[]`)
	res, err := http.Get(h.sim.URL + "/_sim/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") || !bytes.Contains(body, []byte("This is a simulator")) {
		t.Errorf("dashboard %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	if !strings.Contains(res.Header.Get("Content-Security-Policy"), "connect-src 'self'") {
		t.Error("the dashboard should only talk to this server")
	}
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if r2, _ := noFollow.Get(h.sim.URL + "/_sim"); r2.StatusCode != 302 || r2.Header.Get("Location") != "/_sim/" {
		t.Errorf("/_sim should redirect to /_sim/: %d", r2.StatusCode)
	}
}
