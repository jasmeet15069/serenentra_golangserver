-- Distinguish backup artifact kinds (postgres dump vs redis namespace dump) so
-- the UI can label and download each. Existing rows default to postgres.
ALTER TABLE backup_jobs ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'postgres';
