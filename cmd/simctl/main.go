// Command simctl controls a running naira-sim through its control API.
// Run `simctl help` for the commands. SIM_URL says where naira-sim is
// (default http://localhost:7070).
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

const usage = `Usage: simctl <command>

  status                      clock, balance, counts and rules
  load <scenario.json>        load a scenario, resetting everything
  reset                       reload the current scenario
  advance <duration>          move a manual clock, e.g. 2h, 30m, 10s
  export                      print the scenario with today's rules, to replay later
  rules                       list the failure rules
  rule add <type> [flags]     switch a failure on (types below)
  rule rm <id>                switch one off
  transfers | events | deliveries

rule add flags:
  --bank <code> --account <number> --prefix <reference prefix> --rate <0-1>
  --hold 6h --outcome failed --delay 30m --times 3 --gap 10s
  --field amount|status --delta -100 --after 48h --mode error|different_name

types: stuck_pending, bank_delay, duplicate_webhook, settlement_mismatch,
  late_reversal, timeout_then_success, missing_webhook,
  out_of_order_webhooks, name_enquiry_failure

SIM_URL says where naira-sim is (default http://localhost:7070).`

func main() {
	os.Exit(run(os.Args[1:], env("SIM_URL", "http://localhost:7070"), os.Stdout, os.Stderr))
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// run carries out one command and returns the exit code: 0 done, 1 failed, 2 bad usage.
func run(args []string, base string, stdout, stderr io.Writer) int {
	c := client{base: strings.TrimRight(base, "/"), out: stdout, err: stderr}
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(stdout, usage)
		return map[bool]int{true: 2, false: 0}[len(args) == 0]
	}
	switch args[0] {
	case "status":
		return c.show("GET", "/_sim/state", nil)
	case "load":
		if len(args) != 2 {
			return c.usage("load needs a scenario file")
		}
		raw, err := os.ReadFile(args[1])
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return c.show("POST", "/_sim/scenario", raw)
	case "reset":
		return c.show("POST", "/_sim/reset", nil)
	case "advance":
		if len(args) != 2 {
			return c.usage("advance needs a duration, like 2h")
		}
		body, _ := json.Marshal(map[string]string{"by": args[1]})
		return c.show("POST", "/_sim/clock/advance", body)
	case "export":
		return c.show("GET", "/_sim/scenario", nil)
	case "rules", "transfers", "events", "deliveries":
		return c.show("GET", "/_sim/"+args[0], nil)
	case "rule":
		if len(args) < 2 {
			return c.usage("rule needs add or rm")
		}
		switch args[1] {
		case "add":
			return c.addRule(args[2:])
		case "rm":
			if len(args) != 3 {
				return c.usage("rule rm needs a rule id, like rule_001")
			}
			return c.show("DELETE", "/_sim/rules/"+args[2], nil)
		}
		return c.usage("rule needs add or rm")
	}
	return c.usage(fmt.Sprintf("unknown command %q", args[0]))
}

func (c client) addRule(args []string) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return c.usage("rule add needs a failure type first")
	}
	typ := args[0]
	fs := flag.NewFlagSet("rule add", flag.ContinueOnError)
	fs.SetOutput(c.err)
	bank := fs.String("bank", "", "")
	account := fs.String("account", "", "")
	prefix := fs.String("prefix", "", "")
	rate := fs.Float64("rate", 0, "")
	params := map[string]*string{}
	for _, name := range []string{"hold", "outcome", "delay", "gap", "field", "after", "mode"} {
		params[name] = fs.String(name, "", "")
	}
	times := fs.Int("times", 0, "")
	delta := fs.Int64("delta", 0, "")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		return c.usage(fmt.Sprintf("unexpected %q", strings.Join(fs.Args(), " ")))
	}
	p := map[string]any{}
	for name, v := range params {
		if *v != "" {
			p[name] = *v
		}
	}
	if *times != 0 {
		p["times"] = *times
	}
	if *delta != 0 {
		p["delta"] = *delta
	}
	rule := map[string]any{
		"type":   typ,
		"match":  map[string]string{"bank_code": *bank, "account_number": *account, "reference_prefix": *prefix},
		"params": p,
	}
	if *rate != 0 {
		rule["rate"] = *rate
	}
	body, _ := json.Marshal(rule)
	return c.show("POST", "/_sim/rules", body)
}

type client struct {
	base     string
	out, err io.Writer
}

func (c client) usage(problem string) int {
	fmt.Fprintf(c.err, "%s\n\n%s\n", problem, usage)
	return 2
}

// show sends one request and prints the answer: JSON on success, the
// simulator's own error message otherwise.
func (c client) show(method, path string, body []byte) int {
	req, err := http.NewRequest(method, c.base+path, bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(c.err, err)
		return 1
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(c.err, "can't reach naira-sim at %s: %v\n", c.base, err)
		return 1
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		var e struct {
			Error struct{ Code, Message string }
		}
		if json.Unmarshal(raw, &e) == nil && e.Error.Code != "" {
			fmt.Fprintf(c.err, "%s: %s\n", e.Error.Code, e.Error.Message)
		} else {
			fmt.Fprintf(c.err, "HTTP %d\n", res.StatusCode)
		}
		return 1
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		c.out.Write(raw)
	} else {
		fmt.Fprintln(c.out, "done")
	}
	return 0
}
