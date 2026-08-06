# Phase 2 — Enhancement Plan: Front-Office Reservation & Automated Financial Ledger

Companion to `FRONT_OFFICE_ANALYSIS.md` (Phase 1). Plan only — **no code has been
modified.** Written 2026-08-06.

This maps the requested specification onto the existing Serenentra codebase,
reusing what is already there and creating only what genuinely does not exist.

---

## 0. Entity mapping — reuse, do not recreate

The spec asks to "create schema models for Customers, Reservations, Rooms,
PromoCodes, Payments, Transactions, Invoices, Ledgers, Vouchers". **Nine of the
ten already exist.** Creating them again would fork the product.

| Spec entity | Existing table | Action |
|---|---|---|
| Customers | `guests` (front office) + `accounting_customers` (AR) | **Extend** — link the two |
| Reservations | `guest_stays` | **Extend** — add lifecycle/pricing columns |
| Rooms | `rooms` | No change |
| PromoCodes | `promotions` (+ `/api/booking/promotions`) | **Reuse** as-is |
| Payments | `payments`, `bill_payments` | **Extend** — instrument details |
| Transactions | `accounting_journal_entries` + `_lines` | **Reuse** — post through `postJournal` |
| Invoices | `accounting_sales_invoices` + `_lines` | **Reuse** |
| Ledgers | `accounting_accounts` (chart of accounts) | **Reuse** |
| Vouchers | `accounting_voucher_counters` + `voucher_no`/`voucher_type` | **Reuse** — `SAL`/`PAY` types already exist |
| ID documents | — | **New** (`reservation_documents`) |
| OTA ingestion | — | **New** (`channel_bookings`) |

Only two new tables. Everything else is additive columns.

> **Note on a pre-existing duplicate:** `billing_handler.go:859` lazily creates a
> *second*, separate `invoices` table at runtime, unrelated to
> `accounting_sales_invoices`. The spec's "Sales Invoice" must post to the
> accounting one — that is where vouchers, the customer ledger and the trial
> balance live. The legacy `invoices` table should be left untouched for now and
> retired separately; folding it in here would widen this change too far.

---

## 1. Cross-cutting prerequisite: make the write path transactional

**This blocks spec §3 and §5.4 and must land first.**

### Existing implementation

Every repository method calls `poolFromContext(ctx, r.db.Pool)` and executes
against a `*pgxpool.Pool` directly. The current check-in sequence is three
independent, uncommitted calls with their errors deliberately discarded:

```go
_, _ = h.roomRepo.EnsureGuest(...)
_, _ = h.roomRepo.EnsureFolioForBooking(...)
created, err := h.roomRepo.CreateStay(...)
```

### Problems

- No atomicity. A reservation can be created whose guest record failed, or a
  folio can exist for a stay that was never inserted.
- `DB.WithTx` exists (`db.go:112`) but calls `d.Pool.BeginTx` — **the shared
  pool**, ignoring `poolFromContext`. For a dedicated-database tenant it would
  open the transaction against the *wrong database* while the rest of the
  request writes to the tenant's own. Using it as-is would be a correctness
  regression, not a fix.
- `poolFromContext` returns a concrete `*pgxpool.Pool`, so a `pgx.Tx` cannot be
  substituted.

### Proposed solution

A minimal seam that makes **every existing repository method transaction-capable
without rewriting any SQL**:

1. Define a `Querier` interface in `postgres` with the three methods both
   `*pgxpool.Pool` and `pgx.Tx` already satisfy (`Query`, `QueryRow`, `Exec`).
2. Change `poolFromContext` to return `Querier`, and have it check for a
   transaction stashed in the context *first*, falling back to the tenant pool,
   then the shared pool. Signature change only — no call site changes, because
   every caller already just calls `.Query`/`.Exec` on the result.
3. Add `WithTenantTx(ctx, fallback, fn)` that begins on the **tenant-resolved**
   pool and puts the `pgx.Tx` into the context it passes to `fn`.

Then a service method wraps existing repo calls and they all enlist
automatically:

