package handler

import (
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hotelharmony/api/pkg/response"
)

// Branch (property) management — the "Add Branch to Client" capability from the
// admin control plane in the architecture diagram. A branch is a properties row
// inside a client (hotel). Adding one is cheap: it reuses the client's database,
// cache namespace and plan; no new container or DB is provisioned.
//
// Two surfaces are exposed:
//   - Tenant self-service:  GET/POST /api/branches            (hotel admins)
//   - Platform admin:       GET/POST /api/platform/tenants/:id/branches
//
// Branch creation is gated by the client's plan max_properties limit so a Basic
// client (1 property) cannot silently grow into a multi-branch group.

type createBranchRequest struct {
	Name       string `json:"name"`
	Code       string `json:"code"`
	Address    string `json:"address"`
	Phone      string `json:"phone"`
	Email      string `json:"email"`
	Timezone   string `json:"timezone"`
	Currency   string `json:"currency"`
	StarRating *int   `json:"star_rating"`
	TotalRooms *int   `json:"total_rooms"`
}

// poolForHotel resolves the DB a given hotel's operational rows live in: the
// tenant's dedicated pool when it is in dedicated isolation, else the shared pool.
// Branches (properties) are operational data, so they must be read/written on this
// pool — using the shared pool unconditionally lost a dedicated tenant's branches.
func (h *OperationsHandler) poolForHotel(c *fiber.Ctx, hotelID uuid.UUID) *pgxpool.Pool {
	if h.tenants != nil {
		return h.tenants.PoolForHotel(c.Context(), hotelID)
	}
	return h.pool
}

