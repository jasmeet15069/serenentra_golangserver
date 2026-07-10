CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

-- SaaS tenant root. Existing single-hotel installs are attached to the demo
-- hotel so current credentials and screens keep working after migration.
CREATE TABLE IF NOT EXISTS hotels (
  id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
  name VARCHAR(200) NOT NULL,
  slug VARCHAR(100) UNIQUE NOT NULL,
  plan_tier VARCHAR(20) NOT NULL DEFAULT 'starter',
  is_active BOOLEAN NOT NULL DEFAULT TRUE,
  settings JSONB NOT NULL DEFAULT '{}',
  logo_url TEXT,
  primary_color VARCHAR(7) DEFAULT '#000000',
  address TEXT,
  country VARCHAR(100),
  timezone VARCHAR(100) DEFAULT 'UTC',
  currency VARCHAR(10) DEFAULT 'USD',
  phone TEXT,
  email TEXT,
  website TEXT,
  stripe_account_id VARCHAR(100),
  stripe_enabled BOOLEAN NOT NULL DEFAULT FALSE,
  razorpay_key_id VARCHAR(100),
  razorpay_key_secret_encrypted TEXT,
  razorpay_enabled BOOLEAN NOT NULL DEFAULT FALSE,
  active_payment_gateway VARCHAR(20) NOT NULL DEFAULT 'none',
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO hotels (
  id, name, slug, plan_tier, is_active, settings, primary_color,
  address, country, timezone, currency, phone, email, website
) VALUES (
  '00000000-0000-0000-0000-000000000001',
  'The Grand Demo Hotel',
  'grand-demo',
  'enterprise',
  TRUE,
  '{"max_rooms": null, "max_properties": null, "ai_addon": true}',
  '#000000',
  '123 Demo Street, Demo City',
  'United States',
  'UTC',
  'USD',
  NULL,
  NULL,
  NULL
) ON CONFLICT (id) DO NOTHING;

CREATE TABLE IF NOT EXISTS properties (
  id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
  hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
  name VARCHAR(200) NOT NULL,
  address TEXT,
  star_rating INT,
  total_rooms INT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO properties (id, hotel_id, name, address, star_rating, total_rooms)
VALUES (
  '00000000-0000-0000-0000-000000000002',
  '00000000-0000-0000-0000-000000000001',
  'Main Property',
  '123 Demo Street, Demo City',
  4,
  20
) ON CONFLICT (id) DO NOTHING;

CREATE TABLE IF NOT EXISTS room_types (
  id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
  hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
  property_id UUID REFERENCES properties(id) ON DELETE SET NULL,
  name VARCHAR(100) NOT NULL,
  description TEXT,
  base_price_per_night NUMERIC(10,2) NOT NULL DEFAULT 0,
  max_capacity INT NOT NULL DEFAULT 2,
  amenities JSONB NOT NULL DEFAULT '[]',
  is_active BOOLEAN NOT NULL DEFAULT TRUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO room_types (id, hotel_id, property_id, name, base_price_per_night, max_capacity)
VALUES
  ('00000000-0000-0000-0000-000000000010', '00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000002', 'Standard Room', 99.00, 2),
  ('00000000-0000-0000-0000-000000000011', '00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000002', 'Deluxe Room', 149.00, 2),
  ('00000000-0000-0000-0000-000000000012', '00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000002', 'Suite', 249.00, 4)
ON CONFLICT (id) DO NOTHING;

CREATE TABLE IF NOT EXISTS tax_configs (
  id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
  hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
  name VARCHAR(100) NOT NULL,
  rate NUMERIC(5,2) NOT NULL,
  applies_to VARCHAR(20) NOT NULL DEFAULT 'both',
  is_inclusive BOOLEAN NOT NULL DEFAULT FALSE,
  is_active BOOLEAN NOT NULL DEFAULT TRUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO tax_configs (hotel_id, name, rate, applies_to, is_inclusive)
SELECT '00000000-0000-0000-0000-000000000001', 'GST', 18.00, 'both', FALSE
WHERE NOT EXISTS (
  SELECT 1 FROM tax_configs
  WHERE hotel_id = '00000000-0000-0000-0000-000000000001' AND name = 'GST'
);

CREATE TABLE IF NOT EXISTS payment_configs (
  id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
  hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
  active_gateway VARCHAR(20) NOT NULL DEFAULT 'none',
  stripe_enabled BOOLEAN NOT NULL DEFAULT FALSE,
  stripe_account_id VARCHAR(100),
  razorpay_enabled BOOLEAN NOT NULL DEFAULT FALSE,
  razorpay_key_id VARCHAR(100),
  razorpay_key_secret_encrypted TEXT,
  cash_enabled BOOLEAN NOT NULL DEFAULT TRUE,
  card_enabled BOOLEAN NOT NULL DEFAULT TRUE,
  bank_transfer_enabled BOOLEAN NOT NULL DEFAULT FALSE,
  deposit_type VARCHAR(20) DEFAULT 'percentage',
  deposit_value NUMERIC(10,2) DEFAULT 0,
  cancellation_free_hours INT DEFAULT 24,
  cancellation_penalty_percent NUMERIC(5,2) DEFAULT 0,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (hotel_id)
);

INSERT INTO payment_configs (hotel_id)
VALUES ('00000000-0000-0000-0000-000000000001')
ON CONFLICT (hotel_id) DO NOTHING;

CREATE TABLE IF NOT EXISTS hotel_branding (
  hotel_id UUID PRIMARY KEY REFERENCES hotels(id) ON DELETE CASCADE,
  logo_url TEXT,
  primary_color VARCHAR(7) NOT NULL DEFAULT '#000000',
  welcome_message TEXT,
  footer_text TEXT,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO hotel_branding (hotel_id, primary_color, welcome_message, footer_text)
VALUES (
  '00000000-0000-0000-0000-000000000001',
  '#000000',
  'Welcome to The Grand Demo Hotel',
  'Powered by HotelOps'
) ON CONFLICT (hotel_id) DO NOTHING;

CREATE TABLE IF NOT EXISTS permissions (
  id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
  role VARCHAR(50) NOT NULL,
  resource VARCHAR(100) NOT NULL,
  action VARCHAR(50) NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (role, resource, action)
);

INSERT INTO permissions (role, resource, action)
VALUES
  ('platform_admin', '*', '*'),
  ('super_admin', '*', '*'),
  ('admin', '*', '*'),
  ('hotel_admin', '*', '*'),
  ('property_manager', 'bookings', 'read'),
  ('property_manager', 'bookings', 'write'),
  ('property_manager', 'rooms', 'read'),
  ('property_manager', 'rooms', 'write'),
  ('receptionist', 'dashboard', 'read'),
  ('receptionist', 'rooms', 'read'),
  ('receptionist', 'bookings', 'read'),
  ('receptionist', 'bookings', 'write'),
  ('receptionist', 'payments', 'read'),
  ('receptionist', 'complaints', 'read'),
  ('receptionist', 'complaints', 'write'),
  ('housekeeping', 'housekeeping', 'read'),
  ('housekeeping', 'housekeeping', 'write'),
  ('housekeeping', 'rooms', 'read'),
  ('maintenance', 'maintenance', 'read'),
  ('maintenance', 'maintenance', 'write'),
  ('food_manager', 'menu', 'read'),
  ('food_manager', 'menu', 'write'),
  ('food_manager', 'inventory', 'read'),
  ('food_manager', 'inventory', 'write'),
  ('kitchen_manager', 'orders', 'read'),
  ('kitchen_manager', 'orders', 'write'),
  ('kitchen_manager', 'inventory', 'read'),
  ('waiter', 'orders', 'read'),
  ('waiter', 'orders', 'write'),
  ('guest', 'guest_portal', 'read'),
  ('guest', 'guest_portal', 'write')
ON CONFLICT (role, resource, action) DO NOTHING;

CREATE TABLE IF NOT EXISTS staff_invitations (
  id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
  hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
  email VARCHAR(255) NOT NULL,
  role VARCHAR(50) NOT NULL,
  token VARCHAR(128) UNIQUE NOT NULL,
  invited_by UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at TIMESTAMPTZ NOT NULL DEFAULT NOW() + INTERVAL '48 hours',
  accepted_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS rate_plans (
  id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
  hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
  name VARCHAR(100) NOT NULL,
  description TEXT,
  discount_type VARCHAR(20),
  discount_value NUMERIC(10,2) DEFAULT 0,
  min_stay_nights INT NOT NULL DEFAULT 1,
  is_active BOOLEAN NOT NULL DEFAULT TRUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS folios (
  id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
  hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
  booking_id UUID NOT NULL REFERENCES guest_stays(id) ON DELETE CASCADE,
  guest_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  status VARCHAR(20) NOT NULL DEFAULT 'open',
  currency VARCHAR(10) NOT NULL DEFAULT 'USD',
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  closed_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS folio_charges (
  id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
  folio_id UUID NOT NULL REFERENCES folios(id) ON DELETE CASCADE,
  hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
  description TEXT NOT NULL,
  charge_type VARCHAR(50),
  amount NUMERIC(10,2) NOT NULL,
  tax_amount NUMERIC(10,2) NOT NULL DEFAULT 0,
  reference_id UUID,
  posted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  posted_by UUID REFERENCES users(id) ON DELETE SET NULL
);

CREATE TABLE IF NOT EXISTS housekeeping_assignments (
  id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
  hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
  room_id UUID NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
  assigned_to UUID REFERENCES users(id) ON DELETE SET NULL,
  task_type VARCHAR(30) NOT NULL DEFAULT 'checkout_clean',
  priority VARCHAR(10) NOT NULL DEFAULT 'normal',
  status VARCHAR(20) NOT NULL DEFAULT 'pending',
  notes TEXT,
  started_at TIMESTAMPTZ,
  completed_at TIMESTAMPTZ,
  inspected_by UUID REFERENCES users(id) ON DELETE SET NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS work_orders (
  id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
  hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
  room_id UUID REFERENCES rooms(id) ON DELETE SET NULL,
  reported_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  assigned_to UUID REFERENCES users(id) ON DELETE SET NULL,
  category VARCHAR(50),
  priority VARCHAR(10) NOT NULL DEFAULT 'normal',
  status VARCHAR(20) NOT NULL DEFAULT 'open',
  title VARCHAR(200) NOT NULL,
  description TEXT,
  resolution_notes TEXT,
  estimated_minutes INT,
  actual_minutes INT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  resolved_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS channel_manager_configs (
  hotel_id UUID PRIMARY KEY REFERENCES hotels(id) ON DELETE CASCADE,
  provider VARCHAR(50),
  api_key_encrypted TEXT,
  mapping JSONB NOT NULL DEFAULT '{}',
  last_sync_at TIMESTAMPTZ,
  is_active BOOLEAN NOT NULL DEFAULT FALSE
);

CREATE TABLE IF NOT EXISTS ai_concierge_logs (
  id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
  hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
  guest_id UUID REFERENCES users(id) ON DELETE SET NULL,
  role VARCHAR(10) NOT NULL,
  message TEXT NOT NULL,
  tokens_used INT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS ai_inventory_alerts (
  id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
  hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
  alert_summary TEXT NOT NULL,
  items_flagged JSONB NOT NULL DEFAULT '[]',
  generated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  acknowledged_at TIMESTAMPTZ,
  acknowledged_by UUID REFERENCES users(id) ON DELETE SET NULL
);

ALTER TABLE users ADD COLUMN IF NOT EXISTS hotel_id UUID REFERENCES hotels(id);
ALTER TABLE users ADD COLUMN IF NOT EXISTS platform_admin BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE profiles ADD COLUMN IF NOT EXISTS hotel_id UUID REFERENCES hotels(id);
ALTER TABLE user_roles ADD COLUMN IF NOT EXISTS hotel_id UUID REFERENCES hotels(id);
ALTER TABLE rooms ADD COLUMN IF NOT EXISTS hotel_id UUID REFERENCES hotels(id);
ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS hotel_id UUID REFERENCES hotels(id);
ALTER TABLE menu_categories ADD COLUMN IF NOT EXISTS hotel_id UUID REFERENCES hotels(id);
ALTER TABLE menu_items ADD COLUMN IF NOT EXISTS hotel_id UUID REFERENCES hotels(id);
ALTER TABLE menu_item_customizations ADD COLUMN IF NOT EXISTS hotel_id UUID REFERENCES hotels(id);
ALTER TABLE inventory_items ADD COLUMN IF NOT EXISTS hotel_id UUID REFERENCES hotels(id);
ALTER TABLE IF EXISTS recipes ADD COLUMN IF NOT EXISTS hotel_id UUID REFERENCES hotels(id);
ALTER TABLE orders ADD COLUMN IF NOT EXISTS hotel_id UUID REFERENCES hotels(id);
ALTER TABLE order_items ADD COLUMN IF NOT EXISTS hotel_id UUID REFERENCES hotels(id);
ALTER TABLE payments ADD COLUMN IF NOT EXISTS hotel_id UUID REFERENCES hotels(id);
ALTER TABLE complaints ADD COLUMN IF NOT EXISTS hotel_id UUID REFERENCES hotels(id);
ALTER TABLE payment_settings ADD COLUMN IF NOT EXISTS hotel_id UUID REFERENCES hotels(id);
ALTER TABLE staff_shifts ADD COLUMN IF NOT EXISTS hotel_id UUID REFERENCES hotels(id);
ALTER TABLE guest_preferences ADD COLUMN IF NOT EXISTS hotel_id UUID REFERENCES hotels(id);

UPDATE users SET hotel_id = '00000000-0000-0000-0000-000000000001' WHERE hotel_id IS NULL AND COALESCE(platform_admin, FALSE) = FALSE;
UPDATE profiles SET hotel_id = u.hotel_id FROM users u WHERE profiles.user_id = u.id AND profiles.hotel_id IS NULL;
UPDATE user_roles SET hotel_id = u.hotel_id FROM users u WHERE user_roles.user_id = u.id AND user_roles.hotel_id IS NULL;
UPDATE rooms SET hotel_id = '00000000-0000-0000-0000-000000000001' WHERE hotel_id IS NULL;
UPDATE guest_stays SET hotel_id = '00000000-0000-0000-0000-000000000001' WHERE hotel_id IS NULL;
UPDATE menu_categories SET hotel_id = '00000000-0000-0000-0000-000000000001' WHERE hotel_id IS NULL;
UPDATE menu_items SET hotel_id = '00000000-0000-0000-0000-000000000001' WHERE hotel_id IS NULL;
UPDATE menu_item_customizations mic SET hotel_id = mi.hotel_id FROM menu_items mi WHERE mic.menu_item_id = mi.id AND mic.hotel_id IS NULL;
UPDATE inventory_items SET hotel_id = '00000000-0000-0000-0000-000000000001' WHERE hotel_id IS NULL;
UPDATE orders SET hotel_id = '00000000-0000-0000-0000-000000000001' WHERE hotel_id IS NULL;
UPDATE order_items oi SET hotel_id = o.hotel_id FROM orders o WHERE oi.order_id = o.id AND oi.hotel_id IS NULL;
UPDATE payments SET hotel_id = '00000000-0000-0000-0000-000000000001' WHERE hotel_id IS NULL;
UPDATE complaints SET hotel_id = '00000000-0000-0000-0000-000000000001' WHERE hotel_id IS NULL;
UPDATE payment_settings SET hotel_id = '00000000-0000-0000-0000-000000000001' WHERE hotel_id IS NULL;
UPDATE staff_shifts ss SET hotel_id = u.hotel_id FROM users u WHERE ss.user_id = u.id AND ss.hotel_id IS NULL;
UPDATE guest_preferences gp SET hotel_id = u.hotel_id FROM users u WHERE gp.user_id = u.id AND gp.hotel_id IS NULL;

ALTER TABLE rooms ALTER COLUMN hotel_id SET NOT NULL;
ALTER TABLE guest_stays ALTER COLUMN hotel_id SET NOT NULL;
ALTER TABLE menu_categories ALTER COLUMN hotel_id SET NOT NULL;
ALTER TABLE menu_items ALTER COLUMN hotel_id SET NOT NULL;
ALTER TABLE menu_item_customizations ALTER COLUMN hotel_id SET NOT NULL;
ALTER TABLE inventory_items ALTER COLUMN hotel_id SET NOT NULL;
ALTER TABLE orders ALTER COLUMN hotel_id SET NOT NULL;
ALTER TABLE order_items ALTER COLUMN hotel_id SET NOT NULL;
ALTER TABLE payments ALTER COLUMN hotel_id SET NOT NULL;
ALTER TABLE complaints ALTER COLUMN hotel_id SET NOT NULL;
ALTER TABLE payment_settings ALTER COLUMN hotel_id SET NOT NULL;
ALTER TABLE staff_shifts ALTER COLUMN hotel_id SET NOT NULL;
ALTER TABLE guest_preferences ALTER COLUMN hotel_id SET NOT NULL;

ALTER TABLE audit_logs ADD COLUMN IF NOT EXISTS hotel_id UUID REFERENCES hotels(id);
ALTER TABLE audit_logs ADD COLUMN IF NOT EXISTS resource_type VARCHAR(100);
ALTER TABLE audit_logs ADD COLUMN IF NOT EXISTS resource_id UUID;
ALTER TABLE audit_logs ADD COLUMN IF NOT EXISTS ip_address INET;
ALTER TABLE audit_logs ADD COLUMN IF NOT EXISTS user_agent TEXT;
ALTER TABLE audit_logs ADD COLUMN IF NOT EXISTS ai_triggered BOOLEAN NOT NULL DEFAULT FALSE;
UPDATE audit_logs SET hotel_id = '00000000-0000-0000-0000-000000000001' WHERE hotel_id IS NULL;
UPDATE audit_logs SET resource_type = COALESCE(table_name, 'unknown') WHERE resource_type IS NULL;
UPDATE audit_logs SET resource_id = record_id WHERE resource_id IS NULL AND record_id IS NOT NULL;
ALTER TABLE audit_logs ALTER COLUMN hotel_id SET NOT NULL;
ALTER TABLE audit_logs ALTER COLUMN resource_type SET NOT NULL;

CREATE INDEX IF NOT EXISTS idx_users_hotel_id ON users(hotel_id);
CREATE INDEX IF NOT EXISTS idx_user_roles_hotel_id ON user_roles(hotel_id);
CREATE INDEX IF NOT EXISTS idx_rooms_hotel_id ON rooms(hotel_id);
CREATE INDEX IF NOT EXISTS idx_guest_stays_hotel_id ON guest_stays(hotel_id);
CREATE INDEX IF NOT EXISTS idx_menu_categories_hotel_id ON menu_categories(hotel_id);
CREATE INDEX IF NOT EXISTS idx_menu_items_hotel_id ON menu_items(hotel_id);
CREATE INDEX IF NOT EXISTS idx_inventory_items_hotel_id ON inventory_items(hotel_id);
CREATE INDEX IF NOT EXISTS idx_orders_hotel_id ON orders(hotel_id);
CREATE INDEX IF NOT EXISTS idx_order_items_hotel_id ON order_items(hotel_id);
CREATE INDEX IF NOT EXISTS idx_payments_hotel_id ON payments(hotel_id);
CREATE INDEX IF NOT EXISTS idx_complaints_hotel_id ON complaints(hotel_id);
CREATE INDEX IF NOT EXISTS idx_folios_hotel_id ON folios(hotel_id);
CREATE INDEX IF NOT EXISTS idx_folio_charges_folio_id ON folio_charges(folio_id);
CREATE INDEX IF NOT EXISTS idx_housekeeping_hotel_id ON housekeeping_assignments(hotel_id);
CREATE INDEX IF NOT EXISTS idx_work_orders_hotel_id ON work_orders(hotel_id);
CREATE INDEX IF NOT EXISTS idx_audit_logs_hotel_id ON audit_logs(hotel_id);
CREATE INDEX IF NOT EXISTS idx_staff_invitations_token ON staff_invitations(token);
