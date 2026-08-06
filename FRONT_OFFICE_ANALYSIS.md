# Front Office / Reservation Module — Phase 1 Code Analysis

Analysis only. **No code was modified to produce this document.**

Written 2026-08-06. Scope: the reservation path end to end across the unified Go
backend (`golangserver`) and the tenant portal (`HMS admin portal`), plus its
integration points with Finance, CRM, Housekeeping, Booking Engine, Channel
Manager, Night Audit and Reporting.

---

## 0. Project state at time of analysis

All four repositories are clean and level with `origin/main`. Nothing was
uncommitted, and nothing is pending a push.

| Repo | HEAD | GitHub | Role |
|---|---|---|---|
| `serenentra_golangserver` | `38e3ebf` | in sync | unified backend, serves all three frontends |
| `HmsAdminStaffPortal` | `571dc83` | in sync | **main product** — tenant admin/staff portal |
| `superadmin_serenentra` | `f351fe7` | in sync | platform console (Vercel) |
| `serenentra-landing` | `a32ee7f` | in sync | marketing site (Vercel) |

`hms-provisioner` is a fifth component and is **not under version control at
all** — no `.git`, no remote, no backup. It runs on the VM as a systemd service.

### Correction to the brief

The brief refers to "the existing Supabase schema" and "RLS Policies". **This
system does not use Supabase and has no row-level security.** It is:

- **Go + Fiber v2** (not chi), **pgx/v5** connection pools (not sqlx), no ORM,
  raw SQL written inline in repositories and handlers.
- **PostgreSQL** self-hosted in Docker on a Hetzner VM (`docker-postgres-1`,
  database `hotel_harmony`).
- Multi-tenancy is enforced **in application code** — every query carries an
  explicit `WHERE hotel_id = $1`, with an optional dedicated pool per tenant
  resolved by `internal/tenant/manager.go`.

This matters for the security section: because isolation is application-level,
a single query that forgets `hotel_id` leaks across tenants with **no second
line of defence**. Adding RLS is a legitimate future hardening step, but it is a
new capability, not an existing one to review.

---

## 1. Current architecture

### Backend

```
cmd/server/main.go
  └── internal/handler/router.go        route registration (order is security-significant)
        ├── public handlers             auth, hotels, payments, AI, demo, newsletter
        ├── api.Use(authGate(secret))   ← everything below requires a bearer token
        ├── tenant pool resolution      c.Locals("tenant_pool")
        ├── rate limit → plan gate → feature-matrix gate   (all fail-open)
        └── staff handlers              reservations, billing, housekeeping, POS, accounting…
```

Schema is managed **two different ways**, which is itself a source of drift:

1. `migrations/*.sql` — 26 files run by a custom splitter (`splitSQLStatements`).
2. `internal/repository/postgres/schema.go` — `EnsureAppSchema`, a 935-line Go
   list of `CREATE TABLE IF NOT EXISTS` / `ALTER TABLE … ADD COLUMN IF NOT EXISTS`.

Migrations run **before** the Go-defined tables exist, so a migration may not
reference anything created in `schema.go`.

### Reservation module surface

| Layer | File | Size |
|---|---|---|
| HTTP | `internal/handler/reservation_handler.go` | 456 lines, 8 endpoints |
| Persistence | `internal/repository/postgres/room_repository.go` | 516 lines |
| Model | `internal/domain/models.go` → `GuestStay` | — |
| UI list | `src/routes/reservations.tsx` | 398 lines |
| UI wizard | `src/routes/reservations.new.tsx` | 258 lines |
| UI detail | `src/routes/reservations.$id.tsx` | 732 lines |
| UI front desk | `src/routes/front-desk.tsx` | 530 lines |
| Data hooks | `src/lib/api/hooks.ts` | 1347 lines (TanStack Query) |

