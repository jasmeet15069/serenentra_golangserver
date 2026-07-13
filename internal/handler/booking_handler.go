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
//   CREATE TABLE IF NOT EXISTS promotions (
//     id uuid PK, hotel_id uuid, code text, name text,
//     discount_type text, discount_value numeric, min_nights int,
//     valid_from date, valid_to date, usage_limit int,
//     used_count int, active bool, created_at timestamptz
//   );

type BookingHandler struct {
	pool *pgxpool.Pool
}

func NewBookingHandler(pool *pgxpool.Pool) *BookingHandler {
	return &BookingHandler{pool: pool}
}

func (h *BookingHandler) Register(r fiber.Router) {
	r.Get("/booking/availability", h.CheckAvailability)
	r.Post("/booking/search", h.SearchRooms)
	r.Get("/booking/promotions", h.ListPromotions)
	r.Post("/booking/promotions", h.CreatePromotion)
	r.Delete("/booking/promotions/:id", h.DeletePromotion)
	r.Patch("/booking/promotions/:id", h.TogglePromotion)
	r.Post("/booking/validate-promo", h.ValidatePromo)
	r.Get("/booking/rate-plans", h.ListRatePlans)
	r.Post("/booking/rate-plans", h.CreateRatePlan)
	r.Patch("/booking/rate-plans/:id", h.UpdateRatePlan)
	r.Delete("/booking/rate-plans/:id", h.DeleteRatePlan)
}

// ---------------------------------------------------------------------------
// Availability & Search
// ---------------------------------------------------------------------------

type availabilityResponse struct {
	Date      string `json:"date"`
	Available int    `json:"available"`
	Total     int    `json:"total"`
}

func (h *BookingHandler) CheckAvailability(c *fiber.Ctx) error {
	checkIn := c.Query("check_in")
	checkOut := c.Query("check_out")
	if checkIn == "" || checkOut == "" {
		return response.Error(c, fiber.StatusBadRequest, "check_in and check_out are required")
	}

	rows, err := tenantPool(c, h.pool).Query(c.Context(), `
		SELECT d.date::text,
		       (SELECT COUNT(*) FROM rooms r WHERE r.hotel_id = $1 AND r.status = 'available')
		       - COALESCE(COUNT(b.id) FILTER (WHERE b.status IN ('confirmed','checked_in')), 0) AS available,
		       (SELECT COUNT(*) FROM rooms r WHERE r.hotel_id = $1) AS total
		FROM generate_series($2::date, $3::date - 1, '1 day') d(date)
		LEFT JOIN bookings b ON b.hotel_id = $1
			AND d.date BETWEEN b.check_in AND b.check_out - 1
			AND b.status IN ('confirmed', 'checked_in')
		GROUP BY d.date
		ORDER BY d.date`,
		tenantHotelID(c), checkIn, checkOut,
	)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()

	items := make([]availabilityResponse, 0)
	for rows.Next() {
		var item availabilityResponse
		if err := rows.Scan(&item.Date, &item.Available, &item.Total); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		items = append(items, item)
	}
	return response.OK(c, items)
}

type searchRoomsRequest struct {
	CheckIn  string `json:"check_in"`
	CheckOut string `json:"check_out"`
	Guests   int    `json:"guests,omitempty"`
	RoomType string `json:"room_type,omitempty"`
}

type searchRoomItem struct {
	ID            uuid.UUID `json:"id"`
	RoomNumber    string    `json:"room_number"`
	RoomType      string    `json:"room_type"`
	Floor         int       `json:"floor"`
	MaxGuests     int       `json:"max_guests"`
	BaseRate      float64   `json:"base_rate"`
	TotalNights   int       `json:"total_nights"`
	TotalEstimate float64   `json:"total_estimate"`
}

