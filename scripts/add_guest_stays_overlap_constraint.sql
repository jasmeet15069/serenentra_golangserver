-- add_guest_stays_overlap_constraint.sql
--
-- Makes double-booking impossible at the database level.
--
-- WHY THIS IS NOT IN migrations/
-- The constraint validates the whole table when it is added. On a database
-- that already contains overlapping stays the ALTER fails, and a migration
-- that fails aborts API start-up -- turning a data problem into an outage.
-- So it is applied deliberately, after the audit below returns nothing.
--
-- WHY A CONSTRAINT AND NOT A CODE CHECK
-- The handler's existing check reads the overlapping stays and then inserts,
-- so two concurrent bookings for the same room both pass it. It is also
-- bypassed entirely by /api/tables/:table, which can INSERT INTO guest_stays
-- directly, and by the coming channel-manager webhook, which is concurrent
-- with the front desk by definition. Only the database sees every writer.
--
-- Run with psql (this file is not touched by the boot-time migration runner):
--   ssh root@<vm> 'docker exec -i docker-postgres-1 \
--     psql -U hotel -d hotel_harmony' < scripts/add_guest_stays_overlap_constraint.sql

-- --- STEP 1: audit ---------------------------------------------------------
-- Run this alone first. Every row it returns is a pair of stays already
-- double-booked on one room. The ALTER in step 2 will fail until each is
-- resolved -- by cancelling one side, moving it to another room, or correcting
-- its dates.

SELECT a.id            AS stay_a,
       b.id            AS stay_b,
       a.room_id,
       a.guest_name    AS guest_a,
       b.guest_name    AS guest_b,
       a.check_in_date AS a_in, a.check_out_date AS a_out,
       b.check_in_date AS b_in, b.check_out_date AS b_out
  FROM guest_stays a
  JOIN guest_stays b
    ON a.room_id = b.room_id
   AND a.hotel_id = b.hotel_id
   AND a.id < b.id
   AND a.check_in_date  < b.check_out_date
   AND a.check_out_date > b.check_in_date
 WHERE a.status <> 'cancelled'
   AND b.status <> 'cancelled'
 ORDER BY a.room_id, a.check_in_date;

-- --- STEP 2: apply ---------------------------------------------------------
-- Only once step 1 returns zero rows.
--
-- The range is half-open, '[)', so a departure on the 10th does not collide
-- with an arrival on the 10th -- matching ListRoomsAvailableBetween, whose
-- overlap comparisons are strict for the same reason.
--
-- tstzrange over the raw columns, NOT daterange(check_in_date::date, ...).
-- The columns are timestamptz and casting timestamptz to date is only STABLE,
-- not IMMUTABLE, because the result depends on the session TimeZone -- so
-- Postgres refuses it in an index expression with "functions in index
-- expression must be marked IMMUTABLE". tstzrange of two timestamptz values is
-- immutable and describes the real interval, which is the more precise
-- comparison anyway.
--
-- Cancelled stays are excluded: a cancelled booking must not hold inventory,
-- and the room has to be resellable for those dates immediately.
--
-- room_id needs btree_gist because a GiST index cannot handle plain equality
-- on a uuid without it.

BEGIN;

CREATE EXTENSION IF NOT EXISTS btree_gist;

ALTER TABLE guest_stays DROP CONSTRAINT IF EXISTS guest_stays_no_overlap;
ALTER TABLE guest_stays ADD CONSTRAINT guest_stays_no_overlap
  EXCLUDE USING gist (
    room_id WITH =,
    tstzrange(check_in_date, check_out_date, '[)') WITH &&
  ) WHERE (status <> 'cancelled');

COMMIT;

-- Applied to production 2026-08-06. The audit above returned 0 rows first, and
-- a rehearsed overlapping insert was refused with 23P01 and rolled back.

-- --- Rollback --------------------------------------------------------------
-- ALTER TABLE guest_stays DROP CONSTRAINT IF EXISTS guest_stays_no_overlap;
--
-- Once applied, a colliding INSERT or UPDATE raises SQLSTATE 23P01. The
-- reservation handler maps that to 409 with the same message its pre-check
-- produces, so the API contract is unchanged -- the constraint is the
-- guarantee behind the friendly error, not a replacement for it.