```go
return db.WithTenantTx(ctx, pool, func(txCtx context.Context) error {
    guestID, err := repo.EnsureGuest(txCtx, ...)      // now runs in the tx
    stay,    err := repo.CreateStay(txCtx, ...)       // same tx
    folioID, err := repo.EnsureFolioForBooking(txCtx, ...)
    ...                                                // rollback on any error
})
```

### Files to modify

- `internal/repository/postgres/db.go` — add `Querier`, `WithTenantTx`; fix the
  tenant-pool bug in `WithTx`.
- `internal/repository/postgres/room_repository.go` — no SQL changes; the
  helper's return type changes underneath it.
- New `internal/service/reservation_service.go` — owns the transaction and the
  orchestration.

### Testing

- Unit: force an error at each step, assert nothing persisted.
- Integration: confirm a dedicated-DB tenant's transaction lands in its own
  database, not the shared one — this is the regression `WithTx` would have
  caused.

---

## 2. F1 — Unified walk-in / manual reservation form

### Existing implementation

`POST /api/reservations` accepts eight fields (`guest_name`, `guest_email`,
`guest_phone`, `room_id`, `check_in_date`, `check_out_date`, `source`, `notes`).
The 4-step wizard at `reservations.new.tsx` already collects source from a
10-channel dropdown and already uses live date-range availability.

### Problems

- Dates are date-only (`2006-01-02`); the spec requires check-in **time**.
- No duration field — the spec wants nights entered and check-out derived.
- Adults/children/nationality are collected by the UI and **silently dropped**.
- Guest count is never validated against `rooms.capacity`.
- No `approach_type` distinct from `source`.

### Proposed solution

Extend the existing request DTO and wizard rather than adding a second form.
Accept `duration_nights` as an alternative to `check_out_date` (server derives
one from the other; supplying both and disagreeing is a 400). Keep every
existing field working exactly as now.

### Database changes

```sql
-- 0xx_reservation_frontoffice.up.sql
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS adults        INT  NOT NULL DEFAULT 1;
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS children      INT  NOT NULL DEFAULT 0;
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS approach_type TEXT NOT NULL DEFAULT 'manual';
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS confirmation_no TEXT;
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS status        TEXT NOT NULL DEFAULT 'confirmed';

UPDATE guest_stays SET status =
  CASE WHEN actual_check_out IS NOT NULL THEN 'checked_out'
       WHEN actual_check_in  IS NOT NULL THEN 'in_house'
       ELSE 'confirmed' END
WHERE status = 'confirmed';

CREATE UNIQUE INDEX IF NOT EXISTS guest_stays_confirmation_no_key
  ON guest_stays (hotel_id, confirmation_no) WHERE confirmation_no IS NOT NULL;
```

Down migration drops the columns and index. Every column has a default, so
existing readers and the compat `/tables/:table` path keep working untouched.

### API changes

`POST /api/reservations` gains optional `adults`, `children`, `approach_type`,
`duration_nights`, `check_in_time`. All optional → **fully backward compatible**.
Response gains `confirmation_no` and `status`.

### UI changes

`reservations.new.tsx`: send the already-collected adults/children; add a
duration input bound to check-out; validate `adults + children <= capacity`
before enabling Continue. Reuse existing `Field`/`Input`/`Select` components —
no new design work.

### Testing

Existing 8-field payload must still return the same shape (regression test).
New fields round-trip. Capacity violation returns 400.

---

## 3. F2 — ID proof type and document upload

### Existing implementation

`guests` already has **unused** `id_type` and `id_number` columns. There is **no
file-upload capability anywhere in the backend** — no `multipart`/`FormFile`
handling, no object-storage client.

### Problems

- This is genuinely new infrastructure, not an extension.
- **Storage location is a trap this project has already fallen into once.**
  `PROGRESS.md` §7 records that backups written to `/app/backups` were silently
  destroyed by every deploy because no volume was mounted. ID documents written
  the same way would be deleted on each deploy while the database still claimed
  they existed — the identical failure, with legal consequences.
- ID documents are sensitive PII and must never be served from a public path or
  a guessable URL.

### Proposed solution

