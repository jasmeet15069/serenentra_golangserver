# Serenentra — Platform Progress

Living record of what has been built, what is deployed, and what remains.
Last updated: 2026-08-06 (front-office reservation module: walk-in settlement
into the ledger, promo codes, restaurant room charges linked to the folio).

---

## Where things stand

| Repo | HEAD | GitHub | Production VM |
|---|---|---|---|
| `serenentra_golangserver` | `c995e88` | ✅ pushed | ✅ deployed, health 200 |
| `HmsAdminStaffPortal` | `6c42443` | ✅ pushed | ✅ deployed, `/reservations/new` 200 |
| `superadmin_serenentra` | `f351fe7` | ✅ pushed | ✅ Vercel, login verified 200 |
| `serenentra-landing` | unchanged | — | — |

All five containers healthy: `api`, `portal`, `superadmin`, `postgres`, `redis`.

Both code repos are deployed at the HEAD shown. Documentation-only commits
after these (including this file) ship no code and need no redeploy.

> This table went stale once, listing `d9f81c5` while three commits had already
> shipped past it. Update it in the same commit as the work, not afterwards.

---

## Front office — reservation module (2026-08-06)

Analysis and plan: `FRONT_OFFICE_ANALYSIS.md`, `FRONT_OFFICE_ENHANCEMENT_PLAN.md`.

Walk-in and manual reservations are one form. Submitting it with payment writes
the CRM guest and ID proof, the accounting customer, the reservation, the
payment, the sales invoice, and the ledger entry with its voucher — in one
transaction. Room revenue reaches the books for the first time; previously only
POS posted.

Nothing was reimplemented. The customer resolver is the POS one, so a guest who
ate in the restaurant and then books a room is one customer. Posting goes
through `postJournal`, and a `FOLIO <confirmation>` reference already mapped to
the `SAL` series, so room revenue joined the same numbered sequence as POS bills
without that mapping changing. Room revenue books to 4000, distinct from 4100
F&B.

### The transaction seam

`poolFromContext` now returns a `Querier` that both `*pgxpool.Pool` and `pgx.Tx`
satisfy, and `WithTenantTx` puts the transaction in the context — so every
existing repository method enlists unchanged, with none of its SQL moved. The
accounting and promo helpers took the same parameter change and needed no
call-site edits.

`WithTx` had a related bug: it began on the shared pool regardless of context,
so for a dedicated-database tenant it would have committed to the wrong
database.

### Restaurant → folio → check-out

A dine-in bill charged to a room collects nothing at the table, yet settlement
wrote no `folio_charge` — so the meal never reached the guest's bill and
check-out never asked for it — *and* mirrored a row into `payments`, which
`reports/revenue` sums as collections, so the same charge counted once when
signed and again when the guest actually paid.

Room charges now post to the folio, skip the `payments` mirror, and are
idempotent on the bill id. A charge is refused unless the table is linked to a
stay that is checked in and not yet departed. Check-out refuses on an
outstanding balance (`allow_outstanding=true` for company billing), closes the
folio, and raises the housekeeping task the departure never used to create.

### Pricing and promo codes

One `priceStay` serves the new `POST /reservations/quote` and create. The wizard
hardcoded 18% GST, displayed a total including it and sent neither, so the guest
agreed to one figure and the database stored another on every booking. The rate
is now `hotels.gst_rate`; an unconfigured tenant is quoted no tax rather than a
guess.

Promo rules lived only inside `ValidatePromo` and are now shared, with
`min_nights` / `min_amount` / `max_discount` — columns `promotions` always had —
enforced. Redemption puts the limit inside the `UPDATE`, so two desks cannot
over-redeem the last use.

### Double-booking is now impossible, not merely checked

The handler's read-then-write pre-check could be beaten by two concurrent
requests, and was bypassed entirely by `/api/tables/:table`, which inserts into
`guest_stays` directly. `guest_stays_no_overlap` was applied to production after
the audit returned zero rows; a rehearsed overlapping insert was refused with
`23P01` and rolled back. The handler keeps its friendly pre-check and maps
`23P01` to the same 409.

