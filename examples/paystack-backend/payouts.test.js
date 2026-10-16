// The Paystack payout service, tested against naira-sim. The service gets
// naira-sim's address as its Paystack base URL and naira-sim's secret key as
// its Paystack key; nothing else changes.
//
// Needs naira-sim running: SIM_URL (default http://localhost:7070). If
// naira-sim runs in a container, WEBHOOK_HOST is how it reaches this machine,
// such as host.docker.internal.

import assert from 'node:assert/strict';
import { after, before, beforeEach, test } from 'node:test';

import { Payouts } from './payouts.js';

const SIM = process.env.SIM_URL ?? 'http://localhost:7070';
const WEBHOOK_HOST = process.env.WEBHOOK_HOST ?? '127.0.0.1';
const ADA = { bankCode: '990001', accountNumber: '0123456789', expectedName: 'Ada Obi' };
const TUNDE = { bankCode: '990002', accountNumber: '1234567890', expectedName: 'Tunde Bakare' };

let payouts;
let server;
let webhookUrl;

before(async () => {
  payouts = new Payouts({ baseUrl: SIM, secretKey: 'sk_test_sim' });
  server = payouts.server();
  // Listen beyond this machine only when a container has to reach us.
  const listenOn = WEBHOOK_HOST === '127.0.0.1' ? '127.0.0.1' : '0.0.0.0';
  await new Promise((resolve) => server.listen(0, listenOn, resolve));
  webhookUrl = `http://${WEBHOOK_HOST}:${server.address().port}/webhooks/paystack`;
});

after(() => server.close());

beforeEach(() => {
  payouts.payouts.clear();
  payouts.seen.clear();
});

async function control(method, path, body) {
  const res = await fetch(SIM + path, { method, body: body ? JSON.stringify(body) : undefined });
  const text = await res.text();
  assert.ok(res.ok, `${method} ${path}: ${res.status} ${text}`);
  return text ? JSON.parse(text) : null;
}

/** A fresh scenario sending Paystack-format webhooks, with these failures. */
const scenario = (rules = []) =>
  control('POST', '/_sim/scenario', { start: '2026-10-16T09:00:00Z', webhook: { url: webhookUrl, format: 'paystack' }, rules });

const advance = (by) => control('POST', '/_sim/clock/advance', { by });

const deliveries = async () => (await control('GET', '/_sim/deliveries')).deliveries;

test('happy path: queued, then transfer.success', async () => {
  await scenario();
  const p = await payouts.send({ reference: 'payout-happy-0001', amount: 500_000, ...ADA });
  assert.equal(p.status, 'pending');
  await advance('2s');
  assert.equal(p.status, 'success');
});

test('a forged webhook is refused', async () => {
  assert.equal(payouts.handleWebhook('{"event":"transfer.success","data":{}}', { 'x-paystack-signature': 'f'.repeat(128) }), 401);
});

test('duplicate_webhook: three copies, applied once', async () => {
  await scenario([{ type: 'duplicate_webhook', params: { times: 3, gap: '10s' } }]);
  const p = await payouts.send({ reference: 'payout-dupes-0001', amount: 1000, ...ADA });
  await advance('1m');
  assert.equal((await deliveries()).length, 3);
  assert.equal(p.status, 'success');
  assert.equal(payouts.seen.size, 1);
});

test('late_reversal: success, then reversed two days later', async () => {
  await scenario([{ type: 'late_reversal', params: { after: '48h' } }]);
  const p = await payouts.send({ reference: 'payout-reversal-01', amount: 1000, ...ADA });
  await advance('2s');
  assert.equal(p.status, 'success');
  await advance('48h');
  assert.equal(p.status, 'reversed');
  assert.ok(p.refunded);
});

test('timeout_then_success: a 504, verified, paid once', async () => {
  await scenario([{ type: 'timeout_then_success' }]);
  const p = await payouts.send({ reference: 'payout-timeout-01', amount: 1000, ...ADA });
  await advance('2s');
  assert.equal(p.status, 'success');
  const { transfers } = await control('GET', '/_sim/transfers');
  assert.equal(transfers.length, 1, 'one transfer, not two');
});

test('missing_webhook: only verifying finds out', async () => {
  await scenario([{ type: 'missing_webhook' }]);
  const p = await payouts.send({ reference: 'payout-silent-0001', amount: 1000, ...ADA });
  await advance('1h');
  assert.equal(p.status, 'pending', 'no webhook came');
  await payouts.refreshPending();
  assert.equal(p.status, 'success');
});

test('stuck_pending and bank_delay: pending for hours is not a failure', async () => {
  await scenario([
    { type: 'stuck_pending', match: { bank_code: '990001' }, params: { hold: '6h' } },
    { type: 'bank_delay', match: { bank_code: '990002' }, params: { delay: '30m' } },
  ]);
  const stuck = await payouts.send({ reference: 'payout-stuck-0001', amount: 1000, ...ADA });
  const slow = await payouts.send({ reference: 'payout-slow-00001', amount: 1000, ...TUNDE });
  await advance('31m');
  assert.equal(slow.status, 'success');
  await payouts.refreshPending();
  assert.equal(stuck.status, 'pending');
  await advance('6h');
  assert.equal(stuck.status, 'success');
});

test('name_enquiry_failure: a different name stops the payout', async () => {
  await scenario([{ type: 'name_enquiry_failure', params: { mode: 'different_name' } }]);
  const p = await payouts.send({ reference: 'payout-wrongname-1', amount: 1000, ...ADA });
  assert.equal(p.state, 'name_mismatch');
  assert.equal((await control('GET', '/_sim/transfers')).transfers.length, 0, 'no money moved');
});

test('name_enquiry_failure: the bank not answering stops the payout', async () => {
  await scenario([{ type: 'name_enquiry_failure', params: { mode: 'error' } }]);
  const p = await payouts.send({ reference: 'payout-nobank-0001', amount: 1000, ...ADA });
  assert.equal(p.state, 'name_check_unavailable');
});
