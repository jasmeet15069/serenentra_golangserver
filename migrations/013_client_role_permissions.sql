-- Per-client role-feature access matrix (Phase 2 — featureMatrixGate).
--
-- Each row is one (hotel, role, feature) toggle. Semantics are DEFAULT-ON to
-- match the module registry: a MISSING row means the role keeps access, and only
-- an explicit enabled=false denies that role this feature for that client. This
-- lets the superadmin give the same role different access on different clients,
-- independent of the plan tier (plan sets the ceiling, this sets what each role
-- actually sees). No seeding is required -- existing tenants keep full access
-- until the superadmin explicitly turns something off.
--
-- feature_key aligns with moduleRegistry keys (dashboard, billing, crm, pos, ...)
-- so the gate can map a request path -> module -> this matrix on the same path
-- the plan gate already uses.
CREATE TABLE IF NOT EXISTS client_role_permissions (
    hotel_id    UUID        NOT NULL,
    role        TEXT        NOT NULL,
    feature_key TEXT        NOT NULL,
    enabled     BOOLEAN     NOT NULL DEFAULT TRUE,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (hotel_id, role, feature_key)
);

CREATE INDEX IF NOT EXISTS idx_client_role_permissions_hotel
    ON client_role_permissions (hotel_id);
