-- Revenue categorization for the payments table.
--
-- The dashboard revenue-trend and department-revenue queries
-- (dashboard_repository.go) group/filter payments by a `category` column
-- ('room' / 'fnb' / other), but that column never existed on the payments table,
-- so those two queries errored at runtime (the code ignores the error) and both
-- charts were silently empty for every tenant. Add the column so they work.
--
-- Additive and idempotent. Existing rows are left NULL (they bucket into "other"
-- in the trend and a NULL group in the department breakdown) rather than guessing
-- a category retroactively. New payments set it explicitly at insert time
-- (dine-in bill payments now mirror in as 'fnb').
ALTER TABLE payments ADD COLUMN IF NOT EXISTS category TEXT;

-- Speeds up the dashboard revenue aggregations (status + created_at + category).
CREATE INDEX IF NOT EXISTS idx_payments_revenue ON payments (hotel_id, status, created_at);