It uses `tstzrange(check_in_date, check_out_date, '[)')`, **not**
`daterange(check_in_date::date, …)`: the columns are `timestamptz` and that cast
is only STABLE, so Postgres rejects it in an index expression with *"functions
in index expression must be marked IMMUTABLE"*.

Applied by `scripts/add_guest_stays_overlap_constraint.sql`, deliberately not by
a migration — it validates the whole table on creation, and on a database with
existing overlaps that failure would abort API start-up.

### Also fixed

- `POST /booking/search` selected `rooms.max_guests` and `rooms.base_rate`.
  Neither exists in any migration or in `schema.go`, so the endpoint had never
  returned anything but `42703`. It and `/booking/availability` also excluded on
  `bookings`, which nothing writes, so the booking engine offered rooms the
  front desk had already sold. Both now read `guest_stays`.
- Cancelling hard-deleted the row, destroying the only evidence the booking
  existed. It now records status, reason, fee, timestamp and actor. The list
  still hides cancelled bookings by default, because deletion meant they were
  never visible.
- `PATCH` re-validated nothing: dates could be moved onto an occupied room, and
  `total_amount` was never recomputed, so extending a two-night stay to seven
  kept the two-night price.
- Check-in could be repeated, overwriting when the guest arrived, and worked on
  a cancelled booking.
- Guest notifications hardcoded "Grand Hotel Mumbai" for every tenant, and the
  booking confirmation was sent at check-in rather than at booking.
- Card capture is last four digits only. The API refuses a longer value rather
  than truncating it, because truncating would mean the full number had already
  been transmitted into the request logs and every `pg_dump`.
  `payments_card_last4_check` enforces the same rule at the column, so
  `/api/tables/:table` cannot store one either.

---

## Night audit and channel analytics — off `bookings` (2026-08-06)

`bookings` had no writers, and night audit plus channel analytics were its last
readers. Every figure they produced was zero or empty, and `CloseDay` persisted
those zeros as the day's official record — a full hotel filed a day with no
revenue, no tax, no occupancy, no arrivals and no departures.

Revenue and tax now come from the ledger, the same source the trial balance
reads, so the day-close and accounting cannot disagree. Occupancy, arrivals and
departures come from `guest_stays` — occupancy being who is in the building, not
who arrived today, and movements being the actual check-in/check-out stamps
rather than the dates the stay was booked for.

The revenue audit now reconciles two independent sources. It read
`revenue_daily`, which nothing populates, and assigned the one figure it got to
all three categories, so the difference was always zero. *Expected* is the
operation (folio charges raised, bills settled); *Actual* is the ledger. The gap
is the point: settlement posts to the ledger after committing the sale and only
logs on failure, so a failed posting used to leave revenue in the operation with
nothing behind it in the books.

The tax audit compared nothing and always answered "verified" — its grouping
expression `CASE WHEN b.total > 0 THEN 'GST' ELSE 'GST' END` returns 'GST'
either way. It now compares GST charged against GST posted to 2100, with a cent
of tolerance so a rounding tail is not called a discrepancy.

Also: `occupancy_rate` is a percentage and was being handed the raw occupied-room
count, so eight occupied rooms filed an occupancy of 8%.

`grep -rn "FROM bookings" internal/` now returns nothing.

---

## OTA and booking-engine ingestion (2026-08-06)

`POST /api/v1/channel-manager/:connectionID/booking` accepts a pushed
reservation; `/cancel` withdraws one. Booking.com, Agoda, OYO, MakeMyTrip and a
client's own website previously had nowhere to push to — `channel_connections`
recorded that a channel existed and nothing could arrive through it.

**Registered in the public block of `router.go`, and it must stay there.**
`api.Use(authGate)` gates only what is registered after it. In the staff block
the webhook would 401 every delivery, and an OTA treats 401 as retryable, so it
would retry forever while nothing was ever booked. It is not unauthenticated:
the URL names a `channel_connection` and the body is HMAC-SHA256 signed with
that connection's `api_key`, compared with `hmac.Equal`. An unknown connection
and a bad signature return the identical 401, because distinguishing them would
tell an unauthenticated caller which connection ids are real. A connection with
no `api_key` authenticates nothing.

Three properties matter more than the field mapping:

