package handler

import (
	"encoding/json"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/hotelharmony/api/pkg/response"
)

// currentHotelID resolves the tenant for the current request from the JWT
// hotel_id claim, falling back to the demo hotel (matches the inline pattern
// used elsewhere in OperationsHandler, which does not embed baseHandler).
func (h *OperationsHandler) currentHotelID(c *fiber.Ctx) uuid.UUID {
	hotelID := tenantHotelID(c)
	if claims, err := jwtClaimsFromRequest(c, h.secretKey); err == nil {
		if raw, _ := claims["hotel_id"].(string); strings.TrimSpace(raw) != "" {
			if parsed, perr := uuid.Parse(strings.TrimSpace(raw)); perr == nil {
				hotelID = parsed
			}
		}
	}
	return hotelID
}

// moduleDef is one toggleable portal module/feature. Keys align 1:1 with the
// admin portal's route groups so the master-admin can mask any module per tenant.
type moduleDef struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Group string `json:"group"`
}

// moduleRegistry is the canonical, ordered list of maskable modules. Adding a
// portal route group? Add it here and the master-admin can toggle it per tenant.
var moduleRegistry = []moduleDef{
	{"dashboard", "Dashboard", "Operations"},
	{"front_desk", "Front Desk", "Operations"},
	{"reservations", "Reservations", "Operations"},
	{"guests", "Guests", "Operations"},
	{"housekeeping", "Housekeeping", "Operations"},
	{"crm", "CRM & Loyalty", "Guest"},
	{"pos", "POS & Restaurant", "F&B"},
	{"restaurant", "Restaurant Mgmt", "F&B"},
	{"menu_management", "Menu Management", "F&B"},
	{"billing", "Billing", "Finance"},
	{"revenue", "Revenue", "Finance"},
	{"reports", "Reports", "Finance"},
	{"inventory", "Inventory", "Supply"},
	{"procurement", "Procurement", "Supply"},
	{"maintenance", "Maintenance", "Supply"},
	{"channel_manager", "Channel Manager", "Distribution"},
	{"booking_engine", "Booking Engine", "Distribution"},
	{"properties", "Properties", "Admin"},
	{"users", "Users & Roles", "Admin"},
	{"night_audit", "Night Audit", "Admin"},
	{"admin", "Admin Panel", "Admin"},
	{"accounting", "Accounting", "Operations"},
}

// effectiveModules merges the stored per-tenant overrides over the registry's
// default-on baseline: every known module is enabled unless explicitly false.
func effectiveModules(stored map[string]bool) map[string]bool {
	out := make(map[string]bool, len(moduleRegistry))
	for _, m := range moduleRegistry {
		enabled := true
		if v, ok := stored[m.Key]; ok {
			enabled = v
		}
		out[m.Key] = enabled
	}
	return out
}

// loadTenantModules reads the raw modules override map for a hotel.
func (h *OperationsHandler) loadTenantModules(c *fiber.Ctx, hotelID uuid.UUID) (map[string]bool, error) {
	var raw []byte
	err := h.pool.QueryRow(c.Context(), `SELECT COALESCE(modules, '{}'::jsonb) FROM hotels WHERE id = $1`, hotelID).Scan(&raw)
	if err != nil {
		return nil, err
	}
	stored := map[string]bool{}
	_ = json.Unmarshal(raw, &stored)
	return stored, nil
}

// moduleMinRank is the minimum plan rank that unlocks each module, aligned with
// the plan catalog (PRO = revenue, channel, F&B [pos/restaurant/menu],
// procurement, night audit). It is a superset of the plan gate's featureRules:
// the gate enforces API routes (F&B data lives under the already-gated /pos),
// while this also hides the F&B *nav* items. Modules not listed are basic
// (rank 0). Keep in sync with featureRules in plan_gate.go.
var moduleMinRank = map[string]int{
	"revenue":         1,
	"channel_manager": 1,
	"pos":             1,
	"restaurant":      1,
	"menu_management": 1,
	"procurement":     1,
	"night_audit":     1,
}

