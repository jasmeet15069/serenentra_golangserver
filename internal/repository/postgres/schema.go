package postgres

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// EnsureAppSchema creates compatibility tables that older local databases may
// be missing. It is intentionally additive and safe to run on every boot.
func (d *DB) EnsureAppSchema(ctx context.Context) error {
	if err := d.runSQLMigrations(ctx); err != nil {
		return err
	}

	statements := []string{
		`CREATE TABLE IF NOT EXISTS order_items (
			id UUID PRIMARY KEY,
			order_id UUID NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
			menu_item_id UUID NOT NULL REFERENCES menu_items(id) ON DELETE RESTRICT,
			quantity INTEGER NOT NULL DEFAULT 1,
			unit_price NUMERIC(12,2) NOT NULL DEFAULT 0,
			notes TEXT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS audit_logs (
			id UUID PRIMARY KEY,
			user_id UUID REFERENCES users(id) ON DELETE SET NULL,
			action TEXT NOT NULL,
			table_name TEXT NOT NULL,
			record_id UUID,
			old_data JSONB,
			new_data JSONB,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_order_items_order_id ON order_items(order_id)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_logs_created ON audit_logs(created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS lost_items (
			id UUID PRIMARY KEY,
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			room_id UUID REFERENCES rooms(id) ON DELETE SET NULL,
			guest_name TEXT,
			item_name TEXT NOT NULL,
			description TEXT,
			found_by UUID REFERENCES users(id) ON DELETE SET NULL,
			found_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			status VARCHAR(20) NOT NULL DEFAULT 'lost',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS linen_inventory (
			id UUID PRIMARY KEY,
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			item_name TEXT NOT NULL,
			total_count INT NOT NULL DEFAULT 0,
			in_use INT NOT NULL DEFAULT 0,
			in_laundry INT NOT NULL DEFAULT 0,
			damaged INT NOT NULL DEFAULT 0,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS linen_transactions (
			id UUID PRIMARY KEY,
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			linen_id UUID NOT NULL REFERENCES linen_inventory(id) ON DELETE CASCADE,
			transaction_type VARCHAR(10) NOT NULL,
			quantity INT NOT NULL,
			damaged INT NOT NULL DEFAULT 0,
			issued_to TEXT,
			notes TEXT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`INSERT INTO payment_settings (id, hotel_id, gateway_name, webhook_url, is_active, created_at, updated_at)
		 VALUES
		   (uuid_generate_v4(), '00000000-0000-0000-0000-000000000001', 'cash', NULL, true, now(), now()),
		   (uuid_generate_v4(), '00000000-0000-0000-0000-000000000001', 'card', NULL, true, now(), now()),
		   (uuid_generate_v4(), '00000000-0000-0000-0000-000000000001', 'stripe', NULL, true, now(), now())
		 ON CONFLICT (hotel_id, gateway_name) DO NOTHING`,
		`CREATE TABLE IF NOT EXISTS guests (
			id UUID PRIMARY KEY,
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			user_id UUID REFERENCES users(id) ON DELETE SET NULL,
			full_name TEXT NOT NULL,
			email TEXT,
			phone TEXT,
			address TEXT,
			city TEXT,
			country TEXT,
			id_type TEXT,
			id_number TEXT,
			vip_status TEXT DEFAULT 'standard',
			notes TEXT,
			preferences JSONB DEFAULT '{}',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS loyalty_tiers (
			id UUID PRIMARY KEY,
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			min_points INT NOT NULL DEFAULT 0,
			multiplier NUMERIC(5,2) NOT NULL DEFAULT 1.0,
			benefits JSONB DEFAULT '{}',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS loyalty_members (
			id UUID PRIMARY KEY,
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			guest_id UUID NOT NULL REFERENCES guests(id) ON DELETE CASCADE,
			tier_id UUID REFERENCES loyalty_tiers(id) ON DELETE SET NULL,
			points INT NOT NULL DEFAULT 0,
			lifetime_points INT NOT NULL DEFAULT 0,
			enrolled_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE(guest_id, hotel_id)
		)`,
		`CREATE TABLE IF NOT EXISTS loyalty_transactions (
			id UUID PRIMARY KEY,
			member_id UUID NOT NULL REFERENCES loyalty_members(id) ON DELETE CASCADE,
			type TEXT NOT NULL,
			points INT NOT NULL,
			reference TEXT,
			description TEXT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS pricing_rules (
			id UUID PRIMARY KEY,
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			rule_type TEXT NOT NULL,
			conditions JSONB DEFAULT '{}',
			adjustment NUMERIC(5,2) NOT NULL DEFAULT 0,
			priority INT NOT NULL DEFAULT 0,
			active BOOLEAN NOT NULL DEFAULT true,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS vendors (
			id UUID PRIMARY KEY,
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			contact_person TEXT,
			email TEXT,
			phone TEXT,
			address TEXT,
			category TEXT,
			rating NUMERIC(3,1),
			active BOOLEAN NOT NULL DEFAULT true,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS purchase_orders (
			id UUID PRIMARY KEY,
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			vendor_id UUID REFERENCES vendors(id) ON DELETE SET NULL,
			po_number TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			items JSONB DEFAULT '[]',
			total NUMERIC(12,2) NOT NULL DEFAULT 0,
			notes TEXT,
			issued_at TIMESTAMPTZ,
			received_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS channel_connections (
			id UUID PRIMARY KEY,
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			channel_name TEXT NOT NULL,
			channel_type TEXT NOT NULL,
			api_key TEXT,
			settings JSONB DEFAULT '{}',
			connected BOOLEAN NOT NULL DEFAULT false,
			last_sync_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS night_audit_reports (
			id UUID PRIMARY KEY,
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			audit_date DATE NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			expected_revenue NUMERIC(12,2),
			actual_revenue NUMERIC(12,2),
			total_tax NUMERIC(12,2),
			occupancy_rate NUMERIC(5,2),
			notes TEXT,
			closed_by UUID REFERENCES users(id) ON DELETE SET NULL,
			closed_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS promotions (
			id UUID PRIMARY KEY,
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			code TEXT NOT NULL,
			name TEXT NOT NULL,
			description TEXT,
			discount_type TEXT NOT NULL,
			discount_value NUMERIC(12,2) NOT NULL DEFAULT 0,
			min_nights INT DEFAULT 0,
			min_amount NUMERIC(12,2) DEFAULT 0,
			max_discount NUMERIC(12,2),
			usage_limit INT DEFAULT 0,
			used_count INT NOT NULL DEFAULT 0,
			valid_from DATE NOT NULL,
			valid_to DATE NOT NULL,
			active BOOLEAN NOT NULL DEFAULT true,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS assets (
			id UUID PRIMARY KEY,
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			category TEXT,
			location TEXT,
			serial_number TEXT,
			purchase_date DATE,
			purchase_cost NUMERIC(12,2),
			warranty_until DATE,
			status TEXT NOT NULL DEFAULT 'active',
			notes TEXT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS revenue_daily (
			id UUID PRIMARY KEY,
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			date DATE NOT NULL,
			total NUMERIC(12,2) NOT NULL DEFAULT 0,
			revpar NUMERIC(12,2) NOT NULL DEFAULT 0,
			adr NUMERIC(12,2) NOT NULL DEFAULT 0,
			occupancy_pct NUMERIC(5,2) NOT NULL DEFAULT 0,
			goppar NUMERIC(12,2) NOT NULL DEFAULT 0,
			UNIQUE(hotel_id, date)
		)`,
		`CREATE TABLE IF NOT EXISTS revenue_forecast (
			id UUID PRIMARY KEY,
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			date DATE NOT NULL,
			occupancy_pct NUMERIC(5,2) NOT NULL DEFAULT 0,
			revenue NUMERIC(12,2) NOT NULL DEFAULT 0,
			UNIQUE(hotel_id, date)
		)`,
		`CREATE TABLE IF NOT EXISTS bookings (
			id UUID PRIMARY KEY,
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			guest_id UUID REFERENCES guests(id) ON DELETE SET NULL,
			room_id UUID REFERENCES rooms(id) ON DELETE SET NULL,
			status TEXT NOT NULL DEFAULT 'confirmed',
			check_in DATE,
			check_out DATE,
			total NUMERIC(12,2) NOT NULL DEFAULT 0,
			tax_amount NUMERIC(12,2) NOT NULL DEFAULT 0,
			channel_name TEXT DEFAULT 'direct',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		// Booking source / OTA channel for reservations (Direct, Booking.com, Expedia, …).
		`ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'Direct'`,
		// Point-of-sale orders (restaurant/bar/room-service). Line items are JSONB so
		// the cart shape round-trips without a menu_items foreign key.
		`CREATE TABLE IF NOT EXISTS pos_orders (
			id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			order_number TEXT NOT NULL,
			outlet TEXT NOT NULL,
			channel TEXT,
			table_label TEXT,
			room_id TEXT,
			customer_name TEXT,
			delivery_address TEXT,
			status TEXT NOT NULL DEFAULT 'Open',
			total NUMERIC(12,2) NOT NULL DEFAULT 0,
			subtotal NUMERIC(12,2) NOT NULL DEFAULT 0,
			discount NUMERIC(12,2) NOT NULL DEFAULT 0,
			service_charge NUMERIC(12,2) NOT NULL DEFAULT 0,
			tax_rate NUMERIC(5,2) NOT NULL DEFAULT 0,
			tax_mode TEXT NOT NULL DEFAULT 'gst',
			tax NUMERIC(12,2) NOT NULL DEFAULT 0,
			items JSONB NOT NULL DEFAULT '[]',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_pos_orders_hotel_status ON pos_orders(hotel_id, status)`,
		// Bill breakdown for accurate invoicing — additive for databases that
		// already have pos_orders from before these columns existed.
		`ALTER TABLE pos_orders ADD COLUMN IF NOT EXISTS subtotal NUMERIC(12,2) NOT NULL DEFAULT 0`,
		`ALTER TABLE pos_orders ADD COLUMN IF NOT EXISTS discount NUMERIC(12,2) NOT NULL DEFAULT 0`,
		`ALTER TABLE pos_orders ADD COLUMN IF NOT EXISTS service_charge NUMERIC(12,2) NOT NULL DEFAULT 0`,
		`ALTER TABLE pos_orders ADD COLUMN IF NOT EXISTS tax_rate NUMERIC(5,2) NOT NULL DEFAULT 0`,
		`ALTER TABLE pos_orders ADD COLUMN IF NOT EXISTS tax_mode TEXT NOT NULL DEFAULT 'gst'`,
		`ALTER TABLE pos_orders ADD COLUMN IF NOT EXISTS tax NUMERIC(12,2) NOT NULL DEFAULT 0`,
		// Dedicated GST / tax-invoice identity fields on hotels (see migration 010).
		// Mirrored here so the columns exist on the always-run schema path even when
		// the migrations directory is not present at runtime (e.g. some containers).
		`ALTER TABLE hotels ADD COLUMN IF NOT EXISTS legal_entity_name TEXT`,
		`ALTER TABLE hotels ADD COLUMN IF NOT EXISTS restaurant_name TEXT`,
		`ALTER TABLE hotels ADD COLUMN IF NOT EXISTS restaurant_address TEXT`,
		`ALTER TABLE hotels ADD COLUMN IF NOT EXISTS gstin TEXT`,
		`ALTER TABLE hotels ADD COLUMN IF NOT EXISTS fssai TEXT`,
		`ALTER TABLE hotels ADD COLUMN IF NOT EXISTS gst_state TEXT`,
		`ALTER TABLE hotels ADD COLUMN IF NOT EXISTS place_of_supply TEXT`,
		`ALTER TABLE hotels ADD COLUMN IF NOT EXISTS hsn_code TEXT NOT NULL DEFAULT '996331'`,
		`ALTER TABLE hotels ADD COLUMN IF NOT EXISTS gst_rate NUMERIC(5,2) NOT NULL DEFAULT 0`,
		// POS outlets (see migration 011): a restaurant/bar/cafe under a hotel that
		// can also serve walk-ins and run standalone. Created before the dine-in
		// tables so their outlet_id columns can reference it.
		`CREATE TABLE IF NOT EXISTS outlets (
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
		)`,
		`CREATE INDEX IF NOT EXISTS idx_outlets_hotel ON outlets(hotel_id)`,
		// --- Restaurant POS: Dine-In workflow ---
		// tables -> dining_sessions -> kots -> kot_items, consolidated into bills
		// settled by bill_payments. The legacy flat pos_orders table is untouched
		// so room-service / quick-bill flows keep working unchanged.
		`CREATE TABLE IF NOT EXISTS restaurant_tables (
			id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			table_number TEXT NOT NULL,
			section TEXT,
			seats INT NOT NULL DEFAULT 2,
			status TEXT NOT NULL DEFAULT 'available',
			pos_x INT,
			pos_y INT,
			is_active BOOLEAN NOT NULL DEFAULT true,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE (hotel_id, table_number)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_restaurant_tables_hotel_status ON restaurant_tables(hotel_id, status)`,
		`CREATE TABLE IF NOT EXISTS dining_sessions (
			id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			session_number TEXT NOT NULL,
			table_id UUID NOT NULL REFERENCES restaurant_tables(id) ON DELETE CASCADE,
			covers INT NOT NULL DEFAULT 1,
			status TEXT NOT NULL DEFAULT 'open',
			guest_name TEXT,
			opened_by UUID,
			closed_by UUID,
			opened_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			billed_at TIMESTAMPTZ,
			closed_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE (hotel_id, session_number)
		)`,
		// At most one active seating per physical table.
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_active_session_per_table ON dining_sessions(table_id) WHERE status IN ('open','billed')`,
		`CREATE INDEX IF NOT EXISTS idx_dining_sessions_hotel_status ON dining_sessions(hotel_id, status)`,
		`CREATE TABLE IF NOT EXISTS kots (
			id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			dining_session_id UUID NOT NULL REFERENCES dining_sessions(id) ON DELETE CASCADE,
			kot_number TEXT NOT NULL,
			round_no INT NOT NULL DEFAULT 1,
			status TEXT NOT NULL DEFAULT 'draft',
			station TEXT,
			notes TEXT,
			created_by UUID,
			sent_at TIMESTAMPTZ,
			ready_at TIMESTAMPTZ,
			served_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE (hotel_id, kot_number)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_kots_session ON kots(dining_session_id)`,
		`CREATE INDEX IF NOT EXISTS idx_kots_hotel_status ON kots(hotel_id, status)`,
		`CREATE TABLE IF NOT EXISTS kot_items (
			id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			kot_id UUID NOT NULL REFERENCES kots(id) ON DELETE CASCADE,
			menu_item_id UUID,
			item_name TEXT NOT NULL,
			quantity INT NOT NULL DEFAULT 1,
			unit_price NUMERIC(12,2) NOT NULL DEFAULT 0,
			modifiers JSONB NOT NULL DEFAULT '[]',
			line_total NUMERIC(12,2) NOT NULL DEFAULT 0,
			notes TEXT,
			status TEXT NOT NULL DEFAULT 'queued',
			void_reason TEXT,
			voided_by UUID,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_kot_items_kot ON kot_items(kot_id)`,
		`CREATE TABLE IF NOT EXISTS bills (
			id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			dining_session_id UUID NOT NULL REFERENCES dining_sessions(id) ON DELETE CASCADE,
			bill_number TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'open',
			subtotal NUMERIC(12,2) NOT NULL DEFAULT 0,
			discount_type TEXT,
			discount_value NUMERIC(12,2) NOT NULL DEFAULT 0,
			discount_amount NUMERIC(12,2) NOT NULL DEFAULT 0,
			tax_rate NUMERIC(5,2) NOT NULL DEFAULT 0,
			tax_amount NUMERIC(12,2) NOT NULL DEFAULT 0,
			tip_type TEXT,
			tip_value NUMERIC(12,2) NOT NULL DEFAULT 0,
			tip_amount NUMERIC(12,2) NOT NULL DEFAULT 0,
			rounding_adjust NUMERIC(12,2) NOT NULL DEFAULT 0,
			total_amount NUMERIC(12,2) NOT NULL DEFAULT 0,
			amount_paid NUMERIC(12,2) NOT NULL DEFAULT 0,
			amount_due NUMERIC(12,2) NOT NULL DEFAULT 0,
			currency TEXT NOT NULL DEFAULT 'INR',
			generated_by UUID,
			finalized_at TIMESTAMPTZ,
			paid_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE (hotel_id, bill_number)
		)`,
		// At most one non-void bill per session (guards double billing).
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_one_bill_per_session ON bills(dining_session_id) WHERE status <> 'void'`,
		`CREATE TABLE IF NOT EXISTS bill_payments (
			id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			bill_id UUID NOT NULL REFERENCES bills(id) ON DELETE CASCADE,
			payment_number TEXT NOT NULL,
			method TEXT NOT NULL DEFAULT 'cash',
			amount NUMERIC(12,2) NOT NULL DEFAULT 0,
			tendered NUMERIC(12,2),
			change_due NUMERIC(12,2) NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'completed',
			txn_reference TEXT,
			gateway TEXT,
			received_by UUID,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_bill_payments_bill ON bill_payments(bill_id)`,
		// Scope dine-in entities to an outlet (migration 011); nullable for back-compat.
		`ALTER TABLE restaurant_tables ADD COLUMN IF NOT EXISTS outlet_id UUID REFERENCES outlets(id) ON DELETE SET NULL`,
		`ALTER TABLE dining_sessions ADD COLUMN IF NOT EXISTS outlet_id UUID REFERENCES outlets(id) ON DELETE SET NULL`,
		`ALTER TABLE dining_sessions ADD COLUMN IF NOT EXISTS customer_type TEXT NOT NULL DEFAULT 'walk_in'`,
		`ALTER TABLE dining_sessions ADD COLUMN IF NOT EXISTS guest_stay_id UUID`,
		`ALTER TABLE bills ADD COLUMN IF NOT EXISTS outlet_id UUID REFERENCES outlets(id) ON DELETE SET NULL`,
		`ALTER TABLE bills ADD COLUMN IF NOT EXISTS charge_mode TEXT NOT NULL DEFAULT 'pay_on_spot'`,
		`CREATE TABLE IF NOT EXISTS competitor_rates (
			id UUID PRIMARY KEY,
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			competitor_name TEXT NOT NULL,
			room_type TEXT NOT NULL,
			our_rate NUMERIC(12,2) NOT NULL DEFAULT 0,
			their_rate NUMERIC(12,2) NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS maintenance_schedule (
			id UUID PRIMARY KEY,
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			asset_id UUID REFERENCES assets(id) ON DELETE CASCADE,
			task_name TEXT NOT NULL,
			frequency TEXT NOT NULL,
			last_done DATE,
			next_due DATE NOT NULL,
			assigned_to TEXT,
			notes TEXT,
			completed BOOLEAN NOT NULL DEFAULT false,
			completed_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		// Branches (properties) — see migration 012. Mirrored here so the columns
		// exist on the always-run schema path even when the migrations directory is
		// absent at runtime. CLIENT (hotel) → BRANCH (property); property_id is a
		// nullable branch FK on core operational tables (NULL = client-level scope).
		`ALTER TABLE properties ADD COLUMN IF NOT EXISTS code TEXT`,
		`ALTER TABLE properties ADD COLUMN IF NOT EXISTS phone TEXT`,
		`ALTER TABLE properties ADD COLUMN IF NOT EXISTS email TEXT`,
		`ALTER TABLE properties ADD COLUMN IF NOT EXISTS timezone TEXT NOT NULL DEFAULT 'UTC'`,
		`ALTER TABLE properties ADD COLUMN IF NOT EXISTS currency TEXT NOT NULL DEFAULT 'USD'`,
		`ALTER TABLE properties ADD COLUMN IF NOT EXISTS is_active BOOLEAN NOT NULL DEFAULT true`,
		`ALTER TABLE properties ADD COLUMN IF NOT EXISTS is_primary BOOLEAN NOT NULL DEFAULT false`,
		`ALTER TABLE properties ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT now()`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_properties_hotel_code ON properties(hotel_id, code) WHERE code IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_properties_hotel ON properties(hotel_id)`,
		`ALTER TABLE rooms ADD COLUMN IF NOT EXISTS property_id UUID REFERENCES properties(id) ON DELETE SET NULL`,
		`ALTER TABLE guest_stays ADD COLUMN IF NOT EXISTS property_id UUID REFERENCES properties(id) ON DELETE SET NULL`,
		`ALTER TABLE pos_orders ADD COLUMN IF NOT EXISTS property_id UUID REFERENCES properties(id) ON DELETE SET NULL`,
		`ALTER TABLE folios ADD COLUMN IF NOT EXISTS property_id UUID REFERENCES properties(id) ON DELETE SET NULL`,
		`ALTER TABLE work_orders ADD COLUMN IF NOT EXISTS property_id UUID REFERENCES properties(id) ON DELETE SET NULL`,
		`ALTER TABLE housekeeping_assignments ADD COLUMN IF NOT EXISTS property_id UUID REFERENCES properties(id) ON DELETE SET NULL`,
		`ALTER TABLE outlets ADD COLUMN IF NOT EXISTS property_id UUID REFERENCES properties(id) ON DELETE SET NULL`,
		`CREATE INDEX IF NOT EXISTS idx_rooms_property ON rooms(property_id)`,
		`CREATE INDEX IF NOT EXISTS idx_guest_stays_property ON guest_stays(property_id)`,
		`CREATE INDEX IF NOT EXISTS idx_pos_orders_property ON pos_orders(property_id)`,
		`CREATE INDEX IF NOT EXISTS idx_folios_property ON folios(property_id)`,
		`CREATE INDEX IF NOT EXISTS idx_work_orders_property ON work_orders(property_id)`,
		`CREATE INDEX IF NOT EXISTS idx_housekeeping_property ON housekeeping_assignments(property_id)`,
		`CREATE INDEX IF NOT EXISTS idx_outlets_property ON outlets(property_id)`,
		// Accounting module tables (Chart of Accounts, Sales/Purchase, GRN, Journals).
		`CREATE TABLE IF NOT EXISTS accounting_accounts (
			id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			code TEXT NOT NULL,
			name TEXT NOT NULL,
			type TEXT NOT NULL,
			sub_type TEXT,
			parent_code TEXT,
			opening_balance NUMERIC(14,2) NOT NULL DEFAULT 0,
			currency TEXT NOT NULL DEFAULT $$USD$$,
			is_active BOOLEAN NOT NULL DEFAULT true,
			display_order INT NOT NULL DEFAULT 0,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE(hotel_id, code)
		)`,
		`CREATE TABLE IF NOT EXISTS accounting_customers (
			id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			code TEXT NOT NULL,
			name TEXT NOT NULL,
			gstin TEXT,
			address TEXT,
			email TEXT,
			phone TEXT,
			credit_days INT NOT NULL DEFAULT 30,
			credit_limit NUMERIC(14,2),
			is_active BOOLEAN NOT NULL DEFAULT true,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE(hotel_id, code)
		)`,
		`CREATE TABLE IF NOT EXISTS accounting_vendors (
			id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			code TEXT NOT NULL,
			name TEXT NOT NULL,
			gstin TEXT,
			address TEXT,
			email TEXT,
			phone TEXT,
			credit_days INT NOT NULL DEFAULT 30,
			credit_limit NUMERIC(14,2),
			is_active BOOLEAN NOT NULL DEFAULT true,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE(hotel_id, code)
		)`,
		`CREATE TABLE IF NOT EXISTS accounting_sales_invoices (
			id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			customer_id UUID REFERENCES accounting_customers(id),
			invoice_number TEXT NOT NULL,
			invoice_date DATE NOT NULL DEFAULT CURRENT_DATE,
			due_date DATE,
			reference TEXT,
			subtotal NUMERIC(14,2) NOT NULL DEFAULT 0,
			discount_total NUMERIC(14,2) NOT NULL DEFAULT 0,
			tax_total NUMERIC(14,2) NOT NULL DEFAULT 0,
			total NUMERIC(14,2) NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT $$draft$$,
			notes TEXT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE(hotel_id, invoice_number)
		)`,
		`CREATE TABLE IF NOT EXISTS accounting_sales_invoice_lines (
			id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
			invoice_id UUID NOT NULL REFERENCES accounting_sales_invoices(id) ON DELETE CASCADE,
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			account_id UUID REFERENCES accounting_accounts(id),
			description TEXT NOT NULL,
			quantity NUMERIC(12,2) NOT NULL DEFAULT 1,
			unit_price NUMERIC(14,2) NOT NULL DEFAULT 0,
			discount NUMERIC(14,2) NOT NULL DEFAULT 0,
			tax_rate NUMERIC(5,2) NOT NULL DEFAULT 0,
			tax_amount NUMERIC(14,2) NOT NULL DEFAULT 0,
			total NUMERIC(14,2) NOT NULL DEFAULT 0,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS accounting_credit_notes (
			id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			invoice_id UUID REFERENCES accounting_sales_invoices(id),
			credit_note_number TEXT NOT NULL,
			date DATE NOT NULL DEFAULT CURRENT_DATE,
			reason TEXT,
			subtotal NUMERIC(14,2) NOT NULL DEFAULT 0,
			tax_total NUMERIC(14,2) NOT NULL DEFAULT 0,
			total NUMERIC(14,2) NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT $$draft$$,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE(hotel_id, credit_note_number)
		)`,
		`CREATE TABLE IF NOT EXISTS accounting_credit_note_lines (
			id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
			credit_note_id UUID NOT NULL REFERENCES accounting_credit_notes(id) ON DELETE CASCADE,
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			account_id UUID REFERENCES accounting_accounts(id),
			invoice_line_id UUID REFERENCES accounting_sales_invoice_lines(id),
			description TEXT NOT NULL,
			quantity NUMERIC(12,2) NOT NULL DEFAULT 1,
			unit_price NUMERIC(14,2) NOT NULL DEFAULT 0,
			tax_amount NUMERIC(14,2) NOT NULL DEFAULT 0,
			total NUMERIC(14,2) NOT NULL DEFAULT 0,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS accounting_debit_notes (
			id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			vendor_id UUID REFERENCES accounting_vendors(id),
			debit_note_number TEXT NOT NULL,
			date DATE NOT NULL DEFAULT CURRENT_DATE,
			reason TEXT,
			subtotal NUMERIC(14,2) NOT NULL DEFAULT 0,
			tax_total NUMERIC(14,2) NOT NULL DEFAULT 0,
			total NUMERIC(14,2) NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT $$draft$$,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE(hotel_id, debit_note_number)
		)`,
		`CREATE TABLE IF NOT EXISTS accounting_purchase_orders (
			id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			vendor_id UUID REFERENCES accounting_vendors(id),
			po_number TEXT NOT NULL,
			order_date DATE NOT NULL DEFAULT CURRENT_DATE,
			expected_date DATE,
			status TEXT NOT NULL DEFAULT $$draft$$,
			subtotal NUMERIC(14,2) NOT NULL DEFAULT 0,
			tax_total NUMERIC(14,2) NOT NULL DEFAULT 0,
			total NUMERIC(14,2) NOT NULL DEFAULT 0,
			notes TEXT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE(hotel_id, po_number)
		)`,
		`CREATE TABLE IF NOT EXISTS accounting_grn (
			id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			po_id UUID REFERENCES accounting_purchase_orders(id),
			grn_number TEXT NOT NULL,
			received_date DATE NOT NULL DEFAULT CURRENT_DATE,
			vendor_invoice_ref TEXT,
			status TEXT NOT NULL DEFAULT $$draft$$,
			notes TEXT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE(hotel_id, grn_number)
		)`,
		`CREATE TABLE IF NOT EXISTS accounting_grn_lines (
			id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
			grn_id UUID NOT NULL REFERENCES accounting_grn(id) ON DELETE CASCADE,
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			item_description TEXT NOT NULL,
			quantity_ordered NUMERIC(12,2) NOT NULL DEFAULT 0,
			quantity_received NUMERIC(12,2) NOT NULL DEFAULT 0,
			quantity_accepted NUMERIC(12,2) NOT NULL DEFAULT 0,
			quantity_rejected NUMERIC(12,2) NOT NULL DEFAULT 0,
			unit_price NUMERIC(14,2) NOT NULL DEFAULT 0,
			total NUMERIC(14,2) NOT NULL DEFAULT 0,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS accounting_journal_entries (
			id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			entry_date DATE NOT NULL DEFAULT CURRENT_DATE,
			description TEXT NOT NULL,
			reference TEXT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS accounting_journal_lines (
			id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
			entry_id UUID NOT NULL REFERENCES accounting_journal_entries(id) ON DELETE CASCADE,
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			account_id UUID REFERENCES accounting_accounts(id),
			debit NUMERIC(14,2) NOT NULL DEFAULT 0,
			credit NUMERIC(14,2) NOT NULL DEFAULT 0,
			memo TEXT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
	}
	for _, statement := range statements {
		if _, err := d.Pool.Exec(ctx, statement); err != nil {
			return fmt.Errorf("ensure schema: %w", err)
		}
	}
	return nil
}

func (d *DB) runSQLMigrations(ctx context.Context) error {
	dir, err := findMigrationsDir()
	if err != nil {
		d.logger.Warn("schema: migrations directory not found")
		return nil
	}

	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return fmt.Errorf("schema: list migrations: %w", err)
	}
	sort.Strings(files)

	// Ledger of applied migrations so we never re-run DDL on every boot.
	if _, err := d.Pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("schema: ensure ledger: %w", err)
	}

	applied := map[string]bool{}
	rows, err := d.Pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("schema: read ledger: %w", err)
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return fmt.Errorf("schema: scan ledger: %w", err)
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("schema: ledger rows: %w", err)
	}

	// NOTE: We deliberately do NOT auto-baseline. A previous version recorded
	// every migration as applied whenever public.users existed, on the
	// assumption that the presence of one core table proved the whole schema
	// was provisioned. That assumption was false on partially-migrated
	// databases (e.g. only 001 applied): it silently skipped real migrations
	// such as the one that creates `hotels`, leaving the schema broken while
	// the ledger claimed it was complete. Every migration here is idempotent
	// (DDL uses IF [NOT] EXISTS, seeds use ON CONFLICT), so it is always safe
	// to run any not-yet-recorded migration. A migration is only marked
	// applied after its DDL has actually executed below.
	for _, file := range files {
		version := filepath.Base(file)
		if applied[version] {
			continue
		}
		body, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("schema: read %s: %w", file, err)
		}
		tx, err := d.Pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("schema: begin %s: %w", version, err)
		}
		for _, statement := range splitSQLStatements(string(body)) {
			if _, err := tx.Exec(ctx, statement); err != nil {
				_ = tx.Rollback(ctx)
				return fmt.Errorf("schema: migration %s failed: %w\nstatement: %s", version, err, statement)
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("schema: record %s: %w", version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("schema: commit %s: %w", version, err)
		}
		d.logger.Info("schema: applied migration " + version)
	}
	return nil
}

func findMigrationsDir() (string, error) {
	candidates := []string{
		"migrations",
		filepath.Join("..", "migrations"),
		filepath.Join("..", "..", "migrations"),
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate, nil
		}
	}
	return "", os.ErrNotExist
}

func splitSQLStatements(sql string) []string {
	parts := strings.Split(sql, ";")
	statements := make([]string, 0, len(parts))
	for _, part := range parts {
		lines := strings.Split(part, "\n")
		cleanLines := make([]string, 0, len(lines))
		for _, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "--") {
				continue
			}
			cleanLines = append(cleanLines, line)
		}
		statement := strings.TrimSpace(strings.Join(cleanLines, "\n"))
		if statement != "" {
			statements = append(statements, statement)
		}
	}
	return statements
}
