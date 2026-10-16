package main

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Temitayo-spec/naira-sim/internal/api"
	"github.com/Temitayo-spec/naira-sim/internal/scenario"
	"github.com/Temitayo-spec/naira-sim/internal/sim"
)

func server(t *testing.T) string {
	s := sim.New(scenario.Default(), api.HTTPSender{})
	t.Cleanup(s.Close)
	srv := httptest.NewServer(api.New(s))
	t.Cleanup(srv.Close)
	return srv.URL
}

func simctl(t *testing.T, base string, want int, args ...string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	if got := run(args, base, &out, &errOut); got != want {
		t.Fatalf("simctl %s exited %d, want %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), got, want, out.String(), errOut.String())
	}
	return out.String() + errOut.String()
}

func TestRuleAddWithFlags(t *testing.T) {
	base := server(t)
	out := simctl(t, base, 0, "rule", "add", "stuck_pending", "--bank", "990001", "--rate", "0.2", "--hold", "6h", "--outcome", "failed")
	for _, want := range []string{`"id": "rule_001"`, `"hold": "6h0m0s"`, `"outcome": "failed"`, `"rate": 0.2`, `"bank_code": "990001"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
	if !strings.Contains(simctl(t, base, 0, "rules"), "rule_001") {
		t.Error("rules should list it")
	}
	simctl(t, base, 0, "rule", "rm", "rule_001")
	if out := simctl(t, base, 1, "rule", "rm", "rule_001"); !strings.Contains(out, "rule_not_found") {
		t.Errorf("removing twice: %s", out)
	}
}

func TestBadRuleShowsTheReason(t *testing.T) {
	base := server(t)
	if out := simctl(t, base, 1, "rule", "add", "explode"); !strings.Contains(out, "unknown failure type") {
		t.Errorf("got %s", out)
	}
	if out := simctl(t, base, 1, "rule", "add", "stuck_pending", "--outcome", "maybe"); !strings.Contains(out, "outcome") {
		t.Errorf("got %s", out)
	}
}

func TestLoadAdvanceExport(t *testing.T) {
	base := server(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "sc.json")
	os.WriteFile(file, []byte(`{"balance": 5000, "rules": [{"type": "missing_webhook"}]}`), 0o644)
	if out := simctl(t, base, 0, "load", file); !strings.Contains(out, `"balance": 5000`) {
		t.Errorf("load: %s", out)
	}
	if out := simctl(t, base, 0, "advance", "2h"); !strings.Contains(out, `"now": "2026-01-01T11:00:00Z"`) {
		t.Errorf("advance: %s", out)
	}
	if out := simctl(t, base, 0, "export"); !strings.Contains(out, "missing_webhook") {
		t.Errorf("export should carry the rule: %s", out)
	}
	simctl(t, base, 0, "reset")
}

func TestUsageErrors(t *testing.T) {
	base := server(t)
	for _, args := range [][]string{{"launch"}, {"load"}, {"advance"}, {"rule"}, {"rule", "add"}, {"rule", "add", "stuck_pending", "extra"}, {"rule", "add", "stuck_pending", "--nope", "1"}} {
		simctl(t, base, 2, args...)
	}
	if out := simctl(t, "http://127.0.0.1:1", 1, "status"); !strings.Contains(out, "can't reach naira-sim") {
		t.Errorf("unreachable: %s", out)
	}
}