- the raw payload is stored **before** interpretation, so a delivery is never
  lost to a mapping bug and can be replayed;
- a unique index on `(hotel_id, channel_name, external_ref)` is the idempotency
  key — without it a redelivery turns one guest into four reservations holding
  four rooms. A redelivery of something already processed answers 200 with the
  original reservation, which is what stops the retry loop;
- the room is chosen and taken inside the transaction that creates the stay,
  against the same overlap constraint the front desk writes through, so an OTA
  and the desk cannot sell the same room concurrently.

A payload with no usable reference is refused: without one there is no
idempotency key. Field aliases (`reservation_id` / `booking_id` / `ref`,
`first_name`+`last_name` or `guest_name`, `check_in` or `check_in_date`) let a
new provider work without a code change.

Accounting: an OTA booking is money owed by the channel, so it books **Dr
Accounts Receivable**, not Cash or Bank, against a customer resolved by the same
resolver POS and the front desk use. Commission books separately as an expense
and a payable — netting it would understate both revenue and the cost of
distribution.

Verified on production that the routes are live and correctly gated: an unknown
connection returns `unauthorized channel` rather than `authentication is
required`, which is the proof the webhook's own signature check is running and
not the auth gate. **The full booking path has not been exercised against
production**, deliberately — doing so writes a reservation, a customer, journal
entries and consumes a voucher number, and the cleanup lesson below says that is
worth doing on purpose rather than as a smoke test.

---

## The problem this work addressed

The tenant `testingxyz` had four settled POS orders worth **₹5,074** and a
completely empty accounting module — zero journal entries, zero chart of
accounts, zero customers. Accounting looked broken. It was not.

*(Both halves are now resolved: the code path is fixed for future sales, and the
historical ₹5,074 has been backfilled — see sections 2 and 4.)*

The root cause was **two parallel POS implementations**:

| | Legacy `POSHandler` | Dine-in `POSDineInHandler` |
|---|---|---|
| Tables | `pos_orders` | `dining_sessions` → `kots` → `bills` |
| Posted to the ledger | **never** | yes, on settlement |
| Tenant's data | all 4 orders | 0 rows |

Every sale the tenant took went through the path with no accounting. Separately,
a dine-in sale captured only a free-text `guest_name`, so no POS customer could
ever reach `accounting_customers` for a manager to review or edit.

---

## Completed and deployed

### 1. POS dine-in → accounting customer link
*(`6970303`, backend · `2c0718c`, frontend)*

- `OpenSession` accepts optional `customer_phone` / `customer_email` /
  `customer_gstin`, resolves them to an accounting customer, stamps the id on
  the session; settlement copies it to the bill.
- Matching is by **phone**, normalised so `+91 98765 43210` and `9876543210`
  resolve to the same customer rather than duplicating a regular each visit.
- **Policy** — attaching a customer is optional, matching Square/Toast/
  Lightspeed. A record is created only when contact details exist *or* money is
  owed. A bare name does not qualify: it identifies nobody and cannot be matched
  later, so it would only pollute the AR ledger. Anonymous cash walk-ins stay
  anonymous.
- Settlement picks the debit side by how the sale settled:
  - `cash` / `card` / `upi` → Dr Cash or Bank (unchanged)
  - `credit` / `room` → **Dr Accounts Receivable**, against the customer
  - a credit sale with no customer is refused rather than posted to a
    receivable nobody can collect.
- Frontend: an optional **Customer details** block on the open-table dialog.

### 2. Legacy `/pos/orders` now posts to the ledger
*(`261d2bf`)*

- Both `Create` (rung up already settled) and `Update` (PATCH to paid) book the
  sale, so no sale escapes the books whichever screen took it.
- Amounts come from the stored row, not the request, so a patched `total`
  cannot mis-state revenue.
- **Idempotent**, keyed on `ORDER <number>`. `PATCH /pos/orders/:id` is a
  generic column patcher a client may repeat; without the guard the same sale
  would be booked on every call. `alreadyPosted` deliberately *fails closed* —
  if the check errors it reports "posted", because a missing journal can be
  reposted whereas double-booked revenue corrupts the books.
- `isPaidStatus` accepts `paid` / `completed` / `settled` / `closed`; the column
  is free text and has been written several ways.

