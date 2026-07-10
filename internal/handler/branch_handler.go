package handler

import (
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

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

// listBranchesFor returns every branch (property) belonging to a client.
func (h *OperationsHandler) listBranchesFor(c *fiber.Ctx, hotelID uuid.UUID) error {
	rows, err := h.pool.Query(c.Context(), `
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
func (h *OperationsHandler) createBranchFor(c *fiber.Ctx, hotelID uuid.UUID) error {
	var req createBranchRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return response.Error(c, fiber.StatusUnprocessableEntity, "branch name is required")
	}

	// Enforce plan max_properties.
	var plan string
	var existing int
	if err := h.pool.QueryRow(c.Context(),
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

	if _, err := h.pool.Exec(c.Context(), `
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

func (h *OperationsHandler) ListBranches(c *fiber.Ctx) error {
	if !requireAuthenticatedRequest(c, h.secretKey) {
		return nil
	}
	return h.listBranchesFor(c, h.currentHotelID(c))
}

func (h *OperationsHandler) CreateBranch(c *fiber.Ctx) error {
	if !requireAnyRoleFromToken(c, h.secretKey, "admin", "hotel_admin", "super_admin", "platform_admin") {
		return nil
	}
	return h.createBranchFor(c, h.currentHotelID(c))
}

// BranchRooms (GET /api/branches/:id/rooms) is the reference branch-scoped read:
// it returns rooms attributed to one branch within the caller's client, proving
// the property_id scoping end-to-end. The predicate is hotel_id + property_id so
// a branch id from another client matches no rows (authorization by data). The
// branch id comes from the path here; other handlers use the X-Branch-Id header
// via branchID(c). Rooms with a NULL property_id (not yet assigned to a branch)
// are intentionally excluded from a specific-branch view.
func (h *OperationsHandler) BranchRooms(c *fiber.Ctx) error {
	if !requireAuthenticatedRequest(c, h.secretKey) {
		return nil
	}
	branch, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid branch id")
	}
	hotelID := h.currentHotelID(c)

	rows, err := h.pool.Query(c.Context(), `
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
	return h.listBranchesFor(c, id)
}

func (h *OperationsHandler) PlatformCreateBranch(c *fiber.Ctx) error {
	if !h.requirePlatformAdmin(c) {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid tenant id")
	}
	return h.createBranchFor(c, id)
}