func (h *BookingHandler) SearchRooms(c *fiber.Ctx) error {
	var req searchRoomsRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if req.CheckIn == "" || req.CheckOut == "" {
		return response.Error(c, fiber.StatusBadRequest, "check_in and check_out are required")
	}

	q := `SELECT r.id, r.room_number, r.room_type, r.floor, r.max_guests, r.base_rate
	      FROM rooms r
	      WHERE r.hotel_id = $1
	        AND r.status = 'available'
	        AND r.id NOT IN (
	            SELECT b.room_id FROM bookings b
	            WHERE b.hotel_id = $1
	              AND b.status IN ('confirmed', 'checked_in')
	              AND b.check_in < $3 AND b.check_out > $2
	        )`
	args := []interface{}{tenantHotelID(c), req.CheckIn, req.CheckOut}
	argIdx := 4

	if req.Guests > 0 {
		q += " AND r.max_guests >= $" + fmt.Sprintf("%d", argIdx)
		args = append(args, req.Guests)
		argIdx++
	}
	if req.RoomType != "" {
		q += " AND r.room_type = $" + fmt.Sprintf("%d", argIdx)
		args = append(args, req.RoomType)
		argIdx++
	}
	q += " ORDER BY r.room_number"

	rows, err := tenantPool(c, h.pool).Query(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()

	checkInTime, _ := time.Parse("2006-01-02", req.CheckIn)
	checkOutTime, _ := time.Parse("2006-01-02", req.CheckOut)
	totalNights := int(checkOutTime.Sub(checkInTime).Hours() / 24)

	items := make([]searchRoomItem, 0)
	for rows.Next() {
		var item searchRoomItem
		if err := rows.Scan(
			&item.ID, &item.RoomNumber, &item.RoomType, &item.Floor,
			&item.MaxGuests, &item.BaseRate,
		); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		item.TotalNights = totalNights
		item.TotalEstimate = item.BaseRate * float64(totalNights)
		items = append(items, item)
	}
	return response.OK(c, items)
}

// ---------------------------------------------------------------------------
// Promotions
// ---------------------------------------------------------------------------

type createPromotionRequest struct {
	Code          string  `json:"code"`
	Name          string  `json:"name"`
	DiscountType  string  `json:"discount_type"`
	DiscountValue float64 `json:"discount_value"`
	MinNights     int     `json:"min_nights,omitempty"`
	ValidFrom     string  `json:"valid_from"`
	ValidTo       string  `json:"valid_to"`
	UsageLimit    int     `json:"usage_limit,omitempty"`
}

type promotionResponse struct {
	ID            uuid.UUID `json:"id"`
	HotelID       uuid.UUID `json:"hotel_id"`
	Code          string    `json:"code"`
	Name          string    `json:"name"`
	DiscountType  string    `json:"discount_type"`
	DiscountValue float64   `json:"discount_value"`
	MinNights     *int      `json:"min_nights"`
	ValidFrom     string    `json:"valid_from"`
	ValidTo       string    `json:"valid_to"`
	UsageLimit    *int      `json:"usage_limit"`
	UsedCount     int       `json:"used_count"`
	Active        bool      `json:"active"`
	CreatedAt     time.Time `json:"created_at"`
}

func (h *BookingHandler) ListPromotions(c *fiber.Ctx) error {
	q := `SELECT id, hotel_id, code, name, discount_type, discount_value,
	             min_nights, valid_from, valid_to, usage_limit, used_count, active, created_at
	      FROM promotions
	      WHERE hotel_id = $1`
	args := []interface{}{tenantHotelID(c)}
	argIdx := 2

	if v := c.Query("active"); v != "" {
		q += " AND active = $" + fmt.Sprintf("%d", argIdx)
		args = append(args, v == "true")
		argIdx++
	}
	q += " ORDER BY created_at DESC"

	rows, err := tenantPool(c, h.pool).Query(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()

	items := make([]promotionResponse, 0)
	for rows.Next() {
		var item promotionResponse
		var validFrom, validTo time.Time
		if err := rows.Scan(
			&item.ID, &item.HotelID, &item.Code, &item.Name,
			&item.DiscountType, &item.DiscountValue, &item.MinNights,
			&validFrom, &validTo, &item.UsageLimit, &item.UsedCount,
			&item.Active, &item.CreatedAt,
		); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		item.ValidFrom = validFrom.Format("2006-01-02")
		item.ValidTo = validTo.Format("2006-01-02")
		items = append(items, item)
	}
	return response.OK(c, items)
}

func (h *BookingHandler) CreatePromotion(c *fiber.Ctx) error {
	var req createPromotionRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if req.Code == "" || req.Name == "" || req.DiscountType == "" || req.DiscountValue <= 0 {
		return response.Error(c, fiber.StatusBadRequest,
			"code, name, discount_type, and positive discount_value are required")
	}
	validTypes := map[string]bool{"percentage": true, "fixed": true}
	if !validTypes[req.DiscountType] {
		return response.Error(c, fiber.StatusBadRequest, "discount_type must be percentage or fixed")
	}
	if req.DiscountType == "percentage" && req.DiscountValue > 100 {
		return response.Error(c, fiber.StatusBadRequest, "percentage discount cannot exceed 100")
	}
	promoID := uuid.New()
	var minNights, usageLimit *int
	if req.MinNights > 0 {
		minNights = &req.MinNights
	}
	if req.UsageLimit > 0 {
		usageLimit = &req.UsageLimit
	}
	_, err := tenantPool(c, h.pool).Exec(c.Context(), `
		INSERT INTO promotions
			(id, hotel_id, code, name, discount_type, discount_value,
			 min_nights, valid_from, valid_to, usage_limit, used_count, active, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8::date,$9::date,$10,0,true,now())`,
		promoID, tenantHotelID(c), req.Code, req.Name,
		req.DiscountType, req.DiscountValue, minNights,
		req.ValidFrom, req.ValidTo, usageLimit,
	)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.Created(c, map[string]interface{}{
		"id":   promoID,
		"code": req.Code,
		"name": req.Name,
	})
}

func (h *BookingHandler) DeletePromotion(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid promotion id")
	}
	tag, err := tenantPool(c, h.pool).Exec(c.Context(),
		"DELETE FROM promotions WHERE id = $1 AND hotel_id = $2",
		id, tenantHotelID(c),
	)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	if tag.RowsAffected() == 0 {
		return response.Error(c, fiber.StatusNotFound, "promotion not found")
	}
	return response.OK(c, map[string]string{"status": "deleted"})
}

type validatePromoRequest struct {
	Code     string  `json:"code"`
	RoomType string  `json:"room_type,omitempty"`
	Total    float64 `json:"total,omitempty"`
}

type validatePromoResponse struct {
	Valid      bool    `json:"valid"`
	Code       string  `json:"code"`
	Name       string  `json:"name,omitempty"`
	Discount   float64 `json:"discount,omitempty"`
	Discounted float64 `json:"discounted,omitempty"`
	Message    string  `json:"message,omitempty"`
}

func (h *BookingHandler) ValidatePromo(c *fiber.Ctx) error {
	var req validatePromoRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if req.Code == "" {
		return response.Error(c, fiber.StatusBadRequest, "code is required")
	}

	var promoID uuid.UUID
	var name, discountType string
	var discountValue float64
	var usageLimit, usedCount int
	var validFrom, validTo time.Time
	var active bool

	err := tenantPool(c, h.pool).QueryRow(c.Context(), `
		SELECT id, name, discount_type, discount_value,
		       usage_limit, used_count, valid_from, valid_to, active
		FROM promotions
		WHERE hotel_id = $1 AND code = $2`,
		tenantHotelID(c), req.Code,
	).Scan(&promoID, &name, &discountType, &discountValue,
		&usageLimit, &usedCount, &validFrom, &validTo, &active)
	if err != nil {
		return response.OK(c, validatePromoResponse{
			Valid:   false,
			Code:    req.Code,
			Message: "promo code not found",
		})
	}

	if !active {
		return response.OK(c, validatePromoResponse{
			Valid:   false,
			Code:    req.Code,
			Message: "promo code is inactive",
		})
	}

	now := time.Now().UTC()
	if now.Before(validFrom) || now.After(validTo) {
		return response.OK(c, validatePromoResponse{
			Valid:   false,
			Code:    req.Code,
			Message: "promo code is expired or not yet valid",
		})
	}

	if usageLimit > 0 && usedCount >= usageLimit {
		return response.OK(c, validatePromoResponse{
			Valid:   false,
			Code:    req.Code,
			Message: "promo code usage limit reached",
		})
	}

	var discount float64
	if discountType == "percentage" {
		discount = req.Total * discountValue / 100
	} else {
		discount = discountValue
	}
	if discount > req.Total {
		discount = req.Total
	}

	return response.OK(c, validatePromoResponse{
		Valid:      true,
		Code:       req.Code,
		Name:       name,
		Discount:   discount,
		Discounted: req.Total - discount,
		Message:    "promo code applied successfully",
	})
}

func (h *BookingHandler) TogglePromotion(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid promotion id")
	}
	var req struct {
		Active *bool `json:"active"`
	}
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if req.Active == nil {
		return response.Error(c, fiber.StatusBadRequest, "active field is required")
	}
	tag, err := tenantPool(c, h.pool).Exec(c.Context(), `UPDATE promotions SET active = $1 WHERE id = $2 AND hotel_id = $3`, *req.Active, id, tenantHotelID(c))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	if tag.RowsAffected() == 0 {
		return response.Error(c, fiber.StatusNotFound, "promotion not found")
	}
	return response.OK(c, map[string]interface{}{"id": id, "active": *req.Active})
}