### 3. D365-style voucher numbering
*(`4f626b1` + `f2f1c62`, backend · `bba3f01`, frontend)*

- Numbering happens inside `postJournal` — the single chokepoint every posting
  site already flows through — so **all eight existing posting events gained
  vouchers without any of them being modified**, and no future call site can
  forget to supply one.
- Type derived from the reference prefix:

  | Type | Meaning | Triggered by |
  |---|---|---|
  | `SAL` | sales | `BILL` / `ORDER` / `INV` / `FOLIO` |
  | `PUR` | purchase | `GRN` / `PO` / `DN` |
  | `PAY` | payment | `VP` / `PAY` |
  | `GEN` | general | everything else, incl. COGS |

- Numbers come from `accounting_voucher_counters` via a **single atomic
  UPSERT**. `MAX(voucher_no)+1` would let two concurrent tills read the same
  maximum and be handed the same number, which the unique index would then
  reject — losing one of the sales.
- A numbering failure logs and posts the entry *unnumbered* rather than dropping
  it: the ledger still has to balance, and an unnumbered entry is repairable
  whereas a lost sale is not.
- Exposed on both `GET /accounting/journal-entries` and `…/:id`.
- Frontend: a Voucher column and a colour-coded chip (green sales, blue
  purchase, amber payment, muted general). Pre-numbering entries render a muted
  dash rather than pretending to a voucher. Search spans voucher number and
  reference as well as description.

### 4. Historical ₹5,074 backfilled into the ledger
*(2026-08-05, data operation — no code change)*

The four legacy orders settled before the path posted to the ledger were
recovered into the books, one journal per order:

| Voucher | Date | Order | Dr Cash | Cr Revenue | Cr GST |
|---|---|---|---|---|---|
| `SAL-000001` | 2026-07-22 | ORD-…-219 | 897.00 | 760.00 | 137.00 |
| `SAL-000002` | 2026-07-27 | ORD-…-78580 | 1,204.00 | 1,020.00 | 184.00 |
| `SAL-000003` | 2026-07-28 | ORD-…-69571 | 1,510.00 | 1,280.00 | 230.00 |
| `SAL-000004` | 2026-07-29 | ORD-…-44092 | 1,463.00 | 1,240.00 | 223.00 |
| | | **Total** | **5,074.00** | **4,300.00** | **774.00** |

How it was done safely:

- **Rehearsed with `ROLLBACK` first**, confirming all four postings and the
  balance check before anything was committed.
- **Dated to each order's own date**, not the run date. Revenue belongs to the
  period it was earned; dating July sales in August would misstate both months.
  This is the one reason it could not go through the API — `postJournal` writes
  `entry_date = CURRENT_DATE`.
- **Mirrors `postSalesToLedger` exactly** (Dr 1000 Cash, Cr 4100 Revenue,
  Cr 2100 GST), and draws vouchers from the same `SAL` counter, so the entries
  are indistinguishable from live ones and sit in one continuous series.
- **Cash (1000) not Bank (1010)** — `pos_orders` records no payment method, and
  the live legacy path defaults to cash for the same reason.
- **Idempotent**: skips any order whose `ORDER <number>` reference already
  exists. A re-run posted 0 entries.
- **Balance guard inside the transaction** — an unbalanced result raises and
  rolls back rather than committing.

Resulting trial balance: Cash `+5,074.00`, GST Payable `−774.00`, F&B Revenue
`−4,300.00` — **net 0.00**. The four `pos_orders` rows were not modified;
backfilling posts to the books, it does not rewrite source records.

Script committed verbatim as executed at
`scripts/backfill_pos_orders_ledger.sql` — kept unmodified so the audit record
matches exactly what was run against the books.

### 5. Superadmin console sign-in restored
*(`f351fe7`, superadmin_serenentra)*

Sign-in at https://superadmin.serenentra.com was failing for every user. The
credentials were never at fault — the same account authenticated against the
backend directly with 200:

```
direct backend  superadminportal.serenentra.com/api/auth/sign-in  -> 200 OK
via Vercel      superadmin.serenentra.com/api/auth/sign-in        -> 404
                                              DNS_HOSTNAME_RESOLVED_PRIVATE
```

