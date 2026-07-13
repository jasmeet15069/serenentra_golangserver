package handler

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hotelharmony/api/internal/config"
	"github.com/hotelharmony/api/internal/repository/postgres"
	"github.com/hotelharmony/api/pkg/response"
)

type CompatHandler struct {
	pool      *pgxpool.Pool
	secretKey string
	// planGate and featureGate are wired by router.go after construction so the
	// compat write path enforces the SAME plan-inclusion, per-tenant module mask,
	// and per-role feature matrix as the dedicated handlers. The compat layer is
	// registered before those middlewares (it self-authenticates), so without
	// this it would bypass all plan/module/role gating. Nil-safe: when unset,
	// enforcement is skipped (fail-open), preserving prior behaviour.
	planGate    *planGateState
	featureGate *featureGateState
}

func NewCompatHandler(pool *pgxpool.Pool, cfg *config.Config) *CompatHandler {
	secret := ""
	if cfg != nil {
		secret = cfg.Auth.AccessTokenSecret
	}
	return &CompatHandler{pool: pool, secretKey: secret}
}

// compatWriteModule maps a compat table to the module that owns WRITES to it.
// Writes map 1:1 to a module (a POS order is created from POS, a menu item from
// Menu Management), unlike reads which are cross-module (POS reads menu), so
// gating writes by owning module is unambiguous and low-regression. Tables not
// listed here — and all identity/support tables (profiles, user_roles,
// guest_preferences, staff_shifts, audit_logs) — are left ungoverned. Keys
// align with moduleRegistry so the mask/matrix/plan checks resolve correctly.
var compatWriteModule = map[string]string{
	"rooms":                    "front_desk",
	"guest_stays":              "front_desk",
	"complaints":               "crm",
	"menu_categories":          "menu_management",
	"menu_items":               "menu_management",
	"menu_item_customizations": "menu_management",
	"inventory_items":          "inventory",
	"orders":                   "pos",
	"order_items":              "pos",
	"payments":                 "billing",
	"payment_settings":         "billing",
	"housekeeping_assignments": "housekeeping",
	"work_orders":              "maintenance",
}

// enforceCompatWrite applies plan-inclusion + per-tenant module mask + per-role
// feature-matrix gating to a compat write, keyed by the table's owning module.
// It mirrors planGateState.handler + featureGateState.handler exactly (default-on,
// fail-open, platform/super admin bypass). Returns true when the write may
// proceed; when it returns false it has already written the 403 response.
func (h *CompatHandler) enforceCompatWrite(c *fiber.Ctx, table string) bool {
	module, governed := compatWriteModule[table]
	if !governed || module == "" || h.planGate == nil || h.featureGate == nil {
		return true
	}

	claims, err := jwtClaimsFromRequest(c, h.secretKey)
	if err != nil {
		return true // authenticated already; fail open on claim re-read
	}
	if pa, _ := claims["platform_admin"].(bool); pa {
		return true
	}
	roles := make([]string, 0, 4)
	if rawRoles, ok := claims["roles"].([]interface{}); ok {
		for _, rr := range rawRoles {
			role, _ := rr.(string)
			if role == "platform_admin" || role == "super_admin" {
				return true
			}
			if role != "" {
				roles = append(roles, role)
			}
		}
	}

	hotelID := h.hotelID(c)

	// 1. Plan inclusion + per-tenant module mask (planGate.confFor).
	if conf, ok := h.planGate.confFor(c.Context(), hotelID); ok {
		included := planFeatureDefault(conf.plan, module)
		if v, exists := conf.planFeat[module]; exists {
			included = v
		}
		if !included {
			_ = response.Error(c, fiber.StatusForbidden,
				"this feature is not included in your plan; please upgrade your subscription")
			return false
		}
		if enabled, exists := conf.modules[module]; exists && !enabled {
			_ = response.Error(c, fiber.StatusForbidden, "this module is disabled for your account")
			return false
		}
	}

	// 2. Per-role feature matrix (featureGate.confFor). Allow when ANY held role
	// is permitted; deny only when every role is explicitly disabled.
	if len(roles) > 0 {
		if conf, ok := h.featureGate.confFor(c.Context(), hotelID); ok {
			allowed := false
			for _, role := range roles {
				d := conf.deny[role]
				if d == nil || !d[module] {
					allowed = true
					break
				}
			}
			if !allowed {
				_ = response.Error(c, fiber.StatusForbidden, "this feature is not enabled for your role")
				return false
			}
		}
	}
	return true
}

// enforceCompatRead gates a compat READ by PLAN-inclusion only (tier level),
// keyed by the table's owning module. It deliberately does NOT apply the
// module-mask or per-role matrix that writes use: reads can be cross-module (a
// POS page reads the menu tables), and both mask and role checks could 403 a
// legitimate shared read, whereas plan tier is uniform for the tenant. This
// still closes the core leak — a basic-plan tenant (or a raw API caller) can no
// longer read a PRO module's data (orders, menu) via /api/tables/*. Default-on,
// fail-open, platform/super admin bypass.
func (h *CompatHandler) enforceCompatRead(c *fiber.Ctx, table string) bool {
	module, governed := compatWriteModule[table]
	if !governed || module == "" || h.planGate == nil {
		return true
	}
	claims, err := jwtClaimsFromRequest(c, h.secretKey)
	if err != nil {
		return true
	}
	if pa, _ := claims["platform_admin"].(bool); pa {
		return true
	}
	if rawRoles, ok := claims["roles"].([]interface{}); ok {
		for _, rr := range rawRoles {
			if role, _ := rr.(string); role == "platform_admin" || role == "super_admin" {
				return true
			}
		}
	}
	if conf, ok := h.planGate.confFor(c.Context(), h.hotelID(c)); ok {
		included := planFeatureDefault(conf.plan, module)
		if v, exists := conf.planFeat[module]; exists {
			included = v
		}
		if !included {
			_ = response.Error(c, fiber.StatusForbidden,
				"this feature is not included in your plan; please upgrade your subscription")
			return false
		}
	}
	return true
}

// hotelID resolves the caller's tenant from the authenticated JWT's hotel_id
// claim, falling back to the demo/default hotel only when the token carries no
// hotel_id. Every tenant-scoped query binds this value so a caller can only
// read/update/delete rows belonging to their own hotel. Modeled on
// operations_handler.go PlanLimits and authctx.go baseHandler.hotelID.
func (h *CompatHandler) hotelID(c *fiber.Ctx) uuid.UUID {
	if claims, err := jwtClaimsFromRequest(c, h.secretKey); err == nil {
		if raw, _ := claims["hotel_id"].(string); strings.TrimSpace(raw) != "" {
			if parsed, perr := uuid.Parse(strings.TrimSpace(raw)); perr == nil {
				return parsed
			}
		}
	}
	return postgres.DemoHotelID
}

// compatTenantScopedTables lists the compat tables that carry a hotel_id column.
// Reads, inserts and updates against these MUST be constrained to the caller's
// tenant. Tables not listed here (profiles, user_roles) are user-keyed identity
// tables with no hotel_id column and are intentionally not tenant-filtered.
var compatTenantScopedTables = map[string]bool{
	"rooms": true, "guest_stays": true, "complaints": true, "menu_categories": true,
	"menu_items": true, "menu_item_customizations": true, "inventory_items": true,
	"guest_preferences": true, "orders": true, "order_items": true, "payments": true,
	"payment_settings": true, "staff_shifts": true, "housekeeping_assignments": true,
	"work_orders": true, "folios": true, "folio_charges": true, "audit_logs": true,
}

// scopeHotel seeds a compat SELECT's WHERE/args slices with the caller's hotel_id
// so the query can only ever return rows belonging to the authenticated tenant.
// alias is the SQL alias of the table that owns the hotel_id column ("" when the
// table is queried without an alias). Subsequent filters append using len(args).
func (h *CompatHandler) scopeHotel(c *fiber.Ctx, alias string) ([]string, []interface{}) {
	col := "hotel_id"
	if alias != "" {
		col = alias + ".hotel_id"
	}
	return []string{fmt.Sprintf("%s = $1", col)}, []interface{}{h.hotelID(c)}
}

func (h *CompatHandler) Register(r fiber.Router) {
	r.Get("/tables/:table", h.Select)
	r.Post("/tables/:table", h.Insert)
	r.Patch("/tables/:table", h.Update)
	r.Delete("/tables/:table", h.Delete)
}

type compatFilter struct {
	Column   string      `json:"column"`
	Operator string      `json:"operator"`
	Value    interface{} `json:"value"`
}

type compatMutation struct {
	Values  interface{}    `json:"values"`
	Filters []compatFilter `json:"filters"`
	Single  interface{}    `json:"single"`
}

func (h *CompatHandler) Select(c *fiber.Ctx) error {
	if !requireAuthenticatedRequest(c, h.secretKey) {
		return nil
	}

	table := c.Params("table")
	if !h.enforceCompatRead(c, table) {
		return nil
	}
	filters := parseCompatFilters(c.Query("filters"))

	switch table {
	case "profiles":
		return h.selectProfiles(c, filters)
	case "user_roles":
		return h.selectUserRoles(c, filters)
	case "rooms":
		return h.selectRooms(c, filters)
	case "guest_stays":
		return h.selectGuestStays(c, filters)
	case "complaints":
		return h.selectComplaints(c, filters)
	case "menu_categories":
		return h.selectMenuCategories(c, filters)
	case "menu_items":
		return h.selectMenuItems(c, filters)
	case "menu_item_customizations":
		return h.selectMenuCustomizations(c, filters)
	case "inventory_items":
		return h.selectInventoryItems(c, filters)
	case "guest_preferences":
		return h.selectGuestPreferences(c, filters)
	case "orders":
		return h.selectOrders(c, filters)
	case "order_items":
		return h.selectOrderItems(c, filters)
	case "payments":
		return h.selectPayments(c, filters)
	case "payment_settings":
		return h.selectPaymentSettings(c, filters)
	case "staff_shifts":
		return h.selectStaffShifts(c, filters)
	case "housekeeping_assignments":
		return h.selectHousekeepingAssignments(c, filters)
	case "work_orders":
		return h.selectWorkOrders(c, filters)
	case "folios":
		return h.selectFolios(c, filters)
	case "folio_charges":
		return h.selectFolioCharges(c, filters)
	case "audit_logs":
		return h.selectAuditLogs(c, filters)
	default:
		return response.Error(c, fiber.StatusNotFound, fmt.Sprintf("unsupported compatibility table: %s", table))
	}
}

