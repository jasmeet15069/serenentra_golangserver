-- Branches (a.k.a. properties) -- the architecture diagram's CLIENT to BRANCH
-- model. A CLIENT is a hotels row and its BRANCHES are properties rows living
-- inside the same client data set, distinguished by property_id. This migration
-- is purely additive: it enriches the properties table and adds a NULLABLE
-- property_id (branch) foreign key to the core operational tables so rows can be
-- attributed to a branch. Existing rows stay branch-less (NULL = client-level /
-- all-branches) so nothing breaks and handlers opt into branch scoping later.

-- 1. Enrich the branch (properties) table.
ALTER TABLE properties ADD COLUMN IF NOT EXISTS code TEXT;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS phone TEXT;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS email TEXT;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS timezone TEXT NOT NULL DEFAULT 'UTC';
ALTER TABLE properties ADD COLUMN IF NOT EXISTS currency TEXT NOT NULL DEFAULT 'USD';
ALTER TABLE properties ADD COLUMN IF NOT EXISTS is_active BOOLEAN NOT NULL DEFAULT true;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS is_primary BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT now();

-- One branch per client is the implicit primary so single-branch clients work
-- with zero configuration. Mark the demo client's Main Property as primary.
UPDATE properties SET is_primary = true
WHERE id = '00000000-0000-0000-0000-000000000002' AND is_primary = false;

-- A client's branch codes are unique within the client.
CREATE UNIQUE INDEX IF NOT EXISTS uq_properties_hotel_code
  ON properties(hotel_id, code) WHERE code IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_properties_hotel ON properties(hotel_id);

-- 2. Add a nullable branch (property_id) FK to core operational tables.
-- NOTE: pos_orders is excluded here because its CREATE TABLE is in
-- EnsureAppSchema (schema.go), not in a migration, so the table does
-- NOT exist when migration 012 runs on a fresh database. The ALTER TABLE
-- and index for pos_orders are duplicated in EnsureAppSchema and applied
-- after all migrations, so they don't need to live in this file.
ALTER TABLE rooms ADD COLUMN IF NOT EXISTS property_id UUID REFERENCES properties(id) ON DELETE SET NULL;
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS property_id UUID REFERENCES properties(id) ON DELETE SET NULL;
ALTER TABLE folios ADD COLUMN IF NOT EXISTS property_id UUID REFERENCES properties(id) ON DELETE SET NULL;
ALTER TABLE work_orders ADD COLUMN IF NOT EXISTS property_id UUID REFERENCES properties(id) ON DELETE SET NULL;
ALTER TABLE housekeeping_assignments ADD COLUMN IF NOT EXISTS property_id UUID REFERENCES properties(id) ON DELETE SET NULL;
ALTER TABLE outlets ADD COLUMN IF NOT EXISTS property_id UUID REFERENCES properties(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_rooms_property ON rooms(property_id);
CREATE INDEX IF NOT EXISTS idx_guest_stays_property ON guest_stays(property_id);
CREATE INDEX IF NOT EXISTS idx_folios_property ON folios(property_id);
CREATE INDEX IF NOT EXISTS idx_work_orders_property ON work_orders(property_id);
CREATE INDEX IF NOT EXISTS idx_housekeeping_property ON housekeeping_assignments(property_id);
CREATE INDEX IF NOT EXISTS idx_outlets_property ON outlets(property_id);
