-- 027_reservation_lifecycle.sql
--
-- Front-office reservation module: lifecycle state, pricing breakdown and
-- payment instrument details.
--
-- Every column is added with a default, so existing readers -- including the
-- generic /api/tables/:table path, which selects whole rows -- keep working
-- unchanged. Nothing is dropped and no existing value is rewritten except the
-- one-time status backfill below, which derives from columns already present.
--
-- Only tables created in migrations/ are touched here. guests and promotions
-- are created by the EnsureAppSchema statement list in schema.go, which runs
-- AFTER this file, so anything referencing them lives there instead. That is
-- why promo_id below carries no foreign key.
--
-- The overlap exclusion constraint that makes double-booking impossible is
-- deliberately NOT in this file. It would fail to apply on a database that
-- already contains overlapping stays, and a failing migration aborts API
-- start-up. It ships as scripts/add_guest_stays_overlap_constraint.sql, to be
-- run deliberately after the audit query that file carries.

-- --- Reservation lifecycle -------------------------------------------------

-- Status was previously derived from the two actual_* timestamps on every
-- read, which cannot express cancelled, no-show or tentative -- the states the
-- front desk actually needs. It is now stored.
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'confirmed';
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS cancelled_at TIMESTAMPTZ;
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS cancellation_reason TEXT;
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS cancellation_fee NUMERIC(12,2) NOT NULL DEFAULT 0;
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS cancelled_by UUID REFERENCES users(id) ON DELETE SET NULL;

-- Backfill from the timestamps the derived status used to read, so no row
-- changes meaning. Guarded on the default so a re-run cannot overwrite a
-- status the application has since set.
UPDATE guest_stays
   SET status = CASE
                  WHEN actual_check_out IS NOT NULL THEN 'checked_out'
                  WHEN actual_check_in  IS NOT NULL THEN 'in_house'
                  ELSE 'confirmed'
                END
 WHERE status = 'confirmed'
   AND (actual_check_in IS NOT NULL OR actual_check_out IS NOT NULL);

-- --- Occupancy and channel ------------------------------------------------

ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS adults INT NOT NULL DEFAULT 1;
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS children INT NOT NULL DEFAULT 0;

-- How the booking reached us, which is a different question from `source`
-- (which OTA or channel it came through).
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS approach_type TEXT NOT NULL DEFAULT 'manual';

-- Human-quotable reference. Staff and guests need something to say on the
-- phone that is not a UUID.
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS confirmation_no TEXT;

-- --- Pricing breakdown ----------------------------------------------------

-- total_amount keeps its existing meaning (the pre-tax accommodation total) so
-- every current reader is unaffected. The payable figure is
-- total_amount - discount_amount + tax_amount.
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS discount_amount NUMERIC(12,2) NOT NULL DEFAULT 0;
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS tax_amount NUMERIC(12,2) NOT NULL DEFAULT 0;
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS promo_code TEXT;
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS promo_id UUID;
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS rate_plan_id UUID;

-- --- Payment instrument details -------------------------------------------

-- The manual front-desk payment path needs the reference staff are given at
-- the terminal. The gateway integration keeps handling online capture.
ALTER TABLE payments ADD COLUMN IF NOT EXISTS upi_id TEXT;
ALTER TABLE payments ADD COLUMN IF NOT EXISTS transaction_ref TEXT;
ALTER TABLE payments ADD COLUMN IF NOT EXISTS card_last4 TEXT;
ALTER TABLE payments ADD COLUMN IF NOT EXISTS auth_code TEXT;
ALTER TABLE payments ADD COLUMN IF NOT EXISTS cash_received NUMERIC(12,2);
ALTER TABLE payments ADD COLUMN IF NOT EXISTS change_given NUMERIC(12,2);

-- --- Constraints ----------------------------------------------------------
--
-- Every ADD is preceded by a DROP IF EXISTS of the same name so the file is
-- idempotent: the runner re-applies any migration missing from its ledger, and
-- Postgres has no ADD CONSTRAINT IF NOT EXISTS. The file runs in one
-- transaction, so the window without the constraint is never observable.

-- Refuses a full card number at the storage layer, not just at the handler.
-- A PAN that reaches the database also reaches every pg_dump backup, so this
-- has to hold for writers that bypass the reservation handler -- including
-- /api/tables/:table.
ALTER TABLE payments DROP CONSTRAINT IF EXISTS payments_card_last4_check;
ALTER TABLE payments ADD CONSTRAINT payments_card_last4_check
  CHECK (card_last4 IS NULL OR card_last4 ~ '^[0-9]{4}$');

ALTER TABLE guest_stays DROP CONSTRAINT IF EXISTS guest_stays_status_check;
ALTER TABLE guest_stays ADD CONSTRAINT guest_stays_status_check
  CHECK (status IN ('tentative','confirmed','in_house','checked_out','cancelled','no_show'));

ALTER TABLE guest_stays DROP CONSTRAINT IF EXISTS guest_stays_occupancy_check;
ALTER TABLE guest_stays ADD CONSTRAINT guest_stays_occupancy_check
  CHECK (adults >= 0 AND children >= 0 AND adults + children >= 1);

-- NOT VALID: the date ordering was only ever enforced in Go, and never at all
-- through /api/tables/:table, so historical rows may violate it. Validating
-- them here would fail the migration and abort API start-up. New and updated
-- rows are checked from now on. Existing rows can be validated separately with
-- ALTER TABLE guest_stays VALIDATE CONSTRAINT guest_stays_dates_check
-- once the offenders have been corrected.
ALTER TABLE guest_stays DROP CONSTRAINT IF EXISTS guest_stays_dates_check;
ALTER TABLE guest_stays ADD CONSTRAINT guest_stays_dates_check
  CHECK (check_out_date > check_in_date) NOT VALID;

-- --- Indexes --------------------------------------------------------------

-- A confirmation number must be unique within a property but is nullable for
-- rows predating it, hence the partial unique index rather than a constraint.
CREATE UNIQUE INDEX IF NOT EXISTS guest_stays_confirmation_no_key
  ON guest_stays (hotel_id, confirmation_no)
  WHERE confirmation_no IS NOT NULL;

-- Serves the tenant-scoped date-range scans behind the reservation list, the
-- calendar and the arrivals/departures boards. The existing indexes lead with
-- room_id or check_in_date, so none of them is selective for "this hotel, this
-- window".
CREATE INDEX IF NOT EXISTS idx_guest_stays_hotel_dates
  ON guest_stays (hotel_id, check_in_date, check_out_date);

-- The reservation list filters by status far more often than anything else.
CREATE INDEX IF NOT EXISTS idx_guest_stays_hotel_status
  ON guest_stays (hotel_id, status);

-- EnsureFolioForBooking looks folios up by exactly this pair on every check-in
-- and only hotel_id was indexed.
CREATE INDEX IF NOT EXISTS idx_folios_hotel_booking
  ON folios (hotel_id, booking_id);

CREATE INDEX IF NOT EXISTS idx_payments_guest_stay
  ON payments (guest_stay_id);
