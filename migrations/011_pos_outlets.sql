-- POS outlets: a restaurant/bar/cafe that belongs to a hotel but can also serve
-- walk-in (outsider) customers and operate standalone. Dine-in tables, sessions
-- and bills scope to an outlet so a single hotel can run multiple outlets, and a
-- standalone restaurant can run without any room/guest context. Each outlet
-- carries its own GST identity for tax invoices. All additive and idempotent.
CREATE TABLE IF NOT EXISTS outlets (
  id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
  hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  code TEXT,
  type TEXT NOT NULL DEFAULT 'restaurant',
  is_standalone BOOLEAN NOT NULL DEFAULT false,
  address TEXT,
  legal_entity_name TEXT,
  gstin TEXT,
  fssai TEXT,
  place_of_supply TEXT,
  hsn_code TEXT NOT NULL DEFAULT '996331',
  default_tax_rate NUMERIC(5,2) NOT NULL DEFAULT 5,
  currency TEXT NOT NULL DEFAULT 'INR',
  is_active BOOLEAN NOT NULL DEFAULT true,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (hotel_id, name)
);
CREATE INDEX IF NOT EXISTS idx_outlets_hotel ON outlets(hotel_id);

-- NOTE: the outlet_id / customer_type / guest_stay_id / charge_mode columns on the
-- dine-in tables (restaurant_tables, dining_sessions, bills) are added by
-- EnsureAppSchema, which both creates those tables and alters them in the correct
-- order. They are intentionally NOT altered here because numbered migrations run
-- before EnsureAppSchema creates those tables.