func (h *CompatHandler) Insert(c *fiber.Ctx) error {
	if !requireAuthenticatedRequest(c, h.secretKey) {
		return nil
	}

	table := c.Params("table")
	if !h.enforceCompatWrite(c, table) {
		return nil
	}
	payload, err := parseMutation(c)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	values, ok := firstValueMap(payload.Values)
	if !ok {
		return response.Error(c, fiber.StatusBadRequest, "insert values are required")
	}

	return h.withCompatAudit(c, "CREATE", table, asString(values["id"]), map[string]interface{}{"values": payload.Values}, func() error {
		switch table {
		case "profiles":
			return h.insertProfile(c, values, mutationSingle(payload.Single))
		case "user_roles":
			return h.insertUserRole(c, values)
		case "rooms":
			return h.insertRoom(c, values)
		case "guest_stays":
			return h.insertGuestStay(c, values)
		case "complaints":
			return h.insertComplaint(c, values)
		case "menu_categories":
			return h.insertMenuCategory(c, values)
		case "menu_items":
			return h.insertMenuItem(c, values, mutationSingle(payload.Single))
		case "menu_item_customizations":
			return h.insertMenuCustomizations(c, valueMaps(payload.Values))
		case "inventory_items":
			return h.insertInventoryItem(c, values)
		case "guest_preferences":
			return h.insertGuestPreferences(c, values, mutationSingle(payload.Single))
		case "orders":
			return h.insertOrder(c, values, mutationSingle(payload.Single))
		case "order_items":
			return h.insertOrderItems(c, valueMaps(payload.Values))
		case "payments":
			return h.insertPayment(c, values)
		case "payment_settings":
			return h.insertPaymentSetting(c, values)
		case "staff_shifts":
			return h.insertStaffShift(c, values)
		case "housekeeping_assignments":
			return h.insertHousekeepingAssignment(c, values)
		case "work_orders":
			return h.insertWorkOrder(c, values)
		default:
			return response.Error(c, fiber.StatusNotFound, fmt.Sprintf("unsupported compatibility insert table: %s", table))
		}
	})
}

func (h *CompatHandler) Update(c *fiber.Ctx) error {
	if !requireAuthenticatedRequest(c, h.secretKey) {
		return nil
	}

	table := c.Params("table")
	if !h.enforceCompatWrite(c, table) {
		return nil
	}
	payload, err := parseMutation(c)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	values, ok := firstValueMap(payload.Values)
	if !ok {
		return response.Error(c, fiber.StatusBadRequest, "update values are required")
	}
	id, ok := stringFilter(payload.Filters, "id")
	if (!ok || id == "") && table != "profiles" && table != "guest_preferences" {
		return response.Error(c, fiber.StatusBadRequest, "id filter is required")
	}

	recordID := id
	if recordID == "" {
		recordID, _ = stringFilter(payload.Filters, "user_id")
	}
	return h.withCompatAudit(c, "UPDATE", table, recordID, map[string]interface{}{"filters": payload.Filters, "values": values}, func() error {
		switch table {
		case "profiles":
			userID, _ := stringFilter(payload.Filters, "user_id")
			return h.updateProfile(c, id, userID, values)
		case "rooms":
			return h.updateRoom(c, id, values)
		case "guest_stays":
			return h.updateGuestStay(c, id, values)
		case "complaints":
			return h.updateComplaint(c, id, values)
		case "menu_categories":
			return h.updateMenuCategory(c, id, values)
		case "menu_items":
			return h.updateMenuItem(c, id, values)
		case "menu_item_customizations":
			return h.updateMenuCustomization(c, id, values)
		case "inventory_items":
			return h.updateInventoryItem(c, id, values)
		case "guest_preferences":
			userID, _ := stringFilter(payload.Filters, "user_id")
			return h.updateGuestPreferences(c, id, userID, values)
		case "orders":
			return h.updateOrder(c, id, values)
		case "payments":
			return h.updatePayment(c, id, values)
		case "payment_settings":
			return h.updatePaymentSetting(c, id, values)
		case "staff_shifts":
			return h.updateStaffShift(c, id, values)
		case "housekeeping_assignments":
			return h.updateHousekeepingAssignment(c, id, values)
		case "work_orders":
			return h.updateWorkOrder(c, id, values)
		default:
			return response.Error(c, fiber.StatusNotFound, fmt.Sprintf("unsupported compatibility update table: %s", table))
		}
	})
}

func (h *CompatHandler) Delete(c *fiber.Ctx) error {
	if !requireAuthenticatedRequest(c, h.secretKey) {
		return nil
	}

	table := c.Params("table")
	if !h.enforceCompatWrite(c, table) {
		return nil
	}
	payload, err := parseMutation(c)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	id, _ := stringFilter(payload.Filters, "id")

	recordID := id
	if recordID == "" {
		recordID, _ = stringFilter(payload.Filters, "user_id")
	}
	return h.withCompatAudit(c, "DELETE", table, recordID, map[string]interface{}{"filters": payload.Filters}, func() error {
		switch table {
		case "rooms":
			if id == "" {
				return response.Error(c, fiber.StatusBadRequest, "id filter is required")
			}
			if _, err := tenantPool(c, h.pool).Exec(c.Context(), `DELETE FROM rooms WHERE id = $1 AND hotel_id = $2`, id, h.hotelID(c)); err != nil {
				return response.Error(c, fiber.StatusBadRequest, err.Error())
			}
			return response.OK(c, []map[string]interface{}{})
		case "guest_stays":
			if id == "" {
				return response.Error(c, fiber.StatusBadRequest, "id filter is required")
			}
			if _, err := tenantPool(c, h.pool).Exec(c.Context(), `DELETE FROM guest_stays WHERE id = $1 AND hotel_id = $2`, id, h.hotelID(c)); err != nil {
				return response.Error(c, fiber.StatusBadRequest, err.Error())
			}
			return response.OK(c, []map[string]interface{}{})
		case "complaints", "menu_categories", "menu_items", "inventory_items", "orders", "payments", "payment_settings", "staff_shifts", "housekeeping_assignments", "work_orders":
			// Tenant-scoped tables: restrict the delete to the caller's hotel so
			// a guessed id cannot reach another tenant's rows.
			if id == "" {
				return response.Error(c, fiber.StatusBadRequest, "id filter is required")
			}
			if _, err := tenantPool(c, h.pool).Exec(c.Context(), fmt.Sprintf(`DELETE FROM %s WHERE id = $1 AND hotel_id = $2`, table), id, h.hotelID(c)); err != nil {
				return response.Error(c, fiber.StatusBadRequest, err.Error())
			}
			return response.OK(c, []map[string]interface{}{})
		case "profiles", "guest_preferences":
			// User-keyed identity tables; not tenant-scoped here (see note in Update).
			if id == "" {
				return response.Error(c, fiber.StatusBadRequest, "id filter is required")
			}
			if _, err := tenantPool(c, h.pool).Exec(c.Context(), fmt.Sprintf(`DELETE FROM %s WHERE id = $1`, table), id); err != nil {
				return response.Error(c, fiber.StatusBadRequest, err.Error())
			}
			return response.OK(c, []map[string]interface{}{})
		case "menu_item_customizations":
			return h.deleteByIDOrColumn(c, table, id, payload.Filters, "menu_item_id")
		case "order_items":
			return h.deleteByIDOrColumn(c, table, id, payload.Filters, "order_id")
		case "user_roles":
			if id != "" {
				if _, err := tenantPool(c, h.pool).Exec(c.Context(), `DELETE FROM user_roles WHERE id = $1`, id); err != nil {
					return response.Error(c, fiber.StatusBadRequest, err.Error())
				}
				return response.OK(c, []map[string]interface{}{})
			}
			userID, _ := stringFilter(payload.Filters, "user_id")
			role, _ := stringFilter(payload.Filters, "role")
			if userID == "" || role == "" {
				return response.Error(c, fiber.StatusBadRequest, "id or user_id+role filters are required")
			}
			if _, err := tenantPool(c, h.pool).Exec(c.Context(), `DELETE FROM user_roles WHERE user_id = $1 AND role = $2`, userID, role); err != nil {
				return response.Error(c, fiber.StatusBadRequest, err.Error())
			}
			return response.OK(c, []map[string]interface{}{})
		default:
			return response.Error(c, fiber.StatusNotFound, fmt.Sprintf("unsupported compatibility delete table: %s", table))
		}
	})
}

func (h *CompatHandler) withCompatAudit(c *fiber.Ctx, action, table, recordIDHint string, details map[string]interface{}, run func() error) error {
	err := run()
	if err != nil {
		return err
	}
	status := c.Response().StatusCode()
	if status < fiber.StatusOK || status >= fiber.StatusMultipleChoices {
		return nil
	}

	recordIDs := compatRecordIDsFromResponse(c.Response().Body())
	if len(recordIDs) == 0 && recordIDHint != "" {
		recordIDs = []string{recordIDHint}
	}
	if len(recordIDs) == 0 {
		recordIDs = []string{""}
	}

	for _, recordID := range recordIDs {
		if err := h.auditCompatMutation(c, action, table, recordID, details); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, fmt.Sprintf("audit log failed: %s", err.Error()))
		}
	}
	return nil
}

func (h *CompatHandler) auditCompatMutation(c *fiber.Ctx, action, table, recordID string, details map[string]interface{}) error {
	claims, err := jwtClaimsFromRequest(c, h.secretKey)
	if err != nil {
		return err
	}

	hotelID := postgres.DemoHotelID
	if rawHotelID, ok := claims["hotel_id"].(string); ok && strings.TrimSpace(rawHotelID) != "" {
		if parsed, err := uuid.Parse(rawHotelID); err == nil {
			hotelID = parsed
		}
	}

	var userID *uuid.UUID
	if rawUserID, ok := claims["sub"].(string); ok && strings.TrimSpace(rawUserID) != "" {
		if parsed, err := uuid.Parse(rawUserID); err == nil {
			userID = &parsed
		}
	}

	var recordUUID *uuid.UUID
	if strings.TrimSpace(recordID) != "" {
		if parsed, err := uuid.Parse(recordID); err == nil {
			recordUUID = &parsed
		}
	}

	payload, _ := json.Marshal(details)
	_, err = tenantPool(c, h.pool).Exec(c.Context(), `
		INSERT INTO audit_logs (
			id, hotel_id, user_id, action, table_name, record_id, resource_type, resource_id,
			new_data, user_agent, ai_triggered, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,false,now())`,
		uuid.New(), hotelID, userID, action, table, recordUUID, table, recordUUID, payload, c.Get("User-Agent"),
	)
	return err
}

func compatRecordIDsFromResponse(body []byte) []string {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if len(body) == 0 || json.Unmarshal(body, &envelope) != nil || len(envelope.Data) == 0 {
		return nil
	}

	var object map[string]interface{}
	if json.Unmarshal(envelope.Data, &object) == nil {
		if id, ok := object["id"].(string); ok && id != "" {
			return []string{id}
		}
	}

	var objects []map[string]interface{}
	if json.Unmarshal(envelope.Data, &objects) != nil {
		return nil
	}
	ids := make([]string, 0, len(objects))
	for _, item := range objects {
		if id, ok := item["id"].(string); ok && id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func mutationSingle(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "single"
		}
	}
	return ""
}