// ---------------------------------------------------------------------------
// Rate plans (rate_plans table). A rate plan is a named pricing rule — e.g.
// "Non-Refundable -10%" or "Long Stay 7+ nights" — with an optional discount and
// a minimum-stay requirement. Parallels promotions above; the booking module gate
// at the router controls access. Uses tenantPool so a dedicated-DB tenant's plans
// live in its own database.
// ---------------------------------------------------------------------------

type ratePlanResponse struct {
	ID            uuid.UUID `json:"id"`
	HotelID       uuid.UUID `json:"hotel_id"`
	Name          string    `json:"name"`
	Description   string    `json:"description"`
	DiscountType  *string   `json:"discount_type"`
	DiscountValue float64   `json:"discount_value"`
	MinStayNights int       `json:"min_stay_nights"`
	IsActive      bool      `json:"is_active"`
	CreatedAt     time.Time `json:"created_at"`
}

type createRatePlanRequest struct {
	Name          string  `json:"name"`
	Description   string  `json:"description"`
	DiscountType  string  `json:"discount_type"`
	DiscountValue float64 `json:"discount_value"`
	MinStayNights int     `json:"min_stay_nights"`
}

func (h *BookingHandler) ListRatePlans(c *fiber.Ctx) error {
	rows, err := tenantPool(c, h.pool).Query(c.Context(), `
		SELECT id, hotel_id, name, COALESCE(description,''), discount_type,
		       COALESCE(discount_value,0), min_stay_nights, is_active, created_at
		FROM rate_plans WHERE hotel_id = $1 ORDER BY created_at DESC`, tenantHotelID(c))
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()
	items := make([]ratePlanResponse, 0)
	for rows.Next() {
		var it ratePlanResponse
		if err := rows.Scan(&it.ID, &it.HotelID, &it.Name, &it.Description, &it.DiscountType,
			&it.DiscountValue, &it.MinStayNights, &it.IsActive, &it.CreatedAt); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		items = append(items, it)
	}
	return response.OK(c, items)
}