Endpoints: `GET /reservations`, `/reservations/calendar`, `/reservations/:id`,
`POST /reservations`, `PATCH /reservations/:id`, `DELETE /reservations/:id`,
`POST /reservations/:id/checkin`, `POST /reservations/:id/checkout`.

---

## 2. Existing features (working, do not rebuild)

- Create / list / read / update reservations against `guest_stays`.
- Overlap rejection on create (application-level, see §4.3 for its limits).
- Date-range availability: `GET /api/rooms/available?check_in=&check_out=`
  backed by `ListRoomsAvailableBetween` — correct strict-inequality overlap
  logic, with `maintenance` as the only hard exclusion.
- Check-in: sets `actual_check_in`, flips room to `occupied`, resolves or
  creates the CRM guest, opens an **idempotent** folio in INR.
- Check-out: sets `actual_check_out`, flips room to `cleaning`.
- Cancel: refuses to cancel a checked-in stay; releases the room **only** when
  no other guest is still in it.
- `EnsureGuest` find-or-create with last-10-digit phone normalisation, so
  `+91 98111 22333` and `9811122333` resolve to one guest.
- Email/SMS dispatch via a worker pool (`worker.SubmitOrRun`).
- Booking-source capture (`source`, defaults `Direct`), 10 OTA channels offered
  in the wizard.
- 4-step booking wizard with live availability and invalidation of a chosen room
  when dates change.

These are sound and should be **extended, not replaced**.

---

## 3. The single most important finding: two parallel reservation tables

There are **two** reservation tables, and different modules read different ones.

| | `guest_stays` | `bookings` |
|---|---|---|
| Created in | `migrations/001_init.sql` | `schema.go` (Go list) |
| Written by | reservation handler, compat `/tables/:table` | **nothing in the front-office path** |
| Read by | reservations, billing, folios, dashboard, CRM, POS | **booking engine, night audit, channel** |
| Status column | none — status is *derived* | `status` (`confirmed`/`checked_in`/`checked_out`) |
| Money columns | `total_amount` | `total`, `tax_amount` |

Consequences, all of them live:

- **Night audit reports zeros for a full hotel.** `CloseDay`, `GetTaxAudit` and
  the revenue audit all aggregate `FROM bookings … WHERE check_in = CURRENT_DATE`.
  A hotel that took every one of its bookings through the front desk has an
  empty `bookings` table, so total revenue, total tax, occupied rooms, arrivals
  and check-outs all compute as 0 — and `night_audit_reports` **persists** those
  zeros as the day's official close.
- **The booking engine sells rooms it believes are free.**
  `GET /booking/availability` and `POST /booking/search` both exclude rooms by
  joining `bookings`. Front-desk reservations are invisible to them.
- `folios.booking_id` is a foreign key to **`guest_stays`**, despite the name —
  so "booking" means two different things in two places in the same schema.

This is the root architectural defect. Every "integrate reservations with X"
item in the brief runs through it.

### 3.1 `POST /api/booking/search` is broken outright

`booking_handler.go:118` selects `r.max_guests` and `r.base_rate`. Neither
column exists — `rooms` has `capacity` and `price_per_night`. The strings
`max_guests` / `base_rate` appear **nowhere** in `schema.go` or in any of the 26
migrations; only in this handler. The endpoint therefore fails with a
`42703 undefined_column` on every call.

---

## 4. Technical debt and defects

### 4.1 Cancellation destroys the record

`DELETE /reservations/:id` calls `DeleteStay`, a hard `DELETE`. There is no
cancellation record, no reason code, no cancellation fee, no timestamp, no
audit-log entry, and no way to distinguish a cancellation from a no-show or from
a row that never existed. Cancelled-revenue reporting is impossible, and the
deletion is unrecoverable.

### 4.2 Reservation status is derived, never stored

`deriveReservationStatus` computes status from the two timestamps:

```
actual_check_out != nil          → checked_out
actual_check_in  != nil          → in_house
check_in_date <= now             → pending_checkin
otherwise                        → upcoming
```

