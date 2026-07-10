package handler

import (
	"context"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hotelharmony/api/pkg/response"
)

// Configurable plan -> feature control. The superadmin decides which modules each
// plan tier (basic/pro/premium) includes. Stored as overrides in plan_features;
// a missing row falls back to the built-in default (planFeatureDefault), so
// behaviour is unchanged until a toggle is saved. This is the single source of
// truth consulted by BOTH the tenant module-visibility endpoint and the plan gate.

var planTierOrder = []string{"basic", "pro", "premium"}

// planFeatureDefault is the built-in default for whether a plan includes a module,
// derived from the module's tier (moduleMinRank). Used when no override row exists.
func planFeatureDefault(plan, feature string) bool {
	return planRank(plan) >= moduleMinRank[feature]
}

// loadPlanFeatureOverrides reads the stored (feature -> enabled) overrides for a
// single plan tier. Returns an empty map on any error (caller falls back to defaults).
func loadPlanFeatureOverrides(ctx context.Context, pool *pgxpool.Pool, plan string) map[string]bool {
	out := map[string]bool{}
	rows, err := pool.Query(ctx, `SELECT feature_key, enabled FROM plan_features WHERE plan_tier = $1`, plan)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var v bool
		if err := rows.Scan(&k, &v); err == nil {
			out[k] = v
		}
	}
	return out
}

// effectivePlanFeatures merges defaults with overrides for every module in the
// registry, for one plan.
func effectivePlanFeatures(plan string, overrides map[string]bool) map[string]bool {
	m := make(map[string]bool, len(moduleRegistry))
	for _, mod := range moduleRegistry {
		v := planFeatureDefault(plan, mod.Key)
		if ov, ok := overrides[mod.Key]; ok {
			v = ov
		}
		m[mod.Key] = v
	}
	return m
}

// PlatformPlanFeatures (GET /api/platform/plan-features) returns the full
// plan × feature matrix the Plans editor renders.
func (h *OperationsHandler) PlatformPlanFeatures(c *fiber.Ctx) error {
	if !h.requirePlatformAdmin(c) {
		return nil
	}
	matrix := map[string]map[string]bool{}
	for _, plan := range planTierOrder {
		overrides := loadPlanFeatureOverrides(c.Context(), h.pool, plan)
		matrix[plan] = effectivePlanFeatures(plan, overrides)
	}
	return response.OK(c, fiber.Map{
		"plans":    planTierOrder,
		"registry": moduleRegistry,
		"matrix":   matrix,
	})
}

type planFeaturesUpdateRequest struct {
	// matrix is plan_tier -> feature_key -> enabled. Only known plans + module
	// keys are persisted; everything sent is stored as an explicit override.
	Matrix map[string]map[string]bool `json:"matrix"`
}

// UpdatePlatformPlanFeatures (PUT /api/platform/plan-features) upserts plan
// feature overrides. Stores explicit rows so the config is predictable and
// fully portal-controlled.
func (h *OperationsHandler) UpdatePlatformPlanFeatures(c *fiber.Ctx) error {
	if !h.requirePlatformAdmin(c) {
		return nil
	}
	var req planFeaturesUpdateRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}

	planKnown := map[string]bool{"basic": true, "pro": true, "premium": true}
	featureKnown := make(map[string]bool, len(moduleRegistry))
	for _, m := range moduleRegistry {
		featureKnown[m.Key] = true
	}

	tx, err := h.pool.Begin(c.Context())
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to update plan features")
	}
	defer tx.Rollback(c.Context())

	for plan, feats := range req.Matrix {
		if !planKnown[plan] {
			continue
		}
		for feature, enabled := range feats {
			if !featureKnown[feature] {
				continue
			}
			if _, err := tx.Exec(c.Context(),
				`INSERT INTO plan_features (plan_tier, feature_key, enabled, updated_at)
				 VALUES ($1, $2, $3, now())
				 ON CONFLICT (plan_tier, feature_key)
				 DO UPDATE SET enabled = EXCLUDED.enabled, updated_at = now()`,
				plan, feature, enabled); err != nil {
				return response.Error(c, fiber.StatusInternalServerError, "failed to update plan features")
			}
		}
	}
	if err := tx.Commit(c.Context()); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to commit plan features")
	}

	matrix := map[string]map[string]bool{}
	for _, plan := range planTierOrder {
		matrix[plan] = effectivePlanFeatures(plan, loadPlanFeatureOverrides(c.Context(), h.pool, plan))
	}
	return response.OK(c, fiber.Map{
		"plans":    planTierOrder,
		"registry": moduleRegistry,
		"matrix":   matrix,
	})
}