func (h *BookingHandler) CreateRatePlan(c *fiber.Ctx) error {
	var req createRatePlanRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if strings.TrimSpace(req.Name) == "" {
		return response.Error(c, fiber.StatusBadRequest, "name is required")
	}
	if req.DiscountType != "" {
		if req.DiscountType != "percentage" && req.DiscountType != "fixed" {
			return response.Error(c, fiber.StatusBadRequest, "discount_type must be percentage or fixed")
		}
		if req.DiscountType == "percentage" && req.DiscountValue > 100 {
			return response.Error(c, fiber.StatusBadRequest, "percentage discount cannot exceed 100")
		}
	}
	if req.MinStayNights < 1 {
		req.MinStayNights = 1
	}
	var discountType *string
	if req.DiscountType != "" {
		discountType = &req.DiscountType
	}
	id := uuid.New()
	if _, err := tenantPool(c, h.pool).Exec(c.Context(), `
		INSERT INTO rate_plans
			(id, hotel_id, name, description, discount_type, discount_value, min_stay_nights, is_active, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,true,now(),now())`,
		id, tenantHotelID(c), strings.TrimSpace(req.Name), req.Description, discountType, req.DiscountValue, req.MinStayNights); err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.Created(c, map[string]interface{}{"id": id, "name": strings.TrimSpace(req.Name)})
}