The Nitro proxy fell back to `http://localhost:8787` when `API_BASE_URL` was
unset, and on Vercel's edge localhost is a private address it refuses to proxy
to. The build succeeded and pages rendered, which is exactly why the console
looked healthy while being unusable — and why the failure pointed at DNS rather
than at configuration.

The default is now environment-aware: a Vercel build defaults to the real
backend, local development still defaults to localhost, and `API_BASE_URL`
remains an override for pointing at a different backend. This removes a hidden
operational requirement — the console previously depended on a dashboard setting
that nothing in the repo enforced or documented.

Verified live: sign-in through the website returns 200. The local-dev and
override-set paths follow from the same expression but were not separately
exercised.

### 6. Superadmin console audited end to end

Every screen was exercised against the live site: dashboard, tenants, plans,
plan features, admin accounts, security, monitoring, front office, billing,
night audit, reports, users, modules, POS, marketing leads, and all seven
per-tenant panels. Every one returned 200 with real data. Nothing was broken
on the read side.

Seven **mutation** endpoints answered 200 for an id that does not exist. The
UPDATE matched zero rows and the handler reported success anyway, so the
console said "updated" while nothing had changed:

```
PUT   /platform/tenants/<unknown>/feature-matrix   200
POST  /platform/tenants/<unknown>/backup/run       200
POST  /platform/tenants/<unknown>/redis-backup/run 200
PATCH /rooms/<unknown>/status                      200
POST  /reservations/<unknown>/checkin              200
POST  /reservations/<unknown>/checkout             200
PATCH /housekeeping/tasks/<unknown>                200
```

The backup case was the serious one. `resolveBackupDSN` discards the error
from its `tenant_registry` lookup, so an unknown tenant kept the `"shared"`
default and the endpoint produced a **full pg_dump of the shared platform
database**, written to a file named after a tenant that does not exist and
recorded as that tenant's successful backup. Restoring from it would have
restored the whole platform.

Fixed at the layer that knows in each case: the room and stay repositories
return `ErrNotFound` on a zero-row update and the handlers map that to 404;
the housekeeping update checks `RowsAffected`; and a `requireTenant` guard
covers both backup endpoints and both feature-matrix endpoints.

`requireTenant` keys on `hotels`, not `tenant_registry`. A tenant exists in
`hotels` from creation while its registry row is written later by
provisioning, so keying on the registry would have 404'd a freshly created
tenant - a worse bug than the one being fixed.

Backup **routing** was deliberately left alone. `provisioned` tenants really
do live in the shared database; their per-tenant database exists but holds
empty scaffolding, so falling back to the shared DSN is correct for them.
Only the unknown-tenant case was wrong.

Also enforced the room status enum. `rooms.status` has no CHECK constraint
and the handler passed the raw string through, so `PATCH` with `"clean"`
(the status is `"cleaning"`) stored a value that every board switching on
status would then mis-render.

*(`09700b7`)*

### 7. Backups were being destroyed by their own deploys

`backupDir` pointed at `/app/backups` and the comment there stated it was a
mounted volume. Neither compose file mounted anything, so artifacts lived in
the container's writable layer. Every deploy recreates the api container,
which silently deleted every backup taken so far while `backup_jobs` went on
reporting them successful and downloadable. The two job rows from June have
no file behind them and never will.

Three changes were needed together:

- both compose files mount a `backups` named volume at `/app/backups`
- `deploy.sh` ships `deployments`, which it never did - Dockerfile and
  compose edits had been *appearing* to deploy while the VM kept its old
  files
- backup history reports `artifact_available`, computed by stat-ing the file,
  so the console stops offering downloads that can only fail

Shipping `deployments` means the repo copy now overwrites the VM copy, and
the two had drifted: production ran a `superadmin` service that was never
written down in the repo. Deploying without reconciling that first would have
removed the superadmin console's service definition. It is now in the file,
matching the VM apart from the volume being added.

Verified on the VM: took a backup, forced a container recreation, and the
artifact was still there; history reports `available=true` for it and `false`
for the two June rows whose files are gone.

*(`3e24fec`)*

### 8. New Reservation wizard could not be completed