This cannot express `confirmed`, `tentative`, `waitlisted`, `cancelled`,
`no_show`, `guaranteed`, or `released`. The `bookings` table has the `status`
column the system actually needs — on the table nothing writes.

### 4.3 Double-booking is possible (TOCTOU race)

`Create` reads existing stays, loops in Go to test overlap, then inserts. Two
concurrent requests for the same room and dates both pass the check and both
insert. There is **no database constraint** preventing it.

This is not theoretical, because the check can also simply be walked around:

### 4.4 The generic table endpoint bypasses every reservation rule

`compat_handler.go` exposes `GET/POST/PATCH/DELETE /api/tables/:table`, and
`guest_stays` is on its allowlist (`compat_handler.go:196`, with a direct
`INSERT INTO guest_stays` at :952 and `DELETE FROM guest_stays` at :450). A
reservation can therefore be created, mutated or deleted **without** the overlap
check, the date validation, the CRM guest link, or the folio.

Any fix that lives only in `reservation_handler.go` is bypassable. This is the
strongest argument for enforcing the invariant in the database (§6.1).

### 4.5 `PATCH` neither re-validates availability nor re-prices

Changing `check_in_date` / `check_out_date` runs no overlap check, so a
reservation can be moved onto dates where the room is already occupied. And
`total_amount` is **never recomputed** — a 2-night booking extended to 7 nights
keeps its 2-night price. That is a direct revenue-loss bug.

`Update` also cannot clear a field (empty string means "not supplied"), cannot
change the room (no room-move support), and permits editing a checked-in stay.

### 4.6 Pricing bypasses the pricing engine entirely

`Create` computes `total = room.price_per_night × nights`. It consults none of:
`pricing_rules`, `rate_plans`, `promotions`, `tax_configs`. There is no promo
code field on the request at all, despite `POST /booking/validate-promo` and a
whole promotions CRUD existing.

Worse, **the guest is shown a different number than the one stored.**
`reservations.new.tsx:85` hardcodes `tax = round(subtotal × 0.18)` client-side
and displays `subtotal + tax` as the total — then never sends it. The backend
stores the pre-tax subtotal. The confirmation screen and the database disagree
by 18% on every booking.

### 4.7 Room revenue never reaches the ledger

Check-in opens a folio but posts **no accommodation charge** to it. `PROGRESS.md`
confirms `payment_handler`, `nightaudit_handler` and others have zero ledger
calls. Only POS settlement posts. So the accounting module — which has a working
voucher system, chart of accounts, journal entries and a trial balance — never
sees room revenue, the hotel's primary income.

### 4.8 Check-in and check-out skip their business rules

**Check-in** does not verify the stay is not already checked in (a repeat call
overwrites `actual_check_in`), does not check the arrival date, does not check
the room is clean (a guest can be checked into a `cleaning` or `maintenance`
room), takes no deposit, and captures no identity document — even though
`guests` already has `id_type` and `id_number` columns sitting unused.

**Check-out** does not check the folio balance. A guest can check out owing
money. The invoice email uses `stay.total_amount` and the literal string `"N/A"`
as the invoice number, so it omits every POS charge posted to the folio.

### 4.9 Housekeeping is not actually integrated

Check-out sets the room to `cleaning` but creates no
`housekeeping_assignments` row — despite that table existing with
`task_type` defaulting to `'checkout_clean'`. Housekeeping has no work queue
fed by departures.

### 4.10 Guest notifications are wrong for every tenant

`reservation_handler.go` lines 369, 375, 411 and 417 hardcode the hotel name
**"Grand Hotel Mumbai"** in booking-confirmation and check-out messages. Every
tenant's guests receive another hotel's name.

Separately, the "booking confirmation" email/SMS is sent at **check-in**, not at
booking. Creating a reservation notifies the guest of nothing.

### 4.11 Schema drift risk on `folios.guest_id`

`migrations/002_saas_foundation.sql:227` declares:

```sql
guest_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT
```

