# Premium Hosts and paid Boost: backend

**Source:** *PRD: Premium Hosts and paid Boost marketing* (26 Nov 2026).
**Status:** built in Go (all routes) and Node (Premium gate only). Migration `20261127120000_boost_budget_premium.sql`.

A Boost now has two charges:

- **Ad budget.** The host pays it upfront, plus GST, before go-live. It is not refundable.
- **Marketing fee.** A share of each ticket sold while the Boost is live (band version 2, 4–12%).

Only **Active Premium** hosts can Boost. Premium is free and given by approval.

Boosts that existed before launch stay `pricing_model = 'no_upfront'` on version 1 rates until they end.

## Conventions

These are the same as the existing Boost routes:

- **Response envelope:** `{ success, data, error, message }`. `error` is a stable code; `message` is text for people.
- **Units:** money is in paise (`*_cents`), rates are in basis points.
- **Dates:** budget days are IST calendar days in `YYYY-MM-DD` form, both ends included. Timestamps are RFC 3339.
- **Auth:** host routes take a Firebase or user JWT bearer token. Read access is for the owner or a co-host; write access is for the owner only. Admin routes take an admin token.

## Data model

| Table / column | What it holds |
|---|---|
| `event_boosts.pricing_model` | `no_upfront` \| `budget` |
| `event_boosts.daily_budget_cents`, `budget_from`, `budget_until` | The plan (the request first, then the proposal) |
| `event_boosts.budget_total_cents`, `budget_gst_cents` | Daily × days plus paid top-ups, and its GST |
| `event_boosts.budget_status` | `unpaid` \| `paid` \| `not_required` |
| `event_boosts.payment_due_by` | Accept time + payment window (7 days) |
| `event_boosts.spent_cents`, `closed_at`, `closed_by` | Spend total from the log; close |
| `boost_payments` | One row per budget or top-up payment. `method`: checkout \| payment_link \| offline. `status`: created \| paid \| failed \| expired \| reversed. Each row links to its invoice. |
| `boost_spend_entries` | The team's log of spend, impressions, reach and clicks per (boost, day, channel) |
| `boost_creatives` | Files (JSON list), type, channels, uploader, status, the version it replaces |
| `boost_reports` | `update` \| `final`. A replaced report keeps its row (`replaced_at`) for the history. |
| `premium_memberships` (+ `premium_membership_log`) | Status (`applied` \| `active` \| `declined` \| `removed`), manager, reasons |
| `boost_settings` (one row) | Budget minimums, step, minimum days, payment window, budget GST, `budget_required`, upload limits (MB) |
| `boost_rate_bands` version 2 | 10 / 8 / 7 / 6 / 5% guide rates, 4–12% allowed range |
| `billing_settings.budget_invoice_prefix`, `budget_sac_code` | Budget invoice series and SAC code |
| `invoices.source_type = 'boost_budget'` | Budget tax invoice; `source_id` is the payment |

The migration also:

- drops the old 12–20% `rate_bps` check;
- grants Premium to every host with `boost_invited = true`.

## Rules

**Who can Boost**
- Host request, host accept, host payment and the admin's "Boost an event" all need an Active Premium host (`premium_required`).
- A host removed from Premium keeps a live Boost running to its end date.

**Pricing**
- No_upfront Boosts use band version 1.
- Budget Boosts use the newest version from 2 up. `POST /admin/boost-bands` publishes a new version.

**Budget plan**
- The daily budget must be at least the minimum (₹500) and a multiple of the step (₹100).
- The plan runs at least the minimum days (3).
- The start can't be in the past.
- The end can't be after the event's last upcoming date.

**New requests**
- A request with no budget plan becomes a no_upfront Boost.
- Once `boost_settings.budget_required = true` (the launch switch), requests without a plan are refused (`budget_required`).

**Moving open requests to the new model**
- When an admin re-proposes an open, not-yet-accepted no_upfront Boost with a budget plan, it moves to `budget` with version 2 rates.
- The host is emailed that pricing changed.

**Paying**
- Accepting a budget Boost sets `payment_due_by`.
- If it isn't paid by then, the Boost is cancelled with `ended_reason = payment_expired`, its open payments expire, and nothing is charged.

