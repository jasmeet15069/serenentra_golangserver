package handler

import (
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/hotelharmony/api/pkg/response"
)

// Feature-matrix superadmin endpoints. These let the platform admin read and set
// the per-client role × feature access matrix enforced by featureMatrixGate.
// They live on the platform surface (self-checked via requirePlatformAdmin) and
// invalidate the gate's cache on write so edits take effect immediately.

// matrixRoles is the role set surfaced in the matrix editor. Mirrors the plan
// catalog's full operational role set (guests/staff that can hold a session).
var matrixRoles = allOperationalRoles

// loadDenySet reads a tenant's explicit denials into role -> feature -> true.
func (h *OperationsHandler) loadDenySet(c *fiber.Ctx, hotelID uuid.UUID) (map[string]map[string]bool, error) {
	rows, err := h.pool.Query(c.Context(),
		`SELECT role, feature_key FROM client_role_permissions WHERE hotel_id = $1 AND enabled = false`,
		hotelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	deny := map[string]map[string]bool{}
	for rows.Next() {
		var role, feature string
		if err := rows.Scan(&role, &feature); err != nil {
			return nil, err
		}
		if deny[role] == nil {
			deny[role] = map[string]bool{}
		}
		deny[role][feature] = true
	}
	return deny, rows.Err()
}

// effectiveMatrix builds role -> feature_key -> enabled for the full role and
// module set, applying default-on semantics over the stored denials.
func effectiveMatrix(deny map[string]map[string]bool) map[string]map[string]bool {
	out := make(map[string]map[string]bool, len(matrixRoles))
	for _, role := range matrixRoles {
		fm := make(map[string]bool, len(moduleRegistry))
		for _, m := range moduleRegistry {
			enabled := true
			if d := deny[role]; d != nil && d[m.Key] {
				enabled = false
			}
			fm[m.Key] = enabled
		}
		out[role] = fm
	}
	return out
}

// PlatformTenantFeatureMatrix (GET /api/platform/tenants/:id/feature-matrix) —
// master-admin view of a tenant's role × feature matrix plus the role and module
// registries the editor renders.
func (h *OperationsHandler) PlatformTenantFeatureMatrix(c *fiber.Ctx) error {
	if !h.requirePlatformAdmin(c) {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid tenant id")
	}
	deny, err := h.loadDenySet(c, id)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to load feature matrix")
	}
	return response.OK(c, fiber.Map{
		"roles":    matrixRoles,
		"registry": moduleRegistry,
		"matrix":   effectiveMatrix(deny),
	})
}

type featureMatrixUpdateRequest struct {
	// matrix is role -> feature_key -> enabled. Only known roles and module keys
	// are persisted; unknown keys are ignored. enabled=true clears any denial
	// (default-on), enabled=false records an explicit denial.
	Matrix map[string]map[string]bool `json:"matrix"`
}

// UpdatePlatformTenantFeatureMatrix (PUT /api/platform/tenants/:id/feature-matrix)
// — master-admin sets the role × feature matrix for a tenant. Stores only
// denials (enabled=false); enabling a feature removes its row. Invalidates the
// gate cache so the change is live immediately.
func (h *OperationsHandler) UpdatePlatformTenantFeatureMatrix(c *fiber.Ctx) error {
	if !h.requirePlatformAdmin(c) {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid tenant id")
	}
	var req featureMatrixUpdateRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}

	roleKnown := make(map[string]bool, len(matrixRoles))
	for _, r := range matrixRoles {
		roleKnown[r] = true
	}
	featureKnown := make(map[string]bool, len(moduleRegistry))
	for _, m := range moduleRegistry {
		featureKnown[m.Key] = true
	}

	tx, err := h.pool.Begin(c.Context())
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to update feature matrix")
	}
	defer tx.Rollback(c.Context())

	for role, features := range req.Matrix {
		if !roleKnown[role] {
			continue
		}
		for feature, enabled := range features {
			if !featureKnown[feature] {
				continue
			}
			if enabled {
				// Default-on: enabling clears any explicit denial.
				if _, err := tx.Exec(c.Context(),
					`DELETE FROM client_role_permissions WHERE hotel_id = $1 AND role = $2 AND feature_key = $3`,
					id, role, feature); err != nil {
					return response.Error(c, fiber.StatusInternalServerError, "failed to update feature matrix")
				}
			} else {
				if _, err := tx.Exec(c.Context(),
					`INSERT INTO client_role_permissions (hotel_id, role, feature_key, enabled, updated_at)
					 VALUES ($1, $2, $3, false, now())
					 ON CONFLICT (hotel_id, role, feature_key)
					 DO UPDATE SET enabled = false, updated_at = now()`,
					id, role, feature); err != nil {
					return response.Error(c, fiber.StatusInternalServerError, "failed to update feature matrix")
				}
			}
		}
	}

	if err := tx.Commit(c.Context()); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to update feature matrix")
	}

	// Push-invalidate so the gate reflects the change immediately (not after TTL).
	if h.featureGate != nil {
		h.featureGate.invalidate(id)
	}

	deny, err := h.loadDenySet(c, id)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to reload feature matrix")
	}
	go h.saveConfigSnapshot(id)
	return response.OK(c, fiber.Map{
		"roles":    matrixRoles,
		"registry": moduleRegistry,
		"matrix":   effectiveMatrix(deny),
	})
}