But commit `38e3ebf` states that `folios.guest_id` has *no* foreign key and
deliberately stores a CRM `guests(id)` there — verified working against the live
tenant. Both cannot be true of the same database. Production has evidently
drifted from the migration file.

The risk is asymmetric and serious: **a database built fresh from `migrations/`
would have the FK, and check-in would fail on it** while production is fine.
This needs confirming against the live schema before any new environment is
provisioned.

### 4.12 Smaller items

- `UpdateStay` interpolates map keys straight into SQL with **no column
  allowlist**, unlike `UpdateRoom` which has one. Currently only reached with
  handler-built keys, so not exploitable today — but it is one careless caller
  away from injection.
- `GET /reservations` and `GET /reservations/:id` return **different shapes**:
  the list returns a flattened map with RFC3339 strings, the detail returns the
  raw `domain.GuestStay` with a nested `rooms` object.
- `POST /reservations` returns **200, not 201**, with no `Location` header and
  no idempotency key — a retried request creates a second booking.
- No optimistic concurrency: two agents editing one reservation silently clobber
  each other.
- No audit-log write on any reservation mutation, though `audit_logs` exists.
- `POST /booking/reservations` is registered **twice** — by `Booking.Register`
  and again at `router.go:359` pointing at `Reservations.Create`. First
  registration wins in Fiber; the second is dead code that reads as intent.
- The wizard collects **adults, children and nationality**, and the live API
  accepts none of them. They are silently dropped. Guest count is never
  validated against room capacity.

---

## 5. Performance

- **`GET /reservations` loads every stay for the hotel and filters in Go.**
  Status, search, `from` and `to` are all applied in a Go loop after
  `ListStays(hotelID, nil)`. No `LIMIT`, no `OFFSET`, no pagination, no SQL
  predicate. Cost grows linearly and without bound as the property books.
- **`GET /reservations/calendar` is O(days × stays).** It loads all stays and
  runs a nested loop over every day of the month for every stay, formatting two
  strings per iteration.
- `EnsureGuest`'s phone lookup wraps the column in
  `right(regexp_replace(phone, …), 10)`, which is **non-sargable** — no index can
  serve it, so it is a sequential scan of `guests` on every booking and every
  check-in.
- The `from`/`to` filters test `check_in_date` only, so a stay spanning the
  window is excluded — the filter answers a different question than the UI asks.
- No caching on reservation reads (correct for availability, arguably wasteful
  for the calendar).

### Index reality (verified — better than expected)

`guest_stays` is reasonably covered: `idx_guest_stays_room_dates (room_id,
check_in_date, check_out_date)`, `idx_guest_stays_dates`,
`idx_guest_stays_hotel_id`, `idx_guest_stays_guest_id`, `idx_guest_stays_property`.
The availability `NOT EXISTS` correlates on `room_id`, so it is served.

The genuine gaps are narrower:

- **`guests` has no index at all beyond its primary key** — no `hotel_id`, no
  phone, no email. Every `EnsureGuest` scans the table.
- **`folios` has `hotel_id` and `property_id` but not `(hotel_id, booking_id)`**,
  which is the exact lookup `EnsureFolioForBooking` performs on every check-in.
- `guest_stays` indexes lead with `room_id` / `check_in_date` rather than
  `hotel_id`, so tenant-scoped scans are less selective than they could be.

---

## 6. Database improvements (proposed — migrations only, all reversible)

Nothing here drops a column or destroys data.

### 6.1 Make double-booking impossible at the database level

The decisive fix, because it cannot be bypassed by the compat endpoint:

```sql
CREATE EXTENSION IF NOT EXISTS btree_gist;
ALTER TABLE guest_stays ADD CONSTRAINT guest_stays_no_overlap
  EXCLUDE USING gist (
    room_id WITH =,
    daterange(check_in_date::date, check_out_date::date, '[)') WITH &&
  ) WHERE (cancelled_at IS NULL);
```

