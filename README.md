# MySlotMate — Backend

**A production-grade event booking platform backend built with Go, following Clean Architecture and enterprise design patterns.**

MySlotMate allows users to discover and book event slots, and enables hosts to apply, get admin-approved, and then create and manage events, track earnings, and communicate with attendees — with optional Aadhar-based identity verification, real-time WebSocket updates, and a wallet-based payment/payout system powered by Razorpay (collections) and Cashfree (payouts).

Feature docs: [Premium Hosts and paid Boost](docs/premium-and-paid-boost.md).

---

## Table of Contents

- [Tech Stack](#tech-stack)
- [Architecture](#architecture)
- [Design Patterns](#design-patterns)
- [Project Structure](#project-structure)
- [Database Schema](#database-schema)
- [Payment & Wallet Flow](#payment--wallet-flow)
- [Booking Flow](#booking-flow)
- [Aadhar Verification Flow](#aadhar-verification-flow)
- [Payout Flow](#payout-flow)
- [Fraud Prevention](#fraud-prevention)
- [API Endpoints](#api-endpoints)
- [Getting Started](#getting-started)
- [Configuration](#configuration)
- [Migrations](#migrations)

---

## Tech Stack

| Component | Technology |
|-----------|------------|
| Language | **Go 1.24** |
| HTTP Router | [Chi v5](https://github.com/go-chi/chi) |
| Database | **PostgreSQL** (via [pgx v5](https://github.com/jackc/pgx)) |
| Auth | **Firebase Admin SDK** (ID token verification) |
| File Storage | **AWS S3** (via AWS SDK for Go v2) |
| Real-time | **Socket.IO** ([go-socket.io](https://github.com/googollee/go-socket.io)) |
| Identity (KYC) | **Setu OKYC** (Aadhar verification) |
| Payouts | **Cashfree Payouts API** (bank & UPI transfers) |
| Config | [godotenv](https://github.com/joho/godotenv) (`.env` file) |
| UUID | [google/uuid](https://github.com/google/uuid) |

---

## Architecture

The project follows **Clean Architecture** principles with strict layer separation:

```
┌──────────────────────────────────────────────────────────┐
│                    Transport Layer                        │
│         Router (Chi) → Controllers (HTTP Handlers)       │
├──────────────────────────────────────────────────────────┤
│                  Business Logic Layer                     │
│   Services (Rules, Orchestration, Fee Calculation, etc.) │
├──────────────────────────────────────────────────────────┤
│                   Data Access Layer                       │
│             Repositories (SQL via pgx)                   │
├──────────────────────────────────────────────────────────┤
│                    Infrastructure                         │
│  Worker Pool │ Event Dispatcher │ Socket.IO │ Identity    │
│              │  Payout Provider (Cashfree) │ Storage     │
└──────────────────────────────────────────────────────────┘
                          │
                    ┌─────┴─────┐
                    │ PostgreSQL │
                    └───────────┘
```

### Layer Responsibilities

| Layer | Package | Responsibility |
|-------|---------|---------------|
| **Transport** | `internal/server`, `internal/controller` | HTTP routing, middleware, request decoding, JSON response formatting. **No business logic.** |
| **Business Logic** | `internal/service` | Core rules (overbooking prevention, fee split, wallet debit/credit, verification flow), orchestrates repos + infra. |
| **Data Access** | `internal/repository` | Direct SQL operations. Abstraction over PostgreSQL; mockable for unit tests. |
| **Infrastructure** | `internal/lib/*` | Reusable components — Event Bus, Worker Pool, Identity (KYC), Payout (Cashfree), Storage (AWS S3), Real-time (Socket.IO). |

### Dependency Wiring (Composition Root)

All dependencies are wired in `cmd/api/run.go`:

```
main() → Config → DB → Dispatcher → WorkerPool → Firebase (Auth) → AWS S3
      → Repositories → Identity Provider → Payout Provider (Cashfree)
       → Upload Service → Services → Controllers → Router → HTTP Server
```

---

## Design Patterns

### 1. Singleton
**Where:** `EventDispatcher`, Database Connection  
**Why:** Single instance of the event bus and connection pool across the entire application lifecycle.  
**Implementation:** `sync.Once` in `event.GetDispatcher()`.

### 2. Observer (Pub/Sub)
**Where:** `internal/lib/event/dispatcher.go`  
**Why:** Decouples services. When a booking is created, `BookingService` publishes `BookingCreated` — other subsystems (email, analytics, notifications) subscribe independently without touching booking code.

```go
type Observer interface {
    OnNotify(event EventName, data interface{})
}

// Events: UserCreated, HostCreated, EventCreated, BookingCreated, BookingConfirmed,
//         BookingCancelled, PayoutCompleted, PayoutFailed, PaymentCreated
dispatcher.Subscribe("booking_created", analyticsObserver)
dispatcher.Publish("booking_created", bookingData)
```

### 3. Strategy
**Where:** `internal/lib/identity/` (Aadhar KYC), `internal/lib/payout/` (Cashfree payouts)  
**Why:** Swap providers without changing service code. The service depends on the interface, not the implementation.

```go
// Identity Strategy
type AadharProvider interface {
    InitiateVerification(ctx, aadharNumber) (transactionID, error)
    VerifyOTP(ctx, transactionID, otp) (*AadharVerificationResult, error)
}
// Implementation: SetuAadharProvider (Setu OKYC API)

// Payout Strategy
type Provider interface {
    InitiateTransfer(ctx, TransferRequest) (*TransferResponse, error)
    CheckStatus(ctx, providerRefID) (*TransferResponse, error)
    ValidateWebhookSignature(payload, signature) bool
}
// Implementation: CashfreeProvider (Cashfree Payouts API)
```

### 4. Repository
**Where:** `internal/repository/*.go`  
**Why:** Abstracts database access behind interfaces, enabling service-layer unit tests with mocked repositories.

Repositories: `UserRepository`, `HostRepository`, `EventRepository`, `BookingRepository`, `ReviewRepository`, `InboxRepository`, `AccountRepository`, `PaymentRepository`, `PayoutRepository`, `SupportRepository`, `SavedExperienceRepository`.

### 5. Executor / Worker Pool
**Where:** `internal/lib/worker/pool.go`  
**Why:** Offloads heavy operations (emails, image processing) to background goroutines without blocking HTTP handlers.

```go
pool := worker.NewWorkerPool(5, 100)  // 5 workers, 100-item queue
pool.Start()
pool.Submit(func() { sendWelcomeEmail(user) })
```

Falls back to a standalone goroutine if the queue is full. Supports graceful shutdown with context timeout.

### 6. Factory / Dependency Injection
**Where:** All `New*()` constructors  
**Why:** Encapsulates creation logic; wires dependencies at the composition root (`cmd/api/run.go`).

---

## Project Structure

```
myslotmate-backend/
├── cmd/
│   ├── api/run.go                  # Application entry point & DI wiring
│   ├── checkdb/run.go              # DB connectivity check utility
│   └── migrate/run.go              # Migration runner
├── config/
│   └── firebase-service-account.json
├── internal/
│   ├── auth/
│   │   ├── handler.go              # Firebase ID token verification
│   │   └── admin.go                # IsAdmin middleware (Firebase token + email check)
│   ├── config/
│   │   └── config.go               # Env-based configuration loader
│   ├── controller/                 # HTTP handlers (transport layer)
│   │   ├── admin_controller.go     # Admin: approve/reject host applications
│   │   ├── booking_controller.go   # Create/confirm/cancel bookings
│   │   ├── event_controller.go     # Full event CRUD, publish/pause/resume, calendar
│   │   ├── host_controller.go      # Host application flow, profile, dashboard
│   │   ├── inbox_controller.go     # Multi-party messaging, broadcast, mark-read
│   │   ├── payout_controller.go    # Payout methods, withdrawals, earnings
│   │   ├── response.go             # Standardized JSON response helpers
│   │   ├── review_controller.go    # Submit/list reviews with photo URLs
│   │   ├── support_controller.go   # Support tickets with evidence upload
│   │   ├── upload_controller.go    # Generic file upload endpoint (AWS S3)
│   │   ├── user_controller.go      # Signup, Aadhar, profile, saved experiences
│   │   └── webhook_controller.go   # Cashfree payout webhooks + Razorpay payment webhooks
│   ├── db/
│   │   └── db.go                   # PostgreSQL connection (pgx)
│   ├── firebase/
│   │   └── firebase.go             # Firebase Admin SDK (Auth only)
│   ├── lib/
│   │   ├── event/
│   │   │   └── dispatcher.go       # Singleton Observer (event bus)
│   │   ├── identity/
│   │   │   ├── aadhar_provider.go  # Strategy interface (KYC)
│   │   │   └── setu_provider.go    # Setu OKYC implementation
│   │   ├── payout/
│   │   │   ├── provider.go         # Strategy interface (payouts)
│   │   │   ├── cashfree_provider.go# Cashfree Payouts API implementation
│   │   ├── realtime/
│   │   │   └── socket_service.go   # Socket.IO server
│   │   ├── storage/
│   │   │   └── s3_storage.go       # AWS S3 file upload service
│   │   └── worker/
│   │       └── pool.go             # Background worker pool (executor)
│   ├── models/                     # Domain structs & enums
│   │   ├── account.go              # Wallet account
│   │   ├── booking.go              # Booking with fee breakdown
│   │   ├── enums.go                # All enum types
│   │   ├── event.go                # Event (experience) with full listing details
│   │   ├── fraud.go                # Fraud flags
│   │   ├── host_earnings.go        # Aggregate earnings
│   │   ├── host.go                 # Host profile with application fields
│   │   ├── inbox.go                # Multi-party inbox messages
│   │   ├── payment.go              # Transaction ledger
│   │   ├── payout_method.go        # Bank/UPI payout methods
│   │   ├── platform_settings.go    # Fee config
│   │   ├── review.go               # Reviews with ratings & photos
│   │   ├── saved_experience.go     # User-saved experiences
│   │   ├── support.go              # Support tickets with evidence
│   │   └── user.go                 # User with avatar & city
│   ├── repository/                 # Data access layer (SQL)
│   │   ├── account_repository.go   # Wallet CRUD, credit/debit
│   │   ├── booking_repository.go   # Booking CRUD, status updates
│   │   ├── errors.go               # Sentinel errors (ErrInsufficientBalance, etc.)
│   │   ├── event_repository.go     # Event CRUD with filtered search
│   │   ├── host_repository.go      # Host CRUD with application flow
│   │   ├── inbox_repository.go     # Inbox messages
│   │   ├── payment_repository.go   # Payment/transaction ledger
│   │   ├── payout_repository.go    # Payout methods, host earnings, platform settings
│   │   ├── review_repository.go    # Review CRUD with photo URLs
│   │   ├── support_repository.go   # Support ticket CRUD with evidence
│   │   ├── saved_experience_repository.go # Saved experiences
│   │   └── user_repository.go      # User CRUD
│   ├── server/
│   │   └── router.go               # Chi router, middleware, route mounting
│   └── service/                    # Business logic layer
│       ├── booking_service.go      # Booking with wallet debit/credit, fee split
│       ├── event_service.go        # Event management with publish/pause/resume
│       ├── host_service.go         # Host application & profile management
│       ├── inbox_service.go        # Multi-party messaging & broadcast
│       ├── payout_service.go       # Withdrawal, earnings, webhook handling
│       ├── review_service.go       # Review management with photos
│       ├── support_service.go      # Support tickets with report fields
│       └── user_service.go         # Signup, Aadhar, profile, saved experiences
└── migrations/                     # PostgreSQL migration files
    ├── 20260228120000_init_schema.sql
    ├── 20260228130000_add_processing_status.sql
    ├── 20260228130001_earnings_payouts_schema.sql
    ├── 20260307120000_figma_schema_expansion.sql
    ├── 20260307130000_support_evidence_upload.sql
    └── 20260307130001_review_photo_urls.sql
```

---

## Database Schema

### Entity Relationship Diagram

```
┌──────────┐     1:1      ┌──────────┐      1:N      ┌──────────┐
│   User   │─────────────▶│   Host   │──────────────▶│  Event   │
│          │  (verified)   │          │   (host_id)   │          │
└────┬─────┘               └────┬─────┘               └────┬─────┘
     │                          │                          │
     │ 1:1                      │ 1:1                      │ 1:N
     ▼                          ▼                          ▼
┌──────────┐            ┌────────────┐             ┌──────────┐
│ Account  │            │HostEarnings│             │ Booking  │◀── User (N:1)
│ (wallet) │            └────────────┘             └────┬─────┘
└──────────┘                    │                       │
                                │ 1:N                   │ 1:1
                                ▼                       ▼
                        ┌──────────────┐          ┌──────────┐
                        │ PayoutMethod │          │ Payment  │
                        └──────────────┘          └──────────┘

  Event ◀──1:N── Review ──N:1──▶ User
  Event ◀──1:N── InboxMessage (multi-party: host/guest/system)
  User  ◀──1:N── SupportTicket (with evidence uploads)
  User  ◀──1:N── SavedExperience ──N:1──▶ Event
  User  ◀──1:N── FraudFlag
```

### Tables

#### `users`
| Column | Type | Constraints |
|--------|------|-------------|
| `id` | UUID | PK, default `uuid_generate_v4()` |
| `auth_uid` | VARCHAR | UNIQUE, NOT NULL |
| `name` | VARCHAR | NOT NULL |
| `phn_number` | VARCHAR | |
| `email` | VARCHAR | NOT NULL |
| `avatar_url` | VARCHAR | Profile picture URL |
| `city` | VARCHAR | User's city |
| `account_id` | UUID | FK → `accounts` |
| `is_verified` | BOOLEAN | DEFAULT `false` |
| `verified_at` | TIMESTAMPTZ | |
| `created_at` | TIMESTAMPTZ | DEFAULT `now()` |
| `updated_at` | TIMESTAMPTZ | DEFAULT `now()` |

#### `hosts`
| Column | Type | Constraints |
|--------|------|-------------|
| `id` | UUID | PK |
| `user_id` | UUID | UNIQUE, FK → `users` |
| `account_id` | UUID | FK → `accounts` |
| `first_name` | VARCHAR | NOT NULL |
| `last_name` | VARCHAR | NOT NULL |
| `phn_number` | VARCHAR | |
| `city` | VARCHAR | |
| `avatar_url` | VARCHAR | Profile image URL |
| `tagline` | VARCHAR | Short tagline |
| `bio` | TEXT | Host bio |
| `application_status` | ENUM | `draft` \| `pending` \| `under_review` \| `approved` \| `rejected` |
| `experience_desc` | TEXT | "What Experiences will you Host?" |
| `moods` | TEXT[] | e.g. `["adventure","social","wellness"]` |
| `description` | TEXT | 300-char host description |
| `preferred_days` | TEXT[] | e.g. `["mon","tue","wed"]` |
| `group_size` | INT | Approximate group size |
| `government_id_url` | VARCHAR | Uploaded ID doc URL |
| `submitted_at` | TIMESTAMPTZ | Application submission time |
| `approved_at` | TIMESTAMPTZ | |
| `rejected_at` | TIMESTAMPTZ | |
| `is_identity_verified` | BOOLEAN | Trust badge |
| `is_email_verified` | BOOLEAN | Trust badge |
| `is_phone_verified` | BOOLEAN | Trust badge |
| `is_super_host` | BOOLEAN | Trust badge |
| `is_community_champ` | BOOLEAN | Trust badge |
| `expertise_tags` | TEXT[] | e.g. `["#Minimalism","#Wellness"]` |
| `social_instagram` | VARCHAR | Instagram profile link |
| `social_linkedin` | VARCHAR | LinkedIn profile link |
| `social_website` | VARCHAR | Personal website |
| `avg_rating` | FLOAT | Denormalized average rating |
| `total_reviews` | INT | DEFAULT `0` |
| `created_at` | TIMESTAMPTZ | |
| `updated_at` | TIMESTAMPTZ | |

> **Approval Rule:** Users can submit host applications without pre-verification. On admin approval, `users.is_verified` and `hosts.is_identity_verified` are set to `true`.

#### `accounts` (Wallet)
| Column | Type | Constraints |
|--------|------|-------------|
| `id` | UUID | PK |
| `owner_type` | ENUM | `user` \| `host` |
| `owner_id` | UUID | UNIQUE per `(owner_type, owner_id)` |
| `balance_cents` | BIGINT | CHECK `≥ 0` |
| `bank_details` | JSONB | |

> **Auto-created** via triggers on `users` and `hosts` insert. Wallet can never go negative.

#### `events`
| Column | Type | Constraints |
|--------|------|-------------|
| `id` | UUID | PK |
| `host_id` | UUID | FK → `hosts` |
| `title` | VARCHAR | NOT NULL |
| `hook_line` | VARCHAR | Short hook line |
| `mood` | ENUM | `adventurous` \| `relaxing` \| `creative` \| `social` \| `educational` \| `wellness` \| `culinary` \| `cultural` |
| `description` | TEXT | |
| `cover_image_url` | VARCHAR | Cover image URL |
| `gallery_urls` | TEXT[] | Gallery image URLs |
| `is_online` | BOOLEAN | DEFAULT `false` |
| `location` | VARCHAR | Address/landmark |
| `location_lat` | FLOAT | Latitude |
| `location_lng` | FLOAT | Longitude |
| `duration_minutes` | INT | |
| `min_group_size` | INT | |
| `max_group_size` | INT | |
| `capacity` | INT | CHECK `≥ 0` (overbooking prevention) |
| `price_cents` | BIGINT | Per guest; NULL = free |
| `is_free` | BOOLEAN | DEFAULT `false` |
| `time` | TIMESTAMPTZ | NOT NULL |
| `end_time` | TIMESTAMPTZ | |
| `is_recurring` | BOOLEAN | DEFAULT `false` |
| `recurrence_rule` | VARCHAR | iCal rule, e.g. `FREQ=WEEKLY;BYDAY=MO` |
| `cancellation_policy` | ENUM | `flexible` \| `moderate` \| `strict` |
| `status` | ENUM | `draft` \| `live` \| `paused` |
| `published_at` | TIMESTAMPTZ | |
| `paused_at` | TIMESTAMPTZ | |
| `ai_suggestion` | TEXT | |
| `avg_rating` | FLOAT | Denormalized |
| `total_bookings` | INT | DEFAULT `0` |
| `created_at` | TIMESTAMPTZ | |
| `updated_at` | TIMESTAMPTZ | |

#### `bookings`
| Column | Type | Constraints |
|--------|------|-------------|
| `id` | UUID | PK |
| `event_id` | UUID | FK → `events` |
| `user_id` | UUID | FK → `users` |
| `quantity` | INT | CHECK `> 0` |
| `status` | ENUM | `pending` \| `confirmed` \| `cancelled` \| `refunded` |
| `payment_id` | UUID | FK → `payments` |
| `idempotency_key` | VARCHAR | UNIQUE |
| `amount_cents` | BIGINT | Total booking value |
| `service_fee_cents` | BIGINT | Platform fee (15%) |
| `net_earning_cents` | BIGINT | Host net (85%) |
| `cancelled_at` | TIMESTAMPTZ | Set when status → cancelled |

> **Overbooking prevention:** Service layer checks `SUM(quantity) WHERE status IN ('pending','confirmed') < event.capacity` before confirming.

#### `payments` (Transaction Ledger)
| Column | Type | Constraints |
|--------|------|-------------|
| `id` | UUID | PK |
| `idempotency_key` | VARCHAR | UNIQUE |
| `account_id` | UUID | FK → `accounts` |
| `type` | ENUM | `booking` \| `withdrawal` \| `refund` \| `payout` \| `topup` |
| `reference_id` | UUID | e.g. Booking ID |
| `amount_cents` | BIGINT | |
| `status` | ENUM | `pending` \| `processing` \| `completed` \| `failed` \| `reversed` |
| `payout_method_id` | UUID | FK → `payout_methods` |
| `display_reference` | VARCHAR | Human-readable (e.g. `TXN-88234`) |
| `retry_count` | INT | DEFAULT `0` |
| `last_error` | TEXT | |

#### `payout_methods`
| Column | Type | Constraints |
|--------|------|-------------|
| `id` | UUID | PK |
| `host_id` | UUID | FK → `hosts` |
| `type` | ENUM | `bank` \| `upi` |
| `bank_name` | VARCHAR | Required for type=bank |
| `account_type` | VARCHAR | `checking` \| `savings` |
| `last_four_digits` | VARCHAR | Masked display |
| `account_number_encrypted` | TEXT | Encrypted (never exposed in JSON) |
| `ifsc` | VARCHAR | Bank IFSC code |
| `beneficiary_name` | VARCHAR | Account holder name |
| `upi_id` | VARCHAR | Required for type=upi (e.g. `user@okhdfc`) |
| `is_verified` | BOOLEAN | |
| `is_primary` | BOOLEAN | |

#### `host_earnings`
| Column | Type | Constraints |
|--------|------|-------------|
| `id` | UUID | PK |
| `host_id` | UUID | UNIQUE, FK → `hosts` |
| `total_earnings_cents` | BIGINT | Lifetime earnings |
| `pending_clearance_cents` | BIGINT | Funds awaiting clearance |
| `estimated_clearance_at` | TIMESTAMPTZ | |

> **Auto-created** via trigger on `hosts` insert.

#### `platform_settings`
| Column | Type | Constraints |
|--------|------|-------------|
| `id` | UUID | PK |
| `key` | VARCHAR | UNIQUE |
| `value` | JSONB | |

> Seeded: `platform_fee → { host_percentage: 70, platform_percentage: 30 }`

#### `reviews`
| Column | Type | Constraints |
|--------|------|-------------|
| `id` | UUID | PK |
| `event_id` | UUID | FK → `events` |
| `user_id` | UUID | FK → `users` |
| `rating` | INT | 1–5 stars, NOT NULL |
| `name` | VARCHAR | Reviewer display name |
| `description` | TEXT | NOT NULL |
| `photo_urls` | TEXT[] | Uploaded review photos |
| `reply` | TEXT[] | Host replies |
| `sentiment_score` | FLOAT | AI-generated |
| `created_at` | TIMESTAMPTZ | |
| `updated_at` | TIMESTAMPTZ | |

#### `inbox_messages`
| Column | Type | Constraints |
|--------|------|-------------|
| `id` | UUID | PK |
| `event_id` | UUID | FK → `events` |
| `sender_type` | ENUM | `system` \| `host` \| `guest` |
| `sender_id` | UUID | FK → `users` or `hosts`; NULL for system |
| `message` | TEXT | NOT NULL |
| `attachment_url` | VARCHAR | Attached file URL |
| `is_read` | BOOLEAN | DEFAULT `false` |
| `created_at` | TIMESTAMPTZ | |

#### `support_tickets`
| Column | Type | Constraints |
|--------|------|-------------|
| `id` | UUID | PK |
| `user_id` | UUID | FK → `users` |
| `category` | ENUM | `report_participant` \| `technical_support` \| `policy_help` |
| `reported_user_id` | UUID | FK → `users`; for report_participant |
| `subject` | VARCHAR | |
| `messages` | JSONB | `[{ sender, text, created_at }]` |
| `status` | ENUM | `open` \| `in_progress` \| `resolved` \| `closed` |
| `event_id` | UUID | FK → `events`; for report context |
| `session_date` | DATE | Session date for the report |
| `report_reason` | ENUM | `verbal_harassment` \| `safety_concern` \| `inappropriate_behavior` \| `spam_or_scam` |
| `evidence_urls` | TEXT[] | Uploaded evidence file URLs |
| `is_urgent` | BOOLEAN | DEFAULT `false`; urgent safety concern toggle |
| `created_at` | TIMESTAMPTZ | |
| `updated_at` | TIMESTAMPTZ | |

#### `saved_experiences`
| Column | Type | Constraints |
|--------|------|-------------|
| `id` | UUID | PK |
| `user_id` | UUID | FK → `users` |
| `event_id` | UUID | FK → `events`; UNIQUE `(user_id, event_id)` |
| `saved_at` | TIMESTAMPTZ | DEFAULT `now()` |

#### `fraud_flags`
| Column | Type | Constraints |
|--------|------|-------------|
| `id` | UUID | PK |
| `user_id` | UUID | FK → `users` |
| `type` | ENUM | `abnormal_booking_spike` \| `payment_abuse` \| `suspicious_activity` \| `manual_block` |
| `reason` | TEXT | |
| `blocked_at` | TIMESTAMPTZ | |
| `blocked_until` | TIMESTAMPTZ | Nullable — null = indefinite |
| `is_active` | BOOLEAN | |

### Enums

| Enum Type | Values |
|-----------|--------|
| `account_owner_type` | `user`, `host` |
| `booking_status` | `pending`, `confirmed`, `cancelled`, `refunded` |
| `payment_type` | `booking`, `withdrawal`, `refund`, `payout`, `topup` |
| `payment_status` | `pending`, `processing`, `completed`, `failed`, `reversed` |
| `payout_method_type` | `bank`, `upi` |
| `host_application_status` | `draft`, `pending`, `under_review`, `approved`, `rejected` |
| `event_status` | `draft`, `live`, `paused` |
| `event_mood` | `adventurous`, `relaxing`, `creative`, `social`, `educational`, `wellness`, `culinary`, `cultural` |
| `cancellation_policy` | `flexible`, `moderate`, `strict` |
| `support_ticket_status` | `open`, `in_progress`, `resolved`, `closed` |
| `support_category` | `report_participant`, `technical_support`, `policy_help` |
| `report_reason` | `verbal_harassment`, `safety_concern`, `inappropriate_behavior`, `spam_or_scam` |
| `message_sender_type` | `system`, `host`, `guest` |
| `fraud_flag_type` | `abnormal_booking_spike`, `payment_abuse`, `suspicious_activity`, `manual_block` |

### Database Triggers

| Trigger | Purpose |
|---------|---------|
| `set_updated_at()` | Auto-updates `updated_at` on row modification (all tables) |
| `create_user_account()` | Auto-creates a wallet `Account` when a `User` is inserted |
| `create_host_account()` | Auto-creates a wallet `Account` when a `Host` is inserted |
| `create_host_earnings()` | Auto-creates a `HostEarnings` row when a `Host` is inserted |

---

## Payment & Wallet Flow

MySlotMate uses an **immutable transaction ledger** financial architecture (similar to Swiggy, Zomato, and Stripe merchants). This design ensures:

- **Tamper-proof**: No direct balance updates; balance = SUM(ledger entries)
- **Auditability**: Every money movement is a permanent record with full context
- **Idempotency**: Webhook replays are safely ignored via UNIQUE constraints
- **Reconciliation**: Daily verification detects discrepancies and fraud

```
User Wallet  ◄──booking_credit──  Ledger ──webhook_reversal──► Host Wallet
   (credits)                      (append-only)               (debits on failure)
                                        │
                                  ┌─────┴──────┐
                                  │ Immutable  │
                                  │ Journal    │
                                  │ (SUM=bal)  │
                                  └────────────┘
```

### Core Financial Concepts

| Entity | Purpose |
|--------|---------|
| **Account** | Wallet per user/host/platform. `balance_cents` is **calculated** as SUM(ledger entries), not stored. Auto-created via DB trigger. |
| **TransactionLedger** | **Immutable append-only journal** of all money movements. Each entry is permanent and includes idempotency_key, status, and audit trail. |
| **LedgerTransactionType** | booking_credit, platform_fee_credit, withdrawal_debit, webhook_reversal, manual_credit/debit, etc. |
| **Payment** | Legacy payment tracking (being phased to ledger). Maintains status for provider integration and retry logic. |
| **WebhookExecution** | Tracks processed webhooks with UNIQUE(event_type, provider_id, external_event_id) to prevent replays. |
| **Booking** | Stores fee breakdown: `amount_cents`, `service_fee_cents` (15%), `net_earning_cents` (85%). Triggers 4 ledger entries on creation. |
| **HostEarnings** | Aggregate view: `total_earnings_cents`, `pending_clearance_cents`. Auto-created via DB trigger. |
| **PayoutMethod** | Host's bank account or UPI ID for withdrawals. Supports multiple methods with one primary. |
| **PlatformSettings** | Configurable fee split (default 85% host / 15% platform). |

### Ledger Transaction Types

| Type | Direction | Amount | Description |
|------|-----------|--------|-------------|
| `booking_credit` | User → System → Host | Both ± | User pays for event; platform takes fee; host keeps net. **4 entries per booking**: (1) User debit -total, (2) Platform credit +total, (3) Platform fee debit -fee, (4) Host credit +net. |
| `platform_fee_credit` | User earnings → Platform | Negative | Fee earned by platform on booking (implicit from booking_credit split). |
| `webhook_reversal` | Host balance ← Ledger | Positive | Payout failed/reversed at provider; host automatically credited back. Created by webhook handler. |
| `refund_credit` | Host → User | Positive | Full refund when booking is cancelled. |
| `withdrawal_debit` | Host wallet → External | Negative | Host initiates withdrawal request. Stays pending until Cashfree confirms. |
| `manual_credit` / `manual_debit` | Admin → Account | Both ± | Admin adjustment (with audit trail & approval). |

### Ledger Status Lifecycle

```
pending ──► completed (booking confirmed, payout success)
pending ──► reversed   (booking cancelled, payout failed)

Status in Ledger:      Status in Payment (legacy):
pending                processing
completed              completed / success
failed/reversed        failed / reversed
```

### Immutable Ledger Properties

- **Append-only**: No updates or deletes. Corrections are made via reversals.
- **Idempotency**: Each entry includes `idempotency_key` to prevent duplicates.
- **Balance Calculated**: Not stored. Balance = SUM(ledger.amount_cents) WHERE account_id = X.
- **Balance Verification**: Daily reconciliation checks: stored_balance_cents == calculated_balance_cents.
- **Audit Trail**: `created_by` (user/admin/system), `created_at`, `metadata`, `external_reference_id` (provider IDs).
- **Reference Tracking**: `reference_id` + `reference_type` links entries to bookings, payments, etc.

### Legacy Payment Types (Phasing Out)

| Type | Direction | Description |
|------|-----------|-------------|
| `topup` | External → User wallet | User adds money to wallet (via Razorpay) |
| `booking` | User wallet → Host wallet | Booking payment (now uses ledger entries) |
| `refund` | Reverse booking | Cancelled booking refund |
| `payout` | Host wallet → Bank/UPI | Host withdrawal via Cashfree |

### Safety Features (Ledger-Based)

- **Tamper Detection**: Any direct database modification causes balance mismatch during daily reconciliation.
- **Overdraft Protection**: Ledger entries are created atomically with idempotency; if withdrawal > balance calculated from ledger, the ledger entry is created but becomes "pending" until confirmed.
- **Webhook Idempotency**: `WebhookExecution` table with UNIQUE(event_type, provider_id, external_event_id) — webhook processed at most once.
- **Automatic Reversals**: On payout failure/reversal, webhook handler automatically creates webhook_reversal ledger entry to credit host.
- **Fraud Checks**: Active fraud flags still block booking and payment operations (checked before ledger entry creation).

---

## Transaction Ledger System (Immutable Architecture)

### Design Philosophy

MySlotMate implements a **transaction ledger system** (inspired by Swiggy, Zomato, Stripe) to ensure:

1. **No balance tampering**: Balance cannot be directly edited; it's **calculated** from ledger entries.
2. **Complete auditability**: Every money movement creates a permanent record with full context.
3. **Replay-safe webhooks**: UNIQUE constraints prevent the same webhook from being processed twice.
4. **Reconciliation**: Daily jobs verify ledger sum matches stored balance; discrepancies detected immediately.

### Architecture Diagram

```
┌─────────────────────────────────────────┐
│         User makes Booking              │
│  (POST /bookings with idempotency_key)  │
└───────────────────┬─────────────────────┘
                    │
                    ▼
        ┌─────────────────────────┐
        │  Create 4 Ledger Entries │ (atomic via idempotency_key)
        │  1. User debit -total   │
        │  2. Platform credit     │
        │  3. Platform fee debit  │
        │  4. Host credit +net    │
        └─────────────────────────┘
                    │
                    ▼
        ┌──────────────────────┐
        │   TransactionLedger  │ (append-only)
        │  (immutable journal) │
        └──────────────────────┘
                    │
                    ▼
        ┌──────────────────────┐
        │  Get Current Balance │
        │ SUM(ledger entries)  │
        │  WHERE account_id=X  │
        └──────────────────────┘
```

### Example: Booking Creation Creates 4 Ledger Entries

**Scenario**: User books 2 tickets @ 1000 cents each = 2000 cents total.  
**Fee split**: 15% platform, 85% host.

| Entry | Type | Amount | Account | Idempotency Key | Status | Purpose |
|-------|------|--------|---------|-----------------|--------|---------|
| 1 | booking_credit | -2000 | User | `booking_X_v1` | completed | User pays |
| 2 | booking_credit | +2000 | Platform | `booking_X_v1` | completed | Platform receives |
| 3 | platform_fee_credit | -300 | Platform | `booking_X_v1` | completed | Fee earned (15%) |
| 4 | booking_credit | +1700 | Host | `booking_X_v1` | completed | Host earning (85%) |

All 4 entries share the same `idempotency_key` → guaranteed atomic grouping.  
If the request is retried, the service looks up by key and returns existing entries.

### Webhook Idempotency (Preventing Replays)

```
Cashfree sends: POST /webhooks/payout with transfer_success event (event_id=ABC123)
Webhook Handler:
  1. Check WebhookExecution WHERE event_type='cashfree_payout' AND external_event_id='ABC123'
  2. If exists → already processed, return early (idempotent)
  3. If new → Process webhook:
       - Create withdrawal_complete ledger entry
       - Update payment status
       - Record in WebhookExecution (this insert fails if already exists — UNIQUE constraint)
  4. Next retry of same webhook → Step 1 finds it, returns early

Result: Same webhook can be delivered 1x, 10x, or 100x — always processed exactly once.
```

### Reconciliation Job (Daily Balance Verification)

```
Every 24 hours:
  1. For each account:
       - calculated_balance = SUM(ledger.amount_cents) WHERE account_id = X
       - stored_balance = account.balance_cents
  2. If calculated_balance == stored_balance:
       - ✓ MATCHED
  3. If mismatch:
       - ✗ DISCREPANCY DETECTED
       - INSERT INTO reconciliation_discrepancies
       - Flag for manual investigation

Result: Any direct database tampering is detected within 24 hours.
No way to hide forged money — ledger and stored balance must match!
```

### Data Model

**TransactionLedger Table**:
```sql
CREATE TABLE transaction_ledger (
  id UUID PRIMARY KEY,
  account_id UUID NOT NULL,
  type VARCHAR NOT NULL,                    -- booking_credit, withdrawal_debit, etc.
  amount_cents BIGINT NOT NULL,             -- ±, relative to account
  reference_id UUID,                        -- booking.id, payment.id, etc.
  reference_type VARCHAR,                   -- "booking", "payment", "account"
  idempotency_key VARCHAR UNIQUE,           -- group related entries
  description TEXT,
  balance_after_cents BIGINT,               -- calculated at insert time
  external_reference_id VARCHAR,            -- Cashfree/Razorpay ID
  reversal_of_ledger_id UUID,              -- links reversal to original
  status VARCHAR,                           -- pending, completed, failed, reversed
  metadata JSONB,
  created_at TIMESTAMPTZ,
  created_by UUID                           -- user/admin/system
  -- Indexes: account_id, reference_id, created_at DESC, idempotency_key, status
);
```

**WebhookExecution Table** (prevents replays):
```sql
CREATE TABLE webhook_executions (
  id UUID PRIMARY KEY,
  event_type VARCHAR NOT NULL,              -- "cashfree_payout", "razorpay_payment"
  provider_id VARCHAR NOT NULL,             -- "cashfree", "razorpay"
  external_event_id VARCHAR NOT NULL,       -- provider's unique event ID
  idempotency_key VARCHAR NOT NULL UNIQUE,  -- Ensures at-most-once processing
  ledger_id UUID,                           -- linked ledger entry created
  status VARCHAR,                           -- "success", "failed", "skipped"
  error_message TEXT,
  raw_payload JSONB,
  received_at TIMESTAMPTZ,
  processed_at TIMESTAMPTZ,
  -- UNIQUE(event_type, provider_id, external_event_id) ensures no duplicates
);
```

**ReconciliationRun Table** (daily verification):
```sql
CREATE TABLE reconciliation_runs (
  id UUID PRIMARY KEY,
  run_date TIMESTAMPTZ,
  total_accounts_checked INT,
  accounts_matched INT,
  accounts_discrepancy INT,
  total_ledger_amount_cents BIGINT,
  total_balance_amount_cents BIGINT,
  status VARCHAR
);
```

---

---

## Booking Flow

### Creating a Booking (Ledger-Based)

```
1.  User sends POST /bookings with event_id, quantity, and idempotency_key
2.  Service checks idempotency via ledger:
      SELECT FROM transaction_ledger WHERE idempotency_key = ?
      If found → return existing booking (fully idempotent)
3.  Service checks for active fraud flags on the user
4.  Service checks event capacity:
      SUM(quantity) WHERE status IN ('pending','confirmed') + new_quantity ≤ capacity
5.  Fee calculated from PlatformSettings (default 85/15):
      total    = price × quantity
      fee      = total × 15%
      net      = total − fee    (85%)
6.  CREATE 4 IMMUTABLE LEDGER ENTRIES (atomic via idempotency_key):
      Entry 1: User wallet DEBIT
        type=booking_credit, amount_cents=-total, status=completed
      Entry 2: Platform CREDIT  
        type=booking_credit, amount_cents=+total, status=completed
      Entry 3: Platform FEE DEBIT
        type=platform_fee_credit, amount_cents=-fee, status=completed
      Entry 4: Host wallet CREDIT
        type=booking_credit, amount_cents=+net, status=completed
7.  Booking created with status=confirmed
8.  BookingCreated event published (Observer pattern)
9.  Balance calculated on-the-fly: User balance = SUM(ledger) WHERE account_id=user_id
```

### Error Handling (Automatic Reversal)

If any step after ledger creation fails (e.g., database connection lost):
- Automatic reversal entry created: type=webhook_reversal, amount_cents=+total
- User is NOT double-charged — ledger sum remains correct
- System operator can investigate the discrepancy in `reconciliation_discrepancies` table

### Confirming a Booking

```
1.  POST /bookings/{bookingID}/confirm
2.  Booking status → confirmed
3.  BookingConfirmed event published
```

### Cancelling a Booking (Full Refund via Ledger)

```
1.  POST /bookings/{bookingID}/cancel
2.  Booking.status → cancelled, cancelled_at = now
3.  CREATE REVERSAL LEDGER ENTRIES:
      Entry 1: User wallet CREDIT (refund)
        type=refund_credit, amount_cents=+total, status=completed
      Entry 2: Host wallet DEBIT (reversal of earnings)
        type=refund_credit, amount_cents=-net, status=completed
      Entry 3: Platform FEE REVERSAL
        type=refund_credit, amount_cents=+fee, status=completed
4.  HostEarnings.pending_clearance_cents adjusted
5.  BookingCancelled event published
6.  Balance updates automatically: ledger sum changes, balance recalculated
```

---

## Aadhar Verification Flow

This flow demonstrates the interaction between multiple layers and design patterns (Strategy, Observer, Worker Pool, Repository).

```
┌────────┐          ┌────────────┐         ┌─────────────┐        ┌──────────────┐
│ Client │          │ Controller │         │   Service   │        │ SetuProvider │
└───┬────┘          └─────┬──────┘         └──────┬──────┘        └──────┬───────┘
    │                     │                       │                      │
    │ POST /auth/verify-  │                       │                      │
    │  aadhar/init        │                       │                      │
    │────────────────────►│                       │                      │
    │                     │  UserService.         │                      │
    │                     │  InitiateVerification │                      │
    │                     │──────────────────────►│                      │
    │                     │                       │  Check if already    │
    │                     │                       │  verified (UserRepo) │
    │                     │                       │──────┐               │
    │                     │                       │◄─────┘               │
    │                     │                       │                      │
    │                     │                       │  AadharProvider.     │
    │                     │                       │  InitiateVerification│
    │                     │                       │─────────────────────►│
    │                     │                       │                      │ Call Setu
    │                     │                       │                      │ OKYC API
    │                     │                       │◄─────────────────────│
    │                     │◄──────────────────────│                      │
    │◄────────────────────│  { transaction_id }   │                      │
    │                     │                       │                      │
    │ POST /auth/verify-  │                       │                      │
    │  aadhar/complete    │                       │                      │
    │  (transaction_id    │                       │                      │
    │   + OTP)            │                       │                      │
    │────────────────────►│                       │                      │
    │                     │  VerifyOTP            │                      │
    │                     │──────────────────────►│                      │
    │                     │                       │  AadharProvider.     │
    │                     │                       │  VerifyOTP           │
    │                     │                       │─────────────────────►│
    │                     │                       │◄─────────────────────│
    │                     │                       │                      │
    │                     │                       │  UserRepo.           │
    │                     │                       │  SetVerified(true)   │
    │                     │                       │──────┐               │
    │                     │                       │◄─────┘               │
    │                     │                       │                      │
    │                     │                       │  Publish             │
    │                     │                       │  UserVerified event  │
    │                     │                       │──────┐               │
    │                     │                       │◄─────┘               │
    │                     │◄──────────────────────│                      │
    │◄────────────────────│  "User verified"      │                      │
```

**Step-by-step:**

1. Client POSTs to `/auth/verify-aadhar/init` with Aadhar number
2. `UserController` receives request, calls `UserService`
3. `UserService` checks DB (via `UserRepository`) to ensure user isn't already verified
4. `UserService` calls `AadharProvider.InitiateVerification` (Strategy pattern — uses `SetuAadharProvider` in production)
5. Setu OKYC API sends OTP to user's Aadhar-linked mobile
6. Transaction ID returned to client
7. Client POSTs OTP to `/auth/verify-aadhar/complete`
8. `UserService` validates OTP via `AadharProvider.VerifyOTP`
9. `UserRepository.SetVerified(true)` marks user as verified
10. `UserVerified` event published (Observer pattern)
11. Background worker sends "Verification Successful" notification (Worker Pool pattern)

> Aadhar verification remains available for user KYC. Host applications do not require pre-verification; admin approval marks the applicant as verified.

---

## Payout Flow

Hosts withdraw earnings to their bank account or UPI via Cashfree Payouts API. The payout flow uses the **immutable ledger system** with webhook idempotency to ensure funds are never lost or duplicated, even if webhooks are replayed.

### Adding a Payout Method

```
1. Host sends POST /payouts/methods with bank details or UPI ID
2. Account number is masked (last 4 digits stored as display)
3. First method is automatically set as primary
4. Both host and admin can add payout methods
```

### Requesting a Withdrawal (Ledger-Based)

```
1.  Host sends POST /payouts/withdraw with host_id, amount, and payout_method_id
2.  Service checks for active fraud flags
3.  Service checks idempotency via ledger:
      SELECT FROM transaction_ledger WHERE idempotency_key = ?
      If found → return existing payment (fully idempotent)
4.  Service validates: calculated_balance (SUM ledger) >= amount
5.  CREATE IMMUTABLE LEDGER ENTRY:
      type=withdrawal_debit, amount_cents=-amount, status=pending
      (Note: stays PENDING until Cashfree webhook confirms)
6.  Payment record created: type=payout, status=processing
7.  Cashfree Payouts API called with transfer payload:
      - transfer_id = payment UUID
      - Beneficiary details for bank or UPI
      - Signature verification
8.  On provider success → Payment status = processing (waiting for webhook)
9.  On provider failure → Automatic reversal ledger entry created:
      type=webhook_reversal, amount_cents=+amount
      Host balance restored automatically (ledger sum recalculated)
```

### Webhook Processing (Async Settlement + Idempotency)

```
1.  Cashfree sends POST /webhooks/payout with event payload
2.  Webhook controller verifies webhook signature (HMAC-SHA256)
3.  CHECK IDEMPOTENCY:
      SELECT FROM webhook_executions 
      WHERE event_type='cashfree_payout' 
      AND external_event_id=?
      If found → already processed, return early (webhook is idempotent!)
4.  Process webhook based on event:

      Case: "transfer completed"
      ├─ Update Payment status → completed
      └─ Record in webhook_executions (prevents re-processing)

      Case: "transfer failed"
      ├─ CREATE REVERSAL LEDGER ENTRY:
      │  type=webhook_reversal, amount_cents=+amount, status=completed
      │  (Host wallet automatically credited back)
      ├─ Update Payment status → failed, increment retry_count
      └─ Record in webhook_executions
      
      Case: "transfer reversed"
      ├─ CREATE REVERSAL LEDGER ENTRY:
      │  type=webhook_reversal, amount_cents=+amount, status=completed
      │  (Host wallet automatically credited back)
      ├─ Update Payment status → reversed
      └─ Record in webhook_executions

5.  PayoutCompleted / PayoutFailed / PayoutReversed event published
```

### Webhook Idempotency Details

The system is protected against webhook replays via the `webhook_executions` table:

```sql
CREATE UNIQUE INDEX idx_webhook_unique 
  ON webhook_executions(event_type, provider_id, external_event_id);
```

**Scenario**: Cashfree webhook delivery sometimes retries if acknowledgment is not received.

| Delivery | Event ID | Action |
|----------|----------|--------|
| 1st | ABC123 | Process: create ledger, update payment, INSERT webhook_execution |
| 2nd retry | ABC123 | Check: webhook_execution exists → return early (idempotent) |
| 3rd retry | ABC123 | Check: webhook_execution exists → return early (idempotent) |

**Result**: Host is credited exactly once, even if webhook is delivered 3x.

### Admin Withdrawals

Similar to host withdrawals, but for the **platform account** (uuid.Nil):

```
1. Admin sends POST /payouts/admin/withdraw
2. Ledger entry created: type=withdrawal_debit, account_id=platform_id
3. Cashfree transfer initiated
4. Webhook processing creates reversal if needed
5. Platform balance = SUM(ledger) WHERE account_id=uuid.Nil
```

### Cashfree Payout Integration Details

| Feature | Implementation |
|---------|---------------|
| API | Cashfree Payouts API (`POST /payout/transfers`) |
| Auth | API headers (`x-client-id`, `x-client-secret`) |
| Beneficiary | Inline `beneficiary_details` payload |
| Bank Transfer | `transfer_mode=banktransfer` with account + IFSC |
| UPI Transfer | `transfer_mode=upi` with VPA |
| Webhook | HMAC-SHA256 signature verification + replay prevention |
| Idempotency | Ledger + webhook_executions table (at-most-once processing) |
| Retry | Automatic via retry_count in Payment, with exponential backoff |

### Earnings Dashboard

Available via `GET /payouts/earnings/{hostID}`:

```
Available balance  = SUM(transaction_ledger) WHERE account_id = host_id
                  = guaranteed to be consistent with ledger history
Total earnings     = SUM(booking_credit) WHERE account_id = host_id AND type = 'booking_credit'
Pending withdrawal = SUM(withdrawal_debit) WHERE account_id = host_id AND status = 'pending'
Fee config         = PlatformSettings (default 85% host / 15% platform)
Ledger history     = Full transaction history with all entries
```

---

## Fraud Prevention

Guards the money flow by flagging/blocking suspicious users.

| Flag Type | Trigger |
|-----------|---------|
| `abnormal_booking_spike` | User books unusually high volume in short time |
| `payment_abuse` | Repeated failed payments, chargebacks |
| `suspicious_activity` | General suspicious behavior |
| `manual_block` | Admin manually blocks a user |

When `is_active = true`, the booking and payout services check for active fraud flags and reject operations. The `blocked_until` field supports temporary blocks (null = indefinite).

---

## API Endpoints

All responses follow a standardized JSON envelope:

```json
{
  "success": true,
  "data": { ... },
  "message": "...",
  "error": "..."
}
```

### Health

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/health` | Returns `"ok"` — liveness probe |

### Authentication & User

| Method | Endpoint | Auth | Description |
|--------|----------|------|-------------|
| `POST` | `/auth/signup` | — | Register a new user (Firebase UID + email) |
| `POST` | `/auth/verify-aadhar/init` | 🔒 | Initiate Aadhar OTP verification via Setu |
| `POST` | `/auth/verify-aadhar/complete` | 🔒 | Submit OTP to complete KYC verification |
| `GET` | `/users/me` | 🔒 | Get own user profile |
| `PUT` | `/users/me` | 🔒 | Update own profile (name, avatar_url, city, etc.) |

### Saved Experiences

| Method | Endpoint | Auth | Description |
|--------|----------|------|-------------|
| `POST` | `/users/saved-experiences` | 🔒 | Save/bookmark an experience |
| `GET` | `/users/saved-experiences` | 🔒 | List all saved experiences |
| `GET` | `/users/saved-experiences/{eventID}/check` | 🔒 | Check if an event is saved |
| `DELETE` | `/users/saved-experiences/{eventID}` | 🔒 | Remove a saved experience |

<details>
<summary>Request / Response Examples</summary>

**POST `/auth/signup`**
```json
{
  "auth_uid": "firebase-uid-abc123",
  "email": "user@example.com",
  "name": "Jane Doe",
  "phn_number": "+919876543210"
}
// → 201 { "success": true, "data": { "id": "uuid", ... } }
// → 409 if user already exists
```

**POST `/auth/verify-aadhar/init`**
```json
{
  "user_id": "uuid",
  "aadhar_number": "123456789012"
}
// → 200 { "success": true, "data": { "transaction_id": "...", "message": "OTP sent" } }
```

**POST `/auth/verify-aadhar/complete`**
```json
{
  "user_id": "uuid",
  "transaction_id": "...",
  "otp": "123456"
}
// → 200 { "success": true, "data": { "message": "User verified successfully" } }
```

**PUT `/users/me`**
```json
{
  "name": "Jane Doe",
  "avatar_url": "https://storage.googleapis.com/...",
  "city": "Mumbai"
}
// → 200 updated user profile
```
</details>

### Hosts

| Method | Endpoint | Auth | Description |
|--------|----------|------|-------------|
| `POST` | `/hosts/apply` | 🔒 | Submit host application (status → pending) |
| `POST` | `/hosts/apply/draft` | 🔒 | Save host application as draft |
| `GET` | `/hosts/application-status` | 🔒 | Check own application status |
| `GET` | `/hosts/me` | 🔒 | Get own host profile |
| `PUT` | `/hosts/me` | 🔒 | Update own host profile |
| `GET` | `/hosts/dashboard` | 🔒 | Get host dashboard (earnings, ratings, stats) |

### Admin (Host Applications)

> All admin endpoints are protected by the `IsAdmin` middleware, which verifies the Firebase ID token and checks that the caller's email matches the `ADMIN_EMAIL` environment variable.

| Method | Endpoint | Auth | Description |
|--------|----------|------|-------------|
| `GET` | `/admin/hosts/applications` | 🔒 Admin | List all pending host applications |
| `POST` | `/admin/hosts/{hostID}/approve` | 🔒 Admin | Approve a host application (status → approved) |
| `POST` | `/admin/hosts/{hostID}/reject` | 🔒 Admin | Reject a host application (status → rejected) |

<details>
<summary>Request / Response Examples</summary>

**POST `/admin/hosts/{hostID}/approve`**
```json
// Authorization: Bearer <FIREBASE_ID_TOKEN_OF_ADMIN>
// → 200 { "success": true, "data": { "id": "uuid", "application_status": "approved", ... } }
```

**POST `/admin/hosts/{hostID}/reject`**
```json
{
  "reason": "Insufficient experience description"
}
// Authorization: Bearer <FIREBASE_ID_TOKEN_OF_ADMIN>
// → 200 { "success": true, "data": { "id": "uuid", "application_status": "rejected", ... } }
```

</details>

### Events (Experiences)

| Method | Endpoint | Auth | Description |
|--------|----------|------|-------------|
| `POST` | `/events/` | 🔒 | Create a new event (draft or live) |
| `PUT` | `/events/{eventID}` | 🔒 | Update an event |
| `GET` | `/events/{eventID}` | — | Get event details |
| `GET` | `/events/host/{hostID}` | — | List all events for a host |
| `GET` | `/events/host/{hostID}/filtered` | — | Filtered search (status, mood, date range) |
| `GET` | `/events/calendar/{hostID}` | — | Calendar view of events (`start`/`end` optional; defaults to current month) |
| `POST` | `/events/{eventID}/publish` | 🔒 | Publish a draft event (status → live) |
| `POST` | `/events/{eventID}/pause` | 🔒 | Pause a live event (status → paused) |
| `POST` | `/events/{eventID}/resume` | 🔒 | Resume a paused event (status → live) |
| `GET` | `/events/{eventID}/attendees` | 🔒 | List confirmed attendees for an event |

### Bookings

| Method | Endpoint | Auth | Description |
|--------|----------|------|-------------|
| `POST` | `/bookings/` | 🔒 | Book tickets (overbooking-safe, wallet debit, fee split) |
| `POST` | `/bookings/{bookingID}/confirm` | 🔒 | Confirm a pending booking |
| `POST` | `/bookings/{bookingID}/cancel` | 🔒 | Cancel booking (full refund to wallet) |
| `GET` | `/bookings/user/{userID}` | 🔒 | Get booking history for a user |

<details>
<summary>Request Example</summary>

**POST `/bookings/`**
```json
{
  "user_id": "uuid",
  "event_id": "uuid",
  "quantity": 2,
  "idempotency_key": "unique-request-key"
}
// → 201 (auto-calculates amount_cents, service_fee_cents, net_earning_cents)
// → Wallet debited, host credited, payment record created
```
</details>

### Payouts & Earnings

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/payouts/methods` | Add a bank account or UPI payout method |
| `GET` | `/payouts/methods/{hostID}` | List payout methods for a host |
| `PUT` | `/payouts/methods/{methodID}/primary` | Set a payout method as primary |
| `DELETE` | `/payouts/methods/{methodID}` | Remove a payout method |
| `POST` | `/payouts/withdraw` | Request a withdrawal (via Razorpay) |
| `GET` | `/payouts/earnings/{hostID}` | Get earnings summary (balance, total, pending) |
| `GET` | `/payouts/history/{hostID}` | Get payout transaction history |

### Reviews

| Method | Endpoint | Auth | Description |
|--------|----------|------|-------------|
| `POST` | `/reviews/` | 🔒 | Submit a review with rating & optional photos |
| `GET` | `/reviews/event/{eventID}` | — | List reviews for an event |
| `GET` | `/reviews/event/{eventID}/rating` | — | Get aggregate rating for an event |

### Inbox (Multi-party Messaging)

| Method | Endpoint | Auth | Description |
|--------|----------|------|-------------|
| `POST` | `/inbox/send` | 🔒 | Send a message in an event thread |
| `POST` | `/inbox/broadcast` | 🔒 | Host broadcasts to all event attendees |
| `GET` | `/inbox/event/{eventID}` | 🔒 | Get all messages for an event thread |
| `GET` | `/inbox/host/{hostID}` | 🔒 | Get all messages across host's events |
| `POST` | `/inbox/{messageID}/read` | 🔒 | Mark a message as read |

> Messages support sender types: `system`, `host`, `guest`. Also pushed real-time via **Socket.IO** to room `event_{eventID}`.

### Support

| Method | Endpoint | Auth | Description |
|--------|----------|------|-------------|
| `POST` | `/support/` | 🔒 | Create support ticket (JSON or multipart with evidence) |
| `GET` | `/support/{ticketID}` | 🔒 | Get a support ticket by ID |
| `GET` | `/support/user/{userID}` | 🔒 | List all tickets for a user |
| `POST` | `/support/{ticketID}/message` | 🔒 | Add a message to a ticket thread |
| `POST` | `/support/{ticketID}/resolve` | 🔒 | Mark ticket as resolved |

> For **Report a Participant**, set `category=report_participant` and include `event_id`, `session_date`, `report_reason`, `evidence` (files), `is_urgent`.

### File Upload

| Method | Endpoint | Auth | Description |
|--------|----------|------|-------------|
| `POST` | `/upload/?folder={path}` | 🔒 | Upload files to AWS S3 (max 10MB, SVG/PNG/JPG/PDF) |

Returns `[{ "file_name": "...", "url": "https://...", "size": 12345 }]`

### Webhooks

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/webhooks/payout` | Cashfree payout webhook (signature verified) |

### Real-time (Socket.IO)

| Endpoint | Description |
|----------|-------------|
| `/socket.io/*` | WebSocket endpoint |

**Client Events:**
- `join_room` — Join a room (e.g. `event_{eventID}`)

**Server Events:**
- `inbox_update` — Pushed when host broadcasts a message

---

## Getting Started

### Prerequisites

- **Go 1.24+**
- **PostgreSQL 15+**
- **Firebase project** with service account credentials
- **AWS account** with an S3 bucket and IAM credentials
- **Razorpay account** (for wallet top-up collections)
- **Cashfree account** with payouts enabled
- **Setu account** for Aadhar OKYC

### Setup

```bash
# 1. Clone
git clone <repo-url>
cd myslotmate-backend

# 2. Configure environment
cp .env.example .env
# Edit .env with your database URL, Firebase config, Razorpay keys, Cashfree keys, Setu keys

# 3. Run migrations
go run cmd/migrate/run.go

# 4. Start the server
go run cmd/api/run.go
```

The server starts on the port specified by `HTTP_PORT` (default: `5000`).

### Verify

```bash
curl http://localhost:5000/health
# → "ok"
```

---

## Configuration

Configuration is loaded from environment variables (with `.env` support via godotenv):

| Variable | Default | Description |
|----------|---------|-------------|
| `HTTP_PORT` | `5000` | Server listen port |
| `ADMIN_EMAIL` | — | Email of the admin user (for approve/reject host applications) |
| `DATABASE_URL` | — | PostgreSQL connection string |
| `FIREBASE_CREDENTIALS_FILE` | `config/firebase-service-account.json` | Path to Firebase service account JSON |
| `FIREBASE_PROJECT_ID` | `myslotmate-25994` | Firebase project ID |
| `AWS_S3_BUCKET` | — | AWS S3 bucket name for file uploads |
| `AWS_S3_REGION` | `ap-south-1` | AWS S3 region |
| `AWS_ACCESS_KEY_ID` | — | AWS IAM access key ID |
| `AWS_SECRET_ACCESS_KEY` | — | AWS IAM secret access key |
| `SETU_BASE_URL` | `https://uat.setu.co` | Setu OKYC API base URL |
| `SETU_CLIENT_ID` | — | Setu client ID |
| `SETU_CLIENT_SECRET` | — | Setu client secret |
| `SETU_PRODUCT_INSTANCE_ID` | — | Setu product instance ID |
| `RAZORPAY_KEY_ID` | — | Razorpay API key ID (required) |
| `RAZORPAY_KEY_SECRET` | — | Razorpay API key secret (required) |
| `RAZORPAY_PAYMENT_WEBHOOK_SECRET` | — | Razorpay payment webhook secret |
| `RAZORPAY_WEBHOOK_SECRET` | — | Legacy/shared Razorpay webhook secret fallback |
| `CASHFREE_BASE_URL` | `https://payout-api.cashfree.com` | Cashfree payout API base URL |
| `CASHFREE_CLIENT_ID` | — | Cashfree client ID for payouts |
| `CASHFREE_CLIENT_SECRET` | — | Cashfree client secret for payouts |
| `CASHFREE_WEBHOOK_SECRET` | — | Cashfree payout webhook secret |
| `CASHFREE_API_VERSION` | `2024-01-01` | Cashfree API version header |

---

## Migrations

SQL migration files are in `migrations/` and run in order:

| Migration | Description |
|-----------|-------------|
| `20260228120000_init_schema.sql` | Core schema — 10 tables, enums, triggers (accounts, users, hosts, events, bookings, reviews, payments, inbox, support, fraud) |
| `20260228130000_add_processing_status.sql` | Adds `processing` to `payment_status` enum |
| `20260228130001_earnings_payouts_schema.sql` | Adds `platform_settings`, `payout_methods`, `host_earnings` tables; extends `bookings` and `payments` with fee/payout columns |
| `20260307120000_figma_schema_expansion.sql` | Full Figma expansion — users (avatar, city), hosts (30+ cols), events (25+ cols), reviews (rating), inbox (multi-party), support (category), saved_experiences table, new enums |
| `20260307130000_support_evidence_upload.sql` | Report-a-participant fields — `report_reason` enum, `event_id`, `session_date`, `evidence_urls`, `is_urgent` on `support_tickets` |
| `20260307130001_review_photo_urls.sql` | Adds `photo_urls` column to `reviews` for photo attachments |

```bash
go run cmd/migrate/run.go
```

---

## Graceful Shutdown

The server listens for `SIGINT` / `SIGTERM` and performs:

1. HTTP server shutdown (20s timeout)
2. Worker pool drain (processes remaining tasks)
3. Socket.IO server close
4. Database connection close

---

## License

Private — All rights reserved.
