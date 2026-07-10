-- Extend tenant_registry with provisioning metadata
ALTER TABLE tenant_registry
  ADD COLUMN IF NOT EXISTS vercel_project_id TEXT,
  ADD COLUMN IF NOT EXISTS vercel_domain      TEXT,
  ADD COLUMN IF NOT EXISTS dns_record_id      TEXT,
  ADD COLUMN IF NOT EXISTS provision_status   TEXT NOT NULL DEFAULT 'pending';

-- Track async provisioning job steps
CREATE TABLE IF NOT EXISTS provisioning_jobs (
  id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  hotel_id   UUID        NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
  status     TEXT        NOT NULL DEFAULT 'running',
  steps      JSONB       NOT NULL DEFAULT '[]'::jsonb,
  error      TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS provisioning_jobs_hotel_idx ON provisioning_jobs (hotel_id);
