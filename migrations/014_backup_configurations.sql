-- Per-client backup configuration (Phase 4 slice one -- config only).
--
-- One row per client holds that client's backup policy -- whether automated
-- backups are enabled, the cron cadence, the storage destination, retention,
-- and encryption preference. Nothing is hardcoded -- each client configures its
-- own. The actual dump/upload engine is wired in a later slice. A missing row
-- means backups are disabled with sensible defaults, so existing tenants are
-- unaffected until the superadmin turns it on.
CREATE TABLE IF NOT EXISTS backup_configurations (
    hotel_id       UUID PRIMARY KEY,
    enabled        BOOLEAN     NOT NULL DEFAULT false,
    cron_expr      TEXT        NOT NULL DEFAULT '0 3 * * *',
    destination    TEXT        NOT NULL DEFAULT 'local',
    retention_days INT         NOT NULL DEFAULT 30,
    encrypt        BOOLEAN     NOT NULL DEFAULT true,
    notify_email   TEXT,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