**Amounts**
- The server always computes the amount from the plan.
- A top-up either adds days (the end date moves, not past the event's last date) or raises the daily budget for the days left. It is allowed only while live.

**Recording a payment**
- Payments are idempotent. Verify, the webhook and a replayed webhook all go through one function, and only the first call moves the payment.
- Money that arrives after the plan expired, or a second budget payment, is recorded and the team is emailed to refund it or restore the Boost by hand.

**Go-live**
- Refused with `budget_unpaid` until the budget is paid.
- Refused with `no_creative` until at least one creative is In use.
- `live_until` defaults to midnight IST after `budget_until`.

**Spend**
- Spend entries are upserts.
- A total above the paid budget is refused (`over_budget`, with the amount left).
- Spend is locked once the Boost is closed.
- Reach and clicks on the Boost are the sums of the log.

**Close**
- Only an ended Boost can be closed, and only with a current final report (`no_final_report`).

**Budget invoice**
- Issued when billing is on **and** `budget_sac_code` is set.
- Payments made before that get their invoice from the background sweep.
- A reversed offline payment gets a credit note.

**Refunds**
- The budget is never refunded.
- The marketing fee works as before: it is taken per booking, returned on a full refund, and kept if the host cancels.

## Website routes (host)

| Method and path | Notes |
|---|---|
| `GET /experiences/{id}/boost/estimate?daily_budget_cents=&from=&until=` | `{days, budget_total_cents, budget_gst_cents, total_payable_cents, last_day, fees[], problems[]}` |
| `POST /experiences/{id}/boost` | Also takes `daily_budget_cents`, `budget_from`, `budget_until` |
| `GET /experiences/{id}/boost`, `GET /host/boosts` | Each Boost adds `pricing_model`, budget fields, `budget_status`, `payment_due_by`, `spent_cents`, `paid_cents`, `closed_at`, `creatives_count`, `creatives_need_action`, `reports_count` |
| `POST /boosts/{id}/accept` | Sets `payment_due_by` on a budget Boost |
| `POST /boosts/{id}/payments` | Body `{kind: "budget"}` or `{kind: "top_up", days_added}` / `{kind: "top_up", daily_budget_cents}`. Returns `{payment_id, key_id, order_id, amount_cents, currency}` for Razorpay checkout. |
| `POST /boosts/{id}/payments/{paymentId}/verify` | `{razorpay_order_id, razorpay_payment_id, razorpay_signature}` → the Boost |
| `GET /boosts/{id}/results` | `{days[], channels[], totals}`. Each day has spend, impressions, reach, clicks and tickets_sold. |
| `POST /boosts/{id}/creatives/upload-url` | `{name, mime_type, size}` → `{upload_url, key, url}`. PUT the file to `upload_url` with the same `Content-Type`. |
| `POST /boosts/{id}/creatives` | `{type, file_keys[], caption?, channels?}` |
| `GET /boosts/{id}/creatives` | Files carry 1-hour signed links |
| `POST /boosts/{id}/creatives/{cid}/approve`, `/request-changes` (`{note}`) | For the team's creatives only |
| `DELETE /boosts/{id}/creatives/{cid}` | The host's own creatives, until In use |
| `GET /boosts/{id}/reports` | Current reports with signed links |
| `GET /boosts/{id}/documents` | Budget invoices `{id, kind, number, amount_cents, gst_cents, issued_at, pdf_url}`. `pdf_url` is the API path `/billing/documents/{id}/pdf` and needs the bearer token. |
| `GET /host/premium` | `{status: none\|applied\|active\|declined\|removed, applied_at, decided_at, manager_name, manager_contact, decline_reason, remove_reason}`. Returns `none`, never a 404. |
| `POST /host/premium/apply` | `{note}`. Only for approved hosts. A declined host waits 30 days. |

`GET /hosts/{id}` (public profile) now includes `is_premium` for the badge.

## Admin routes

| Method and path | Notes |
|---|---|
| `GET /admin/boosts?status=&budget_status=` | List rows carry the budget, spent, paid and `creatives_to_review` fields |
| `GET /admin/boosts/{id}` | Adds `premium`, `payments[]` and `spend[]` |
| `POST /admin/boosts` | Takes the budget plan, like the host request. Needs Premium. |
| `PUT /admin/boosts/{id}/proposal` | Adds `daily_budget_cents`, `budget_from`, `budget_until` and `channels`. A paid plan can't change (`budget_locked`). |
| `POST /admin/boosts/{id}/payment-link` | Body like host payments. Razorpay sends the link by SMS and email; the API also emails it, and sends it on WhatsApp when a template is set. Returns `{payment, url}`. |
| `POST /admin/boosts/{id}/payments/offline` | `{kind, amount_cents (incl. GST, must match), paid_on, reference, …}` |
| `POST /admin/boosts/{id}/payments/{pid}/reverse` | `{reason}`. Only for an offline budget payment, and only before go-live. |
| `POST /admin/boosts/{id}/go-live` | Refused with `budget_unpaid` / `no_creative` |
| `POST /admin/boosts/{id}/end` | `{reason: admin_ended\|budget_spent, note}`. The host sees the note. |
| `PUT /admin/boosts/{id}/spend` | `{entries: [{day, channel, spend_cents, impressions, reach, clicks, note}]}` |
| `GET /admin/boosts/{id}/results` | Same shape as the host's |
| `GET/POST /admin/boosts/{id}/creatives`, `POST …/creatives/upload-url` | The team's uploads; `replaces_id` marks a new version |
| `POST /admin/boosts/{id}/creatives/{cid}/status` | `{status: in_use\|needs_changes\|not_used, note}`. A team creative must be approved by the host first. |
| `GET /admin/boosts/{id}/reports`, `POST …/reports/upload-url`, `POST …/reports` | `{kind, title, covers_from, covers_until, note, file_key}`. The GET includes the history. |
| `PUT /admin/boosts/{id}/reports/{rid}` | Replaces the file or details; the old one is kept in the history |
| `POST /admin/boosts/{id}/close` | Refused with `no_final_report` without a final report |
| `GET /admin/premium?status=` | `{items, counts}` |
| `GET /admin/premium/{hostId}` | `{membership, log}` |
| `POST /admin/premium/{hostId}/approve` | `{manager_name, manager_contact}` |
| `POST /admin/premium/{hostId}/decline` | `{reason}` |
| `POST /admin/premium/{hostId}/remove` | `{reason}` |
| `POST /admin/premium/{hostId}/grant` | `{manager_name, manager_contact}` |
| `GET/POST /admin/boost-bands` | All versions; publish `{bands: [...]}` as the next version |
| `GET/PUT /admin/boost-settings` | See `boost_settings` above |
| `PUT /admin/boost-invites/{hostId}` | Turning it on now also grants Premium. Turning it off does **not** remove Premium; use `/admin/premium/{hostId}/remove`. |

## Webhooks

`POST /webhooks/payment` handles these events:

- **`payment.captured`:** looks up a Boost payment by order id, or by `notes.boost_payment_id`, before the wallet top-up path.
- **`payment_link.paid`:** looks up a Boost payment by link id.

Add `payment_link.paid` to the Razorpay webhook's events.

## Emails and WhatsApp

All of these run from the 15-minute Boost job and go out once each (deduplicated through `event_boost_log`):

- payment reminders 2 and 6 days after accepting;
- an upload reminder when the planned start is within 2 days and there are no creatives;
- a weekly summary while live: spend, reach, clicks, tickets sold, ad cost per ticket;
- an email when 80% of the budget is spent, offering a top-up.

Premium applied / approved / granted / declined / removed emails go to the host; new applications are also emailed to the team.

**WhatsApp payment links:** set `KAPSO_BOOST_PAYMENT_TEMPLATE_NAME` (and `_LANG`, default `en_US`) to an approved template with named body parameters `name`, `event_name`, `amount`, `link` and `expires`. If the name is unset, links go by email only (plus Razorpay's own SMS and email).

## Featured slot

The Node `/home` feed fills the Featured places the admin left empty with live-Boost events of Premium hosts. Admin picks always come first. The website reads Featured from the same feed.

## Node

- Node runs the same migration.
- Boost requests through Node need Premium.
- Node prices only no_upfront Boosts (version 1).
- Node refuses to accept, propose or go live on a budget Boost (`use_website`).

The budget, payment, creatives, reports and Premium routes are Go only. The website and admin call Go; the mobile app has no Boost screens.

## Rollout

1. Run `go run ./cmd/migrate` on prod **before** deploying either backend. The new code reads the new columns, and the payment webhook queries `boost_payments`.
2. Deploy Go and Node together.
3. Set S3 CORS to allow `PUT` with `Content-Type` from the website's origin.
4. In Razorpay, add the `payment_link.paid` webhook event.
5. After the CA signs off, set `billing_settings.budget_sac_code`.
6. At launch:
   - set `boost_settings.budget_required = true`;
   - set `NEXT_PUBLIC_ENABLE_BOOST_PLAN=true` on the website;
   - re-propose open requests with a budget plan.

## Not built

- Server-side checks of video length and aspect ratio. The website warns, and the team marks a wrong file Needs changes.
- A paid Premium plan, and reading spend from the Meta or Google ads APIs. Both are out of scope in the PRD.
