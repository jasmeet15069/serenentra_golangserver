# Serenentra — POS ⇄ Accounting Integration: Progress

Living record of what has been built, what is deployed, and what remains.
Last updated: 2026-08-05 (backfill completed; superadmin sign-in fixed).

---

## Where things stand

| Repo | HEAD | GitHub | Production VM |
|---|---|---|---|
| `serenentra_golangserver` | `f2f1c62` (code) | ✅ pushed | ✅ deployed, health 200 |
| `HmsAdminStaffPortal` | `bba3f01` | ✅ pushed | ✅ deployed, `/admin` 200 |
| `superadmin_serenentra` | `f351fe7` | ✅ pushed | ✅ Vercel, login verified 200 |
| `serenentra-landing` | unchanged | — | — |

All five containers healthy: `api`, `portal`, `superadmin`, `postgres`, `redis`.

`f2f1c62` is the last backend commit containing code; the commits after it on
`serenentra_golangserver` are documentation and the audit copy of the backfill
script, so the running binary is current with `main` despite the newer HEAD.

The commits above are the last that changed deployed behaviour. Later
documentation-only commits (including this file) ship no code and require no
redeploy.

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

### 6. Defects found and fixed along the way

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
- **Rehearse financial writes with `ROLLBACK`.** Running the exact script with
  `COMMIT` swapped for `ROLLBACK` shows every posting and exercises the balance
  guard without persisting anything.
