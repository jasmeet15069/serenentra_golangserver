package handler

import (
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hotelharmony/api/pkg/response"
)

// Tables:
//   CREATE TABLE IF NOT EXISTS loyalty_tiers (
//     id uuid PK, hotel_id uuid, name text, min_points int,
//     multiplier numeric, benefits jsonb, created_at timestamptz
//   );
//   CREATE TABLE IF NOT EXISTS loyalty_members (
//     id uuid PK, hotel_id uuid, guest_id uuid, tier_id uuid,
//     points int, lifetime_points int, enrolled_at timestamptz
//   );
//   CREATE TABLE IF NOT EXISTS loyalty_transactions (
//     id uuid PK, member_id uuid, type text, points int,
//     reference text, description text, created_at timestamptz
//   );

type CRMHandler struct {
	baseHandler
	pool *pgxpool.Pool
}

func NewCRMHandler(pool *pgxpool.Pool, secret string) *CRMHandler {
	return &CRMHandler{baseHandler: newBase(secret), pool: pool}
}

func (h *CRMHandler) Register(r fiber.Router) {
	g := r.Group("", authGate(h.secret))
	g.Get("/crm/guests", h.ListGuests)
	g.Post("/crm/guests", h.CreateGuest)
	g.Get("/crm/guests/:id", h.GetGuest)
	g.Patch("/crm/guests/:id", h.UpdateGuest)
	g.Get("/crm/loyalty/tiers", h.ListLoyaltyTiers)
	g.Post("/crm/loyalty/tiers", h.CreateLoyaltyTier)
	g.Put("/crm/loyalty/tiers/:id", h.UpdateLoyaltyTier)
	g.Patch("/crm/loyalty/tiers/:id", h.UpdateLoyaltyTier)
	g.Get("/crm/loyalty/members", h.ListLoyaltyMembers)
	g.Post("/crm/loyalty/points/award", h.AwardPoints)
	g.Post("/crm/loyalty/points/redeem", h.RedeemPoints)
	g.Get("/crm/campaigns", h.ListCampaigns)
	g.Post("/crm/campaigns", h.CreateCampaign)
	g.Patch("/crm/campaigns/:id", h.UpdateCampaign)
	g.Delete("/crm/campaigns/:id", h.DeleteCampaign)
}

// ---------------------------------------------------------------------------
// Guest Profiles
// ---------------------------------------------------------------------------

type createGuestRequest struct {
	FullName string `json:"full_name"`
	Email    string `json:"email,omitempty"`
	Phone    string `json:"phone,omitempty"`
}

type updateGuestRequest struct {
	Preferences string `json:"preferences,omitempty"`
	Notes       string `json:"notes,omitempty"`
	VipStatus   string `json:"vip_status,omitempty"`
}

type guestSummaryResponse struct {
	ID         uuid.UUID `json:"id"`
	FullName   string    `json:"full_name"`
	Email      *string   `json:"email"`
	Phone      *string   `json:"phone"`
	VipStatus  string    `json:"vip_status"`
	TotalStays int       `json:"total_stays"`
	LoyaltyPts int       `json:"loyalty_points"`
}

type guestDetailResponse struct {
	ID          uuid.UUID       `json:"id"`
	FullName    string          `json:"full_name"`
	Email       *string         `json:"email"`
	Phone       *string         `json:"phone"`
	VipStatus   string          `json:"vip_status"`
	Preferences *string         `json:"preferences"`
	Notes       *string         `json:"notes"`
	TotalStays  int             `json:"total_stays"`
	LoyaltyPts  int             `json:"loyalty_points"`
	Stays       []staySummary   `json:"stays"`
	LoyaltyTx   []loyaltyTxItem `json:"loyalty_transactions"`
}

type staySummary struct {
	ID         uuid.UUID `json:"id"`
	RoomNumber string    `json:"room_number"`
	CheckIn    time.Time `json:"check_in"`
	CheckOut   time.Time `json:"check_out"`
	Total      float64   `json:"total"`
}

