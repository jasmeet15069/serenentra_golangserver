-- Configurable plan -> feature mapping. One row per (plan_tier, feature_key)
-- OVERRIDE: when a row exists it decides whether that plan includes that module,
-- otherwise the built-in default applies (plan rank vs the module's tier). This
-- lets the superadmin move features between Basic / Pro / Premium in either
-- direction from the portal, with zero behaviour change until a toggle is saved.
-- feature_key aligns with the module registry. plan_tier is basic, pro or premium.
CREATE TABLE IF NOT EXISTS plan_features (
    plan_tier   TEXT        NOT NULL,
    feature_key TEXT        NOT NULL,
    enabled     BOOLEAN     NOT NULL DEFAULT true,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (plan_tier, feature_key)
);
