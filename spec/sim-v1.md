# naira-sim contract v1

naira-sim is a stand-in for a Nigerian payments provider that misbehaves on
purpose. A team points their backend at it instead of a real provider, switches
on a failure, and checks what their app does. It never moves real money and
never connects to a bank.

The words MUST, MUST NOT, SHOULD and MAY are used as in RFC 2119. Rules are
numbered (S1, S2, …) so tests and issues can point at them.

## 1. Money and time

**S1.** Amounts are whole numbers of kobo (₦1 = 100 kobo), as JSON integers.
Currency is always `"NGN"`.

**S2.** Times are RFC 3339 strings in UTC, such as `"2026-10-16T09:00:00Z"`, and
always come from the simulator's clock (section 6), never the computer's.

## 2. The public API

The API your backend calls, served at `/v1`. Every request needs
`Authorization: Bearer <secret key>` (the scenario's `secret_key`, default
`sk_test_sim`); without it the answer is 401.

Errors are JSON: `{"error": {"code": "<code>", "message": "<words>"}}`.

### Banks and names

**S3.** `GET /v1/banks` lists the banks: `{"banks": [{"code": "990001", "name": "Sandbox Bank One"}]}`.
The default banks are made up (codes `990001` to `990005`); a scenario can
replace them. No bank in naira-sim is a real bank.

**S4.** `POST /v1/name-enquiry` with `{"bank_code", "account_number"}` answers
`{"bank_code", "account_number", "account_name"}` for a known account, and 404
`account_not_found` otherwise. Account numbers are 10 digits (NUBAN length).

### Balance

**S5.** `GET /v1/balance` answers `{"balance": <kobo>, "currency": "NGN"}`: the
team's wallet. A transfer takes its amount out when it's created. A failed or
reversed transfer puts it back.

### Transfers

**S6.** `POST /v1/transfers` with:

```json
{ "reference": "your-unique-id", "amount": 500000, "currency": "NGN",
  "bank_code": "990001", "account_number": "0123456789", "narration": "Rent" }
```

`reference` is yours: 1 to 64 letters, digits, `-` or `_`. It's the
idempotency key.

- A new reference: 201 with the transfer, status `pending`.
- The same reference and the same body again: 200 with the transfer as it is now.
  Nothing new happens.
- The same reference with a different body: 409 `reference_reused`.
- Not enough balance: 402 `insufficient_balance`; nothing is created.
- An unknown bank: 400 `unknown_bank`. A badly formed field: 400 `invalid_request`.

An account number that isn't a known account is accepted, and the transfer
later fails with reason `account_not_found`, as a real transfer would.

**S7.** A transfer is:

```json
{ "id": "trf_000001", "reference": "your-unique-id", "amount": 500000,
  "currency": "NGN", "bank_code": "990001", "account_number": "0123456789",
  "account_name": "Ada Obi", "narration": "Rent", "status": "pending",
  "failure_reason": null, "created_at": "…", "updated_at": "…" }
```

IDs are numbered in order of creation, so a replayed scenario gives the same IDs.

**S8.** `status` moves only along these lines:

```
pending → successful → reversed
pending → failed
```

A transfer never goes back, and never jumps from `failed` or `reversed`.

**S9.** `GET /v1/transfers/{id}` and `GET /v1/transfers?reference=<ref>` answer
the transfer as it is now. The lookup always tells the truth, whatever the
webhooks did.

**S10.** Without any failure switched on, a transfer settles as `successful`
after the scenario's `latency` (default 2 seconds of simulator time).

### Settlement report

**S11.** `GET /v1/settlements?date=YYYY-MM-DD` lists what settled on that day of
the simulator's clock (UTC): `{"date", "entries": [{"transfer_id", "reference",
"amount", "status", "settled_at"}]}`. It's what a team reconciles against.

## 3. Webhooks

**S12.** When a transfer reaches `successful`, `failed` or `reversed`, the
simulator records an event and sends it to the scenario's webhook URL:

```json
{ "id": "evt_000001", "type": "transfer.successful",
  "created_at": "…", "data": { …the transfer at that moment… } }