type loyaltyTxItem struct {
	ID          uuid.UUID `json:"id"`
	Type        string    `json:"type"`
	Points      int       `json:"points"`
	Reference   *string   `json:"reference"`
	Description *string   `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

func (h *CRMHandler) ListGuests(c *fiber.Ctx) error {
	// Reservations are created via /api/reservations (guest_stays), which does not
	// (yet) link back to a guests.id — walk-ins and OTA bookings only carry a free-text
	// guest_email/guest_phone. Correlate by email/phone match rather than the FK, and
	// count only stays where the guest has actually departed (actual_check_out set).
	q := `SELECT g.id, g.full_name, g.email, g.phone,
	             COALESCE(g.vip_status, 'standard'),
	             COUNT(DISTINCT gs.id) FILTER (WHERE gs.actual_check_out IS NOT NULL) AS total_stays,
	             COALESCE(lm.points, 0) AS loyalty_points
	      FROM guests g
	      LEFT JOIN guest_stays gs ON gs.hotel_id = g.hotel_id
	          AND ((g.email IS NOT NULL AND gs.guest_email = g.email)
	               OR (g.phone IS NOT NULL AND gs.guest_phone = g.phone))
	      LEFT JOIN loyalty_members lm ON lm.guest_id = g.id AND lm.hotel_id = g.hotel_id
	      WHERE g.hotel_id = $1`
	args := []interface{}{h.hotelID(c)}
	argIdx := 2

	if v := c.Query("vip_status"); v != "" {
		q += " AND g.vip_status = $" + fmt.Sprintf("%d", argIdx)
		args = append(args, v)
		argIdx++
	}
	if v := c.Query("search"); v != "" {
		q += " AND (g.full_name ILIKE '%' || $" + fmt.Sprintf("%d", argIdx) + " || '%' OR g.email ILIKE '%' || $" + fmt.Sprintf("%d", argIdx) + " || '%')"
		args = append(args, v)
		argIdx++
	}
	q += " GROUP BY g.id, g.full_name, g.email, g.phone, g.vip_status, lm.points"
	q += " ORDER BY g.full_name ASC"

	rows, err := tenantPool(c, h.pool).Query(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()

	items := make([]guestSummaryResponse, 0)
	for rows.Next() {
		var item guestSummaryResponse
		if err := rows.Scan(
			&item.ID, &item.FullName, &item.Email, &item.Phone,
			&item.VipStatus, &item.TotalStays, &item.LoyaltyPts,
		); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		items = append(items, item)
	}
	return response.OK(c, items)
}

func (h *CRMHandler) GetGuest(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid guest id")
	}
	var detail guestDetailResponse
	err = tenantPool(c, h.pool).QueryRow(c.Context(), `
		SELECT g.id, g.full_name, g.email, g.phone,
		       COALESCE(g.vip_status, 'standard'),
		       g.preferences, g.notes,
		       COUNT(DISTINCT gs.id) FILTER (WHERE gs.actual_check_out IS NOT NULL) AS total_stays,
		       COALESCE(lm.points, 0) AS loyalty_points
		FROM guests g
		LEFT JOIN guest_stays gs ON gs.hotel_id = g.hotel_id
		    AND ((g.email IS NOT NULL AND gs.guest_email = g.email)
		         OR (g.phone IS NOT NULL AND gs.guest_phone = g.phone))
		LEFT JOIN loyalty_members lm ON lm.guest_id = g.id AND lm.hotel_id = g.hotel_id
		WHERE g.id = $1 AND g.hotel_id = $2
		GROUP BY g.id, g.full_name, g.email, g.phone, g.vip_status, g.preferences, g.notes, lm.points`,
		id, h.hotelID(c),
	).Scan(
		&detail.ID, &detail.FullName, &detail.Email, &detail.Phone,
		&detail.VipStatus, &detail.Preferences, &detail.Notes,
		&detail.TotalStays, &detail.LoyaltyPts,
	)
	if err != nil {
		return response.Error(c, fiber.StatusNotFound, "guest not found")
	}

	stayRows, err := tenantPool(c, h.pool).Query(c.Context(), `
		SELECT gs.id, COALESCE(r.room_number, '—'), gs.check_in_date, gs.check_out_date, COALESCE(gs.total_amount, 0)
		FROM guest_stays gs
		LEFT JOIN rooms r ON r.id = gs.room_id
		WHERE gs.hotel_id = $1
		    AND (($2::text IS NOT NULL AND gs.guest_email = $2) OR ($3::text IS NOT NULL AND gs.guest_phone = $3))
		ORDER BY gs.check_in_date DESC`,
		h.hotelID(c), detail.Email, detail.Phone,
	)
	if err == nil {
		defer stayRows.Close()
		for stayRows.Next() {
			var s staySummary
			if err := stayRows.Scan(&s.ID, &s.RoomNumber, &s.CheckIn, &s.CheckOut, &s.Total); err == nil {
				detail.Stays = append(detail.Stays, s)
			}
		}
	}

	txRows, err := tenantPool(c, h.pool).Query(c.Context(), `
		SELECT lt.id, lt.type, lt.points, lt.reference, lt.description, lt.created_at
		FROM loyalty_transactions lt
		JOIN loyalty_members lm ON lm.id = lt.member_id
		WHERE lm.guest_id = $1 AND lm.hotel_id = $2
		ORDER BY lt.created_at DESC
		LIMIT 50`,
		id, h.hotelID(c),
	)
	if err == nil {
		defer txRows.Close()
		for txRows.Next() {
			var tx loyaltyTxItem
			if err := txRows.Scan(&tx.ID, &tx.Type, &tx.Points, &tx.Reference, &tx.Description, &tx.CreatedAt); err == nil {
				detail.LoyaltyTx = append(detail.LoyaltyTx, tx)
			}
		}
	}

	if detail.Stays == nil {
		detail.Stays = make([]staySummary, 0)
	}
	if detail.LoyaltyTx == nil {
		detail.LoyaltyTx = make([]loyaltyTxItem, 0)
	}
	return response.OK(c, detail)
}

func (h *CRMHandler) CreateGuest(c *fiber.Ctx) error {
	var req createGuestRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if req.FullName == "" {
		return response.Error(c, fiber.StatusBadRequest, "full_name is required")
	}
	var email, phone *string
	if req.Email != "" {
		email = &req.Email
	}
	if req.Phone != "" {
		phone = &req.Phone
	}
	id := uuid.New()
	hotelID := h.hotelID(c)
	_, err := tenantPool(c, h.pool).Exec(c.Context(), `
		INSERT INTO guests (id, hotel_id, full_name, email, phone, vip_status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'standard', now(), now())
		ON CONFLICT DO NOTHING`,
		id, hotelID, req.FullName, email, phone,
	)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	detail := guestDetailResponse{
		ID:        id,
		FullName:  req.FullName,
		Email:     email,
		Phone:     phone,
		VipStatus: "standard",
		Stays:     make([]staySummary, 0),
		LoyaltyTx: make([]loyaltyTxItem, 0),
	}
	return response.Created(c, detail)
}

func (h *CRMHandler) UpdateGuest(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid guest id")
	}
	var req updateGuestRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	setClauses := ""
	args := make([]interface{}, 0, 3)
	argIdx := 1
	if req.Preferences != "" {
		setClauses += fmt.Sprintf("preferences = $%d, ", argIdx)
		args = append(args, req.Preferences)
		argIdx++
	}
	if req.Notes != "" {
		setClauses += fmt.Sprintf("notes = $%d, ", argIdx)
		args = append(args, req.Notes)
		argIdx++
	}
	if req.VipStatus != "" {
		validVip := map[string]bool{"standard": true, "silver": true, "gold": true, "platinum": true}
		if !validVip[req.VipStatus] {
			return response.Error(c, fiber.StatusBadRequest, "vip_status must be standard, silver, gold, or platinum")
		}
		setClauses += fmt.Sprintf("vip_status = $%d, ", argIdx)
		args = append(args, req.VipStatus)
		argIdx++
	}
	if setClauses == "" {
		return response.Error(c, fiber.StatusBadRequest, "no fields to update")
	}
	// Each fragment is appended with a trailing ", "; strip it so the SET list is
	// valid SQL (otherwise "SET col = $1,  WHERE ..." is a syntax error).
	setClauses = strings.TrimSuffix(strings.TrimSpace(setClauses), ",")
	args = append(args, id)
	args = append(args, h.hotelID(c))
	q := fmt.Sprintf(
		"UPDATE guests SET %s WHERE id = $%d AND hotel_id = $%d",
		setClauses, argIdx, argIdx+1,
	)
	tag, err := tenantPool(c, h.pool).Exec(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	if tag.RowsAffected() == 0 {
		return response.Error(c, fiber.StatusNotFound, "guest not found")
	}
	return response.OK(c, map[string]string{"status": "updated"})
}

// ---------------------------------------------------------------------------
// Loyalty Tiers
// ---------------------------------------------------------------------------

type createLoyaltyTierRequest struct {
	Name       string      `json:"name"`
	MinPoints  int         `json:"min_points"`
	Multiplier float64     `json:"multiplier"`
	Benefits   interface{} `json:"benefits,omitempty"`
}

// updateLoyaltyTierRequest uses pointer types for optional numeric fields so a
// partial PATCH (e.g. name-only) does not silently reset min_points/multiplier
// to their zero values. A nil pointer means "field omitted, leave unchanged".
type updateLoyaltyTierRequest struct {
	Name       string      `json:"name,omitempty"`
	MinPoints  *int        `json:"min_points,omitempty"`
	Multiplier *float64    `json:"multiplier,omitempty"`
	Benefits   interface{} `json:"benefits,omitempty"`
}

type loyaltyTierResponse struct {
	ID         uuid.UUID    `json:"id"`
	HotelID    uuid.UUID    `json:"hotel_id"`
	Name       string       `json:"name"`
	MinPoints  int          `json:"min_points"`
	Multiplier float64      `json:"multiplier"`
	Benefits   *interface{} `json:"benefits"`
	CreatedAt  time.Time    `json:"created_at"`
}

func (h *CRMHandler) ListLoyaltyTiers(c *fiber.Ctx) error {
	rows, err := tenantPool(c, h.pool).Query(c.Context(), `
		SELECT id, hotel_id, name, min_points, multiplier, benefits, created_at
		FROM loyalty_tiers
		WHERE hotel_id = $1
		ORDER BY min_points ASC`,
		h.hotelID(c),
	)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()

	items := make([]loyaltyTierResponse, 0)
	for rows.Next() {
		var item loyaltyTierResponse
		if err := rows.Scan(
			&item.ID, &item.HotelID, &item.Name, &item.MinPoints,
			&item.Multiplier, &item.Benefits, &item.CreatedAt,
		); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		items = append(items, item)
	}
	return response.OK(c, items)
}

func (h *CRMHandler) CreateLoyaltyTier(c *fiber.Ctx) error {
	var req createLoyaltyTierRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if req.Name == "" || req.MinPoints < 0 {
		return response.Error(c, fiber.StatusBadRequest, "name is required and min_points must be >= 0")
	}
	tierID := uuid.New()
	_, err := tenantPool(c, h.pool).Exec(c.Context(), `
		INSERT INTO loyalty_tiers (id, hotel_id, name, min_points, multiplier, benefits, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,now())`,
		tierID, h.hotelID(c), req.Name, req.MinPoints,
		req.Multiplier, req.Benefits,
	)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.Created(c, map[string]interface{}{
		"id":   tierID,
		"name": req.Name,
	})
}

func (h *CRMHandler) UpdateLoyaltyTier(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid tier id")
	}
	var req updateLoyaltyTierRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	q := "UPDATE loyalty_tiers SET "
	args := make([]interface{}, 0)
	argIdx := 1
	if req.Name != "" {
		q += fmt.Sprintf("name = $%d, ", argIdx)
		args = append(args, req.Name)
		argIdx++
	}
	if req.MinPoints != nil {
		if *req.MinPoints < 0 {
			return response.Error(c, fiber.StatusBadRequest, "min_points must be >= 0")
		}
		q += fmt.Sprintf("min_points = $%d, ", argIdx)
		args = append(args, *req.MinPoints)
		argIdx++
	}
	if req.Multiplier != nil {
		q += fmt.Sprintf("multiplier = $%d, ", argIdx)
		args = append(args, *req.Multiplier)
		argIdx++
	}
	if req.Benefits != nil {
		q += fmt.Sprintf("benefits = $%d, ", argIdx)
		args = append(args, req.Benefits)
		argIdx++
	}
	if argIdx == 1 {
		return response.Error(c, fiber.StatusBadRequest, "no fields to update")
	}
	q = q[:len(q)-2] + fmt.Sprintf(" WHERE id = $%d AND hotel_id = $%d", argIdx, argIdx+1)
	args = append(args, id, h.hotelID(c))
	tag, err := tenantPool(c, h.pool).Exec(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	if tag.RowsAffected() == 0 {
		return response.Error(c, fiber.StatusNotFound, "tier not found")
	}
	return response.OK(c, map[string]string{"status": "updated"})
}

// ---------------------------------------------------------------------------
// Loyalty Members & Points
// ---------------------------------------------------------------------------

type loyaltyMemberResponse struct {
	ID          uuid.UUID `json:"id"`
	GuestID     uuid.UUID `json:"guest_id"`
	GuestName   string    `json:"guest_name"`
	TierName    string    `json:"tier_name"`
	Points      int       `json:"points"`
	LifetimePts int       `json:"lifetime_points"`
	EnrolledAt  time.Time `json:"enrolled_at"`
}

func (h *CRMHandler) ListLoyaltyMembers(c *fiber.Ctx) error {
	q := `SELECT lm.id, lm.guest_id, g.full_name, COALESCE(lt.name, 'Standard'),
	             lm.points, lm.lifetime_points, lm.enrolled_at
	      FROM loyalty_members lm
	      JOIN guests g ON g.id = lm.guest_id
	      LEFT JOIN loyalty_tiers lt ON lt.id = lm.tier_id
	      WHERE lm.hotel_id = $1`
	args := []interface{}{h.hotelID(c)}
	argIdx := 2
	if v := c.Query("tier_id"); v != "" {
		q += " AND lm.tier_id = $" + fmt.Sprintf("%d", argIdx)
		args = append(args, v)
		argIdx++
	}
	q += " ORDER BY lm.points DESC"

	rows, err := tenantPool(c, h.pool).Query(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()

	items := make([]loyaltyMemberResponse, 0)
	for rows.Next() {
		var item loyaltyMemberResponse
		if err := rows.Scan(
			&item.ID, &item.GuestID, &item.GuestName,
			&item.TierName, &item.Points, &item.LifetimePts, &item.EnrolledAt,
		); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		items = append(items, item)
	}
	return response.OK(c, items)
}

type pointsAwardRequest struct {
	GuestID     string `json:"guest_id"`
	Points      int    `json:"points"`
	Reference   string `json:"reference,omitempty"`
	Description string `json:"description,omitempty"`
}

func (h *CRMHandler) AwardPoints(c *fiber.Ctx) error {
	var req pointsAwardRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	guestID, err := uuid.Parse(req.GuestID)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid guest_id")
	}
	if req.Points <= 0 {
		return response.Error(c, fiber.StatusBadRequest, "points must be positive")
	}

	var memberID uuid.UUID
	err = tenantPool(c, h.pool).QueryRow(c.Context(), `
		INSERT INTO loyalty_members (id, hotel_id, guest_id, points, lifetime_points, enrolled_at)
		VALUES (gen_random_uuid(), $1, $2, $3, $3, now())
		ON CONFLICT (guest_id, hotel_id) DO UPDATE
			SET points = loyalty_members.points + $3,
			    lifetime_points = loyalty_members.lifetime_points + $3
		RETURNING id`,
		tenantHotelID(c), guestID, req.Points,
	).Scan(&memberID)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}

	txID := uuid.New()
	_, err = tenantPool(c, h.pool).Exec(c.Context(), `
		INSERT INTO loyalty_transactions (id, member_id, type, points, reference, description, created_at)
		VALUES ($1,$2,'earn',$3,$4,$5,now())`,
		txID, memberID, req.Points,
		nullableText(req.Reference), nullableText(req.Description),
	)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.Created(c, map[string]interface{}{
		"transaction_id": txID,
		"member_id":      memberID,
		"points":         req.Points,
		"type":           "earn",
	})
}

type pointsRedeemRequest struct {
	GuestID     string `json:"guest_id"`
	Points      int    `json:"points"`
	Reference   string `json:"reference,omitempty"`
	Description string `json:"description,omitempty"`
}

func (h *CRMHandler) RedeemPoints(c *fiber.Ctx) error {
	var req pointsRedeemRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	guestID, err := uuid.Parse(req.GuestID)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid guest_id")
	}
	if req.Points <= 0 {
		return response.Error(c, fiber.StatusBadRequest, "points must be positive")
	}

	var memberID uuid.UUID
	var currentPoints int
	err = tenantPool(c, h.pool).QueryRow(c.Context(), `
		SELECT id, points FROM loyalty_members
		WHERE guest_id = $1 AND hotel_id = $2`,
		guestID, tenantHotelID(c),
	).Scan(&memberID, &currentPoints)
	if err != nil {
		return response.Error(c, fiber.StatusNotFound, "loyalty member not found")
	}
	if currentPoints < req.Points {
		return response.Error(c, fiber.StatusBadRequest,
			fmt.Sprintf("insufficient points: have %d, need %d", currentPoints, req.Points))
	}

	_, err = tenantPool(c, h.pool).Exec(c.Context(),
		"UPDATE loyalty_members SET points = points - $1 WHERE id = $2 AND hotel_id = $3",
		req.Points, memberID, tenantHotelID(c),
	)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}

	txID := uuid.New()
	_, err = tenantPool(c, h.pool).Exec(c.Context(), `
		INSERT INTO loyalty_transactions (id, member_id, type, points, reference, description, created_at)
		VALUES ($1,$2,'redeem',$3,$4,$5,now())`,
		txID, memberID, req.Points,
		nullableText(req.Reference), nullableText(req.Description),
	)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.Created(c, map[string]interface{}{
		"transaction_id": txID,
		"member_id":      memberID,
		"points":         req.Points,
		"type":           "redeem",
	})
}

// ---------------------------------------------------------------------------
// Marketing campaigns (campaigns table, migration 024). Real CRUD replacing the
// old hardcoded demo. Tenant-pool routed like the rest of CRM.
// ---------------------------------------------------------------------------

type campaignResponse struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Audience  string    `json:"audience"`
	Channel   string    `json:"channel"`
	Status    string    `json:"status"`
	Sent      int       `json:"sent"`
	Opens     int       `json:"opens"`
	Clicks    int       `json:"clicks"`
	Revenue   float64   `json:"revenue"`
	CreatedAt time.Time `json:"created_at"`
}

func (h *CRMHandler) ListCampaigns(c *fiber.Ctx) error {
	rows, err := tenantPool(c, h.pool).Query(c.Context(), `
		SELECT id, name, COALESCE(audience,''), channel, status, sent, opens, clicks, revenue, created_at
		FROM campaigns WHERE hotel_id = $1 ORDER BY created_at DESC`, tenantHotelID(c))
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()
	items := make([]campaignResponse, 0)
	for rows.Next() {
		var it campaignResponse
		if err := rows.Scan(&it.ID, &it.Name, &it.Audience, &it.Channel, &it.Status,
			&it.Sent, &it.Opens, &it.Clicks, &it.Revenue, &it.CreatedAt); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		items = append(items, it)
	}
	return response.OK(c, items)
}

func (h *CRMHandler) CreateCampaign(c *fiber.Ctx) error {
	var req struct {
		Name     string `json:"name"`
		Audience string `json:"audience"`
		Channel  string `json:"channel"`
		Status   string `json:"status"`
	}
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if strings.TrimSpace(req.Name) == "" {
		return response.Error(c, fiber.StatusUnprocessableEntity, "campaign name is required")
	}
	channel := strings.TrimSpace(req.Channel)
	if channel == "" {
		channel = "email"
	}
	status := strings.TrimSpace(req.Status)
	if status == "" {
		status = "draft"
	}
	id := uuid.New()
	if _, err := tenantPool(c, h.pool).Exec(c.Context(), `
		INSERT INTO campaigns (id, hotel_id, name, audience, channel, status, created_at, updated_at)
		VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,now(),now())`,
		id, tenantHotelID(c), strings.TrimSpace(req.Name), strings.TrimSpace(req.Audience), channel, status); err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.Created(c, map[string]interface{}{"id": id, "name": strings.TrimSpace(req.Name), "status": status})
}

func (h *CRMHandler) UpdateCampaign(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid campaign id")
	}
	var req struct {
		Name     *string `json:"name"`
		Audience *string `json:"audience"`
		Channel  *string `json:"channel"`
		Status   *string `json:"status"`
	}
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	sets := []string{}
	args := []interface{}{}
	idx := 1
	if req.Name != nil {
		sets = append(sets, fmt.Sprintf("name = $%d", idx))
		args = append(args, *req.Name)
		idx++
	}
	if req.Audience != nil {
		sets = append(sets, fmt.Sprintf("audience = NULLIF($%d,'')", idx))
		args = append(args, *req.Audience)
		idx++
	}
	if req.Channel != nil {
		sets = append(sets, fmt.Sprintf("channel = $%d", idx))
		args = append(args, *req.Channel)
		idx++
	}
	if req.Status != nil {
		sets = append(sets, fmt.Sprintf("status = $%d", idx))
		args = append(args, *req.Status)
		idx++
	}
	if len(sets) == 0 {
		return response.Error(c, fiber.StatusBadRequest, "no fields to update")
	}
	sets = append(sets, "updated_at = now()")
	args = append(args, id, tenantHotelID(c))
	q := fmt.Sprintf("UPDATE campaigns SET %s WHERE id = $%d AND hotel_id = $%d", strings.Join(sets, ", "), idx, idx+1)
	tag, err := tenantPool(c, h.pool).Exec(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	if tag.RowsAffected() == 0 {
		return response.Error(c, fiber.StatusNotFound, "campaign not found")
	}
	return response.OK(c, map[string]interface{}{"id": id})
}

func (h *CRMHandler) DeleteCampaign(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid campaign id")
	}
	tag, err := tenantPool(c, h.pool).Exec(c.Context(), `DELETE FROM campaigns WHERE id = $1 AND hotel_id = $2`, id, tenantHotelID(c))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	if tag.RowsAffected() == 0 {
		return response.Error(c, fiber.StatusNotFound, "campaign not found")
	}
	return response.OK(c, map[string]string{"status": "deleted"})
}
