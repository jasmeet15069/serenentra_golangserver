-- Backup job history (Phase 4 slice two -- execution). Each row is one backup
-- run for a client: status, the database dumped, the artifact path, byte size,
-- and any error. Powers the Backups detail view history and the manual run.
CREATE TABLE IF NOT EXISTS backup_jobs (
    id          UUID        PRIMARY KEY,
    hotel_id    UUID        NOT NULL,
    trigger     TEXT        NOT NULL DEFAULT 'manual',
    status      TEXT        NOT NULL DEFAULT 'running',
    db_name     TEXT,
    file_path   TEXT,
    bytes       BIGINT      NOT NULL DEFAULT 0,
    error       TEXT,
    started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_backup_jobs_hotel ON backup_jobs (hotel_id, started_at DESC);