Must be added **after** auditing for existing overlaps, or it will fail to
validate. Reversible with `DROP CONSTRAINT`.

### 6.2 Add the lifecycle columns the module is missing

```sql
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'confirmed';
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS cancelled_at TIMESTAMPTZ;
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS cancellation_reason TEXT;
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS cancellation_fee NUMERIC(12,2) NOT NULL DEFAULT 0;
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS confirmation_no TEXT;
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS adults INT NOT NULL DEFAULT 1;
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS children INT NOT NULL DEFAULT 0;
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS rate_plan_id UUID;
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS promo_code TEXT;
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS tax_amount NUMERIC(12,2) NOT NULL DEFAULT 0;
```

Backfill `status` from the existing timestamps so no row changes meaning.
Defaults keep every existing reader working unchanged.

### 6.3 Close the index gaps

```sql
CREATE INDEX IF NOT EXISTS idx_guests_hotel            ON guests(hotel_id);
CREATE INDEX IF NOT EXISTS idx_guests_hotel_phone_last10
  ON guests (hotel_id, right(regexp_replace(COALESCE(phone,''), '\D', '', 'g'), 10));
CREATE INDEX IF NOT EXISTS idx_guests_hotel_email      ON guests (hotel_id, lower(email));
CREATE INDEX IF NOT EXISTS idx_folios_hotel_booking    ON folios(hotel_id, booking_id);
CREATE INDEX IF NOT EXISTS idx_guest_stays_hotel_dates ON guest_stays(hotel_id, check_in_date, check_out_date);
```

The functional index on the last 10 phone digits is what makes `EnsureGuest`
sargable — it must match the expression in the query exactly.

### 6.4 Add the constraints that are missing

- `UNIQUE (hotel_id, confirmation_no)`
- `CHECK (check_out_date > check_in_date)` — currently enforced only in Go, and
  not at all through the compat endpoint
- `CHECK (status IN (...))` and `CHECK (rooms.status IN (...))` — `rooms.status`
  still has no CHECK constraint; the enum is enforced only in Go
- Unique partial index on `guests (hotel_id, phone)` to make find-or-create
  race-safe

### 6.5 Resolve the `bookings` / `guest_stays` split

Recommended: keep `guest_stays` as the single source of truth and **retarget the
booking-engine and night-audit queries onto it**, rather than syncing two tables.
`bookings` is the smaller surface (3 handlers) and has no data worth preserving
in the affected tenant. A compatibility `VIEW` named `bookings` over
`guest_stays` would let those queries keep their current SQL shape while reading
real data — the least invasive path, and reversible.

**Do not** resolve this by writing to both tables. That doubles every write path
and guarantees drift.

---

## 7. API improvements (backward compatible)

Existing endpoints keep their paths, methods and response shapes. Additions
only:

| Change | Compatibility |
|---|---|
| `POST /reservations` accepts optional `adults`, `children`, `promo_code`, `rate_plan_id`, `tax_amount` | Additive — absent fields default |
| `POST /reservations` returns `201` + `Location` | Status-code change; verify no client asserts `=== 200` |
| `POST /reservations` honours an `Idempotency-Key` header | Additive |
| `DELETE /reservations/:id` becomes a soft cancel with `reason` / `fee` | **Behaviour change** — see note |
| New `GET /reservations?page=&per_page=` with SQL-side filters | Additive; unpaginated default preserved initially |
| New `POST /reservations/:id/no-show` | New |
| New `POST /reservations/:id/move-room` | New |
| New `GET /reservations/:id/folio` | New |
| `POST /reservations/:id/checkout` refuses on outstanding balance, overridable | **Behaviour change** — gate behind a setting |
| Fix `POST /booking/search` columns | Repairs a 500 |

The two behaviour changes need explicit sign-off. Soft cancel in particular
changes what `GET /reservations` returns unless cancelled rows are filtered out
by default — they should be, with `?include_cancelled=true` to opt in.

