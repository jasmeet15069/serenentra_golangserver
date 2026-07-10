ALTER TABLE hotel_branding ADD COLUMN IF NOT EXISTS client_primary_color VARCHAR(7);
ALTER TABLE hotel_branding ADD COLUMN IF NOT EXISTS admin_primary_color VARCHAR(7);

UPDATE hotel_branding
SET client_primary_color = COALESCE(client_primary_color, primary_color, '#000000'),
    admin_primary_color = COALESCE(admin_primary_color, primary_color, '#000000')
WHERE client_primary_color IS NULL
   OR admin_primary_color IS NULL;