- Store metadata in a new `reservation_documents` table; store bytes on a
  **mounted Docker volume** at `/app/uploads`, declared in *both* compose files
  in the same commit as the code (per the §7 lesson — and `deploy.sh` must ship
  `deployments/`, which it now does).
- Serve only through an authenticated, role-gated endpoint that streams the file
  after checking `hotel_id` — never a static path.
- Validate by magic bytes, not by the client-supplied MIME type or extension.
  Accept JPEG/PNG/PDF only; cap size (5 MB proposed); store a SHA-256.

### Database changes

```sql
CREATE TABLE IF NOT EXISTS reservation_documents (
  id            UUID PRIMARY KEY,
  hotel_id      UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
  guest_stay_id UUID REFERENCES guest_stays(id) ON DELETE CASCADE,
  guest_id      UUID REFERENCES guests(id) ON DELETE SET NULL,
  doc_type      TEXT NOT NULL,          -- passport | driver_license | national_id | voter_id
  doc_number    TEXT,
  file_path     TEXT NOT NULL,
  mime_type     TEXT NOT NULL,
  size_bytes    BIGINT NOT NULL,
  sha256        TEXT NOT NULL,
  uploaded_by   UUID REFERENCES users(id) ON DELETE SET NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_reservation_documents_stay  ON reservation_documents(hotel_id, guest_stay_id);
CREATE INDEX IF NOT EXISTS idx_reservation_documents_guest ON reservation_documents(hotel_id, guest_id);
```

`guests.id_type` / `id_number` get populated at last, from the same submission.

### API changes

New: `POST /api/reservations/:id/documents` (multipart),
`GET /api/reservations/:id/documents` (metadata list),
`GET /api/reservations/:id/documents/:docId/content` (authenticated stream).

### Infrastructure changes

`deployments/docker-compose*.yml` — add an `uploads` named volume mounted at
`/app/uploads` on the `api` service. **Must ship in the same deploy as the code.**

### Testing

Upload → restart the container → file still readable (this is the exact check
that caught the backup bug). Cross-tenant fetch returns 404. A `.pdf` whose
bytes are actually an executable is rejected.

---

## 4. F3 — Promo codes and the pricing engine

### Existing implementation

`promotions` table with `code`, `discount_type`, `discount_value`, `min_nights`,
`min_amount`, `max_discount`, `usage_limit`, `used_count`, `valid_from/to`,
`active` — plus full CRUD and `POST /api/booking/validate-promo`.

Pricing today: `total = room.price_per_night × nights` in `Create`, and a
**separate hardcoded `tax = subtotal × 0.18`** on the frontend
(`reservations.new.tsx:85`) that is displayed but never sent. The stored total
and the quoted total differ by 18% on every booking.

### Problems

- Two pricing implementations, neither authoritative, disagreeing with each other.
- `pricing_rules`, `rate_plans` and `tax_configs` are consulted by neither.
- `used_count` has no atomic guard — concurrent redemptions can exceed
  `usage_limit`.

### Proposed solution

One server-side quote endpoint as the **single source of truth**, which the
wizard calls and displays verbatim. The client stops calculating money entirely.
Reuse the existing promo validation logic rather than writing a second copy.

Redemption becomes race-safe with a conditional update:

```sql
UPDATE promotions SET used_count = used_count + 1
 WHERE id = $1 AND active
   AND (usage_limit = 0 OR used_count < usage_limit)
```

Zero rows affected → the code is exhausted → reject the booking inside the same
transaction.

### Database changes

```sql
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS rate_plan_id    UUID;
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS promo_code      TEXT;
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS promo_id        UUID REFERENCES promotions(id) ON DELETE SET NULL;
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS discount_amount NUMERIC(12,2) NOT NULL DEFAULT 0;
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS tax_amount      NUMERIC(12,2) NOT NULL DEFAULT 0;
```

`total_amount` keeps its current meaning (pre-tax) so nothing downstream shifts;
the payable figure is `total_amount - discount_amount + tax_amount`.

### API changes

New `POST /api/reservations/quote` → `{ nights, base_total, discount, tax_breakdown[], payable }`.
`POST /api/reservations` gains optional `promo_code`; the server **re-quotes and
ignores any client-supplied amount** (client totals are display-only).

