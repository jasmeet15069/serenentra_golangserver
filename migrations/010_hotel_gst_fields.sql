-- Dedicated GST / tax-invoice identity fields on hotels. These drive the
-- restaurant Tax Invoice (GET /api/pos/bills/:id/invoice): legal entity name,
-- restaurant name/address, GSTIN, FSSAI, state, place of supply, HSN code, and a
-- default GST rate. Previously these were read from the settings JSONB. Real
-- columns make them queryable and editable via a settings form. All additive and
-- idempotent so existing tenants are unaffected.
ALTER TABLE hotels ADD COLUMN IF NOT EXISTS legal_entity_name TEXT;
ALTER TABLE hotels ADD COLUMN IF NOT EXISTS restaurant_name TEXT;
ALTER TABLE hotels ADD COLUMN IF NOT EXISTS restaurant_address TEXT;
ALTER TABLE hotels ADD COLUMN IF NOT EXISTS gstin TEXT;
ALTER TABLE hotels ADD COLUMN IF NOT EXISTS fssai TEXT;
ALTER TABLE hotels ADD COLUMN IF NOT EXISTS gst_state TEXT;
ALTER TABLE hotels ADD COLUMN IF NOT EXISTS place_of_supply TEXT;
ALTER TABLE hotels ADD COLUMN IF NOT EXISTS hsn_code TEXT NOT NULL DEFAULT '996331';
ALTER TABLE hotels ADD COLUMN IF NOT EXISTS gst_rate NUMERIC(5,2) NOT NULL DEFAULT 0;