```

Types: `transfer.successful`, `transfer.failed`, `transfer.reversed`, and
`transfer.pending` (sent only by the out-of-order failure, S22).

These are the `"sim"` format's webhooks, the default. A scenario can ask for
Paystack's instead (S34).

**S13.** Every webhook carries `X-Sim-Signature: <hex HMAC-SHA256 of the raw
body, keyed with the webhook secret>` and `X-Sim-Event-Id: <event id>`.

**S14.** A webhook succeeds on any 2xx answer. Otherwise it's retried after 1
minute, 5 minutes, 30 minutes and 2 hours of simulator time, then given up.
Retries keep the same event ID.

**S15.** The same event can arrive more than once (S19). Receivers MUST treat
the event ID as the key.

## 4. Failures

A failure is a **rule**: a `type`, which transfers it applies to (`match`), how
often (`rate`), and settings (`params`). Rules are set by the scenario or the
control API.

**S16.** `match` may name `bank_code`, `account_number` and `reference_prefix`;
every field it names must match. An empty match applies to every transfer.
`rate` is a share from 0 to 1 (default 1). Whether a transfer is picked is
decided once, when it's created, from the scenario's seed, so a replay picks
the same transfers.

**S17.** Several rules can apply to one transfer. Two rules of the same type:
the first one added wins.

The nine failures:

| Type | What happens | `params` (defaults) |
| --- | --- | --- |
| `stuck_pending` | Stays `pending` for `hold`, then becomes `outcome` | `hold` (`"2h"`), `outcome`: `"successful"` or `"failed"` (`"successful"`) |
| `bank_delay` | Settles `delay` later than usual: the inter-bank slowdown. Usually matched by `bank_code` | `delay` (`"30m"`) |
| `duplicate_webhook` | Every event for the transfer is sent `times` times, `gap` apart | `times` (`3`), `gap` (`"10s"`) |
| `settlement_mismatch` | The settlement report disagrees with the transfer: a different amount or status | `field`: `"amount"` or `"status"` (`"amount"`), `delta` in kobo (`-100`) |
| `late_reversal` | Settles `successful`, then is `reversed` after `after` | `after` (`"48h"`) |
| `timeout_then_success` | The create request gets 504 `upstream_timeout`, but the transfer was created and goes on as usual | none |
| `missing_webhook` | No webhook is ever sent for the transfer; the lookup still tells the truth | none |
| `out_of_order_webhooks` | `transfer.successful` is sent, then a `transfer.pending` dated earlier | none |
| `name_enquiry_failure` | Name enquiry for the matched account fails with 502 `upstream_error`, or answers a different name | `mode`: `"error"` or `"different_name"` (`"error"`) |

**S18.** `stuck_pending` with `outcome: "failed"` fails with reason
`timed_out`, and puts the money back.

**S19.** `duplicate_webhook` repeats deliveries, not events: the copies share
one event ID.

**S20.** `settlement_mismatch` changes only the settlement report; the transfer
and its webhooks stay true.

**S21.** `timeout_then_success` matches on the request's fields as S16 says.
The 504 body is `{"error": {"code": "upstream_timeout", …}}`. Looking the
transfer up by reference shows it.

**S22.** `out_of_order_webhooks`: the `transfer.pending` event's `created_at`
is the transfer's creation time, but it's sent after the final event.

**S23.** `name_enquiry_failure` applies to name enquiries only, matched by
`bank_code` and `account_number`. Its `rate` is decided per enquiry, from the
seed, in the order enquiries arrive.

## 5. Scenarios

**S24.** A scenario is one JSON file that sets everything up:

```json
{
  "seed": 42,
  "clock": "manual",
  "start": "2026-10-16T09:00:00Z",
  "balance": 100000000,
  "latency": "2s",
  "secret_key": "sk_test_sim",
  "webhook": { "url": "http://localhost:3000/webhooks", "secret": "whsec_test", "format": "sim" },
  "banks": [{ "code": "990001", "name": "Sandbox Bank One" }],
  "accounts": [{ "bank_code": "990001", "account_number": "0123456789", "name": "Ada Obi" }],
  "rules": [{ "type": "stuck_pending", "match": { "bank_code": "990001" }, "rate": 0.2, "params": { "hold": "6h" } }]
}
```

Every field is optional; the defaults are those above, with an empty webhook
URL meaning webhooks are recorded but not sent, five default banks, three
default accounts, and no rules.

**S25.** Loading a scenario resets everything: transfers, events, deliveries,
counters and the clock.

**S26.** Replay: the same scenario, with the same requests in the same order
and the same clock moves, gives the same IDs, statuses, events, delivery times
and settlement report.

## 6. The clock

**S27.** The simulator keeps its own time. In `"real"` mode it follows the
computer's clock from the scenario's `start`. In `"manual"` mode it stands
still until it's told to move (S29), which is what tests should use.

**S28.** Everything timed (settling, holds, reversals, webhook sends and
retries) is scheduled on that clock, and runs in time order. Two things due at
the same moment run in the order they were scheduled.

## 7. The control API

For tests and the CLI, served at `/_sim`. It needs no key, so run naira-sim
only where your tests can reach it, never on the open internet.

| Request | Does |
| --- | --- |
| `GET /_sim/state` | The clock, balance, rules, and counts of transfers and events |
| `POST /_sim/scenario` | Loads a scenario (the body), resetting everything (S25) |
| `POST /_sim/reset` | Reloads the current scenario |
| `POST /_sim/clock/advance` | `{"by": "2h"}`: moves a manual clock forward and runs everything due, webhooks included, before answering |
| `GET /_sim/rules`, `POST /_sim/rules`, `DELETE /_sim/rules/{id}` | Lists, adds and removes rules |
| `GET /_sim/events` | Every event, in order |
| `GET /_sim/deliveries` | Every webhook attempt: event, time, answer, attempt number |
| `GET /_sim/transfers` | Every transfer |

**S29.** `POST /_sim/clock/advance` is refused (409 `clock_not_manual`) in real
mode. In manual mode it answers only after everything due has run, so a test
can check the result straight away.

## 8. Paystack's transfer API

naira-sim also speaks Paystack's transfer API, so a backend built on it can be
tested by changing only its base URL and key. It is not Paystack and is not
connected to Paystack. The shapes follow Paystack's public documentation;
where the documentation shows no error message, the words are naira-sim's.

**S30.** Served at the root, next to `/v1`: point a Paystack client's base URL
(normally `https://api.paystack.co`) at naira-sim, and use the scenario's
`secret_key` as the Paystack secret key. Every answer is Paystack's envelope:
`{"status": true, "message", "data"}`, plus `meta` for lists, or
`{"status": false, "message"}`. A request without a key gets 401 "No
Authorization header was found"; a wrong key, 401 "Invalid key". Request
bodies are read leniently: fields naira-sim doesn't use are ignored, and
`amount` may be a number or a string of digits.