// listBranchesFor returns every branch (property) belonging to a client.
func (h *OperationsHandler) listBranchesFor(c *fiber.Ctx, pool *pgxpool.Pool, hotelID uuid.UUID) error {
	rows, err := pool.Query(c.Context(), `
		SELECT id, name, COALESCE(code,''), COALESCE(address,''), COALESCE(phone,''),
		       COALESCE(email,''), COALESCE(timezone,'UTC'), COALESCE(currency,'USD'),
		       COALESCE(is_active,true), COALESCE(is_primary,false),
		       star_rating, total_rooms, created_at
		FROM properties
		WHERE hotel_id = $1
		ORDER BY is_primary DESC, name ASC`, hotelID)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()

	branches := []map[string]interface{}{}
	for rows.Next() {
		var id uuid.UUID
		var name, code, address, phone, email, tz, currency string
		var isActive, isPrimary bool
		var star, totalRooms *int
		var createdAt time.Time
		if err := rows.Scan(&id, &name, &code, &address, &phone, &email, &tz, &currency,
			&isActive, &isPrimary, &star, &totalRooms, &createdAt); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		branches = append(branches, map[string]interface{}{
			"id": id, "name": name, "code": nullableText(code), "address": nullableText(address),
			"phone": nullableText(phone), "email": nullableText(email), "timezone": tz,
			"currency": currency, "is_active": isActive, "is_primary": isPrimary,
			"star_rating": star, "total_rooms": totalRooms, "created_at": createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	return response.OK(c, branches)
}

// createBranchFor inserts a branch for a client, enforcing the plan's
// max_properties ceiling. The first branch of a client is marked primary.
func (h *OperationsHandler) createBranchFor(c *fiber.Ctx, pool *pgxpool.Pool, hotelID uuid.UUID) error {
	var req createBranchRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return response.Error(c, fiber.StatusUnprocessableEntity, "branch name is required")
	}

	// Enforce plan max_properties. Both the hotel row and the properties count are
	// read on the tenant's own pool (the hotel row is seeded into a dedicated DB at
	// provision), so the count reflects the branches that actually exist there.
	var plan string
	var existing int
	if err := pool.QueryRow(c.Context(),
		`SELECT h.plan_tier, (SELECT COUNT(*) FROM properties p WHERE p.hotel_id = h.id)
		 FROM hotels h WHERE h.id = $1`, hotelID).Scan(&plan, &existing); err != nil {
		return response.Error(c, fiber.StatusNotFound, "client not found")
	}
	if max := planTierByID(plan).MaxProperties; max != nil && existing >= *max {
		return response.Error(c, fiber.StatusForbidden,
			"branch limit reached for your plan; please upgrade to add more branches")
	}

	tz := strings.TrimSpace(req.Timezone)
	if tz == "" {
		tz = "UTC"
	}
	currency := strings.ToUpper(strings.TrimSpace(req.Currency))
	if currency == "" {
		currency = "USD"
	}
	isPrimary := existing == 0
	branchID := uuid.New()

	if _, err := pool.Exec(c.Context(), `
		INSERT INTO properties (
			id, hotel_id, name, code, address, phone, email, timezone, currency,
			star_rating, total_rooms, is_active, is_primary, created_at, updated_at
		) VALUES ($1,$2,$3,NULLIF($4,''),NULLIF($5,''),NULLIF($6,''),NULLIF($7,''),$8,$9,$10,$11,true,$12,now(),now())`,
		branchID, hotelID, name, strings.TrimSpace(req.Code), strings.TrimSpace(req.Address),
		strings.TrimSpace(req.Phone), strings.TrimSpace(req.Email), tz, currency,
		req.StarRating, req.TotalRooms, isPrimary,
	); err != nil {
		return response.Error(c, fiber.StatusConflict, err.Error())
	}

	return response.Created(c, map[string]interface{}{
		"id": branchID, "hotel_id": hotelID, "name": name,
		"code": nullableText(strings.TrimSpace(req.Code)), "timezone": tz,
		"currency": currency, "is_primary": isPrimary, "is_active": true,
		"created_at": time.Now().UTC(),
	})
}

// --- Tenant self-service handlers (run behind authGate + plan gate) ---

// hotelAdminRoles is the set of roles that count as "the hotel admin" across
// tenants (the same admin role is named inconsistently: hotel_admin / admin /
// super_admin). The Properties/branches feature is restricted to these — regular
// staff (receptionist, cashier, housekeeping, waiter, food_manager) and the
// platform operator (platform_admin, who uses the Platform* variants) are excluded.
var hotelAdminRoles = []string{"hotel_admin", "admin", "super_admin"}

func (h *OperationsHandler) ListBranches(c *fiber.Ctx) error {
	if !requireAnyRoleFromToken(c, h.secretKey, hotelAdminRoles...) {
		return nil
	}
	return h.listBranchesFor(c, tenantPool(c, h.pool), h.currentHotelID(c))
}

func (h *OperationsHandler) CreateBranch(c *fiber.Ctx) error {
	if !requireAnyRoleFromToken(c, h.secretKey, hotelAdminRoles...) {
		return nil
	}
	return h.createBranchFor(c, tenantPool(c, h.pool), h.currentHotelID(c))
}

// UpdateBranch (PATCH /api/branches/:id) edits a branch's fields. Hotel-admin only.
func (h *OperationsHandler) UpdateBranch(c *fiber.Ctx) error {
	if !requireAnyRoleFromToken(c, h.secretKey, hotelAdminRoles...) {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid branch id")
	}
	var req struct {
		Name       *string `json:"name"`
		Code       *string `json:"code"`
		Address    *string `json:"address"`
		Phone      *string `json:"phone"`
		Email      *string `json:"email"`
		Timezone   *string `json:"timezone"`
		Currency   *string `json:"currency"`
		StarRating *int    `json:"star_rating"`
		TotalRooms *int    `json:"total_rooms"`
		IsActive   *bool   `json:"is_active"`
	}
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	// Safe []string + strings.Join SET builder (no trailing-comma bug).
	sets := []string{}
	args := []interface{}{}
	idx := 1
	add := func(col string, v interface{}) {
		sets = append(sets, col+" = $"+strconv.Itoa(idx))
		args = append(args, v)
		idx++
	}
	if req.Name != nil {
		if strings.TrimSpace(*req.Name) == "" {
			return response.Error(c, fiber.StatusUnprocessableEntity, "branch name cannot be empty")
		}
		add("name", strings.TrimSpace(*req.Name))
	}
	if req.Code != nil {
		add("code", nullableText(*req.Code))
	}
	if req.Address != nil {
		add("address", nullableText(*req.Address))
	}
	if req.Phone != nil {
		add("phone", nullableText(*req.Phone))
	}
	if req.Email != nil {
		add("email", nullableText(*req.Email))
	}
	if req.Timezone != nil {
		add("timezone", strings.TrimSpace(*req.Timezone))
	}
	if req.Currency != nil {
		add("currency", strings.ToUpper(strings.TrimSpace(*req.Currency)))
	}
	if req.StarRating != nil {
		add("star_rating", *req.StarRating)
	}
	if req.TotalRooms != nil {
		add("total_rooms", *req.TotalRooms)
	}
	if req.IsActive != nil {
		add("is_active", *req.IsActive)
	}
	if len(sets) == 0 {
		return response.Error(c, fiber.StatusBadRequest, "no fields to update")
	}
	sets = append(sets, "updated_at = now()")
	args = append(args, id, h.currentHotelID(c))
	q := "UPDATE properties SET " + strings.Join(sets, ", ") +
		" WHERE id = $" + strconv.Itoa(idx) + " AND hotel_id = $" + strconv.Itoa(idx+1)
	tag, err := tenantPool(c, h.pool).Exec(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	if tag.RowsAffected() == 0 {
		return response.Error(c, fiber.StatusNotFound, "branch not found")
	}
	return response.OK(c, map[string]interface{}{"id": id})
}

// DeleteBranch (DELETE /api/branches/:id) removes a branch. Hotel-admin only.
// Rooms attached to it have their property_id set to NULL (schema FK). If the
// deleted branch was the primary and other branches remain, the oldest remaining
// one is promoted to primary so a client always has exactly one primary.
func (h *OperationsHandler) DeleteBranch(c *fiber.Ctx) error {
	if !requireAnyRoleFromToken(c, h.secretKey, hotelAdminRoles...) {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid branch id")
	}
	hotelID := h.currentHotelID(c)
	pool := tenantPool(c, h.pool)

	tx, err := pool.Begin(c.Context())
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to start transaction")
	}
	defer tx.Rollback(c.Context())

	var wasPrimary bool
	if err := tx.QueryRow(c.Context(),
		`DELETE FROM properties WHERE id = $1 AND hotel_id = $2 RETURNING COALESCE(is_primary,false)`,
		id, hotelID).Scan(&wasPrimary); err != nil {
		return response.Error(c, fiber.StatusNotFound, "branch not found")
	}
	if wasPrimary {
		// Promote the oldest remaining branch to primary, if any remain.
		if _, err := tx.Exec(c.Context(), `
			UPDATE properties SET is_primary = true, updated_at = now()
			WHERE hotel_id = $1
			  AND id = (SELECT id FROM properties WHERE hotel_id = $1 ORDER BY created_at ASC LIMIT 1)`,
			hotelID); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
	}
	if err := tx.Commit(c.Context()); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to delete branch")
	}
	return response.OK(c, map[string]string{"status": "deleted"})
}

// BranchRooms (GET /api/branches/:id/rooms) is the reference branch-scoped read:
// it returns rooms attributed to one branch within the caller's client, proving
// the property_id scoping end-to-end. The predicate is hotel_id + property_id so
// a branch id from another client matches no rows (authorization by data). The
// branch id comes from the path here; other handlers use the X-Branch-Id header
// via branchID(c). Rooms with a NULL property_id (not yet assigned to a branch)
// are intentionally excluded from a specific-branch view.
func (h *OperationsHandler) BranchRooms(c *fiber.Ctx) error {
	if !requireAnyRoleFromToken(c, h.secretKey, hotelAdminRoles...) {
		return nil
	}
	branch, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid branch id")
	}
	hotelID := h.currentHotelID(c)

	rows, err := tenantPool(c, h.pool).Query(c.Context(), `
		SELECT id, room_number, status, COALESCE(floor, 0)
		FROM rooms
		WHERE hotel_id = $1 AND property_id = $2
		ORDER BY room_number ASC`, hotelID, branch)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()

	out := []map[string]interface{}{}
	for rows.Next() {
		var id uuid.UUID
		var number, status string
		var floor int
		if err := rows.Scan(&id, &number, &status, &floor); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		out = append(out, map[string]interface{}{
			"id": id, "room_number": number, "status": status, "floor": floor,
			"branch_id": branch,
		})
	}
	if err := rows.Err(); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	return response.OK(c, out)
}

// --- Platform admin handlers (manage any client's branches) ---

func (h *OperationsHandler) PlatformListBranches(c *fiber.Ctx) error {
	if !h.requirePlatformAdmin(c) {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid tenant id")
	}
	return h.listBranchesFor(c, h.poolForHotel(c, id), id)
}

func (h *OperationsHandler) PlatformCreateBranch(c *fiber.Ctx) error {
	if !h.requirePlatformAdmin(c) {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid tenant id")
	}
	return h.createBranchFor(c, h.poolForHotel(c, id), id)
}
