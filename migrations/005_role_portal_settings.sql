CREATE TABLE IF NOT EXISTS role_portal_settings (
  id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
  hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
  role VARCHAR(50) NOT NULL,
  default_path TEXT NOT NULL,
  visible_modules JSONB NOT NULL DEFAULT '[]',
  locked BOOLEAN NOT NULL DEFAULT FALSE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (hotel_id, role)
);

CREATE INDEX IF NOT EXISTS idx_role_portal_settings_hotel_id
  ON role_portal_settings(hotel_id);
