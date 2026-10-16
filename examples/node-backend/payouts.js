// A small payout service, written to survive every failure naira-sim
// simulates. Each defence is marked with the failure it handles, so this file
// doubles as a checklist for your own backend.
//
// No dependencies: Node 20 or later.

import crypto from 'node:crypto';
import http from 'node:http';

// Statuses only move forward (spec S8). A late or repeated webhook can't move
// a payout backwards: defends against out_of_order_webhooks.
const rank = { pending: 0, successful: 1, failed: 1, reversed: 2 };

export class Payouts {
  constructor({ simUrl, secretKey = 'sk_test_sim', webhookSecret = 'whsec_test' }) {
    this.simUrl = simUrl;
    this.secretKey = secretKey;
    this.webhookSecret = webhookSecret;
    this.payouts = new Map(); // reference -> payout
    this.seenEvents = new Set();
    this.applied = 0; // how many webhook events changed a payout
  }

  async api(method, path, body) {
    const res = await fetch(this.simUrl + path, {
      method,
      headers: { Authorization: `Bearer ${this.secretKey}`, 'Content-Type': 'application/json' },
      body: body ? JSON.stringify(body) : undefined,
    });
    return { status: res.status, body: await res.json() };
  }

  /**
   * Pays someone. Returns the payout, or one that was stopped before any
   * money moved (state "name_check_unavailable" or "name_mismatch").
   */
  async send({ reference, amount, bankCode, accountNumber, expectedName }) {
    // Check the name first: defends against name_enquiry_failure. A failed
    // check stops the payout; it never sends money to an unconfirmed account.
    const name = await this.api('POST', '/v1/name-enquiry', { bank_code: bankCode, account_number: accountNumber });
    if (name.status !== 200) {
      return { reference, state: 'name_check_unavailable' };
    }
    if (expectedName && name.body.account_name !== expectedName) {
      return { reference, state: 'name_mismatch', foundName: name.body.account_name };
    }

    const request = { reference, amount, currency: 'NGN', bank_code: bankCode, account_number: accountNumber };
    let res = await this.api('POST', '/v1/transfers', request);
    if (res.status === 504 || res.status >= 500) {
      // A timeout doesn't mean it failed: look it up by reference before
      // retrying, and retry with the SAME reference so it can't pay twice.
      // Defends against timeout_then_success.
      const found = await this.api('GET', `/v1/transfers?reference=${encodeURIComponent(reference)}`);
      res = found.status === 200 ? found : await this.api('POST', '/v1/transfers', request);
    }
    if (res.status !== 200 && res.status !== 201) {
      throw new Error(`transfer refused: ${res.body.error?.code}`);
    }
    const payout = { reference, id: res.body.id, amount, status: res.body.status, refunded: false };
    this.payouts.set(reference, payout);
    return payout;
  }

  /** Handles one webhook. Answers 2xx unless the signature is wrong. */
  handleWebhook(rawBody, headers) {
    const expected = crypto.createHmac('sha256', this.webhookSecret).update(rawBody).digest('hex');
    const given = String(headers['x-sim-signature'] ?? '');
    if (given.length !== expected.length || !crypto.timingSafeEqual(Buffer.from(given), Buffer.from(expected))) {
      return 401;
    }
    const event = JSON.parse(rawBody);
    // The event ID is the key: defends against duplicate_webhook (spec S15).
    if (this.seenEvents.has(event.id)) return 200;
    this.seenEvents.add(event.id);
    const payout = this.payouts.get(event.data.reference);
    if (payout && this.apply(payout, event.data.status)) this.applied++;
    return 200;
  }

  apply(payout, status) {
    if (rank[status] <= rank[payout.status]) return false;
    payout.status = status;
    // A success can still be undone days later: defends against late_reversal.
    if (status === 'reversed' || status === 'failed') payout.refunded = true;
    return true;
  }

  /**
   * Asks about every payout still pending. Run it on a timer. Defends against
   * missing_webhook, and keeps stuck_pending and bank_delay payouts honest:
   * pending is a state to wait in, not a failure.
   */
  async refreshPending() {
    for (const payout of this.payouts.values()) {
      if (payout.status !== 'pending') continue;
      const res = await this.api('GET', `/v1/transfers/${payout.id}`);
      if (res.status === 200) this.apply(payout, res.body.status);
    }
  }

  /**
   * Compares a day's settlement report with our own records and lists every
   * disagreement. Defends against settlement_mismatch.
   */
  async reconcile(date) {
    const res = await this.api('GET', `/v1/settlements?date=${date}`);
    const problems = [];
    for (const entry of res.body.entries) {
      const ours = this.payouts.get(entry.reference);
      if (!ours) problems.push({ reference: entry.reference, problem: 'unknown_transfer' });
      else if (entry.amount !== ours.amount) problems.push({ reference: entry.reference, problem: 'amount', ours: ours.amount, theirs: entry.amount });
      else if (entry.status !== ours.status) problems.push({ reference: entry.reference, problem: 'status', ours: ours.status, theirs: entry.status });
    }
    return problems;
  }

  /** An HTTP server for webhooks at /webhooks. */
  server() {
    return http.createServer((req, res) => {
      const chunks = [];
      req.on('data', (c) => chunks.push(c));
      req.on('end', () => {
        res.statusCode = req.url === '/webhooks' ? this.handleWebhook(Buffer.concat(chunks).toString(), req.headers) : 404;
        res.end();
      });
    });
  }
}
