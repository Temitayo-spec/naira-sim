// One test per failure in naira-sim's contract (spec/sim-v1.md section 4).
// Each loads a scenario with that failure switched on, runs a payout, moves
// the simulator's clock, and checks the backend did the right thing.
//
// Needs naira-sim running: SIM_URL (default http://localhost:7070).

import assert from 'node:assert/strict';
import { after, before, beforeEach, test } from 'node:test';

import { Payouts } from './payouts.js';

const SIM = process.env.SIM_URL ?? 'http://localhost:7070';
const ADA = { bankCode: '990001', accountNumber: '0123456789', expectedName: 'Ada Obi' };
const TUNDE = { bankCode: '990002', accountNumber: '1234567890', expectedName: 'Tunde Bakare' };

let payouts;
let server;
let webhookUrl;

before(async () => {
  payouts = new Payouts({ simUrl: SIM });
  server = payouts.server();
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  webhookUrl = `http://127.0.0.1:${server.address().port}/webhooks`;
});

after(() => server.close());

beforeEach(() => {
  payouts.payouts.clear();
  payouts.seenEvents.clear();
  payouts.applied = 0;
});

async function control(method, path, body) {
  const res = await fetch(SIM + path, { method, body: body ? JSON.stringify(body) : undefined });
  const text = await res.text();
  assert.ok(res.ok, `${method} ${path}: ${res.status} ${text}`);
  return text ? JSON.parse(text) : null;
}

/** Loads a fresh scenario with these failure rules. */
const scenario = (rules = []) =>
  control('POST', '/_sim/scenario', { start: '2026-10-16T09:00:00Z', webhook: { url: webhookUrl }, rules });

const advance = (by) => control('POST', '/_sim/clock/advance', { by });

test('happy path: paid, and the webhook confirms it', async () => {
  await scenario();
  const p = await payouts.send({ reference: 'pay-1', amount: 500_000, ...ADA });
  assert.equal(p.status, 'pending');
  await advance('2s');
  assert.equal(p.status, 'successful');
});

test('stuck_pending: waits, never gives up early, then settles', async () => {
  await scenario([{ type: 'stuck_pending', params: { hold: '6h' } }]);
  const p = await payouts.send({ reference: 'stuck-1', amount: 1000, ...ADA });
  await advance('5h');
  await payouts.refreshPending();
  assert.equal(p.status, 'pending', 'still pending after 5 hours, and not marked failed');
  await advance('1h');
  assert.equal(p.status, 'successful');
});

test('bank_delay: one slow bank, the other unaffected', async () => {
  await scenario([{ type: 'bank_delay', match: { bank_code: '990002' }, params: { delay: '30m' } }]);
  const fast = await payouts.send({ reference: 'fast-1', amount: 1000, ...ADA });
  const slow = await payouts.send({ reference: 'slow-1', amount: 1000, ...TUNDE });
  await advance('2s');
  assert.equal(fast.status, 'successful');
  assert.equal(slow.status, 'pending');
  await advance('30m');
  assert.equal(slow.status, 'successful');
});

test('duplicate_webhook: three copies, applied once', async () => {
  await scenario([{ type: 'duplicate_webhook', params: { times: 3, gap: '10s' } }]);
  const p = await payouts.send({ reference: 'dup-1', amount: 1000, ...ADA });
  await advance('1m');
  const deliveries = (await control('GET', '/_sim/deliveries')).deliveries;
  assert.equal(deliveries.length, 3, 'the simulator sent three copies');
  assert.equal(payouts.applied, 1, 'the payout changed once');
  assert.equal(p.status, 'successful');
});

test('settlement_mismatch: reconciliation catches it', async () => {
  await scenario([{ type: 'settlement_mismatch', match: { reference_prefix: 'odd-' }, params: { delta: -100 } }]);
  await payouts.send({ reference: 'odd-1', amount: 5000, ...ADA });
  await payouts.send({ reference: 'even-1', amount: 5000, ...ADA });
  await advance('2s');
  const problems = await payouts.reconcile('2026-10-16');
  assert.deepEqual(problems, [{ reference: 'odd-1', problem: 'amount', ours: 5000, theirs: 4900 }]);
});

test('late_reversal: a success undone two days later is refunded', async () => {
  await scenario([{ type: 'late_reversal', params: { after: '48h' } }]);
  const p = await payouts.send({ reference: 'rev-1', amount: 7000, ...ADA });
  await advance('2s');
  assert.equal(p.status, 'successful');
  assert.equal(p.refunded, false);
  await advance('48h');
  assert.equal(p.status, 'reversed');
  assert.equal(p.refunded, true);
});

test('timeout_then_success: a 504 never pays twice', async () => {
  await scenario([{ type: 'timeout_then_success' }]);
  const p = await payouts.send({ reference: 'slow-1', amount: 1000, ...ADA });
  assert.equal(p.id, 'trf_000001');
  const transfers = (await control('GET', '/_sim/transfers')).transfers;
  assert.equal(transfers.length, 1, 'one transfer, despite the timeout');
  await advance('2s');
  assert.equal(p.status, 'successful');
});

test('missing_webhook: asking finds the answer', async () => {
  await scenario([{ type: 'missing_webhook' }]);
  const p = await payouts.send({ reference: 'quiet-1', amount: 1000, ...ADA });
  await advance('1h');
  assert.equal(p.status, 'pending', 'no webhook came');
  await payouts.refreshPending();
  assert.equal(p.status, 'successful');
});

test('out_of_order_webhooks: a late "pending" can\'t undo a success', async () => {
  await scenario([{ type: 'out_of_order_webhooks' }]);
  const p = await payouts.send({ reference: 'ooo-1', amount: 1000, ...ADA });
  await advance('2s');
  const types = (await control('GET', '/_sim/events')).events.map((e) => e.type);
  assert.deepEqual(types, ['transfer.successful', 'transfer.pending']);
  assert.equal(p.status, 'successful');
});

test('name_enquiry_failure: no money moves to an unconfirmed account', async () => {
  await scenario([
    { type: 'name_enquiry_failure', match: { account_number: '0123456789' } },
    { type: 'name_enquiry_failure', match: { account_number: '1234567890' }, params: { mode: 'different_name' } },
  ]);
  assert.equal((await payouts.send({ reference: 'n-1', amount: 1000, ...ADA })).state, 'name_check_unavailable');
  assert.equal((await payouts.send({ reference: 'n-2', amount: 1000, ...TUNDE })).state, 'name_mismatch');
  assert.equal((await control('GET', '/_sim/transfers')).transfers.length, 0);
});

test('a webhook with a bad signature is refused', () => {
  assert.equal(payouts.handleWebhook('{"id":"evt_1"}', { 'x-sim-signature': 'nope' }), 401);
});