Step 2 selected rooms from `GET /rooms` by `status === "available"` - whether
a room is free *right now*, not whether it is free for the requested dates.
On testingxyz both rooms read `occupied` and `cleaning`, so the step offered
nothing, Continue stays disabled until a room is chosen, and the wizard could
not be finished for any dates however far ahead. The create call was never at
fault; posting the wizard's exact body returned 200.

Added `GET /api/rooms/available?check_in=&check_out=`, which answers the
question the form is actually asking: rooms with no stay overlapping the
window. Overlap comparisons are strict, so a departure on the 10th does not
block an arrival on the 10th. Only `maintenance` excludes a room outright,
being the one status meaning the room cannot be occupied at all. Dates are
required rather than defaulted, so the endpoint cannot quietly answer the
wrong question, and it is uncached because a stale hit would offer a room
that was just taken.

The wizard now calls it, names the dates in its empty state, and drops a
chosen room that a date change has invalidated rather than submitting a room
the server would refuse.

A second bug surfaced while reproducing this: **cancelling a reservation
released the room unconditionally**. A room routinely carries a current guest
plus later bookings, so cancelling one of those advertised an occupied room
as free and opened it to double-booking. The room is now released only when
no stay is checked in and not yet checked out.

Verified against the live tenant: the booked room disappears from its own
dates, remains available for a non-overlapping window and from its checkout
date onward, and a cancel no longer frees a room whose guest is still in it.

*(`d9f81c5` backend · `571dc83` frontend)*

### 9. Release pipeline: `scripts/ship.sh`

The build → test → lint → deploy → smoke-test → commit → push cycle was being
run by hand every time, which is how the portal went down for four minutes on
2026-08-05 (a build without `NITRO_PRESET`) and how compose changes appeared to
deploy while never leaving the laptop. It is now one command:

```bash
bash scripts/ship.sh -m "commit message"    # backend + frontend
bash scripts/ship.sh -m "msg" --backend     # one side only
bash scripts/ship.sh --check                # verify only: no deploy, no push
DRY_RUN=1 bash scripts/ship.sh -m "msg"     # everything except deploy/push
```

Each step gates the next, so nothing reaches production that has not passed
the step before it and **nothing is committed or pushed unless the deployed
result answered 200**. Failures print the last 20 lines of output and set a
non-zero exit.

Two guards encode incidents rather than good intentions:

- the frontend build is forced to `NITRO_PRESET=node-server` and then checked
  for `.output/server/index.mjs`; without it the ship is refused, because the
  auto-detected Cloudflare preset emits no server entry and the container
  crash-loops into a 502
- the push retries three times with backoff, since the GitHub DNS lookup from
  this machine fails intermittently

`scripts/deploy_portal.sh` was extracted at the same time — the portal ship
(package `.output` + Dockerfile, back up the remote copy, extract, rebuild,
wait for healthy) had only ever existed as a sequence typed by hand.

The API's `/health` is bound to the VM's loopback and is not exposed through
nginx, so the smoke test checks it from inside the VM the way `deploy.sh`
does, rather than asserting on the 401 that the public `/api/health` returns.

### 10. Defects found and fixed along the way

- **Silently swallowed ledger errors.** The posting call was `journalID, _ =`,
  so a failure left a completed sale with no journal entry and no trace. Now
  logged with bill number, amount and method.
- **`deploy.sh` had been broken since an SMTP password was added.** The remote
  step ran `set -a; . "$REMOTE_DIR/.env"`, which *executes* the file; the Gmail
  app password (four space-separated groups, unquoted) ran its second word as a
  command and aborted under `set -e`. Now passed to compose with `--env-file`,
  whose dotenv parser handles such values and never executes them. *(`29baeb3`)*
- **Migration `022` was not idempotent.** Three bare `ADD CONSTRAINT` statements
  fail with `42P07` on re-run, aborting start-up — precisely the
  partially-migrated recovery case the no-auto-baseline runner was designed to
  survive. Now drop-then-add. *(`63ec063`)*
- **Migration linter extended** to reject dollar-quoted bodies and
  non-idempotent `ADD CONSTRAINT`, and rewritten from ~4 min to ~1 s by
  replacing per-line `grep`/`sed` subprocesses with bash builtins. *(`b6acb21`)*