func parseCompatFilters(raw string) []compatFilter {
	if raw == "" {
		return nil
	}
	var filters []compatFilter
	if err := json.Unmarshal([]byte(raw), &filters); err == nil {
		return filters
	}
	var single compatFilter
	if err := json.Unmarshal([]byte(raw), &single); err == nil && single.Column != "" {
		return []compatFilter{single}
	}
	return filters
}

func parseMutation(c *fiber.Ctx) (*compatMutation, error) {
	var payload compatMutation
	if err := json.Unmarshal(c.Body(), &payload); err != nil {
		return nil, err
	}
	if payload.Filters == nil {
		payload.Filters = parseCompatFilters(c.Query("filters"))
	}
	return &payload, nil
}

func filterValue(filters []compatFilter, column string) (interface{}, bool) {
	for _, f := range filters {
		if f.Column == column && f.Operator == "eq" {
			return f.Value, true
		}
	}
	return nil, false
}

func stringFilter(filters []compatFilter, column string) (string, bool) {
	v, ok := filterValue(filters, column)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func firstValueMap(values interface{}) (map[string]interface{}, bool) {
	switch v := values.(type) {
	case map[string]interface{}:
		return v, true
	case []interface{}:
		if len(v) == 0 {
			return nil, false
		}
		m, ok := v[0].(map[string]interface{})
		return m, ok
	default:
		return nil, false
	}
}

func valueMaps(values interface{}) []map[string]interface{} {
	switch v := values.(type) {
	case map[string]interface{}:
		return []map[string]interface{}{v}
	case []interface{}:
		items := make([]map[string]interface{}, 0, len(v))
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				items = append(items, m)
			}
		}
		return items
	default:
		return nil
	}
}

func singleMode(c *fiber.Ctx) string {
	return c.Query("single")
}

func (h *CompatHandler) selectProfiles(c *fiber.Ctx, filters []compatFilter) error {
	q := `SELECT id, user_id, full_name, phone, avatar_url, created_at, updated_at FROM profiles`
	args := []interface{}{}
	if v, ok := filterValue(filters, "user_id"); ok {
		q += " WHERE user_id = $1"
		args = append(args, v)
	}
	q += " ORDER BY created_at DESC"

	rows, err := tenantPool(c, h.pool).Query(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()

	items := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, userID, fullName string
		var phone, avatarURL *string
		var createdAt, updatedAt interface{}
		if err := rows.Scan(&id, &userID, &fullName, &phone, &avatarURL, &createdAt, &updatedAt); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		items = append(items, map[string]interface{}{
			"id":         id,
			"user_id":    userID,
			"full_name":  fullName,
			"phone":      phone,
			"avatar_url": avatarURL,
			"created_at": createdAt,
			"updated_at": updatedAt,
		})
	}
	if singleMode(c) != "" {
		if len(items) == 0 {
			return response.OK(c, nil)
		}
		return response.OK(c, items[0])
	}
	return response.OK(c, items)
}

func (h *CompatHandler) selectUserRoles(c *fiber.Ctx, filters []compatFilter) error {
	q := `SELECT id, user_id, role, created_at FROM user_roles`
	args := []interface{}{}
	if v, ok := filterValue(filters, "user_id"); ok {
		q += " WHERE user_id = $1"
		args = append(args, v)
	}
	q += " ORDER BY created_at"

	rows, err := tenantPool(c, h.pool).Query(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()

	items := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, userID, role string
		var createdAt interface{}
		if err := rows.Scan(&id, &userID, &role, &createdAt); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		items = append(items, map[string]interface{}{
			"id":         id,
			"user_id":    userID,
			"role":       role,
			"created_at": createdAt,
		})
	}
	return response.OK(c, items)
}

func (h *CompatHandler) insertProfile(c *fiber.Ctx, v map[string]interface{}, single string) error {
	id := uuid.New().String()
	rows, err := tenantPool(c, h.pool).Query(c.Context(), `INSERT INTO profiles (id, user_id, full_name, phone, avatar_url, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,now(),now()) ON CONFLICT (user_id) DO UPDATE SET full_name = EXCLUDED.full_name, phone = EXCLUDED.phone, avatar_url = EXCLUDED.avatar_url, updated_at = now() RETURNING id, user_id, full_name, phone, avatar_url, created_at, updated_at`,
		id, asString(v["user_id"]), asStringDefault(v["full_name"], "Guest"), nullableString(v["phone"]), nullableString(v["avatar_url"]))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	defer rows.Close()
	items, err := scanProfileMaps(rows)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	if single != "" && len(items) > 0 {
		return response.Created(c, items[0])
	}
	return response.Created(c, items)
}

func (h *CompatHandler) insertUserRole(c *fiber.Ctx, v map[string]interface{}) error {
	id := uuid.New().String()
	rows, err := tenantPool(c, h.pool).Query(c.Context(), `INSERT INTO user_roles (id, user_id, role, created_at) VALUES ($1,$2,$3,now()) ON CONFLICT (user_id, role) DO UPDATE SET role = EXCLUDED.role RETURNING id, user_id, role, created_at`, id, asString(v["user_id"]), asString(v["role"]))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	defer rows.Close()
	items := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, userID, role string
		var createdAt interface{}
		if err := rows.Scan(&id, &userID, &role, &createdAt); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		items = append(items, map[string]interface{}{"id": id, "user_id": userID, "role": role, "created_at": createdAt})
	}
	return response.Created(c, items)
}

func (h *CompatHandler) updateProfile(c *fiber.Ctx, id string, userID string, v map[string]interface{}) error {
	allowed := map[string]bool{"full_name": true, "phone": true, "avatar_url": true}
	if id != "" {
		return h.updateAllowed(c, "profiles", id, allowed, v)
	}
	if userID == "" {
		return response.Error(c, fiber.StatusBadRequest, "id or user_id filter is required")
	}
	return h.updateAllowedByColumn(c, "profiles", "user_id", userID, allowed, v)
}

func (h *CompatHandler) selectRooms(c *fiber.Ctx, filters []compatFilter) error {
	q := `SELECT id, room_number, room_type, floor, capacity, price_per_night, status, amenities, created_at, updated_at FROM rooms`
	where, args := h.scopeHotel(c, "")
	if v, ok := filterValue(filters, "status"); ok {
		args = append(args, v)
		where = append(where, fmt.Sprintf("status = $%d", len(args)))
	}
	q += " WHERE " + strings.Join(where, " AND ") + " ORDER BY floor, room_number"

	rows, err := tenantPool(c, h.pool).Query(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()

	items := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, roomNumber, roomType, status string
		var floor, capacity int
		var price float64
		var amenities []string
		var createdAt, updatedAt interface{}
		if err := rows.Scan(&id, &roomNumber, &roomType, &floor, &capacity, &price, &status, &amenities, &createdAt, &updatedAt); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		items = append(items, map[string]interface{}{
			"id":              id,
			"room_number":     roomNumber,
			"room_type":       roomType,
			"floor":           floor,
			"capacity":        capacity,
			"price_per_night": price,
			"status":          status,
			"amenities":       amenities,
			"created_at":      createdAt,
			"updated_at":      updatedAt,
		})
	}
	return response.OK(c, items)
}

