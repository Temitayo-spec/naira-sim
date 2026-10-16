# Changelog

## 0.1.0 (16 October 2026)

The first release.

- A simulated naira payments provider: a wallet, transfers, made-up banks and accounts, webhooks signed and retried, and a settlement report (spec sections 1 to 3).
- Nine failures to switch on per bank, account, reference prefix or share of traffic: stuck in pending, a slow bank, duplicate webhooks, a settlement mismatch, a late reversal, a timeout after the money moved, a missing webhook, out-of-order webhooks and a failed or wrong name enquiry (section 4).
- Scenarios that replay exactly, from a seed, on a clock tests control (sections 5 and 6).
- A control API, `simctl` and a dashboard at `/_sim/` (section 7).
- Paystack's transfer API: a Paystack backend works with only its base URL and key changed, with Paystack-format webhooks (section 8).
- An image, `ghcr.io/temitayo-spec/naira-sim`, for amd64 and arm64.
