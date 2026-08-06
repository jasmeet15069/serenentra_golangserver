# Version History & Rollback

Every shipped version, what it changed, and exactly how to undo it.

Read `## How rollback works` first — the code, the database and the deployed
artifact roll back by three different mechanisms, and undoing a release usually
means doing more than one of them.

Two repos ship together and their versions are paired below:
`serenentra_golangserver` (API) and `HmsAdminStaffPortal` (portal).

---

## How rollback works

### 1. Code — `git revert`, not `git reset`

```bash
cd "MHMS_final/golangserver"
git revert --no-edit <sha>      # or <oldest>^..<newest> for a range
bash scripts/ship.sh -m "revert: <what and why>"
```

`git revert` because both repos are pushed and shared. `git reset --hard`
rewrites published history and would need a force-push, which discards whatever
anyone else pushed in the meantime.

Reverting alone is not enough if the version added a migration or a constraint —
see 2 and 3.

### 2. Database — migrations are additive, constraints are not

Every migration in this history is additive: it adds columns with defaults and
adds indexes. **Reverting the code does not remove them, and it does not need
to** — an older binary simply ignores columns it does not select. Leaving them
in place is the safe default.

Only drop a column if you are certain nothing reads it, and never to "clean up"
after a revert:

```sql
-- reversal for 027, if genuinely required
ALTER TABLE guest_stays DROP COLUMN IF EXISTS status;   -- etc.
```

Dropping `status` destroys every cancellation record, because the cancelled
state lives nowhere else. Prefer to leave it.

### 3. Deployed artifact — the VM keeps a backup per deploy

`scripts/deploy.sh` copies the remote source to `/tmp/deploy-backup-<stamp>/`
before every deploy, so the previous release is on the VM already:

```bash
ssh <VM_HOST> 'ls -1dt /tmp/deploy-backup-* | head'
```

To restore one without a rebuild from source:

```bash
ssh <VM_HOST> '
  cd /opt/hms/mhms_final/golangserver &&
  cp -a /tmp/deploy-backup-<stamp>/. . &&
  docker compose -f deployments/docker-compose.yml --env-file .env up -d --build api
'
```

⚠️ `/tmp` is cleared on reboot. For a rollback you might want days from now,
`git revert` + `ship.sh` is the reliable path; these snapshots are for getting
back in minutes.

**The portal is a prebuilt deploy** — the VM holds only `Dockerfile` +
`.output`, not source. Rolling it back means rebuilding from the older commit:

```bash
cd "MHMS_final/HMS admin portal"
git checkout <old-sha> -- src/
rm -rf .output && NITRO_PRESET=node-server npm run build
test -f .output/server/index.mjs || echo "WRONG PRESET — do not ship"
cd ../golangserver && bash scripts/deploy_portal.sh
```

`NITRO_PRESET=node-server` is not optional. A bare build auto-detects the
Cloudflare preset, emits no `.output/server/index.mjs`, and the container
crash-loops into a 502 — that took the portal down for ~4 minutes on 2026-08-05.

### 4. Verify after any rollback

```bash
bash scripts/ship.sh --check     # gates only: no deploy, no push
```

Then confirm the live 200s the same way `ship.sh` does: the tenant admin,
`/reservations/new`, the superadmin login, and the API `/health` from inside the
VM (it is bound to loopback; the public `/api/health` answers 401).

---

## Versions

Newest first. "Safe to revert alone" means reverting the code needs no database
change.

### v6 — `47ac13c` · OTA commission account and cancellation reversal
2026-08-06 · API only

Commission moves from `5000` Cost of Goods Sold to a new `5100` Channel
Commission. Cancelling an OTA booking now reverses both the sale and the
commission. Reversals are voucher-numbered, which they never were.

Both defects were found by running a signed Booking.com delivery end to end
against production and then removing every trace of it — neither is visible from
reading the code.

**Revert:** `git revert 47ac13c`, then ship.

**Database:** `5100 Channel Commission` is seeded into `accounting_accounts` by
`ensureChartOfAccounts` on the first posting. Reverting the code leaves the
account; it is harmless and unused. Do **not** delete it if anything has posted
to it — the journal lines reference it.

⚠️ **Reverting reinstates the two defects.** Commission returns to COGS, and
cancelled OTA bookings again leave an uncollectable receivable standing.

⚠️ Commission already posted to `5100` before a revert stays there. It does not
migrate back to `5000`, and it should not — moving posted entries between
accounts rewrites history. Leave them and note the changeover date.

---

### v5 — `665f528` · docs: night-audit fix and OTA ingestion
2026-08-06 · docs only

Documentation. Ships no code and needs no redeploy.

**Revert:** `git revert 665f528`. Safe to revert alone.

---

### v4 — `c995e88` · OTA and booking-engine ingestion
2026-08-06 · API only

`POST /api/v1/channel-manager/:connectionID/booking` and `/cancel`. HMAC-signed
webhooks for Booking.com, Agoda, OYO, MakeMyTrip, Expedia and direct booking
engines. Adds the `channel_bookings` table and its unique idempotency index.

**Revert:** `git revert c995e88`, then ship.

**Database:** adds `channel_bookings` via `EnsureAppSchema` (not a migration).
Reverting the code leaves the table; it is inert with no writer. Drop it only if
you are sure no delivery in it still needs replaying — **it holds the raw
payloads, so dropping it loses any booking that failed to map.**

