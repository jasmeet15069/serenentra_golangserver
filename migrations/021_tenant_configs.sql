-- 021_tenant_configs.sql
-- Persistent per-client configuration snapshot.
-- Automatically written/updated whenever the superadmin changes any client setting
-- (plan, modules, feature-matrix, backup policy, profile). Allows the superadmin
-- portal to read back each client's isolated config (dedicated DB name, Redis
-- namespace, DNS slug, plan, modules, role matrix, backup policy) without
-- regenerating it from scratch on every request.

CREATE TABLE IF NOT EXISTS tenant_configs (
    hotel_id   UUID        PRIMARY KEY REFERENCES hotels(id) ON DELETE CASCADE,
    config     JSONB       NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS tenant_configs_updated_at_idx ON tenant_configs (updated_at DESC);