func (h *CompatHandler) selectGuestStays(c *fiber.Ctx, filters []compatFilter) error {
	q := `SELECT gs.id, gs.guest_id, gs.room_id, gs.guest_name, gs.guest_email, gs.guest_phone,
		         gs.check_in_date, gs.check_out_date, gs.actual_check_in, gs.actual_check_out,
		         gs.total_amount, gs.notes, gs.created_by, gs.created_at, gs.updated_at,
		         r.room_number, r.room_type
		  FROM guest_stays gs
		  LEFT JOIN rooms r ON r.id = gs.room_id`
	where, args := h.scopeHotel(c, "gs")
	if v, ok := filterValue(filters, "guest_id"); ok {
		args = append(args, v)
		where = append(where, fmt.Sprintf("gs.guest_id = $%d", len(args)))
	}
	if v, ok := filterValue(filters, "room_id"); ok {
		args = append(args, v)
		where = append(where, fmt.Sprintf("gs.room_id = $%d", len(args)))
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY gs.check_in_date DESC"

	rows, err := tenantPool(c, h.pool).Query(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()

	items := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, roomID, guestName string
		var guestID, guestEmail, guestPhone, notes, createdBy, roomNumber, roomType *string
		var checkIn, checkOut, actualCheckIn, actualCheckOut, createdAt, updatedAt interface{}
		var totalAmount *float64
		if err := rows.Scan(
			&id, &guestID, &roomID, &guestName, &guestEmail, &guestPhone,
			&checkIn, &checkOut, &actualCheckIn, &actualCheckOut,
			&totalAmount, &notes, &createdBy, &createdAt, &updatedAt,
			&roomNumber, &roomType,
		); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		item := map[string]interface{}{
			"id":               id,
			"guest_id":         guestID,
			"room_id":          roomID,
			"guest_name":       guestName,
			"guest_email":      guestEmail,
			"guest_phone":      guestPhone,
			"check_in_date":    checkIn,
			"check_out_date":   checkOut,
			"actual_check_in":  actualCheckIn,
			"actual_check_out": actualCheckOut,
			"total_amount":     totalAmount,
			"notes":            notes,
			"created_by":       createdBy,
			"created_at":       createdAt,
			"updated_at":       updatedAt,
			"rooms":            nil,
		}
		if roomNumber != nil {
			item["rooms"] = map[string]interface{}{
				"room_number": *roomNumber,
				"room_type":   roomType,
			}
		}
		items = append(items, item)
	}
	return response.OK(c, items)
}

func (h *CompatHandler) insertRoom(c *fiber.Ctx, v map[string]interface{}) error {
	id := uuid.New().String()
	roomNumber := asString(v["room_number"])
	roomType := asStringDefault(v["room_type"], "Standard")
	floor := asIntDefault(v["floor"], 1)
	capacity := asIntDefault(v["capacity"], 2)
	price := asFloatDefault(v["price_per_night"], 0)
	status := asStringDefault(v["status"], "available")
	amenities := asStringSlice(v["amenities"])

	const q = `INSERT INTO rooms (id, hotel_id, room_number, room_type, floor, capacity, price_per_night, status, amenities, created_at, updated_at)
	           VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,now(),now())
	           RETURNING id, room_number, room_type, floor, capacity, price_per_night, status, amenities, created_at, updated_at`
	rows, err := tenantPool(c, h.pool).Query(c.Context(), q, id, h.hotelID(c), roomNumber, roomType, floor, capacity, price, status, amenities)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	defer rows.Close()
	items, err := scanRoomMaps(rows)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	return response.Created(c, items)
}

func (h *CompatHandler) updateRoom(c *fiber.Ctx, id string, v map[string]interface{}) error {
	allowed := map[string]bool{
		"room_number": true, "room_type": true, "floor": true, "capacity": true,
		"price_per_night": true, "status": true, "amenities": true,
	}
	return h.updateAllowed(c, "rooms", id, allowed, v)
}

func (h *CompatHandler) insertGuestStay(c *fiber.Ctx, v map[string]interface{}) error {
	id := uuid.New().String()
	guestName := asStringDefault(v["guest_name"], "Guest")
	const q = `INSERT INTO guest_stays (
	             id, hotel_id, guest_id, room_id, guest_name, guest_email, guest_phone,
	             check_in_date, check_out_date, actual_check_in, actual_check_out,
	             total_amount, notes, created_by, created_at, updated_at
	           ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,now(),now())
	           RETURNING id`
	if _, err := tenantPool(c, h.pool).Exec(c.Context(), q,
		id, h.hotelID(c), nullableString(v["guest_id"]), asString(v["room_id"]), guestName,
		nullableString(v["guest_email"]), nullableString(v["guest_phone"]),
		v["check_in_date"], v["check_out_date"], v["actual_check_in"], v["actual_check_out"],
		nullableFloat(v["total_amount"]), nullableString(v["notes"]), nullableString(v["created_by"]),
	); err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.Created(c, []map[string]interface{}{{"id": id}})
}

func (h *CompatHandler) updateGuestStay(c *fiber.Ctx, id string, v map[string]interface{}) error {
	allowed := map[string]bool{
		"guest_id": true, "room_id": true, "guest_name": true, "guest_email": true,
		"guest_phone": true, "check_in_date": true, "check_out_date": true,
		"actual_check_in": true, "actual_check_out": true, "total_amount": true,
		"notes": true, "created_by": true,
	}
	return h.updateAllowed(c, "guest_stays", id, allowed, v)
}

func (h *CompatHandler) selectComplaints(c *fiber.Ctx, filters []compatFilter) error {
	q := `SELECT c.id, c.complaint_number, c.guest_stay_id, c.guest_id, c.category, c.priority,
		         c.status, c.description, c.resolution, c.resolved_by, c.resolved_at, c.guest_feedback,
		         c.created_by, c.created_at, c.updated_at, gs.guest_name, r.room_number
		  FROM complaints c
		  LEFT JOIN guest_stays gs ON gs.id = c.guest_stay_id
		  LEFT JOIN rooms r ON r.id = gs.room_id`
	rows, err := tenantPool(c, h.pool).Query(c.Context(), q+" WHERE c.hotel_id = $1 ORDER BY c.created_at DESC", h.hotelID(c))
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()
	items := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, number, category, priority, status, description string
		var guestStayID, guestID, resolution, resolvedBy, guestFeedback, createdBy, guestName, roomNumber *string
		var resolvedAt, createdAt, updatedAt interface{}
		if err := rows.Scan(&id, &number, &guestStayID, &guestID, &category, &priority, &status, &description, &resolution, &resolvedBy, &resolvedAt, &guestFeedback, &createdBy, &createdAt, &updatedAt, &guestName, &roomNumber); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		guestStay := interface{}(nil)
		if guestName != nil {
			room := interface{}(nil)
			if roomNumber != nil {
				room = map[string]interface{}{"room_number": *roomNumber}
			}
			guestStay = map[string]interface{}{"guest_name": *guestName, "rooms": room}
		}
		items = append(items, map[string]interface{}{"id": id, "complaint_number": number, "guest_stay_id": guestStayID, "guest_id": guestID, "category": category, "priority": priority, "status": status, "description": description, "resolution": resolution, "resolved_by": resolvedBy, "resolved_at": resolvedAt, "guest_feedback": guestFeedback, "created_by": createdBy, "created_at": createdAt, "updated_at": updatedAt, "guest_stays": guestStay})
	}
	return response.OK(c, items)
}

func (h *CompatHandler) selectMenuCategories(c *fiber.Ctx, filters []compatFilter) error {
	rows, err := tenantPool(c, h.pool).Query(c.Context(), `SELECT id, name, description, display_order, is_active, created_at FROM menu_categories WHERE hotel_id = $1 ORDER BY display_order, name`, h.hotelID(c))
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()
	items := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, name string
		var description *string
		var displayOrder int
		var isActive bool
		var createdAt interface{}
		if err := rows.Scan(&id, &name, &description, &displayOrder, &isActive, &createdAt); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		items = append(items, map[string]interface{}{"id": id, "name": name, "description": description, "display_order": displayOrder, "is_active": isActive, "created_at": createdAt})
	}
	return response.OK(c, items)
}

func (h *CompatHandler) selectMenuItems(c *fiber.Ctx, filters []compatFilter) error {
	rows, err := tenantPool(c, h.pool).Query(c.Context(), `SELECT mi.id, mi.category_id, mi.name, mi.description, mi.price, mi.image_url, mi.is_available, mi.preparation_time, mi.created_at, mi.updated_at, mc.name FROM menu_items mi LEFT JOIN menu_categories mc ON mc.id = mi.category_id WHERE mi.hotel_id = $1 ORDER BY mi.name`, h.hotelID(c))
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()
	items := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, name string
		var categoryID, description, imageURL, categoryName *string
		var price float64
		var isAvailable bool
		var prep int
		var createdAt, updatedAt interface{}
		if err := rows.Scan(&id, &categoryID, &name, &description, &price, &imageURL, &isAvailable, &prep, &createdAt, &updatedAt, &categoryName); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		customizations, err := h.menuCustomizationsFor(c, id)
		if err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		category := interface{}(nil)
		if categoryName != nil {
			category = map[string]interface{}{"name": *categoryName}
		}
		items = append(items, map[string]interface{}{"id": id, "category_id": categoryID, "name": name, "description": description, "price": price, "image_url": imageURL, "is_available": isAvailable, "preparation_time": prep, "created_at": createdAt, "updated_at": updatedAt, "menu_categories": category, "menu_item_customizations": customizations})
	}
	return response.OK(c, items)
}

func (h *CompatHandler) selectMenuCustomizations(c *fiber.Ctx, filters []compatFilter) error {
	menuItemID, _ := stringFilter(filters, "menu_item_id")
	items, err := h.menuCustomizationsFor(c, menuItemID)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	return response.OK(c, items)
}

func (h *CompatHandler) menuCustomizationsFor(c *fiber.Ctx, menuItemID string) ([]map[string]interface{}, error) {
	q := `SELECT id, menu_item_id, name, price, is_available, created_at, updated_at FROM menu_item_customizations`
	where, args := h.scopeHotel(c, "")
	if menuItemID != "" {
		args = append(args, menuItemID)
		where = append(where, fmt.Sprintf("menu_item_id = $%d", len(args)))
	}
	q += " WHERE " + strings.Join(where, " AND ") + " ORDER BY name"
	rows, err := tenantPool(c, h.pool).Query(c.Context(), q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, itemID, name string
		var price float64
		var isAvailable bool
		var createdAt, updatedAt interface{}
		if err := rows.Scan(&id, &itemID, &name, &price, &isAvailable, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		items = append(items, map[string]interface{}{"id": id, "menu_item_id": itemID, "name": name, "price": price, "is_available": isAvailable, "created_at": createdAt, "updated_at": updatedAt})
	}
	return items, rows.Err()
}

func (h *CompatHandler) selectInventoryItems(c *fiber.Ctx, filters []compatFilter) error {
	rows, err := tenantPool(c, h.pool).Query(c.Context(), `SELECT id, name, unit, current_stock, min_stock, cost_per_unit, is_perishable, expiry_date, supplier, created_at, updated_at FROM inventory_items WHERE hotel_id = $1 ORDER BY name`, h.hotelID(c))
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()
	items := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, name, unit string
		var currentStock, minStock float64
		var costPerUnit *float64
		var isPerishable bool
		var expiryDate, createdAt, updatedAt interface{}
		var supplier *string
		if err := rows.Scan(&id, &name, &unit, &currentStock, &minStock, &costPerUnit, &isPerishable, &expiryDate, &supplier, &createdAt, &updatedAt); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		items = append(items, map[string]interface{}{"id": id, "name": name, "unit": unit, "current_stock": currentStock, "min_stock": minStock, "cost_per_unit": costPerUnit, "is_perishable": isPerishable, "expiry_date": expiryDate, "supplier": supplier, "created_at": createdAt, "updated_at": updatedAt})
	}
	return response.OK(c, items)
}

func (h *CompatHandler) selectGuestPreferences(c *fiber.Ctx, filters []compatFilter) error {
	q := `SELECT id, user_id, dietary_restrictions, allergies, favorite_categories, notes, created_at, updated_at FROM guest_preferences`
	where, args := h.scopeHotel(c, "")
	if v, ok := filterValue(filters, "user_id"); ok {
		args = append(args, v)
		where = append(where, fmt.Sprintf("user_id = $%d", len(args)))
	}
	q += " WHERE " + strings.Join(where, " AND ") + " ORDER BY created_at DESC"
	rows, err := tenantPool(c, h.pool).Query(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()
	items := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, userID string
		var dietary, allergies, favorites []string
		var notes *string
		var createdAt, updatedAt interface{}
		if err := rows.Scan(&id, &userID, &dietary, &allergies, &favorites, &notes, &createdAt, &updatedAt); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		country, currency, cleanNotes := splitPreferenceNotes(notes)
		items = append(items, map[string]interface{}{"id": id, "user_id": userID, "dietary_restrictions": dietary, "allergies": allergies, "favorite_categories": favorites, "country": country, "currency": currency, "notes": notes, "created_at": createdAt, "updated_at": updatedAt})
		items[len(items)-1]["notes"] = cleanNotes
	}
	if singleMode(c) != "" {
		if len(items) == 0 {
			return response.OK(c, nil)
		}
		return response.OK(c, items[0])
	}
	return response.OK(c, items)
}

func (h *CompatHandler) insertComplaint(c *fiber.Ctx, v map[string]interface{}) error {
	id := uuid.New().String()
	number := asStringDefault(v["complaint_number"], fmt.Sprintf("C-%s", id[:6]))
	_, err := tenantPool(c, h.pool).Exec(c.Context(), `INSERT INTO complaints (id, hotel_id, complaint_number, guest_stay_id, guest_id, category, priority, status, description, resolution, resolved_by, resolved_at, guest_feedback, created_by, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,now(),now())`, id, h.hotelID(c), number, nullableString(v["guest_stay_id"]), nullableString(v["guest_id"]), asStringDefault(v["category"], "Other"), asStringDefault(v["priority"], "medium"), asStringDefault(v["status"], "open"), asString(v["description"]), nullableString(v["resolution"]), nullableString(v["resolved_by"]), v["resolved_at"], nullableString(v["guest_feedback"]), nullableString(v["created_by"]))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.Created(c, []map[string]interface{}{{"id": id}})
}