---

## 8. UI improvements (reuse the existing design system)

No redesign. The portal uses shadcn/ui + Tailwind with an established
`PageHeader` / `Card` / `Badge` vocabulary; all of it stays.

- **Wizard step 3**: replace the hardcoded 18% GST with the rate quoted by the
  API, and send the quoted total. Add a promo-code field with inline validation
  against the existing `POST /booking/validate-promo`.
- **Wizard step 1**: send `adults` / `children`, and validate the total against
  the selected room's `capacity` before Continue enables.
- **Reservation detail**: surface the folio balance, and show a cancellation
  reason where one exists.
- **Cancel dialog**: capture reason and fee instead of a bare confirm.
- **List**: server-side pagination, and a filter for cancelled/no-show once
  those states exist.
- The `mhms-store` demo fallback and its "Demo data" badge are a deliberate
  offline affordance — keep them.

---

## 9. Security

Isolation is application-level, so these matter more than they would under RLS.

- **No RBAC on the reservation module.** `reservation_handler.go` contains no
  role check whatsoever. Any authenticated staff account — housekeeping, waiter,
  kitchen — can list every guest's name, email and phone, create bookings,
  cancel them, and check guests in and out. The only thing in the way is the
  feature-matrix gate, which is **default-on and fail-open** by design. The
  helper `requireAnyRoleFromToken` already exists and is used elsewhere; it is
  simply not applied here.
- **No audit trail on reservation mutations.** Combined with hard-delete
  cancellation (§4.1), a booking can be erased leaving no evidence it existed.
- **`/api/tables/:table` is a broad generic data surface** (§4.4) that can write
  `guest_stays` directly. It self-checks plan/module/role rules table-by-table,
  but it bypasses all domain validation.
- **`UpdateStay` has no column allowlist** (§4.12).
- Guest PII (`guest_email`, `guest_phone`) is returned in full on every list
  call with no masking or field-level authorisation.
- Positive: route registration order is deliberate and documented; the auth gate
  correctly precedes all staff handlers; tenant pool resolution happens after
  authentication.

---

## 10. Suggested sequencing for Phase 2

Ordered by risk retired per unit of work. Each is independently shippable and
testable, per the incremental-delivery requirement.

1. **Fix `POST /booking/search`** — a broken endpoint, a one-line column fix.
2. **Add the overlap exclusion constraint** — closes the double-booking race
   including the compat bypass. Audit for existing overlaps first.
3. **Stored status + soft cancellation** — additive columns, backfilled;
   unlocks no-show, cancellation reporting, and an audit trail.
4. **Re-validate and re-price on `PATCH`** — closes the revenue-loss bug.
5. **Unify `bookings` → `guest_stays` via a compatibility view** — makes night
   audit and the booking engine tell the truth.
6. **Post room revenue to the existing ledger** — reuse `postJournal`; do not
   build a second posting path.
7. **RBAC + audit logging on the module.**
8. **Pricing engine + promo integration**, replacing both hardcoded 18% sites.
9. **Pagination and SQL-side filtering**; index gaps from §6.3.
10. **Housekeeping task on check-out; deposit and ID capture on check-in.**

Items 1–4 are contained and low-risk. Item 5 is the architecturally significant
one and deserves its own review cycle.

---

## 11. Open questions for the product owner

1. **Cancellation policy** — should cancelling charge a fee, and on what
   schedule? This determines whether §6.2's `cancellation_fee` needs a rules
   table rather than a column.
2. **Should room revenue post on accrual (per night) or on check-in?**
   `PROGRESS.md` records this as a known gap; night audit is the natural place
   to post a nightly accommodation charge, and that decision shapes items 5 and 6.
3. **Is the legacy `bookings` table needed by anything outside this repo** — a
   report, an export, an OTA integration not visible in the code?
4. **Confirm the live `folios.guest_id` constraint** (§4.11) before provisioning
   any new environment from `migrations/`.