### UI changes

Wizard step 3: promo-code input with inline validation; replace the hardcoded
18% line with the tax lines returned by `/quote`.

### Testing

Quote matches stored amounts exactly. An exhausted code is rejected under
concurrency (N parallel redemptions of a limit-1 code yield exactly one success).

---

## 5. F4 — Payment capture

### Existing implementation

`payments` table (`amount`, `payment_method`, `status`, `guest_stay_id`) and a
1027-line `payment_service.go` covering Stripe/Razorpay.

### Problems

No method-specific fields exist for the manual front-desk case (UPI reference,
cash tendered/change, card auth code).

### ⚠ Compliance constraint on the card path

The spec asks for "Card Number (masked/last 4 digits for PCI compliance) AND
IFSC Code / Bank Authorization Code".

**The system must never receive or store a full card number (PAN).** Masking a
PAN *after* it reaches the server does not achieve PCI compliance — the moment
the full number touches the application it is in scope, including in request
logs, error traces and database backups. This project logs request bodies on
error and takes `pg_dump` backups, so a PAN would land in both.

Design accordingly:

- The form field accepts **last 4 digits only** (`maxlength=4`, numeric).
- The API **rejects** any `card_last4` longer than 4 characters — it does not
  truncate, because silently accepting a PAN means it was transmitted.
- The authorisation/reference code from the terminal is stored as free text.
- Actual card capture stays with the existing gateway integration, which is the
  correct place for it.

I will implement it this way unless directed otherwise; if full PAN storage is
genuinely required, that is a decision that needs explicit sign-off and a
different architecture (a tokenisation vault), not a column.

### Database changes

```sql
ALTER TABLE payments ADD COLUMN IF NOT EXISTS guest_stay_id   UUID REFERENCES guest_stays(id) ON DELETE SET NULL;
ALTER TABLE payments ADD COLUMN IF NOT EXISTS upi_id          TEXT;
ALTER TABLE payments ADD COLUMN IF NOT EXISTS transaction_ref TEXT;
ALTER TABLE payments ADD COLUMN IF NOT EXISTS card_last4      TEXT CHECK (card_last4 IS NULL OR card_last4 ~ '^[0-9]{4}$');
ALTER TABLE payments ADD COLUMN IF NOT EXISTS auth_code       TEXT;
ALTER TABLE payments ADD COLUMN IF NOT EXISTS cash_received   NUMERIC(12,2);
ALTER TABLE payments ADD COLUMN IF NOT EXISTS change_given    NUMERIC(12,2);
```

The `CHECK` is the enforcement point — a PAN cannot be stored even by a caller
that bypasses the handler (including `/api/tables/:table`).

### Validation

Server-side, mirroring the spec's conditional requirements: UPI ⇒ `upi_id` +
`transaction_ref`; Card ⇒ `card_last4` + `auth_code`; Cash ⇒ `cash_received >=
payable`, with `change_given` derived server-side, not trusted from the client.

### Testing

Each method's missing-field cases return 400. A 16-digit `card_last4` is
rejected at both the handler and the constraint.

---

## 6. F5 — Automated accounting on submission

### Existing implementation

A complete accounting engine already exists and is proven in production:
`accounting_accounts`, `accounting_journal_entries` + `_lines`,
`accounting_sales_invoices` + `_lines`, `accounting_customers`,
`accounting_voucher_counters`. `postJournal` is the single chokepoint that
assigns voucher numbers via one atomic UPSERT, with `SAL`/`PUR`/`PAY`/`GEN`
types derived from the reference prefix. POS already posts through it.

### Problems

Room revenue never reaches the ledger (Phase 1 §4.7). Reservations post nothing.

### Proposed solution

**Reuse `postJournal` exactly as POS does — do not write a second posting path.**
Inside the F1 transaction, after the stay and payment are written:

| Step | Mechanism |
|---|---|
| Customer record | `EnsureGuest` (existing) + resolve/create `accounting_customers` by normalised phone, reusing the POS resolver |
| Sales invoice | Insert `accounting_sales_invoices` + lines, linked to `guest_stay_id` |
| Ledger posting | `postJournal` with reference `FOLIO <confirmation_no>` — already maps to voucher type `SAL` with no change to the type table |
| Receipt voucher | `postJournal` with reference `PAY <payment_number>` → voucher type `PAY` |