// planAwareModules applies the plan tier on top of the per-tenant module mask: a
// module is enabled only when the mask allows it AND the client's PLAN includes it.
// Plan→feature inclusion is the superadmin-configurable plan_features matrix (with
// a built-in default per the module's tier). This is what makes a plan change
// actually add/hide features in the portal nav, and lets features be moved between
// plans from the Plans tab.
func planAwareModules(stored map[string]bool, plan string, planOverrides map[string]bool) map[string]bool {
	eff := effectiveModules(stored)
	for _, m := range moduleRegistry {
		included := planFeatureDefault(plan, m.Key)
		if ov, ok := planOverrides[m.Key]; ok {
			included = ov
		}
		if !included {
			eff[m.Key] = false
		}
	}
	return eff
}

// tenantPlan loads a tenant's plan tier (normalized). On a lookup error it fails
// open to premium so a transient hiccup never hides features the client paid for.
func (h *OperationsHandler) tenantPlan(c *fiber.Ctx, hotelID uuid.UUID) string {
	var plan string
	if err := h.pool.QueryRow(c.Context(), `SELECT plan_tier FROM hotels WHERE id = $1`, hotelID).Scan(&plan); err != nil {
		return "premium"
	}
	return normalizePlanTier(plan)
}

// TenantModules (GET /api/tenant/modules) returns the effective enabled modules
// for the CURRENT tenant. The portal uses this to gate nav and route access. The
// result is plan-aware: a module is on only when the per-tenant mask AND the
// client's plan (per the configurable plan_features matrix) allow it.
func (h *OperationsHandler) TenantModules(c *fiber.Ctx) error {
	if !requireAuthenticatedRequest(c, h.secretKey) {
		return nil
	}
	hotelID := h.currentHotelID(c)
	stored, err := h.loadTenantModules(c, hotelID)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to load modules")
	}
	plan := h.tenantPlan(c, hotelID)
	overrides := loadPlanFeatureOverrides(c.Context(), h.pool, plan)
	return response.OK(c, fiber.Map{
		"registry": moduleRegistry,
		"modules":  planAwareModules(stored, plan, overrides),
	})
}

// PlatformTenantModules (GET /api/platform/tenants/:id/modules) — master-admin
// view of a specific tenant's module flags plus the full registry.
func (h *OperationsHandler) PlatformTenantModules(c *fiber.Ctx) error {
	if !h.requirePlatformAdmin(c) {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid tenant id")
	}
	stored, err := h.loadTenantModules(c, id)
	if err != nil {
		return response.Error(c, fiber.StatusNotFound, "tenant not found")
	}
	return response.OK(c, fiber.Map{
		"registry":  moduleRegistry,
		"modules":   effectiveModules(stored),
		"overrides": stored,
	})
}

// TenantIsolation (GET /api/platform/tenants/:id/isolation) — master-admin view
// of how a tenant's data is isolated (shared vs dedicated DB + Redis namespace).
// Falls back to effective shared defaults for tenants not yet in the registry.
func (h *OperationsHandler) TenantIsolation(c *fiber.Ctx) error {
	if !h.requirePlatformAdmin(c) {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid tenant id")
	}
	var mode, redisNS string
	var dbName *string
	err = h.pool.QueryRow(c.Context(),
		`SELECT isolation_mode, db_name, redis_namespace FROM tenant_registry WHERE hotel_id = $1`, id).
		Scan(&mode, &dbName, &redisNS)
	if err != nil {
		return response.OK(c, fiber.Map{
			"hotel_id": id, "isolation_mode": "shared", "db_name": nil, "redis_namespace": id.String(), "registered": false,
		})
	}
	return response.OK(c, fiber.Map{
		"hotel_id": id, "isolation_mode": mode, "db_name": dbName, "redis_namespace": redisNS, "registered": true,
	})
}

// deriveTenantDBName mirrors settingsForPlanTier's database naming (slug → db).
func deriveTenantDBName(slug string) string {
	db := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(slug)), "-", "_")
	db = strings.Trim(db, "_")
	if db == "" {
		db = "hotelops_tenant"
	}
	return db + "_hotelops"
}

