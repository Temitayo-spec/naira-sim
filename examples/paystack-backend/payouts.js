// A payout service written for Paystack's transfer API, with nothing
// naira-sim-specific in it. In production, baseUrl is https://api.paystack.co
// and secretKey is your live key; in tests, baseUrl is naira-sim. That's the
// only difference.
//
// Each defence is marked with the naira-sim failure that tests it.
//
// No dependencies: Node 20 or later.

import crypto from 'node:crypto';
import http from 'node:http';

// Statuses only move forward, so a late or repeated webhook can't undo a
// newer one.
const rank = { pending: 0, otp: 0, success: 1, failed: 1, reversed: 2 };

export class Payouts {
  constructor({ baseUrl = 'https://api.paystack.co', secretKey }) {
    this.baseUrl = baseUrl;
    this.secretKey = secretKey;
    this.payouts = new Map(); // reference -> payout
    this.seen = new Set(); // webhooks already applied
  }

  async api(method, path, body) {
    const res = await fetch(this.baseUrl + path, {
      method,
      headers: { Authorization: `Bearer ${this.secretKey}`, 'Content-Type': 'application/json' },
      body: body ? JSON.stringify(body) : undefined,
    });
    let json = null;
    try {
      json = await res.json();
    } catch {
      // a gateway error may not be JSON
    }
    return { status: res.status, body: json };
  }

  /**
   * Pays someone. Returns the payout, or one stopped before any money moved
   * (state "name_check_unavailable" or "name_mismatch").
   */
  async send({ reference, amount, bankCode, accountNumber, expectedName, reason }) {
    // Resolve first, and compare the bank's name with the one we expect.
    // Tested by name_enquiry_failure (both modes).
    const resolved = await this.api('GET', `/bank/resolve?account_number=${accountNumber}&bank_code=${bankCode}`);
    if (resolved.status !== 200) {
      return { reference, state: 'name_check_unavailable' };
    }
    if (expectedName && resolved.body.data.account_name !== expectedName) {
      return { reference, state: 'name_mismatch', foundName: resolved.body.data.account_name };
    }

    const recipient = await this.api('POST', '/transferrecipient', {
      type: 'nuban', name: expectedName ?? resolved.body.data.account_name,
      account_number: accountNumber, bank_code: bankCode, currency: 'NGN',
    });
    if (recipient.status !== 200) {
      return { reference, state: 'name_check_unavailable' };
    }

    const request = { source: 'balance', amount, recipient: recipient.body.data.recipient_code, reference, reason };
    let res = await this.api('POST', '/transfer', request);
    if (res.status >= 500) {
      // A timeout doesn't mean it failed. Verify by reference, and retry
      // only with the SAME reference, so it can't pay twice.
      // Tested by timeout_then_success.
      const found = await this.api('GET', `/transfer/verify/${reference}`);
      res = found.status === 200 ? found : await this.api('POST', '/transfer', request);
    }
    if (res.status !== 200 || !res.body?.status) {
      throw new Error(`transfer refused: ${res.body?.message}`);
    }
    const payout = { reference, code: res.body.data.transfer_code, amount, status: res.body.data.status, refunded: false };
    this.payouts.set(reference, payout);
    return payout;
  }

  /** Handles one webhook. Answers 2xx unless the signature is wrong. */
  handleWebhook(rawBody, headers) {
    // Paystack signs with your secret key: hex HMAC-SHA512 of the raw body.
    const expected = crypto.createHmac('sha512', this.secretKey).update(rawBody).digest('hex');
    const given = String(headers['x-paystack-signature'] ?? '');
    if (given.length !== expected.length || !crypto.timingSafeEqual(Buffer.from(given), Buffer.from(expected))) {
      return 401;
    }
    const { event, data } = JSON.parse(rawBody);
    // Paystack's events carry no ID, so the event and the transfer are the
    // key. Tested by duplicate_webhook.
    const key = `${event}:${data.transfer_code}`;
    if (this.seen.has(key)) return 200;
    this.seen.add(key);
    const payout = this.payouts.get(data.reference);
    if (payout) this.apply(payout, data.status);
    return 200;
  }

  apply(payout, status) {
    if ((rank[status] ?? -1) <= rank[payout.status]) return;
    payout.status = status;
    // A success can be undone days later. Tested by late_reversal.
    if (status === 'reversed' || status === 'failed') payout.refunded = true;
  }

  /**
   * Verifies every payout still pending. Run it on a timer: a webhook may
   * never come (missing_webhook), and pending can last hours (stuck_pending,
   * bank_delay) without being a failure.
   */
  async refreshPending() {
    for (const payout of this.payouts.values()) {
      if (rank[payout.status] !== 0) continue;
      const res = await this.api('GET', `/transfer/verify/${payout.reference}`);
      if (res.status === 200) this.apply(payout, res.body.data.status);
    }
  }

  /** An HTTP server for webhooks at /webhooks/paystack. */
  server() {
    return http.createServer((req, res) => {
      const chunks = [];
      req.on('data', (c) => chunks.push(c));
      req.on('end', () => {
        res.statusCode = req.url === '/webhooks/paystack' ? this.handleWebhook(Buffer.concat(chunks).toString(), req.headers) : 404;
        res.end();
      });
    });
  }
}