**Blast radius if left broken:** OTAs receive 5xx and retry. No double bookings —
the unique index on `(hotel_id, channel_name, external_ref)` holds regardless.

---

### v3 — `8383d9c` · night audit reports from the ledger, not `bookings`
2026-08-06 · API only

Night audit and channel analytics moved off `bookings` (a table with no writers,
so every figure was zero) onto the general ledger and `guest_stays`. Fixes
`occupancy_rate` being stored as a raw room count.

**Revert:** `git revert 8383d9c`. Safe to revert alone — read-path only, no
schema change.

⚠️ **Reverting restores the zeros.** Any `night_audit_reports` row filed while
this version was live holds real figures; rows filed before it hold zeros. The
two are not distinguishable by looking at them, so note the changeover date if
you revert.

---

### v2 — `e03f2e1` · restaurant room charges linked to folio and check-out
2026-08-06 · API only

Dine-in bills settled as `room_charge` now post a `folio_charge` instead of a
`payments` row; check-out refuses on an outstanding balance, closes the folio,
and raises the housekeeping task.

**Revert:** `git revert e03f2e1`. Safe to revert alone.

⚠️ **Folio charges written while this was live remain on their folios.** After a
revert nothing collects them at check-out, so settle or write them off first:

```sql
SELECT f.id, f.booking_id, SUM(fc.amount + fc.tax_amount) AS owed
  FROM folios f JOIN folio_charges fc ON fc.folio_id = f.id
 WHERE f.hotel_id = '<hotel>' AND f.status = 'open' AND fc.charge_type = 'restaurant'
 GROUP BY f.id, f.booking_id;
```

---

### v1 — `bdcee69` + portal `6c42443` · walk-in reservations settle into the ledger
2026-08-06 · **API + portal — ship and revert together**

One form for walk-in and manual reservations. Submitting with payment writes the
CRM guest, accounting customer, reservation, payment, sales invoice and ledger
entry with its voucher, in one transaction. Adds the transaction seam
(`Querier` / `WithTenantTx`), promo codes, server-side pricing, and fixes
`POST /booking/search`, hard-delete cancellation, `PATCH` re-pricing and the
hardcoded hotel name.

**Revert:** both repos, or the portal will call endpoints the API no longer has:

```bash
git -C golangserver revert --no-edit bdcee69
git -C "HMS admin portal" revert --no-edit 6c42443
cd golangserver && bash scripts/ship.sh -m "revert: front-office settlement"
```

**Database — migration `027_reservation_lifecycle.sql`.** Additive: columns with
defaults on `guest_stays` and `payments`, four CHECK constraints, five indexes,
plus a one-time backfill of `status` from the existing timestamps. **Leave it in
place after a revert** — the older code ignores the columns.

If you must reverse it:

```sql
ALTER TABLE guest_stays  DROP CONSTRAINT IF EXISTS guest_stays_status_check;
ALTER TABLE guest_stays  DROP CONSTRAINT IF EXISTS guest_stays_occupancy_check;
ALTER TABLE guest_stays  DROP CONSTRAINT IF EXISTS guest_stays_dates_check;
ALTER TABLE payments     DROP CONSTRAINT IF EXISTS payments_card_last4_check;
DELETE FROM schema_migrations WHERE version = '027_reservation_lifecycle.sql';
-- columns intentionally not dropped: cancellation history lives only in them
```

⚠️ Dropping `payments_card_last4_check` removes the guarantee that a full card
number cannot be stored. Do not drop it as routine cleanup.

⚠️ **Ledger entries posted by this version are real.** Reverting stops future
postings; it does not unpost past ones, and it should not — a posted sale is
corrected by a reversing entry, never by deletion. Use
`reverseJournalsByReference` (`accounting_autopost.go`) if a specific posting
must be undone.

---

### Applied separately — `guest_stays_no_overlap` exclusion constraint
2026-08-06 · database only, **not** in any migration

Makes double-booking impossible for every writer, including
`/api/tables/:table` and the OTA webhook. Applied by hand after the audit
returned zero overlapping pairs, and proved by a rehearsed overlapping insert
that was refused with `23P01` and rolled back.

Deliberately not a migration: it validates the whole table on creation, and on a
database with existing overlaps that failure would abort API start-up — turning
a data problem into an outage.

**Reverse:**

```sql
ALTER TABLE guest_stays DROP CONSTRAINT IF EXISTS guest_stays_no_overlap;
```

**Re-apply:** `scripts/add_guest_stays_overlap_constraint.sql` — run its audit
query first.

⚠️ It uses `tstzrange(check_in_date, check_out_date, '[)')`, **not**
`daterange(check_in_date::date, …)`. The columns are `timestamptz` and that cast
is only STABLE, so Postgres rejects it with *"functions in index expression must
be marked IMMUTABLE"*. This is written down because it is not obvious and it
will be hit again by anyone reconstructing the constraint from memory.

---

## Baseline before this session

`38e3ebf` (API) · `571dc83` (portal) — the last state before the front-office
work began. To return the whole stack to it:

```bash
git -C golangserver revert --no-edit 38e3ebf..HEAD
git -C "HMS admin portal" revert --no-edit 571dc83..HEAD
cd golangserver && bash scripts/ship.sh -m "revert: back to pre-front-office baseline"
```

Then decide, separately and deliberately, what to do about the schema additions
and the overlap constraint — the sections above cover each. The default answer
is to leave them: they are additive and inert without the code that writes them.
