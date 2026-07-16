-- Restaurant waitlist. The restaurant floor page had a hardcoded demo waitlist
-- with no persistence. This backs it with a real per-tenant table so guests can be
-- added, notified, seated, or removed.
CREATE TABLE IF NOT EXISTS waitlists (
  id UUID PRIMARY KEY,
  hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  party_size INT NOT NULL DEFAULT 2,
  section TEXT,
  phone TEXT,
  quoted_wait INT,                       -- minutes quoted to the guest
  status TEXT NOT NULL DEFAULT 'waiting', -- waiting | notified | seated | left
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_waitlists_hotel ON waitlists (hotel_id, created_at DESC);
