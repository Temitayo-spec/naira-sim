# naira-sim

A stand-in for a Nigerian payments provider that misbehaves on purpose. Point your backend at it instead of the real provider, switch on a failure (a transfer stuck in pending, a webhook sent three times, a reversal two days later) and see what your app does, before real money finds out.

It never moves real money and never connects to a bank or a provider. Its banks are made up. It speaks two APIs, both described in [`spec/sim-v1.md`](spec/sim-v1.md):

- **Paystack's transfer API.** If your backend uses Paystack for payouts, point its Paystack base URL at naira-sim and use `sk_test_sim` as the key. Nothing else changes. naira-sim isn't Paystack and isn't connected to it; it answers in the shapes Paystack documents.
- **A neutral API** at `/v1`, for anything else.

## Quickstart

```bash
docker run -p 7070:7070 ghcr.io/temitayo-spec/naira-sim
```

Or from the code: `go run ./cmd/nairasim`.

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

## Using it with Paystack

Point your Paystack client at naira-sim, and ask for Paystack-format webhooks in the scenario:

```bash
PAYSTACK_BASE_URL=http://localhost:7070 PAYSTACK_SECRET_KEY=sk_test_sim npm test
```

```json
{ "webhook": { "url": "http://localhost:3000/webhooks/paystack", "format": "paystack" } }
```

Recipients, transfers, verify, the bank list, account resolve and balance all work, and webhooks arrive as `transfer.success`, `transfer.failed` and `transfer.reversed`, signed with `X-Paystack-Signature` (HMAC-SHA512 with your secret key), as Paystack sends them. References must meet Paystack's rule: 16 to 50 lowercase letters, digits, `-` or `_`.

Every failure below works with Paystack's API except two. `out_of_order_webhooks` is refused, because Paystack never sends a pending event. `settlement_mismatch` changes only `/v1`'s settlement report, which Paystack's API has no equivalent of.

[`examples/paystack-backend`](examples/paystack-backend) is a payout service written for Paystack, with nothing naira-sim-specific in it, and tested against naira-sim.

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
(cd examples/node-backend && node --test)
(cd examples/paystack-backend && node --test)
```

## In CI

On GitHub Actions, run naira-sim as a service container next to your tests:

```yaml
jobs:
  test:
    runs-on: ubuntu-latest
    services:
      naira-sim:
        image: ghcr.io/temitayo-spec/naira-sim:0.1.0
        ports: ["7070:7070"]
        options: --add-host=host.docker.internal:host-gateway
    steps:
      - uses: actions/checkout@v7
      - run: npm ci && npm test
        env:
          PAYSTACK_BASE_URL: http://localhost:7070
          PAYSTACK_SECRET_KEY: sk_test_sim
```

Your tests reach naira-sim at `localhost:7070`. For webhooks, naira-sim has to reach your tests, and from inside its container `localhost` is itself. So give scenarios a webhook URL on `host.docker.internal` (that's what the `--add-host` line is for), and have your webhook server listen on `0.0.0.0`, not `127.0.0.1`. naira-sim's own CI runs [`examples/paystack-backend`](examples/paystack-backend) this way, with `WEBHOOK_HOST=host.docker.internal`.

Pin a version, as above, so a new release can't change your tests under you.

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
