-- Per-tenant module masking. The modules column is a JSONB map of moduleKey to
-- bool. Default-on semantics mean an empty object or a missing key leaves the
-- module ENABLED, and only an explicit false value masks a module for a tenant.
-- This lets the platform master-admin hide features per client without changing
-- the portal route registry.
ALTER TABLE hotels ADD COLUMN IF NOT EXISTS modules JSONB NOT NULL DEFAULT '{}'::jsonb;