Postings mirror the proven POS shape:

```
Dr 1000 Cash / 1010 Bank / 1200 AR      payable
   Cr 4000 Room Revenue                        base_total - discount
   Cr 2100 GST Payable                         tax_amount
```

Debit account chosen by method exactly as dine-in settlement does (cash/card/upi
→ Cash or Bank; OTA/credit → Accounts Receivable). A credit sale with no
resolvable customer is refused rather than posted to an uncollectable receivable
— the existing POS policy, applied consistently.

Idempotency keyed on the reference, matching the existing `alreadyPosted`
pattern, so a retried submission cannot double-book revenue.

### Files to modify

- `internal/handler/accounting_autopost.go` — add a room-revenue posting
  function beside the existing POS ones; reuse `postJournal`.
- `internal/service/reservation_service.go` (new) — orchestration.

### Testing

Rehearse with `COMMIT` swapped for `ROLLBACK` (the documented house practice)
before any live run. Assert the trial balance still nets 0.00. Assert a repeated
submission produces exactly one journal entry. Assert a forced failure at the
invoice step rolls back the stay, the payment **and** the journal.

---

## 7. F6 — OTA and direct booking-engine ingestion

### Existing implementation

`channel_connections` records channels and sync attempts. **No OTA adapter
exists** — nothing calls Booking.com or any other provider. `/api/booking/*`
serves the direct engine but its search endpoint is broken (Phase 1 §3.1).

### Problems

- The API has no versioning convention; the spec asks for `/api/v1/...`.
- **Router ordering constraint:** `router.go:242` mounts `api.Use(authGate)`, and
  in Fiber that gates only routes registered *after* it. A webhook must be
  registered **before** that line or it will 401 every OTA callback — and it must
  then authenticate itself by HMAC signature instead. Registering it in the wrong
  block is the single most likely way to get this wrong.
- Payload replay and duplicate delivery are normal OTA behaviour; ingestion must
  be idempotent on the channel's own booking reference.

### Proposed solution

- New `POST /api/v1/channel-manager/booking`, registered in the **public block**,
  authenticated by an HMAC-SHA256 signature header over the raw body using a
  per-channel secret held in `channel_connections`. Constant-time comparison.
- Persist the raw payload to `channel_bookings` **first**, then process
  asynchronously via the existing `worker` pool — so a slow ledger write never
  causes the OTA to retry.
- Reuse the same `reservation_service` used by the front desk, so an OTA booking
  and a walk-in produce identical records and identical ledger entries.
- OTA postings debit **Accounts Receivable** (money is owed by the OTA, not
  received), with commission recorded as an expense line.

### Database changes

```sql
CREATE TABLE IF NOT EXISTS channel_bookings (
  id             UUID PRIMARY KEY,
  hotel_id       UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
  connection_id  UUID REFERENCES channel_connections(id) ON DELETE SET NULL,
  channel_name   TEXT NOT NULL,
  external_ref   TEXT NOT NULL,
  payload        JSONB NOT NULL,
  status         TEXT NOT NULL DEFAULT 'received',  -- received|processed|failed|duplicate
  guest_stay_id  UUID REFERENCES guest_stays(id) ON DELETE SET NULL,
  commission     NUMERIC(12,2) NOT NULL DEFAULT 0,
  error          TEXT,
  received_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  processed_at   TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS channel_bookings_external_key
  ON channel_bookings (hotel_id, channel_name, external_ref);
```

The unique index is the idempotency guarantee — a replayed webhook conflicts and
is marked `duplicate` rather than creating a second reservation.

### Testing

Replay the same payload 5× → one reservation, one journal entry, four
`duplicate` rows. A bad signature returns 401. A malformed payload is stored
with `status='failed'` and an error, never silently dropped.

---

## 8. F7 — Conflict prevention / inventory locking

### Existing implementation

`Create` reads overlapping stays and loops in Go.

### Problems

