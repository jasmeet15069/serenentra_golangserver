-- Fix globally-unique constraints that block per-tenant template seeding.
-- Before this migration, only one tenant could have a room numbered "101",
-- a menu category named "Beverages", etc. These constraints are dropped and
-- replaced with composite (hotel_id, ...) keys so every client gets their own
-- isolated namespace within the same schema.

ALTER TABLE rooms DROP CONSTRAINT IF EXISTS rooms_room_number_key;
ALTER TABLE rooms ADD CONSTRAINT rooms_hotel_id_room_number_key UNIQUE (hotel_id, room_number);

ALTER TABLE menu_categories DROP CONSTRAINT IF EXISTS menu_categories_name_key;
ALTER TABLE menu_categories ADD CONSTRAINT menu_categories_hotel_id_name_key UNIQUE (hotel_id, name);

ALTER TABLE payment_settings DROP CONSTRAINT IF EXISTS payment_settings_gateway_name_key;
ALTER TABLE payment_settings ADD CONSTRAINT payment_settings_hotel_id_gateway_name_key UNIQUE (hotel_id, gateway_name);