func (h *BookingHandler) UpdateRatePlan(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid rate plan id")
	}
	var req struct {
		Name          *string  `json:"name"`
		Description   *string  `json:"description"`
		DiscountType  *string  `json:"discount_type"`
		DiscountValue *float64 `json:"discount_value"`
		MinStayNights *int     `json:"min_stay_nights"`
		IsActive      *bool    `json:"is_active"`
	}
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	// Build the SET list with the safe []string + strings.Join pattern so there is
	// never a trailing-comma syntax error (see reference_update_handler_bug).
	sets := []string{}
	args := []interface{}{}
	idx := 1
	if req.Name != nil {
		sets = append(sets, fmt.Sprintf("name = $%d", idx))
		args = append(args, *req.Name)
		idx++
	}
	if req.Description != nil {
		sets = append(sets, fmt.Sprintf("description = $%d", idx))
		args = append(args, *req.Description)
		idx++
	}
	if req.DiscountType != nil {
		sets = append(sets, fmt.Sprintf("discount_type = $%d", idx))
		args = append(args, *req.DiscountType)
		idx++
	}
	if req.DiscountValue != nil {
		sets = append(sets, fmt.Sprintf("discount_value = $%d", idx))
		args = append(args, *req.DiscountValue)
		idx++
	}
	if req.MinStayNights != nil {
		sets = append(sets, fmt.Sprintf("min_stay_nights = $%d", idx))
		args = append(args, *req.MinStayNights)
		idx++
	}
	if req.IsActive != nil {
		sets = append(sets, fmt.Sprintf("is_active = $%d", idx))
		args = append(args, *req.IsActive)
		idx++
	}
	if len(sets) == 0 {
		return response.Error(c, fiber.StatusBadRequest, "no fields to update")
	}
	sets = append(sets, "updated_at = now()")
	args = append(args, id, tenantHotelID(c))
	q := fmt.Sprintf("UPDATE rate_plans SET %s WHERE id = $%d AND hotel_id = $%d", strings.Join(sets, ", "), idx, idx+1)
	tag, err := tenantPool(c, h.pool).Exec(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	if tag.RowsAffected() == 0 {
		return response.Error(c, fiber.StatusNotFound, "rate plan not found")
	}
	return response.OK(c, map[string]interface{}{"id": id})
}

func (h *BookingHandler) DeleteRatePlan(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid rate plan id")
	}
	tag, err := tenantPool(c, h.pool).Exec(c.Context(), `DELETE FROM rate_plans WHERE id = $1 AND hotel_id = $2`, id, tenantHotelID(c))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	if tag.RowsAffected() == 0 {
		return response.Error(c, fiber.StatusNotFound, "rate plan not found")
	}
	return response.OK(c, map[string]string{"status": "deleted"})
}