---

## Verified end-to-end on production

A real dine-in sale was run through the whole chain and then removed:

```
open table (customer attached)  → customer_id created
add KOT → send to kitchen       → 201 / 200
generate + finalize bill        → ₹840 (₹800 + ₹40 GST)
settle cash                     → paid

Dr  1000 Cash                      840.00
  Cr 4100 Food & Beverage Revenue          800.00
  Cr 2100 GST Payable                       40.00
                          BALANCE  840.00 = 840.00

BILL_CUSTOMER | E2E Ramesh | 9876500999
```

Submitted `+91 98765 00999`; stored `9876500999` — the normaliser collapsed it.
Chart of accounts auto-seeded from 0 → 10 accounts on the first posting, exactly
as the lazy-seed design intends.

Voucher numbering verified separately: two probe sales produced `SAL-000001` and
`SAL-000002`, sequential and correctly typed, on both the list and detail
endpoints. Repeat `PATCH status=Paid` produced **one** entry, confirming
idempotency.

Those probe entries *and the voucher counter* were then removed, so numbering
restarted at `SAL-000001` for the backfill below. The `SAL-000001..000004` now
in the ledger are the four historical sales, not the discarded probes.

### Tenant state after all work and cleanup

```
journal entries : 4   (SAL-000001..000004, the backfill)
chart of accounts: 10
trial balance   : 10 accounts, net 0.00  BALANCED
pos_orders      : 4   (historical, untouched)
bills / dining_sessions / kots / outlets / restaurant_tables
menu_categories / accounting_customers / payments / bill_payments : 0
```

`GET /reports/revenue` correctly reads `total=0, payments=0`: it reports
collections from the `payments` table, and these historical orders were settled
before that table was in use. The ledger is the authoritative record and holds
the full ₹5,074.

**Cleanup miss, corrected.** An earlier claim that all test artifacts had been
removed was wrong. Dine-in settlement writes to **both** `bill_payments` *and*
`payments`; only the former was cleaned, leaving a ₹840 row
(`PAY-20260805-75326`) that surfaced in `reports/revenue`. It was found while
verifying the backfill and removed, and nine related tables were then audited to
zero. Lesson recorded in Operational notes below.

---

## Not done — known gaps

### Roughly half the business processes still never post
Handlers with **zero** ledger calls: `payment_handler`, `procurement_handler`,
`inventory_handler`, `housekeeping_handler`, `nightaudit_handler`,
`asset_handler`. Concretely unposted:

- **Folio charges** — only *settlement* posts, so revenue appears when money is
  collected rather than when earned. Cash-basis behaviour inside an otherwise
  accrual system.
- **Inventory adjustments / wastage / stock issues** — stock moves with no COGS
  or shrinkage entry.
- **Payroll and expenses** — nothing.
- **Asset depreciation** — nothing.
- **Night audit** — computes figures but posts no day-end voucher.

### No P&L or Balance Sheet
The only financial-statement endpoint is `/accounting/trial-balance`. Both are
derivable from the existing chart of accounts (types are already
`asset/liability/equity/revenue/expense`), so this is mostly aggregation work.

### No fiscal period control
Nothing prevents posting into a closed day. Night audit runs but does not seal
the ledger.

### Backups are manual, unscheduled, and unverified
`backup_configurations` stores a cron expression per tenant, but nothing reads
it — every backup so far was triggered by hand from the console. There is also
no restore path and no integrity check, so an artifact's usefulness is
untested. The two June job rows have no file behind them (see section 7) and
should be treated as absent rather than as history.

Separately, `provisioned` tenants have a per-tenant database that exists but
holds empty scaffolding; their live data is in the shared `hotel_harmony`.
`testing123_hotelops` is an orphan of a deleted tenant. Neither is harmful,
but both will mislead anyone reading `tenant_registry` as a map of where data
lives.

### No customer linkage on the legacy POS path
`pos_orders` carries a free-text `customer_name` and no phone, so there is no
key to match an accounting customer on. Contact capture lives on the dine-in
path, which has the fields. Closing this means adding customer columns to
`pos_orders` — worth doing only if that screen is staying long-term.