// ProvisionTenantDB (POST /api/platform/tenants/:id/provision-db) — master-admin
// creates + migrates a dedicated database for a tenant and records it in the
// registry as 'provisioned'. Live query routing to it activates in Phase 4c, so
// the tenant keeps using the shared DB until cutover (no inconsistent state).
func (h *OperationsHandler) ProvisionTenantDB(c *fiber.Ctx) error {
	if !h.requirePlatformAdmin(c) {
		return nil
	}
	if h.tenants == nil {
		return response.Error(c, fiber.StatusServiceUnavailable, "provisioning unavailable")
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid tenant id")
	}

	var slug string
	var existing *string
	if err := h.pool.QueryRow(c.Context(),
		`SELECT h.slug, tr.db_name FROM hotels h
		 LEFT JOIN tenant_registry tr ON tr.hotel_id = h.id
		 WHERE h.id = $1`, id).Scan(&slug, &existing); err != nil {
		return response.Error(c, fiber.StatusNotFound, "tenant not found")
	}
	dbName := deriveTenantDBName(slug)
	if existing != nil && strings.TrimSpace(*existing) != "" {
		dbName = strings.TrimSpace(*existing)
	}

	if err := h.tenants.Provision(c.Context(), dbName); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}

	// Attempt to seed essential hotel metadata and activate dedicated routing.
	isolationMode := "provisioned"
	if tPool, tErr := h.tenants.PoolFor(c.Context(), "dedicated", dbName); tErr == nil {
		var hotelName, slug, plan, currency, settings string
		var country *string
		_ = h.pool.QueryRow(c.Context(),
			`SELECT name, slug, plan_tier, COALESCE(currency,'USD'), COALESCE(country,''), settings::text FROM hotels WHERE id = $1`, id,
		).Scan(&hotelName, &slug, &plan, &currency, &country, &settings)
		cnt := ""
		if country != nil {
			cnt = *country
		}
		if hotelName != "" {
			if sErr := seedTenantDB(c.Context(), tPool, id, hotelName, slug, plan, currency, cnt, settings); sErr == nil {
				isolationMode = "dedicated"
			}
		}
	}

	if _, err := h.pool.Exec(c.Context(), `
		INSERT INTO tenant_registry (hotel_id, isolation_mode, db_name, redis_namespace)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (hotel_id) DO UPDATE
		SET isolation_mode = $2, db_name = EXCLUDED.db_name, updated_at = now()`,
		id, isolationMode, dbName, id.String()); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "db created but registry update failed: "+err.Error())
	}

	return response.OK(c, fiber.Map{
		"hotel_id":       id,
		"db_name":        dbName,
		"isolation_mode": isolationMode,
	})
}

type updateTenantModulesRequest struct {
	Modules map[string]bool `json:"modules"`
}

// UpdatePlatformTenantModules (PUT /api/platform/tenants/:id/modules) —
// master-admin sets which modules are masked for a tenant. Only registry keys
// are persisted; unknown keys are ignored so the column stays clean.
func (h *OperationsHandler) UpdatePlatformTenantModules(c *fiber.Ctx) error {
	if !h.requirePlatformAdmin(c) {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid tenant id")
	}
	var req updateTenantModulesRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}

	known := make(map[string]bool, len(moduleRegistry))
	for _, m := range moduleRegistry {
		known[m.Key] = true
	}
	clean := map[string]bool{}
	for k, v := range req.Modules {
		if known[k] {
			clean[k] = v
		}
	}
	payload, _ := json.Marshal(clean)

	if _, err := h.pool.Exec(c.Context(),
		`UPDATE hotels SET modules = $1::jsonb, updated_at = now() WHERE id = $2`, string(payload), id); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to update modules")
	}
	go h.saveConfigSnapshot(id)
	return response.OK(c, fiber.Map{
		"registry":  moduleRegistry,
		"modules":   effectiveModules(clean),
		"overrides": clean,
	})
}
