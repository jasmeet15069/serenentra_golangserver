-- 028_guest_services.sql
--
-- Guest services on the existing housekeeping_assignments table: wake-up calls,
-- bell-boy dispatch, room service, and the supervisor sign-off that
-- inspected_by has implied since migration 002 without ever having a workflow.
--
-- A separate table was the obvious alternative and the wrong one. These are all
-- "a job for a member of staff against a room, with a status" -- the same shape
-- housekeeping already has, and the same board the floor supervisor already
-- watches. A second table would have meant a second board, a second status
-- vocabulary, and two places to look for what is owed to a guest.
--
-- Every column is nullable or defaulted, so existing rows and every current
-- reader are unaffected.

-- --- When the job is due ----------------------------------------------------
--
-- A wake-up call is defined by its time. Without this the table can record that
-- one was requested but not when for, which is the only fact that matters. Null
-- means "as soon as someone can", which is how every existing row behaves.
ALTER TABLE housekeeping_assignments ADD COLUMN IF NOT EXISTS scheduled_for TIMESTAMPTZ;

-- --- Who it is for ----------------------------------------------------------
--
-- The guest-request endpoint looked a stay up to find its room and then threw
-- the stay away, storing only room_id. A request could not be traced back to
-- the guest who made it, and a room changing occupants left its open requests
-- silently attached to whoever came next.
--
-- ON DELETE SET NULL, not CASCADE: a completed wake-up call is a record of
-- service delivered and should outlive the reservation row.
ALTER TABLE housekeeping_assignments
  ADD COLUMN IF NOT EXISTS guest_stay_id UUID REFERENCES guest_stays(id) ON DELETE SET NULL;

-- --- Supervisor sign-off ----------------------------------------------------
--
-- inspected_by has existed since 002 with nothing to say WHEN the room passed
-- inspection, so "inspected" could not be distinguished from "inspected weeks
-- ago" and no board could show what was still awaiting sign-off.
ALTER TABLE housekeeping_assignments ADD COLUMN IF NOT EXISTS inspected_at TIMESTAMPTZ;

-- --- Constraints ------------------------------------------------------------
--
-- DROP before ADD because Postgres has no ADD CONSTRAINT IF NOT EXISTS and the
-- runner re-applies any migration missing from its ledger.

-- NOT VALID: task_type is free text today and tenants may hold values this list
-- does not know. Validating them here would fail the migration and abort API
-- start-up. New and updated rows are checked from now on.
ALTER TABLE housekeeping_assignments DROP CONSTRAINT IF EXISTS housekeeping_task_type_check;
ALTER TABLE housekeeping_assignments ADD CONSTRAINT housekeeping_task_type_check
  CHECK (task_type IN (
    'checkout_clean','daily_clean','deep_clean','turndown','linen_change',
    'maintenance','inspection','guest_request','room_service','wake_up_call',
    'bell_boy','luggage','laundry_pickup','amenity_request'
  )) NOT VALID;

-- --- Indexes ----------------------------------------------------------------

-- The wake-up board asks "what is due in the next N minutes, still pending",
-- which is this index exactly. Partial, because a scheduled time only exists on
-- the small minority of rows that carry one.
CREATE INDEX IF NOT EXISTS idx_housekeeping_due
  ON housekeeping_assignments (hotel_id, scheduled_for)
  WHERE scheduled_for IS NOT NULL;

-- "What is this guest waiting on" -- the question the desk asks when a guest
-- rings down to chase a request.
CREATE INDEX IF NOT EXISTS idx_housekeeping_stay
  ON housekeeping_assignments (hotel_id, guest_stay_id)
  WHERE guest_stay_id IS NOT NULL;

-- The supervisor's queue: completed work not yet signed off.
CREATE INDEX IF NOT EXISTS idx_housekeeping_awaiting_inspection
  ON housekeeping_assignments (hotel_id, status)
  WHERE inspected_at IS NULL;
