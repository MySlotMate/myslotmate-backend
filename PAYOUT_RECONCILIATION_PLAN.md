# Payout Reconciliation — Root-Cause Remediation Plan

**Status:** planned, not implemented
**Owner:** _tbd_
**Context:** Two host payouts (POP HAUS ₹1,896 UPI, House of Play ₹4,048 IMPS) stranded
at `status='processing'` because the Cashfree payout webhook has **never** successfully
processed a single event (0 rows in `webhook_executions`, ever). The failed one locked
the host's withdrawable balance. Both were corrected by hand; this plan removes the
underlying fragility so it cannot recur.

---

## Root cause (confirmed)

The payout lifecycle is **100% dependent on an inbound Cashfree webhook that has never
worked**, with:

1. **No fallback** — `provider.CheckStatus()` exists (`internal/lib/payout/cashfree_provider.go:243`)
   but has **zero callers**. A missed/failed webhook strands the payout forever.
2. **No stored gateway reference** — `InitiateTransfer` returns `resp.ProviderRefID`
   (Cashfree `cf_transfer_id`, already parsed at `cashfree_provider.go:652`), but
   `RequestWithdrawal` discards it. `payments.gateway_payment_id` stays empty, so there is
   nothing internal to reconcile against and `CheckStatus` can't be driven by id.
3. **Webhook likely never configured / secret mismatch** — the route `POST /webhooks/payout`
   IS registered (`webhook_controller.go:38`); the absence of *any* execution row points to
   an ops-side config gap (URL not registered in Cashfree, or signature secret mismatch),
   not a code bug in the handler.

Separately, the UPI transfer was **Rejected** by Cashfree for an *"account configuration
issue"* while IMPS succeeded → **UPI payouts are not enabled/configured** on the Cashfree
account.

---

## Fix overview

| # | Item | Type | Removes |
|---|------|------|---------|
| 1 | Store `gateway_payment_id` at initiation | code | audit gap; unblocks CheckStatus-by-id |
| 2 | Extract shared `finalizePayout()` | code (refactor) | divergent webhook vs reconcile logic |
| 3 | `ReconcilePendingPayouts()` + **cron ticker** + **admin button** | code | "missed webhook = stuck forever" |
| 4 | Verify/fix `CheckStatus` endpoint shape | code | reconciler that can't resolve status |
| 5 | Enable UPI payouts on Cashfree | **ops** | the actual rejection cause |
| 6 | Register webhook URL + verify signing secret | **ops** | why 0 webhooks arrive |

Code items 1–4 make the system self-healing **independent of** whether 5–6 ever get done.
Reconciliation becomes the safety net; the webhook becomes an optimization.

No DB migration required — `payments.gateway_payment_id` and every needed column already exist.

---

## 1. Store the gateway reference at initiation

**File:** `internal/service/payout_service.go` — `RequestWithdrawal`, right after a
successful `InitiateTransfer` (around line 588).

- After `resp, err := s.provider.InitiateTransfer(...)`, when `resp.ProviderRefID != ""`,
  persist it: `s.paymentRepo.SetGatewayRef(ctx, payment.ID, resp.ProviderRefID)`.
- Set it on **all** non-error branches (completed / processing / failed) so we always
  capture what Cashfree gave us.

**File:** `internal/repository/payment_repository.go`

```go
// SetGatewayRef stores the provider's transfer/payout id (Cashfree cf_transfer_id)
// so the payout can later be reconciled via provider.CheckStatus.
func (r *postgresPaymentRepository) SetGatewayRef(ctx context.Context, id uuid.UUID, ref string) error {
    _, err := r.db.ExecContext(ctx,
        `UPDATE payments SET gateway_payment_id = $1, updated_at = now() WHERE id = $2`, ref, id)
    return err
}
```
Add `SetGatewayRef` to the `PaymentRepository` interface (top of the same file).

---

## 2. Extract a shared `finalizePayout()`

**Problem:** the completed/failed/reversed transitions currently live *inside*
`HandlePayoutWebhook` (`payout_service.go:768-`). The reconciler must apply the **exact**
same effects (same reversal ledger, same `<idempotency_key>_failed_reversal`, same
`webhook_executions` dedup). Duplicating that risks divergence and double-credit bugs.

**Change:** pull the `switch status { case "completed" / "failed" / "reversed" }` body out
into one method:

```go
// finalizePayout applies a terminal payout status exactly once. Safe to call from
// both the webhook and the reconciler: the reversal ledger's unique idempotency key
// (`<payment.IdempotencyKey>_failed_reversal`) and the webhook_executions row make it
// idempotent, so concurrent webhook + reconcile can't double-process.
// `source` is "webhook" | "reconcile" (for the execution record / logs).
func (s *payoutService) finalizePayout(ctx context.Context, payment *models.Payment, status, providerError, source string) error
```

- `HandlePayoutWebhook` becomes: fetch payment → validate type → `finalizePayout(..., "webhook")`.
- Keep the existing idempotency check (`GetWebhookExecution`) **inside** `finalizePayout`
  so both entry points share it.
- Preserve the exact ledger shapes we validated by hand:
  - **failed** → `IncrementRetry` (sets `status='failed'`) + `webhook_reversal` credit
    (`+amount`, status `completed`, key `…_failed_reversal`, `reference_type='payment'`).
  - **completed** → `UpdateStatus(completed)` only (no ledger entry). *Optional hygiene:*
    also flip the pending `withdrawal_debit` ledger row → `completed` (cosmetic; balance
    math is unaffected).
  - **reversed** → mirror the existing `case "reversed"` block.

**Acceptance:** `HandlePayoutWebhook` behavior is byte-for-byte unchanged (regression test
below), and `finalizePayout` is now reusable.

---

## 3. Reconciliation: `ReconcilePendingPayouts` + cron + admin button

### 3a. Repository — list stuck payouts

**File:** `internal/repository/payment_repository.go`

```go
// ListStuckPayouts returns payout/withdrawal payments still 'processing' and older
// than `olderThan`, so a reconciler can poll the provider for their real status.
func (r *postgresPaymentRepository) ListStuckPayouts(ctx context.Context, olderThan time.Duration, limit int) ([]*models.Payment, error)
```
Query: `type IN ('payout','withdrawal') AND status='processing' AND updated_at < now()-$1
ORDER BY created_at ASC LIMIT $2`. Add to the interface.

### 3b. Service — the reconciler

**File:** `internal/service/payout_service.go`