func (h *CompatHandler) insertMenuCategory(c *fiber.Ctx, v map[string]interface{}) error {
	name := strings.TrimSpace(asString(v["name"]))
	if name == "" {
		return response.Error(c, fiber.StatusBadRequest, "name is required")
	}
	id := uuid.New().String()
	_, err := tenantPool(c, h.pool).Exec(c.Context(), `INSERT INTO menu_categories (id, hotel_id, name, description, display_order, is_active, created_at) VALUES ($1,$2,$3,$4,$5,$6,now())`, id, h.hotelID(c), name, nullableString(v["description"]), asIntDefault(v["display_order"], 0), asBoolDefault(v["is_active"], true))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.Created(c, []map[string]interface{}{{"id": id}})
}

func (h *CompatHandler) insertMenuItem(c *fiber.Ctx, v map[string]interface{}, single string) error {
	if strings.TrimSpace(asString(v["name"])) == "" {
		return response.Error(c, fiber.StatusBadRequest, "name is required")
	}
	if asFloatDefault(v["price"], 0) < 0 {
		return response.Error(c, fiber.StatusBadRequest, "price cannot be negative")
	}
	id := uuid.New().String()
	rows, err := tenantPool(c, h.pool).Query(c.Context(), `INSERT INTO menu_items (id, hotel_id, category_id, name, description, price, image_url, is_available, preparation_time, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,now(),now()) RETURNING id, category_id, name, description, price, image_url, is_available, preparation_time, created_at, updated_at`, id, h.hotelID(c), nullableString(v["category_id"]), asString(v["name"]), nullableString(v["description"]), asFloatDefault(v["price"], 0), nullableString(v["image_url"]), asBoolDefault(v["is_available"], true), asIntDefault(v["preparation_time"], 15))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	defer rows.Close()
	items, err := scanMenuItemBasicMaps(rows)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	if single != "" && len(items) > 0 {
		return response.Created(c, items[0])
	}
	return response.Created(c, items)
}

func (h *CompatHandler) insertMenuCustomization(c *fiber.Ctx, v map[string]interface{}) error {
	id := uuid.New().String()
	_, err := tenantPool(c, h.pool).Exec(c.Context(), `INSERT INTO menu_item_customizations (id, hotel_id, menu_item_id, name, price, is_available, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$6,now(),now())`, id, h.hotelID(c), asString(v["menu_item_id"]), asString(v["name"]), asFloatDefault(v["price"], 0), asBoolDefault(v["is_available"], true))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.Created(c, []map[string]interface{}{{"id": id}})
}

func (h *CompatHandler) insertMenuCustomizations(c *fiber.Ctx, values []map[string]interface{}) error {
	items := make([]map[string]interface{}, 0, len(values))
	for _, v := range values {
		id := uuid.New().String()
		_, err := tenantPool(c, h.pool).Exec(c.Context(), `INSERT INTO menu_item_customizations (id, hotel_id, menu_item_id, name, price, is_available, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$6,now(),now())`, id, h.hotelID(c), asString(v["menu_item_id"]), asString(v["name"]), asFloatDefault(v["price"], 0), asBoolDefault(v["is_available"], true))
		if err != nil {
			return response.Error(c, fiber.StatusBadRequest, err.Error())
		}
		items = append(items, map[string]interface{}{"id": id})
	}
	return response.Created(c, items)
}

func (h *CompatHandler) insertInventoryItem(c *fiber.Ctx, v map[string]interface{}) error {
	id := uuid.New().String()
	_, err := tenantPool(c, h.pool).Exec(c.Context(), `INSERT INTO inventory_items (id, hotel_id, name, unit, current_stock, min_stock, cost_per_unit, is_perishable, expiry_date, supplier, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,now(),now())`, id, h.hotelID(c), asString(v["name"]), asStringDefault(v["unit"], "unit"), asFloatDefault(v["current_stock"], 0), asFloatDefault(v["min_stock"], 0), nullableFloat(v["cost_per_unit"]), asBoolDefault(v["is_perishable"], false), v["expiry_date"], nullableString(v["supplier"]))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.Created(c, []map[string]interface{}{{"id": id}})
}

func (h *CompatHandler) insertGuestPreferences(c *fiber.Ctx, v map[string]interface{}, single string) error {
	id := uuid.New().String()
	notes := mergePreferenceNotes(nullableString(v["notes"]), asStringDefault(v["country"], "United States"), asStringDefault(v["currency"], "USD"))
	rows, err := tenantPool(c, h.pool).Query(c.Context(), `INSERT INTO guest_preferences (id, hotel_id, user_id, dietary_restrictions, allergies, favorite_categories, notes, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$6,$7,now(),now()) ON CONFLICT (user_id) DO UPDATE SET dietary_restrictions = EXCLUDED.dietary_restrictions, allergies = EXCLUDED.allergies, favorite_categories = EXCLUDED.favorite_categories, notes = EXCLUDED.notes, updated_at = now() RETURNING id, user_id, dietary_restrictions, allergies, favorite_categories, notes, created_at, updated_at`,
		id, h.hotelID(c), asString(v["user_id"]), asStringSlice(v["dietary_restrictions"]), asStringSlice(v["allergies"]), asStringSlice(v["favorite_categories"]), notes)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	defer rows.Close()
	items, err := scanGuestPreferenceMaps(rows)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	if single != "" && len(items) > 0 {
		return response.Created(c, items[0])
	}
	return response.Created(c, items)
}

func (h *CompatHandler) updateComplaint(c *fiber.Ctx, id string, v map[string]interface{}) error {
	return h.updateAllowed(c, "complaints", id, map[string]bool{"guest_stay_id": true, "guest_id": true, "category": true, "priority": true, "status": true, "description": true, "resolution": true, "resolved_by": true, "resolved_at": true, "guest_feedback": true, "created_by": true}, v)
}

func (h *CompatHandler) updateMenuCategory(c *fiber.Ctx, id string, v map[string]interface{}) error {
	return h.updateAllowedWithoutTimestamp(c, "menu_categories", id, map[string]bool{"name": true, "description": true, "display_order": true, "is_active": true}, v)
}

func (h *CompatHandler) updateMenuItem(c *fiber.Ctx, id string, v map[string]interface{}) error {
	return h.updateAllowed(c, "menu_items", id, map[string]bool{"category_id": true, "name": true, "description": true, "price": true, "image_url": true, "is_available": true, "preparation_time": true}, v)
}

func (h *CompatHandler) updateMenuCustomization(c *fiber.Ctx, id string, v map[string]interface{}) error {
	return h.updateAllowed(c, "menu_item_customizations", id, map[string]bool{"menu_item_id": true, "name": true, "price": true, "is_available": true}, v)
}

func (h *CompatHandler) updateInventoryItem(c *fiber.Ctx, id string, v map[string]interface{}) error {
	return h.updateAllowed(c, "inventory_items", id, map[string]bool{"name": true, "unit": true, "current_stock": true, "min_stock": true, "cost_per_unit": true, "is_perishable": true, "expiry_date": true, "supplier": true}, v)
}

func (h *CompatHandler) updateGuestPreferences(c *fiber.Ctx, id string, userID string, v map[string]interface{}) error {
	for _, key := range []string{"dietary_restrictions", "allergies", "favorite_categories"} {
		if _, ok := v[key]; ok {
			v[key] = asStringSlice(v[key])
		}
	}
	if _, hasCountry := v["country"]; hasCountry {
		v["notes"] = mergePreferenceNotes(nullableString(v["notes"]), asStringDefault(v["country"], "United States"), asStringDefault(v["currency"], "USD"))
		delete(v, "country")
		delete(v, "currency")
	}
	allowed := map[string]bool{"dietary_restrictions": true, "allergies": true, "favorite_categories": true, "notes": true}
	if id != "" {
		return h.updateAllowed(c, "guest_preferences", id, allowed, v)
	}
	if userID == "" {
		return response.Error(c, fiber.StatusBadRequest, "id or user_id filter is required")
	}
	return h.updateAllowedByColumn(c, "guest_preferences", "user_id", userID, allowed, v)
}

