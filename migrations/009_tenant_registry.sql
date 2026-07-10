-- Tenant isolation registry. One row per hotel tenant recording how its data is
-- isolated. isolation_mode of shared means the tenant lives in the main database
-- with row-level hotel_id scoping plus a Redis key namespace. isolation_mode of
-- dedicated means the tenant has its own database (db_name) and Redis namespace.
-- Existing hotels are backfilled as shared with redis_namespace set to the hotel
-- id, which matches the t:hotelID: cache key convention.
CREATE TABLE IF NOT EXISTS tenant_registry (
    hotel_id UUID PRIMARY KEY REFERENCES hotels(id) ON DELETE CASCADE,
    isolation_mode TEXT NOT NULL DEFAULT 'shared',
    db_name TEXT,
    redis_namespace TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO tenant_registry (hotel_id, isolation_mode, db_name, redis_namespace)
SELECT id, 'shared', NULLIF(settings->>'database_name', ''), id::text
FROM hotels
ON CONFLICT (hotel_id) DO NOTHING;
