-- Fix globally-unique constraints that block per-tenant template seeding.
-- Before this migration, only one tenant could have a room numbered "101",
-- a menu category named "Beverages", etc. These constraints are dropped and
-- replaced with composite (hotel_id, ...) keys so every client gets their own
-- isolated namespace within the same schema.
--
-- Each new constraint is dropped before it is added. Postgres has no
-- ADD CONSTRAINT IF NOT EXISTS, and the usual dollar-quoted DO-block guard
-- cannot be used here: splitSQLStatements in
-- internal/repository/postgres/schema.go splits on the statement separator with
-- no dollar-quote awareness, so a DO block would be cut into invalid fragments.
-- Drop-then-add is the split-safe way to stay idempotent.
--
-- Idempotency matters because the runner does not auto-baseline. On a database
-- whose schema exists but whose schema_migrations ledger is incomplete, it
-- re-runs every unrecorded migration. A bare ADD CONSTRAINT then fails with
-- SQLSTATE 42P07 and the server aborts at start-up, which is exactly the
-- partially-migrated recovery case the runner was designed to survive. The whole
-- file runs in one transaction, so the brief window without the constraint is
-- never observable.

ALTER TABLE rooms DROP CONSTRAINT IF EXISTS rooms_room_number_key;
ALTER TABLE rooms DROP CONSTRAINT IF EXISTS rooms_hotel_id_room_number_key;
ALTER TABLE rooms ADD CONSTRAINT rooms_hotel_id_room_number_key UNIQUE (hotel_id, room_number);

ALTER TABLE menu_categories DROP CONSTRAINT IF EXISTS menu_categories_name_key;
ALTER TABLE menu_categories DROP CONSTRAINT IF EXISTS menu_categories_hotel_id_name_key;
ALTER TABLE menu_categories ADD CONSTRAINT menu_categories_hotel_id_name_key UNIQUE (hotel_id, name);

ALTER TABLE payment_settings DROP CONSTRAINT IF EXISTS payment_settings_gateway_name_key;
ALTER TABLE payment_settings DROP CONSTRAINT IF EXISTS payment_settings_hotel_id_gateway_name_key;
ALTER TABLE payment_settings ADD CONSTRAINT payment_settings_hotel_id_gateway_name_key UNIQUE (hotel_id, gateway_name);
