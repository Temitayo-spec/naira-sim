# naira-sim

A stand-in for a Nigerian payments provider that misbehaves on purpose. Point your backend at it instead of the real provider, switch on a failure (a transfer stuck in pending, a webhook sent three times, a reversal two days later) and see what your app does, before real money finds out.

It never moves real money and never connects to a bank. Its banks are made up, and it doesn't pretend to be any provider: its API is a neutral shape, described in [`spec/sim-v1.md`](spec/sim-v1.md).

## Quickstart

```bash
go run ./cmd/nairasim
```

Or with Docker: `docker build -t naira-sim . && docker run -p 7070:7070 naira-sim`.

Send a transfer:

```bash
curl -s -H 'Authorization: Bearer sk_test_sim' -X POST localhost:7070/v1/transfers \
  -d '{"reference":"rent-1","amount":500000,"currency":"NGN","bank_code":"990001","account_number":"0123456789"}'
```

The clock is manual by default, so the transfer waits until you move it:

```bash
go run ./cmd/simctl advance 2s
```

```bash
curl -s -H 'Authorization: Bearer sk_test_sim' 'localhost:7070/v1/transfers?reference=rent-1'
```

It's `successful` now. Switch a failure on and send another:

```bash
go run ./cmd/simctl rule add stuck_pending --hold 6h --outcome failed
```

## The failures

| Type | What happens |
| --- | --- |
| `stuck_pending` | Stays `pending` for hours, then settles or fails |
| `bank_delay` | One bank is slow: the inter-bank slowdown |
| `duplicate_webhook` | Every event arrives two or more times, with the same event ID |
| `settlement_mismatch` | The settlement report disagrees with the transfer |
| `late_reversal` | Succeeds, then is reversed days later |
| `timeout_then_success` | The create request gets a 504, but the money moved |
| `missing_webhook` | No webhook ever comes; only asking tells you |
| `out_of_order_webhooks` | `successful` arrives, then a `pending` dated earlier |
| `name_enquiry_failure` | The name check fails, or returns someone else's name |

Each one can apply to every transfer, to one bank or account, to references with a prefix, or to a share of traffic (`--rate 0.2`). A share is picked from the scenario's seed, so the same run picks the same transfers every time.

## Using it in tests

- **Load a scenario** for each test: `POST /_sim/scenario` with the setup and the failures (see [`examples/scenarios/bad-day.json`](examples/scenarios/bad-day.json)). Everything resets.
- **Move the clock** with `POST /_sim/clock/advance {"by": "48h"}`. It answers only after everything due has run, webhooks included, so a two-day reversal takes milliseconds and you can check the result straight away.
- **Check what was sent** with `GET /_sim/events` and `GET /_sim/deliveries`.

[`examples/node-backend`](examples/node-backend) is a small payout service with one test per failure. Each defence in it is marked with the failure it handles, so it doubles as a checklist for your own backend. Run naira-sim, then:

```bash
cd examples/node-backend && node --test
```

## The dashboard

Open `http://localhost:7070/_sim/` while naira-sim runs. It shows the clock, the wallet, every transfer, event and webhook attempt, updated each second, and lets you move the clock and switch failures on and off. It reads the same control API as `simctl` and loads nothing from anywhere else.

## `simctl`

```bash
go run ./cmd/simctl help
```

Status, load and reset a scenario, move the clock, add and remove failures, list transfers, events and webhook attempts, and `export` the current setup to replay later. `SIM_URL` says where naira-sim is (default `http://localhost:7070`).

## Settings

| Variable | Default | |
| --- | --- | --- |
| `PORT` | `7070` | Where to listen |
| `SIM_SCENARIO` | none | A scenario file to load at start |

The control API (`/_sim`) needs no key. Run naira-sim where your tests can reach it, never on the open internet.

MIT licensed.