func (h *CompatHandler) selectOrders(c *fiber.Ctx, filters []compatFilter) error {
	q := `SELECT o.id, o.order_number, o.guest_stay_id, o.room_id, o.guest_id, o.status,
	             o.special_instructions, o.total_amount, o.assigned_waiter_id, o.created_by,
	             o.kitchen_notes, o.pickup_time, o.delivery_time, o.rating, o.feedback,
	             o.created_at, o.updated_at, r.room_number, gs.guest_name
	      FROM orders o
	      LEFT JOIN rooms r ON r.id = o.room_id
	      LEFT JOIN guest_stays gs ON gs.id = o.guest_stay_id`
	where, args := h.scopeHotel(c, "o")
	for _, col := range []string{"guest_id", "room_id", "guest_stay_id", "status"} {
		if v, ok := filterValue(filters, col); ok {
			args = append(args, v)
			where = append(where, fmt.Sprintf("o.%s = $%d", col, len(args)))
		}
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY o.created_at DESC"
	rows, err := tenantPool(c, h.pool).Query(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()
	items := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, orderNumber, status string
		var guestStayID, roomID, guestID, instructions, waiterID, createdBy, kitchenNotes, feedback, roomNumber, guestName *string
		var total float64
		var pickup, delivery, createdAt, updatedAt interface{}
		var rating *int
		if err := rows.Scan(&id, &orderNumber, &guestStayID, &roomID, &guestID, &status, &instructions, &total, &waiterID, &createdBy, &kitchenNotes, &pickup, &delivery, &rating, &feedback, &createdAt, &updatedAt, &roomNumber, &guestName); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		orderItems, err := h.orderItemsFor(c, id)
		if err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		item := map[string]interface{}{
			"id": id, "order_number": orderNumber, "guest_stay_id": guestStayID, "room_id": roomID, "guest_id": guestID,
			"status": status, "special_instructions": instructions, "total_amount": total, "assigned_waiter_id": waiterID,
			"created_by": createdBy, "kitchen_notes": kitchenNotes, "pickup_time": pickup, "delivery_time": delivery,
			"rating": rating, "feedback": feedback, "created_at": createdAt, "updated_at": updatedAt,
			"order_items": orderItems, "rooms": nil, "guest_stays": nil,
		}
		if roomNumber != nil {
			item["rooms"] = map[string]interface{}{"room_number": *roomNumber}
		}
		if guestName != nil {
			item["guest_stays"] = map[string]interface{}{"guest_name": *guestName}
		}
		items = append(items, item)
	}
	return response.OK(c, items)
}

func (h *CompatHandler) selectOrderItems(c *fiber.Ctx, filters []compatFilter) error {
	orderID, _ := stringFilter(filters, "order_id")
	items, err := h.orderItemsFor(c, orderID)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	return response.OK(c, items)
}

func (h *CompatHandler) orderItemsFor(c *fiber.Ctx, orderID string) ([]map[string]interface{}, error) {
	q := `SELECT oi.id, oi.order_id, oi.menu_item_id, oi.quantity, oi.unit_price, oi.notes, oi.created_at, mi.name
	      FROM order_items oi LEFT JOIN menu_items mi ON mi.id = oi.menu_item_id`
	where, args := h.scopeHotel(c, "oi")
	if orderID != "" {
		args = append(args, orderID)
		where = append(where, fmt.Sprintf("oi.order_id = $%d", len(args)))
	}
	q += " WHERE " + strings.Join(where, " AND ") + " ORDER BY oi.created_at"
	rows, err := tenantPool(c, h.pool).Query(c.Context(), q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, oid, menuID string
		var qty int
		var price float64
		var notes, itemName *string
		var createdAt interface{}
		if err := rows.Scan(&id, &oid, &menuID, &qty, &price, &notes, &createdAt, &itemName); err != nil {
			return nil, err
		}
		menu := interface{}(nil)
		if itemName != nil {
			menu = map[string]interface{}{"name": *itemName}
		}
		items = append(items, map[string]interface{}{"id": id, "order_id": oid, "menu_item_id": menuID, "quantity": qty, "unit_price": price, "notes": notes, "created_at": createdAt, "menu_items": menu})
	}
	return items, rows.Err()
}

func (h *CompatHandler) selectPayments(c *fiber.Ctx, filters []compatFilter) error {
	q := `SELECT p.id, p.payment_number, p.guest_stay_id, p.order_id, p.amount, p.payment_method,
	             p.status, p.processed_by, p.notes, p.created_at,
	             o.order_number, gs.guest_name, gs.guest_id, r.room_number
	      FROM payments p
	      LEFT JOIN orders o ON o.id = p.order_id
	      LEFT JOIN guest_stays gs ON gs.id = p.guest_stay_id
	      LEFT JOIN rooms r ON r.id = gs.room_id`
	where, args := h.scopeHotel(c, "p")
	if v, ok := filterValue(filters, "guest_stays.guest_id"); ok {
		args = append(args, v)
		where = append(where, fmt.Sprintf("gs.guest_id = $%d", len(args)))
	}
	if v, ok := filterValue(filters, "guest_stay_id"); ok {
		args = append(args, v)
		where = append(where, fmt.Sprintf("p.guest_stay_id = $%d", len(args)))
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY p.created_at DESC"
	rows, err := tenantPool(c, h.pool).Query(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()
	items := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, number, method, status string
		var stayID, orderID, processedBy, notes, orderNumber, guestName, guestID, roomNumber *string
		var amount float64
		var createdAt interface{}
		if err := rows.Scan(&id, &number, &stayID, &orderID, &amount, &method, &status, &processedBy, &notes, &createdAt, &orderNumber, &guestName, &guestID, &roomNumber); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		order := interface{}(nil)
		if orderNumber != nil {
			order = map[string]interface{}{"order_number": *orderNumber}
		}
		stay := interface{}(nil)
		if guestName != nil {
			room := interface{}(nil)
			if roomNumber != nil {
				room = map[string]interface{}{"room_number": *roomNumber}
			}
			stay = map[string]interface{}{"guest_name": *guestName, "guest_id": guestID, "rooms": room}
		}
		items = append(items, map[string]interface{}{"id": id, "payment_number": number, "guest_stay_id": stayID, "order_id": orderID, "amount": amount, "payment_method": method, "status": status, "processed_by": processedBy, "notes": notes, "created_at": createdAt, "orders": order, "guest_stays": stay})
	}
	return response.OK(c, items)
}

func (h *CompatHandler) selectPaymentSettings(c *fiber.Ctx, filters []compatFilter) error {
	rows, err := tenantPool(c, h.pool).Query(c.Context(), `SELECT id, gateway_name, webhook_url, is_active, created_by, created_at, updated_at FROM payment_settings WHERE hotel_id = $1 ORDER BY gateway_name`, h.hotelID(c))
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()
	items := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, name string
		var webhook, createdBy *string
		var active bool
		var createdAt, updatedAt interface{}
		if err := rows.Scan(&id, &name, &webhook, &active, &createdBy, &createdAt, &updatedAt); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		items = append(items, map[string]interface{}{"id": id, "gateway_name": name, "webhook_url": webhook, "is_active": active, "created_by": createdBy, "created_at": createdAt, "updated_at": updatedAt})
	}
	return response.OK(c, items)
}

func (h *CompatHandler) selectStaffShifts(c *fiber.Ctx, filters []compatFilter) error {
	q := `SELECT ss.id, ss.user_id, ss.clock_in, ss.clock_out, ss.notes, ss.created_at, p.full_name
	      FROM staff_shifts ss LEFT JOIN profiles p ON p.user_id = ss.user_id`
	where, args := h.scopeHotel(c, "ss")
	if v, ok := filterValue(filters, "user_id"); ok {
		args = append(args, v)
		where = append(where, fmt.Sprintf("ss.user_id = $%d", len(args)))
	}
	q += " WHERE " + strings.Join(where, " AND ") + " ORDER BY ss.clock_in DESC"
	rows, err := tenantPool(c, h.pool).Query(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()
	items := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, userID string
		var clockIn, clockOut, createdAt interface{}
		var notes, fullName *string
		if err := rows.Scan(&id, &userID, &clockIn, &clockOut, &notes, &createdAt, &fullName); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		profile := interface{}(nil)
		if fullName != nil {
			profile = map[string]interface{}{"full_name": *fullName}
		}
		items = append(items, map[string]interface{}{"id": id, "user_id": userID, "clock_in": clockIn, "clock_out": clockOut, "notes": notes, "created_at": createdAt, "profiles": profile})
	}
	return response.OK(c, items)
}

func (h *CompatHandler) selectHousekeepingAssignments(c *fiber.Ctx, filters []compatFilter) error {
	q := `SELECT ha.id, ha.hotel_id, ha.room_id, ha.assigned_to, ha.task_type, ha.priority, ha.status,
	             ha.notes, ha.started_at, ha.completed_at, ha.inspected_by, ha.created_at, ha.updated_at,
	             r.room_number, r.room_type, r.floor, p.full_name
	      FROM housekeeping_assignments ha
	      LEFT JOIN rooms r ON r.id = ha.room_id
	      LEFT JOIN profiles p ON p.user_id = ha.assigned_to`
	where, args := h.scopeHotel(c, "ha")
	for _, col := range []string{"room_id", "assigned_to", "status"} {
		if v, ok := filterValue(filters, col); ok {
			args = append(args, v)
			where = append(where, fmt.Sprintf("ha.%s = $%d", col, len(args)))
		}
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY ha.created_at DESC"
	rows, err := tenantPool(c, h.pool).Query(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()
	items := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, hotelID, roomID, taskType, priority, status string
		var assignedTo, notes, inspectedBy, roomNumber, roomType, assignedName *string
		var floor *int
		var startedAt, completedAt, createdAt, updatedAt interface{}
		if err := rows.Scan(&id, &hotelID, &roomID, &assignedTo, &taskType, &priority, &status, &notes, &startedAt, &completedAt, &inspectedBy, &createdAt, &updatedAt, &roomNumber, &roomType, &floor, &assignedName); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		room := interface{}(nil)
		if roomNumber != nil {
			room = map[string]interface{}{"room_number": *roomNumber, "room_type": roomType, "floor": floor}
		}
		profile := interface{}(nil)
		if assignedName != nil {
			profile = map[string]interface{}{"full_name": *assignedName}
		}
		items = append(items, map[string]interface{}{"id": id, "hotel_id": hotelID, "room_id": roomID, "assigned_to": assignedTo, "task_type": taskType, "priority": priority, "status": status, "notes": notes, "started_at": startedAt, "completed_at": completedAt, "inspected_by": inspectedBy, "created_at": createdAt, "updated_at": updatedAt, "rooms": room, "profiles": profile})
	}
	return response.OK(c, items)
}

func (h *CompatHandler) selectWorkOrders(c *fiber.Ctx, filters []compatFilter) error {
	q := `SELECT wo.id, wo.hotel_id, wo.room_id, wo.reported_by, wo.assigned_to, wo.category, wo.priority,
	             wo.status, wo.title, wo.description, wo.resolution_notes, wo.estimated_minutes,
	             wo.actual_minutes, wo.created_at, wo.updated_at, wo.resolved_at,
	             r.room_number, rp.full_name, ap.full_name
	      FROM work_orders wo
	      LEFT JOIN rooms r ON r.id = wo.room_id
	      LEFT JOIN profiles rp ON rp.user_id = wo.reported_by
	      LEFT JOIN profiles ap ON ap.user_id = wo.assigned_to`
	where, args := h.scopeHotel(c, "wo")
	for _, col := range []string{"room_id", "assigned_to", "status", "priority"} {
		if v, ok := filterValue(filters, col); ok {
			args = append(args, v)
			where = append(where, fmt.Sprintf("wo.%s = $%d", col, len(args)))
		}
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY CASE wo.priority WHEN 'urgent' THEN 0 WHEN 'high' THEN 1 WHEN 'normal' THEN 2 ELSE 3 END, wo.created_at DESC"
	rows, err := tenantPool(c, h.pool).Query(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()
	items := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, hotelID, reportedBy, priority, status, title string
		var roomID, assignedTo, category, description, resolutionNotes, roomNumber, reporterName, assigneeName *string
		var estimatedMinutes, actualMinutes *int
		var createdAt, updatedAt, resolvedAt interface{}
		if err := rows.Scan(&id, &hotelID, &roomID, &reportedBy, &assignedTo, &category, &priority, &status, &title, &description, &resolutionNotes, &estimatedMinutes, &actualMinutes, &createdAt, &updatedAt, &resolvedAt, &roomNumber, &reporterName, &assigneeName); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		room := interface{}(nil)
		if roomNumber != nil {
			room = map[string]interface{}{"room_number": *roomNumber}
		}
		items = append(items, map[string]interface{}{"id": id, "hotel_id": hotelID, "room_id": roomID, "reported_by": reportedBy, "assigned_to": assignedTo, "category": category, "priority": priority, "status": status, "title": title, "description": description, "resolution_notes": resolutionNotes, "estimated_minutes": estimatedMinutes, "actual_minutes": actualMinutes, "created_at": createdAt, "updated_at": updatedAt, "resolved_at": resolvedAt, "rooms": room, "reporter": reporterName, "assignee": assigneeName})
	}
	return response.OK(c, items)
}

func (h *CompatHandler) selectFolios(c *fiber.Ctx, filters []compatFilter) error {
	q := `SELECT id, hotel_id, booking_id, guest_id, status, currency, created_at, closed_at FROM folios`
	where, args := h.scopeHotel(c, "")
	for _, col := range []string{"booking_id", "guest_id", "status"} {
		if v, ok := filterValue(filters, col); ok {
			args = append(args, v)
			where = append(where, fmt.Sprintf("%s = $%d", col, len(args)))
		}
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY created_at DESC"
	rows, err := tenantPool(c, h.pool).Query(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()
	items := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, hotelID, bookingID, guestID, status, currency string
		var createdAt, closedAt interface{}
		if err := rows.Scan(&id, &hotelID, &bookingID, &guestID, &status, &currency, &createdAt, &closedAt); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		items = append(items, map[string]interface{}{"id": id, "hotel_id": hotelID, "booking_id": bookingID, "guest_id": guestID, "status": status, "currency": currency, "created_at": createdAt, "closed_at": closedAt})
	}
	return response.OK(c, items)
}

func (h *CompatHandler) selectFolioCharges(c *fiber.Ctx, filters []compatFilter) error {
	q := `SELECT id, folio_id, hotel_id, description, charge_type, amount, tax_amount, reference_id, posted_at, posted_by FROM folio_charges`
	where, args := h.scopeHotel(c, "")
	if v, ok := filterValue(filters, "folio_id"); ok {
		args = append(args, v)
		where = append(where, fmt.Sprintf("folio_id = $%d", len(args)))
	}
	q += " WHERE " + strings.Join(where, " AND ") + " ORDER BY posted_at DESC"
	rows, err := tenantPool(c, h.pool).Query(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()
	items := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, folioID, hotelID, description string
		var chargeType, referenceID, postedBy *string
		var amount, taxAmount float64
		var postedAt interface{}
		if err := rows.Scan(&id, &folioID, &hotelID, &description, &chargeType, &amount, &taxAmount, &referenceID, &postedAt, &postedBy); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		items = append(items, map[string]interface{}{"id": id, "folio_id": folioID, "hotel_id": hotelID, "description": description, "charge_type": chargeType, "amount": amount, "tax_amount": taxAmount, "reference_id": referenceID, "posted_at": postedAt, "posted_by": postedBy})
	}
	return response.OK(c, items)
}

func (h *CompatHandler) selectAuditLogs(c *fiber.Ctx, filters []compatFilter) error {
	rows, err := tenantPool(c, h.pool).Query(c.Context(), `SELECT id, user_id, action, table_name, record_id, old_data, new_data, created_at FROM audit_logs WHERE hotel_id = $1 ORDER BY created_at DESC`, h.hotelID(c))
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()
	items := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, action, tableName string
		var userID, recordID *string
		var oldData, newData map[string]interface{}
		var createdAt interface{}
		if err := rows.Scan(&id, &userID, &action, &tableName, &recordID, &oldData, &newData, &createdAt); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		items = append(items, map[string]interface{}{"id": id, "user_id": userID, "action": action, "table_name": tableName, "record_id": recordID, "old_data": oldData, "new_data": newData, "created_at": createdAt})
	}
	return response.OK(c, items)
}

func (h *CompatHandler) insertOrder(c *fiber.Ctx, v map[string]interface{}, single string) error {
	id := uuid.New().String()
	number := asStringDefault(v["order_number"], fmt.Sprintf("ORD-%s", id[:8]))
	rows, err := tenantPool(c, h.pool).Query(c.Context(), `INSERT INTO orders (id, hotel_id, order_number, guest_stay_id, room_id, guest_id, status, special_instructions, total_amount, assigned_waiter_id, created_by, kitchen_notes, pickup_time, delivery_time, rating, feedback, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,now(),now()) RETURNING id, order_number, guest_stay_id, room_id, guest_id, status, special_instructions, total_amount, assigned_waiter_id, created_by, kitchen_notes, pickup_time, delivery_time, rating, feedback, created_at, updated_at`,
		id, h.hotelID(c), number, nullableString(v["guest_stay_id"]), nullableString(v["room_id"]), nullableString(v["guest_id"]), asStringDefault(v["status"], "pending"), nullableString(v["special_instructions"]), asFloatDefault(v["total_amount"], 0), nullableString(v["assigned_waiter_id"]), nullableString(v["created_by"]), nullableString(v["kitchen_notes"]), v["pickup_time"], v["delivery_time"], nullableInt(v["rating"]), nullableString(v["feedback"]))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	defer rows.Close()
	items, err := scanOrderBasicMaps(rows)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	if single != "" && len(items) > 0 {
		return response.Created(c, items[0])
	}
	return response.Created(c, items)
}

func (h *CompatHandler) insertOrderItems(c *fiber.Ctx, values []map[string]interface{}) error {
	items := make([]map[string]interface{}, 0, len(values))
	for _, v := range values {
		id := uuid.New().String()
		_, err := tenantPool(c, h.pool).Exec(c.Context(), `INSERT INTO order_items (id, hotel_id, order_id, menu_item_id, quantity, unit_price, notes, created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,now())`, id, h.hotelID(c), asString(v["order_id"]), asString(v["menu_item_id"]), asIntDefault(v["quantity"], 1), asFloatDefault(v["unit_price"], 0), nullableString(v["notes"]))
		if err != nil {
			return response.Error(c, fiber.StatusBadRequest, err.Error())
		}
		items = append(items, map[string]interface{}{"id": id})
	}
	return response.Created(c, items)
}

func (h *CompatHandler) insertPayment(c *fiber.Ctx, v map[string]interface{}) error {
	id := uuid.New().String()
	number := asStringDefault(v["payment_number"], fmt.Sprintf("PAY-%s", id[:8]))
	_, err := tenantPool(c, h.pool).Exec(c.Context(), `INSERT INTO payments (id, hotel_id, payment_number, guest_stay_id, order_id, amount, payment_method, status, processed_by, notes, created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,now())`, id, h.hotelID(c), number, nullableString(v["guest_stay_id"]), nullableString(v["order_id"]), asFloatDefault(v["amount"], 0), asStringDefault(v["payment_method"], "cash"), asStringDefault(v["status"], "pending"), nullableString(v["processed_by"]), nullableString(v["notes"]))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.Created(c, []map[string]interface{}{{"id": id}})
}

func (h *CompatHandler) insertPaymentSetting(c *fiber.Ctx, v map[string]interface{}) error {
	id := uuid.New().String()
	_, err := tenantPool(c, h.pool).Exec(c.Context(), `INSERT INTO payment_settings (id, hotel_id, gateway_name, webhook_url, is_active, created_by, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$6,now(),now()) ON CONFLICT (gateway_name) DO UPDATE SET webhook_url = EXCLUDED.webhook_url, is_active = EXCLUDED.is_active, updated_at = now()`, id, h.hotelID(c), asString(v["gateway_name"]), nullableString(v["webhook_url"]), asBoolDefault(v["is_active"], true), nullableString(v["created_by"]))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.Created(c, []map[string]interface{}{{"id": id}})
}

func (h *CompatHandler) insertStaffShift(c *fiber.Ctx, v map[string]interface{}) error {
	id := uuid.New().String()
	clockIn := v["clock_in"]
	if clockIn == nil {
		clockIn = "now()"
	}
	_, err := tenantPool(c, h.pool).Exec(c.Context(), `INSERT INTO staff_shifts (id, hotel_id, user_id, clock_in, clock_out, notes, created_at) VALUES ($1,$2,$3,$4,$5,$6,now())`, id, h.hotelID(c), asString(v["user_id"]), clockIn, v["clock_out"], nullableString(v["notes"]))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.Created(c, []map[string]interface{}{{"id": id}})
}

func (h *CompatHandler) insertHousekeepingAssignment(c *fiber.Ctx, v map[string]interface{}) error {
	id := uuid.New().String()
	_, err := tenantPool(c, h.pool).Exec(c.Context(), `INSERT INTO housekeeping_assignments (id, hotel_id, room_id, assigned_to, task_type, priority, status, notes, started_at, completed_at, inspected_by, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,now(),now())`,
		id, h.hotelID(c), asString(v["room_id"]), nullableString(v["assigned_to"]), asStringDefault(v["task_type"], "guest_request"), asStringDefault(v["priority"], "normal"), asStringDefault(v["status"], "pending"), nullableString(v["notes"]), v["started_at"], v["completed_at"], nullableString(v["inspected_by"]))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.Created(c, []map[string]interface{}{{"id": id}})
}

func (h *CompatHandler) insertWorkOrder(c *fiber.Ctx, v map[string]interface{}) error {
	id := uuid.New().String()
	_, err := tenantPool(c, h.pool).Exec(c.Context(), `INSERT INTO work_orders (id, hotel_id, room_id, reported_by, assigned_to, category, priority, status, title, description, resolution_notes, estimated_minutes, actual_minutes, created_at, updated_at, resolved_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,now(),now(),$14)`,
		id, h.hotelID(c), nullableString(v["room_id"]), asString(v["reported_by"]), nullableString(v["assigned_to"]), nullableString(v["category"]), asStringDefault(v["priority"], "normal"), asStringDefault(v["status"], "open"), asString(v["title"]), nullableString(v["description"]), nullableString(v["resolution_notes"]), nullableInt(v["estimated_minutes"]), nullableInt(v["actual_minutes"]), v["resolved_at"])
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.Created(c, []map[string]interface{}{{"id": id}})
}

func (h *CompatHandler) updateOrder(c *fiber.Ctx, id string, v map[string]interface{}) error {
	return h.updateAllowed(c, "orders", id, map[string]bool{"guest_stay_id": true, "room_id": true, "guest_id": true, "status": true, "special_instructions": true, "total_amount": true, "assigned_waiter_id": true, "created_by": true, "kitchen_notes": true, "pickup_time": true, "delivery_time": true, "rating": true, "feedback": true}, v)
}

func (h *CompatHandler) updatePayment(c *fiber.Ctx, id string, v map[string]interface{}) error {
	return h.updateAllowedWithoutTimestamp(c, "payments", id, map[string]bool{"guest_stay_id": true, "order_id": true, "amount": true, "payment_method": true, "status": true, "processed_by": true, "notes": true}, v)
}

func (h *CompatHandler) updatePaymentSetting(c *fiber.Ctx, id string, v map[string]interface{}) error {
	return h.updateAllowed(c, "payment_settings", id, map[string]bool{"gateway_name": true, "webhook_url": true, "is_active": true, "created_by": true}, v)
}

func (h *CompatHandler) updateStaffShift(c *fiber.Ctx, id string, v map[string]interface{}) error {
	return h.updateAllowedWithoutTimestamp(c, "staff_shifts", id, map[string]bool{"user_id": true, "clock_in": true, "clock_out": true, "notes": true}, v)
}

func (h *CompatHandler) updateHousekeepingAssignment(c *fiber.Ctx, id string, v map[string]interface{}) error {
	return h.updateAllowed(c, "housekeeping_assignments", id, map[string]bool{"room_id": true, "assigned_to": true, "task_type": true, "priority": true, "status": true, "notes": true, "started_at": true, "completed_at": true, "inspected_by": true}, v)
}

func (h *CompatHandler) updateWorkOrder(c *fiber.Ctx, id string, v map[string]interface{}) error {
	return h.updateAllowed(c, "work_orders", id, map[string]bool{"room_id": true, "reported_by": true, "assigned_to": true, "category": true, "priority": true, "status": true, "title": true, "description": true, "resolution_notes": true, "estimated_minutes": true, "actual_minutes": true, "resolved_at": true}, v)
}

func (h *CompatHandler) deleteByIDOrColumn(c *fiber.Ctx, table, id string, filters []compatFilter, column string) error {
	// Tenant-scoped tables must always constrain the delete to the caller's
	// hotel so a guessed id (or foreign-key value) cannot reach another tenant's
	// rows. Every other compat delete path already does this; these two
	// (order_items, menu_item_customizations) previously did not.
	scoped := compatTenantScopedTables[table]
	if id != "" {
		q := fmt.Sprintf(`DELETE FROM %s WHERE id = $1`, table)
		args := []interface{}{id}
		if scoped {
			args = append(args, h.hotelID(c))
			q += " AND hotel_id = $2"
		}
		if _, err := tenantPool(c, h.pool).Exec(c.Context(), q, args...); err != nil {
			return response.Error(c, fiber.StatusBadRequest, err.Error())
		}
		return response.OK(c, []map[string]interface{}{})
	}
	value, ok := stringFilter(filters, column)
	if !ok || value == "" {
		return response.Error(c, fiber.StatusBadRequest, "id filter is required")
	}
	q := fmt.Sprintf(`DELETE FROM %s WHERE %s = $1`, table, column)
	args := []interface{}{value}
	if scoped {
		args = append(args, h.hotelID(c))
		q += " AND hotel_id = $2"
	}
	if _, err := tenantPool(c, h.pool).Exec(c.Context(), q, args...); err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.OK(c, []map[string]interface{}{})
}

func scanGuestPreferenceMaps(rows interface {
	Next() bool
	Scan(...interface{}) error
	Err() error
}) ([]map[string]interface{}, error) {
	items := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, userID string
		var dietary, allergies, favorites []string
		var notes *string
		var createdAt, updatedAt interface{}
		if err := rows.Scan(&id, &userID, &dietary, &allergies, &favorites, &notes, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		country, currency, cleanNotes := splitPreferenceNotes(notes)
		items = append(items, map[string]interface{}{"id": id, "user_id": userID, "dietary_restrictions": dietary, "allergies": allergies, "favorite_categories": favorites, "country": country, "currency": currency, "notes": notes, "created_at": createdAt, "updated_at": updatedAt})
		items[len(items)-1]["notes"] = cleanNotes
	}
	return items, rows.Err()
}

func scanMenuItemBasicMaps(rows interface {
	Next() bool
	Scan(...interface{}) error
	Err() error
}) ([]map[string]interface{}, error) {
	items := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, name string
		var categoryID, description, imageURL *string
		var price float64
		var isAvailable bool
		var prep int
		var createdAt, updatedAt interface{}
		if err := rows.Scan(&id, &categoryID, &name, &description, &price, &imageURL, &isAvailable, &prep, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		items = append(items, map[string]interface{}{"id": id, "category_id": categoryID, "name": name, "description": description, "price": price, "image_url": imageURL, "is_available": isAvailable, "preparation_time": prep, "created_at": createdAt, "updated_at": updatedAt})
	}
	return items, rows.Err()
}

func scanOrderBasicMaps(rows interface {
	Next() bool
	Scan(...interface{}) error
	Err() error
}) ([]map[string]interface{}, error) {
	items := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, number, status string
		var guestStayID, roomID, guestID, instructions, waiterID, createdBy, kitchenNotes, feedback *string
		var total float64
		var pickup, delivery, createdAt, updatedAt interface{}
		var rating *int
		if err := rows.Scan(&id, &number, &guestStayID, &roomID, &guestID, &status, &instructions, &total, &waiterID, &createdBy, &kitchenNotes, &pickup, &delivery, &rating, &feedback, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		items = append(items, map[string]interface{}{
			"id": id, "order_number": number, "guest_stay_id": guestStayID, "room_id": roomID, "guest_id": guestID,
			"status": status, "special_instructions": instructions, "total_amount": total, "assigned_waiter_id": waiterID,
			"created_by": createdBy, "kitchen_notes": kitchenNotes, "pickup_time": pickup, "delivery_time": delivery,
			"rating": rating, "feedback": feedback, "created_at": createdAt, "updated_at": updatedAt,
		})
	}
	return items, rows.Err()
}

func (h *CompatHandler) updateAllowed(c *fiber.Ctx, table, id string, allowed map[string]bool, v map[string]interface{}) error {
	return h.updateAllowedWithTimestamp(c, table, id, allowed, v, true)
}

func (h *CompatHandler) updateAllowedByColumn(c *fiber.Ctx, table, column, value string, allowed map[string]bool, v map[string]interface{}) error {
	return h.updateAllowedByColumnWithTimestamp(c, table, column, value, allowed, v, true)
}

func (h *CompatHandler) updateAllowedWithoutTimestamp(c *fiber.Ctx, table, id string, allowed map[string]bool, v map[string]interface{}) error {
	return h.updateAllowedWithTimestamp(c, table, id, allowed, v, false)
}

func (h *CompatHandler) updateAllowedWithTimestamp(c *fiber.Ctx, table, id string, allowed map[string]bool, v map[string]interface{}, hasUpdatedAt bool) error {
	return h.updateAllowedByColumnWithTimestamp(c, table, "id", id, allowed, v, hasUpdatedAt)
}

func (h *CompatHandler) updateAllowedByColumnWithTimestamp(c *fiber.Ctx, table, column, value string, allowed map[string]bool, v map[string]interface{}, hasUpdatedAt bool) error {
	set := []string{}
	args := []interface{}{}
	for key, value := range v {
		if !allowed[key] {
			continue
		}
		args = append(args, value)
		set = append(set, fmt.Sprintf("%s = $%d", key, len(args)))
	}
	if len(set) == 0 {
		return response.OK(c, []map[string]interface{}{})
	}
	args = append(args, value)
	setSQL := strings.Join(set, ", ")
	if hasUpdatedAt {
		setSQL += ", updated_at = now()"
	}
	q := fmt.Sprintf("UPDATE %s SET %s WHERE %s = $%d", table, setSQL, column, len(args))
	if compatTenantScopedTables[table] {
		args = append(args, h.hotelID(c))
		q += fmt.Sprintf(" AND hotel_id = $%d", len(args))
	}
	if _, err := tenantPool(c, h.pool).Exec(c.Context(), q, args...); err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.OK(c, []map[string]interface{}{})
}

func scanRoomMaps(rows interface {
	Next() bool
	Scan(...interface{}) error
	Err() error
}) ([]map[string]interface{}, error) {
	items := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, roomNumber, roomType, status string
		var floor, capacity int
		var price float64
		var amenities []string
		var createdAt, updatedAt interface{}
		if err := rows.Scan(&id, &roomNumber, &roomType, &floor, &capacity, &price, &status, &amenities, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		items = append(items, map[string]interface{}{
			"id":              id,
			"room_number":     roomNumber,
			"room_type":       roomType,
			"floor":           floor,
			"capacity":        capacity,
			"price_per_night": price,
			"status":          status,
			"amenities":       amenities,
			"created_at":      createdAt,
			"updated_at":      updatedAt,
		})
	}
	return items, rows.Err()
}

func scanProfileMaps(rows interface {
	Next() bool
	Scan(...interface{}) error
	Err() error
}) ([]map[string]interface{}, error) {
	items := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, userID, fullName string
		var phone, avatarURL *string
		var createdAt, updatedAt interface{}
		if err := rows.Scan(&id, &userID, &fullName, &phone, &avatarURL, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		items = append(items, map[string]interface{}{
			"id":         id,
			"user_id":    userID,
			"full_name":  fullName,
			"phone":      phone,
			"avatar_url": avatarURL,
			"created_at": createdAt,
			"updated_at": updatedAt,
		})
	}
	return items, rows.Err()
}

func asString(v interface{}) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func asStringDefault(v interface{}, fallback string) string {
	if s := asString(v); s != "" {
		return s
	}
	return fallback
}

func nullableString(v interface{}) interface{} {
	if s := asString(v); s != "" {
		return s
	}
	return nil
}

var preferenceMetaPattern = regexp.MustCompile(`(?s)^__currency_preferences__=(\{.*?\})\n?`)

type preferenceMeta struct {
	Country  string `json:"country"`
	Currency string `json:"currency"`
}

func splitPreferenceNotes(notes *string) (string, string, *string) {
	country := "United States"
	currency := "USD"
	if notes == nil || *notes == "" {
		return country, currency, notes
	}
	text := *notes
	matches := preferenceMetaPattern.FindStringSubmatch(text)
	if len(matches) == 2 {
		var meta preferenceMeta
		if json.Unmarshal([]byte(matches[1]), &meta) == nil {
			if strings.TrimSpace(meta.Country) != "" {
				country = meta.Country
			}
			if strings.TrimSpace(meta.Currency) != "" {
				currency = meta.Currency
			}
		}
		text = preferenceMetaPattern.ReplaceAllString(text, "")
	}
	text = strings.TrimLeft(text, "\r\n")
	if text == "" {
		return country, currency, nil
	}
	return country, currency, &text
}

func mergePreferenceNotes(rawNotes interface{}, country, currency string) interface{} {
	var clean *string
	switch notes := rawNotes.(type) {
	case *string:
		_, _, clean = splitPreferenceNotes(notes)
	case string:
		_, _, clean = splitPreferenceNotes(&notes)
	}
	meta, _ := json.Marshal(preferenceMeta{Country: country, Currency: currency})
	if clean == nil || *clean == "" {
		return fmt.Sprintf("__currency_preferences__=%s", string(meta))
	}
	return fmt.Sprintf("__currency_preferences__=%s\n%s", string(meta), *clean)
}

func asIntDefault(v interface{}, fallback int) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		if parsed, err := strconv.Atoi(n); err == nil {
			return parsed
		}
	}
	return fallback
}

func asFloatDefault(v interface{}, fallback float64) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case string:
		if parsed, err := strconv.ParseFloat(n, 64); err == nil {
			return parsed
		}
	}
	return fallback
}

func asBoolDefault(v interface{}, fallback bool) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		if parsed, err := strconv.ParseBool(b); err == nil {
			return parsed
		}
	}
	return fallback
}

func nullableFloat(v interface{}) interface{} {
	if v == nil {
		return nil
	}
	return asFloatDefault(v, 0)
}

func nullableInt(v interface{}) interface{} {
	if v == nil {
		return nil
	}
	return asIntDefault(v, 0)
}

func asStringSlice(v interface{}) []string {
	raw, ok := v.([]interface{})
	if !ok {
		if existing, ok := v.([]string); ok {
			return existing
		}
		return []string{}
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