A read-then-write race (Phase 1 §4.3), and it is bypassable entirely through
`/api/tables/:table` (§4.4). Application-level checks cannot satisfy the spec's
"instantly lock room inventory … to prevent double bookings" because the OTA
path and the front desk are concurrent by definition.

### Proposed solution

A database exclusion constraint — the only mechanism that holds across *all*
write paths including the compat endpoint and direct SQL:

```sql
CREATE EXTENSION IF NOT EXISTS btree_gist;

ALTER TABLE guest_stays ADD CONSTRAINT guest_stays_no_overlap
  EXCLUDE USING gist (
    room_id WITH =,
    daterange(check_in_date::date, check_out_date::date, '[)') WITH &&
  ) WHERE (status <> 'cancelled');
```

**Must be preceded by an audit for existing overlaps**, which would make the
`ALTER` fail:

```sql
SELECT a.id, b.id, a.room_id
FROM guest_stays a JOIN guest_stays b
  ON a.room_id = b.room_id AND a.id < b.id
 AND a.check_in_date < b.check_out_date
 AND a.check_out_date > b.check_in_date;
```

The handler keeps its friendly pre-check for a clean 409 message; the constraint
is the guarantee behind it. A `23P01` violation maps to 409.

### Testing

Two concurrent identical bookings → exactly one succeeds, one gets 409. Insert
an overlap through `/api/tables/guest_stays` → rejected.

---

## 9. Sequencing

Each step is independently shippable and independently testable, per the
incremental-delivery requirement. Ordered so that nothing depends on something
not yet built.

| # | Step | Depends on | Risk |
|---|---|---|---|
| 1 | Transaction seam (`Querier`, `WithTenantTx`, fix `WithTx` tenant bug) | — | Low, no behaviour change |
| 2 | Overlap audit + exclusion constraint | — | Medium — **audit first** |
| 3 | Reservation lifecycle columns + status backfill | — | Low, all defaulted |
| 4 | Quote endpoint + promo integration; remove the hardcoded 18% | 3 | Medium — money-visible |
| 5 | Payment capture fields + validation | 3 | Low |
| 6 | Atomic accounting posting (voucher, ledger, invoice, customer) | 1,4,5 | **High — rehearse with ROLLBACK** |
| 7 | ID upload + volume mount | 3 | Medium — infra must ship together |
| 8 | Channel-manager webhook | 1,6 | Medium |
| 9 | Fix `/api/booking/search`; retarget night audit off `bookings` | 3 | Medium |

Steps 1–3 are safe groundwork. **Step 6 is the one that touches the books** and
should be reviewed on its own, rehearsed with `ROLLBACK`, and verified against
the trial balance before and after.

---

## 10. Decisions needed before implementation starts

1. **Card data (§5)** — confirm last-4-only. I will not build full PAN capture
   without an explicit decision, and would recommend against it regardless.
2. **Deposit vs full payment at booking** — the spec implies payment is taken at
   reservation time. Is a partial deposit allowed? This changes whether the
   ledger entry is revenue or an advance liability (unearned income), which is a
   materially different posting.
3. **Room revenue timing** — post the whole stay at booking (matching the spec's
   "instantly reflect revenue"), or accrue per night at night audit?
   `PROGRESS.md` flags the current arrival-date attribution as a known
   correctness gap. The spec's wording implies the former; accounting practice
   prefers the latter. **This is the most consequential open question.**
4. **OTA commission treatment** — expense on booking, or netted against the
   receivable?
5. **`bookings` table** — confirm nothing outside this repo reads it before
   retargeting night audit (Phase 1 §6.5).
6. **`folios.guest_id` constraint** — confirm the live schema (Phase 1 §4.11)
   before any new environment is built from `migrations/`.

---

## 11. What this plan deliberately does not do

- Does not rebuild the accounting engine — it posts through `postJournal`.
- Does not create a second reservations table or a second promo system.
- Does not redesign the portal — it extends the existing wizard and shadcn
  components.
- Does not remove the `mhms-store` demo fallback or the legacy `invoices` table.
- Does not change any existing endpoint's response shape; every addition is
  optional and defaulted.