```go
// ReconcilePendingPayouts polls the provider for every payout stuck in 'processing'
// past the grace window and finalizes it. Returns a summary for logging/telemetry.
// Skips rows with no gateway ref (nothing to query) and logs them for manual review.
func (s *payoutService) ReconcilePendingPayouts(ctx context.Context) (ReconcileResult, error)
```
Logic per stuck payment:
1. If `gateway_payment_id` is empty → log + skip (can't query; surface in admin).
2. `resp, err := s.provider.CheckStatus(ctx, payment.GatewayPaymentID)`; on transient
   error, skip (retry next tick).
3. Map `resp.Status` → `completed | failed | reversed`; if still `processing`/unknown, skip.
4. `s.finalizePayout(ctx, payment, mapped, resp.Error, "reconcile")`.

`ReconcileResult{ Checked, Finalized, Skipped, Errors int }`.
Add `ReconcilePendingPayouts` to the `PayoutService` interface.

**Grace window / interval:** config-driven (below). Suggest 15 min grace, 5 min tick.

### 3c. Cron ticker

**File:** `cmd/api/main.go` (server bootstrap, after services are constructed).

- Start a goroutine with `time.NewTicker(reconcileInterval)`; each tick calls
  `payoutService.ReconcilePendingPayouts(ctx)` with a timeout context; log the summary.
- Guard with a `PAYOUT_RECONCILE_ENABLED` flag (default true) and stop on server-shutdown
  context cancellation.
- Single-instance note: if the API ever runs multiple replicas, add a DB advisory lock
  (`pg_try_advisory_lock`) around a tick so only one instance reconciles. (PM2 on Render =
  single instance today, so optional now — note it.)

### 3d. Admin endpoint + button

**Backend** — `internal/controller/admin_controller.go`, under the existing
`/admin/payments` route group:
```go
r.Post("/reconcile-payouts", c.ReconcilePayouts) // auth.IsAdmin (group already protected)
```
Handler calls `payoutService.ReconcilePendingPayouts(ctx)` and returns the
`ReconcileResult` as JSON.

**Admin frontend** — `myslotmate-admin`:
- `src/api/payments.ts`: `export function reconcilePayouts(): Promise<ReconcileResult>` →
  `POST /admin/payments/reconcile-payouts`.
- `src/modules/payments/PaymentsDirectory.tsx`: a **"Sync payout status"** button in the
  header that calls it, shows a spinner, then toasts
  `Checked N · Finalized M · Skipped K` and refreshes the list.

---

## 4. Verify / fix `CheckStatus`

**File:** `internal/lib/payout/cashfree_provider.go:243`.

Current: `GET {BaseURL}/payout/transfers/{providerRefID}`. Before trusting the reconciler:
- Confirm against Cashfree Payouts API docs whether get-status is
  `GET /payout/transfers/{cf_transfer_id}` (path) or `GET /payout/transfers?transfer_id={ourId}`
  (query by our client id).
- With item 1 storing `cf_transfer_id`, the path form should work. If not, switch to the
  query-by-`transfer_id` form (we always know our `transfer_id` = `payment.ID`, so this also
  covers historical rows that lack a stored ref).
- Reuse the existing `parseCashfreeTransferResponse` + `mapCashfreeWebhookStatus` so status
  mapping is identical to the webhook path.

**Validation:** a throwaway `cmd/` script (or unit test with a recorded response) that calls
`CheckStatus` for a known transfer id and prints the mapped status.

---

## 5–6. Ops (cannot be done in code)

5. **Enable UPI payouts** on the Cashfree account — the rejection was *"account
   configuration issue"*; IMPS worked, UPI didn't. Cashfree dashboard → contact care.
   Until then, prefer IMPS/bank for payouts.
6. **Register the webhook** — Cashfree dashboard → Developers → Webhooks →
   `https://<api-host>/webhooks/payout`; verify the signing secret matches the
   `CASHFREE_*` webhook secret in the API env. This is the most likely reason 0 webhooks
   have ever arrived. Test with a sandbox transfer and confirm a `webhook_executions` row
   appears.

---

## Config (env)

| Var | Default | Meaning |
|-----|---------|---------|
| `PAYOUT_RECONCILE_ENABLED` | `true` | master switch for the cron |
| `PAYOUT_RECONCILE_INTERVAL` | `5m` | ticker period |
| `PAYOUT_RECONCILE_GRACE` | `15m` | min age before a `processing` payout is polled |

Read via the existing config path (godotenv). Document in `.env.example`.

---

## Testing

- **Regression (must pass unchanged):** existing `HandlePayoutWebhook` tests — failed →
  reversal ledger + `status='failed'`; completed → `status='completed'`, no ledger.
- **`finalizePayout` idempotency:** call twice with `failed` → exactly one reversal ledger
  row (second insert rejected by unique key), balance unchanged.
- **Reconciler:** seed a `processing` payout older than grace with a mocked provider
  returning `failed` → row becomes `failed`, one reversal, host `available` restored.
  Provider returns `completed` → row `completed`, no reversal. Provider returns
  `processing` → untouched. Empty `gateway_payment_id` → skipped, logged.
- **Reconciliation math:** after finalize, assert
  `SumActivePayoutAmountByAccount` drops and `SUM(transaction_ledger)` per account still
  equals the host's owed balance (the invariant from the payment-flow skill).
- **Concurrency:** webhook + reconcile firing together on the same payout → single
  finalize (unique idempotency key holds).

---

## Rollout order

1. Item 1 (store ref) + item 4 (CheckStatus verify) — safe, additive.
2. Item 2 (extract `finalizePayout`) behind full regression tests — behavior-preserving.
3. Item 3d (admin button) — manual, observable, low blast radius. Dry-run on the two
   already-fixed rows: they're terminal, so reconcile should report them skipped/unchanged.
4. Item 3c (cron) last, with `PAYOUT_RECONCILE_ENABLED` to disable fast if noisy.
5. Ops 5–6 in parallel; once the webhook is verified live, the cron is pure backup.

---

## Files touched (summary)

- `internal/service/payout_service.go` — `finalizePayout`, `ReconcilePendingPayouts`,
  store-ref call; interface additions.
- `internal/repository/payment_repository.go` — `SetGatewayRef`, `ListStuckPayouts`;
  interface additions.
- `internal/controller/webhook_controller.go` — none (behavior via service unchanged).
- `internal/controller/admin_controller.go` — `ReconcilePayouts` handler + route.
- `internal/lib/payout/cashfree_provider.go` — verify/fix `CheckStatus` endpoint.
- `cmd/api/main.go` — reconcile ticker goroutine + config.
- `.env.example` — three new vars.
- `myslotmate-admin/src/api/payments.ts` + `src/modules/payments/PaymentsDirectory.tsx` —
  "Sync payout status" button.
- Tests as above.

**No migration.** **No change to `RequestWithdrawal`'s money math.**