| Request | Does |
| --- | --- |
| `GET /bank` | The scenario's banks, in Paystack's bank shape |
| `GET /bank/resolve?account_number&bank_code` | A name enquiry (S35) |
| `GET /balance` | `[{"currency": "NGN", "balance": <kobo>}]` |
| `POST /transferrecipient` | Creates a recipient (S31) |
| `GET /transferrecipient/{id_or_code}` | Fetches one |
| `POST /transfer` | Initiates a transfer (S32, S33) |
| `GET /transfer` | Lists transfers, newest first; `perPage` (50) and `page` (1) |
| `GET /transfer/{id_or_code}` | Fetches one |
| `GET /transfer/verify/{reference}` | Fetches one by reference |

**S31.** `POST /transferrecipient` takes `type` (only `"nuban"`), `name`,
`account_number`, `bank_code`, and optionally `description` and `currency`
(only `"NGN"`). A new recipient is checked with a name enquiry, so
`name_enquiry_failure` applies, and `details.account_name` is the name the
bank gave, which may differ from `name`. An account that can't be resolved
gets 400 "Cannot resolve account". As at Paystack, an account that already has
a recipient gets that recipient back. Codes are `RCP_sim` and nine digits; IDs
are the same number. Every transfer has a recipient, including those sent
through `/v1`.

**S32.** `POST /transfer` takes `source` (`"balance"`), `amount` in kobo,
`recipient` (a recipient code), `reference`, `reason` (the narration) and
optionally `currency` (`"NGN"`). It answers 200 "Transfer has been queued"
with the transfer, its `recipient` and `integration` as IDs; fetch, verify
and list show the recipient as an object. Statuses are Paystack's words:
`pending`, `success`, `failed`, `reversed`. Codes are `TRF_sim` and nine
digits; the ID is the same number. `transferred_at` is when it settled.
An unknown recipient gets Paystack's 400 `invalid_transfer_recipient`; too
little balance, 400 "Your balance is not enough to fulfil this request".
`timeout_then_success` answers 504 with `"status": false` (S21).

**S33.** A `reference` must be 16 to 50 lowercase letters, digits, `-` or
`_`, as Paystack requires; anything else gets 400. The same request with the
same reference again is answered with the existing transfer and moves no
money; the same reference with a different request gets 400 "Duplicate
reference(s) found". A transfer sent without a reference gets one made up,
`sim_generated_` and six digits, and can't be matched on a retry.

**S34.** With `"webhook": {"format": "paystack"}`, webhooks are Paystack's:
`{"event", "data"}`, the events `transfer.success`, `transfer.failed` and
`transfer.reversed`, `data` the transfer with the recipient and integration
as objects, signed with `X-Paystack-Signature: <hex HMAC-SHA512 of the raw
body, keyed with the scenario's secret_key>`, and no `X-Sim-` headers.
Paystack's events carry no ID, so a duplicate (S19) is recognised by its event
and transfer. Retries follow S14. Paystack never sends a pending event, so
`out_of_order_webhooks` is refused in this format, both in a scenario and from
the control API. The format changes only webhooks: `/v1` and the Paystack
routes both work either way.

**S35.** `GET /bank/resolve` answers 200 "Account number resolved" with
`{"account_number", "account_name"}`. An unknown bank or account, a malformed
number, and `name_enquiry_failure`'s `"error"` mode all get 422 "Could not
resolve account name. Check parameters or try again.": Paystack gives the
same answer whatever the reason. `"different_name"` mode answers someone
else's name, as in S23.