---

## Suggested next steps, in order

1. **P&L + Balance Sheet endpoints** — pure aggregation over data that already
   exists; no schema change, high visible value.
2. **Folio charges post on accrual** — closes the largest correctness gap; makes
   the system genuinely accrual rather than mixed.
3. **Fiscal period locking** — make night audit seal the ledger.
4. **Inventory / expense / payroll posting** — each needs its own debit/credit
   design; largest remaining chunk.

---

## Operational notes

- **Portal deploys are prebuilt.** The VM holds only `Dockerfile` + `.output`.
  Build with **`NITRO_PRESET=node-server`** — a bare `npm run build`
  auto-detects the Cloudflare preset, produces no `.output/server/index.mjs`,
  and the container crash-loops. This took the portal down for ~4 minutes on
  2026-08-05; the deploy step now aborts if that file is missing.
- **Backend deploys** via `bash scripts/deploy.sh` (needs gitignored
  `scripts/deploy.env` with `VM_HOST` and `REMOTE_DIR`). `DRY_RUN=1` shows the
  steps without shipping. Each run backs up the remote copy to
  `/tmp/deploy-backup-<stamp>/`.
- **Migrations cannot reference schema.go tables.** `EnsureAppSchema` runs
  `runSQLMigrations` *before* creating the Go-defined tables, so a migration
  touching `dining_sessions`, `bills` or `accounting_*` fails on a fresh
  database. Evolve those in the `EnsureAppSchema` statement list instead — it
  already carries 42 `ALTER TABLE … ADD COLUMN IF NOT EXISTS` statements.
- **`splitSQLStatements` has no dollar-quote awareness.** No migration may
  contain a `DO $$ … $$` block, function or trigger body; it would be cut into
  invalid fragments. The linter now rejects them.
- **API quirks:** bills must be finalized (`POST /pos/bills/:id/finalize`)
  before payment, else 409. KOT items use `item_name`, not `name`, and
  `menu_item_id` is optional — free-text items are supported.
- **A dine-in settlement writes two payment rows.** `bill_payments` (the bill's
  own tender line) *and* `payments` (the hotel-wide collections table that
  `reports/revenue` reads). Deleting a test bill leaves the `payments` row
  behind, where it silently inflates the revenue report. Clean both.
- **Backfills must set `entry_date` explicitly.** `postJournal` always writes
  `CURRENT_DATE`, so any historical posting driven through the API lands in the
  wrong period. Historical corrections need direct SQL, in a transaction, with a
  balance guard and an idempotency key on the reference.
- **A deploy must not depend on an unset env var to be usable.** The superadmin
  console fell back to `localhost:8787` when `API_BASE_URL` was missing; on
  Vercel that is a private address the edge refuses, so every API call 404'd
  while the build and the pages looked fine. Defaults for production builds
  should point at production. Treat env vars as overrides, not requirements.
- **A zero-row UPDATE is not a success.** Seven endpoints returned 200 for
  ids that do not exist because `Exec`'s command tag was discarded. Check
  `RowsAffected()` (or have the repository return `ErrNotFound`) wherever the
  caller will report the outcome to a user.
- **A default that hides a missing lookup is worse than an error.** The
  backup DSN resolver discarded its lookup error and fell back to "shared",
  which turned an unknown tenant into a full platform dump. Prefer failing to
  defaulting when the input was supposed to identify something.
- **Check what the deploy actually ships.** `deploy.sh` tarred only source
  directories, so compose and Dockerfile edits appeared to deploy while the
  VM kept its old files. Before trusting an infra change, confirm it on the
  VM rather than trusting a clean deploy log.
- **Reconcile drift before letting the repo overwrite the VM.** Production
  ran a `superadmin` service the repo did not define; shipping `deployments`
  without adding it first would have deleted that service.
- **Ask the question the screen is asking.** Booking forms need availability
  for a date range, not current room status - filtering by status hid every
  bookable room at a full hotel and made the wizard impossible to finish.
- **Rehearse financial writes with `ROLLBACK`.** Running the exact script with
  `COMMIT` swapped for `ROLLBACK` shows every posting and exercises the balance
  guard without persisting anything.
