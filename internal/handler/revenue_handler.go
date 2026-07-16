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
//   CREATE TABLE IF NOT EXISTS pricing_rules (
//     id uuid PK, hotel_id uuid, name text, rule_type text,
//     conditions jsonb, adjustment numeric, priority int,
//     active bool, created_at timestamptz
//   );

type RevenueHandler struct {
	pool *pgxpool.Pool
}

func NewRevenueHandler(pool *pgxpool.Pool) *RevenueHandler {
	return &RevenueHandler{pool: pool}
}

func (h *RevenueHandler) Register(r fiber.Router) {
	r.Get("/revenue/pricing", h.ListPricingRules)
	r.Get("/revenue/pricing-rules", h.ListPricingRules)
	r.Post("/revenue/pricing", h.CreatePricingRule)
	r.Post("/revenue/pricing-rules", h.CreatePricingRule)
	r.Delete("/revenue/pricing/:id", h.DeletePricingRule)
	r.Delete("/revenue/pricing-rules/:id", h.DeletePricingRule)
	r.Put("/revenue/pricing-rules/:id", h.UpdatePricingRule)
	r.Patch("/revenue/pricing-rules/:id", h.UpdatePricingRule)
	r.Get("/revenue/yield", h.GetYieldMetrics)
	r.Get("/revenue/competitors", h.GetCompetitorRates)
	r.Post("/revenue/apply-adjustment", h.ApplyAdjustment)
	r.Get("/revenue/forecast", h.GetForecast)
}

// ---------------------------------------------------------------------------
// Pricing Rules
// ---------------------------------------------------------------------------

type createPricingRuleRequest struct {
	Name       string      `json:"name"`
	RuleType   string      `json:"rule_type"`
	Conditions interface{} `json:"conditions,omitempty"`
	Adjustment float64     `json:"adjustment"`
	Priority   int         `json:"priority,omitempty"`
	Active     bool        `json:"active,omitempty"`
}

type updatePricingRuleRequest struct {
	Name       string      `json:"name,omitempty"`
	RuleType   string      `json:"rule_type,omitempty"`
	Conditions interface{} `json:"conditions,omitempty"`
	Adjustment *float64    `json:"adjustment,omitempty"`
	Priority   *int        `json:"priority,omitempty"`
	Active     *bool       `json:"active,omitempty"`
}

type pricingRuleResponse struct {
	ID         uuid.UUID    `json:"id"`
	HotelID    uuid.UUID    `json:"hotel_id"`
	Name       string       `json:"name"`
	RuleType   string       `json:"rule_type"`
	Conditions *interface{} `json:"conditions"`
	Adjustment float64      `json:"adjustment"`
	Priority   int          `json:"priority"`
	Active     bool         `json:"active"`
	CreatedAt  time.Time    `json:"created_at"`
}

func (h *RevenueHandler) ListPricingRules(c *fiber.Ctx) error {
	q := `SELECT id, hotel_id, name, rule_type, conditions, adjustment, priority, active, created_at
	      FROM pricing_rules
	      WHERE hotel_id = $1`
	args := []interface{}{tenantHotelID(c)}
	argIdx := 2

	if v := c.Query("rule_type"); v != "" {
		q += " AND rule_type = $" + fmt.Sprintf("%d", argIdx)
		args = append(args, v)
		argIdx++
	}
	q += " ORDER BY priority ASC, created_at DESC"

	rows, err := tenantPool(c, h.pool).Query(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()

	items := make([]pricingRuleResponse, 0)
	for rows.Next() {
		var item pricingRuleResponse
		if err := rows.Scan(
			&item.ID, &item.HotelID, &item.Name, &item.RuleType,
			&item.Conditions, &item.Adjustment, &item.Priority,
			&item.Active, &item.CreatedAt,
		); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		items = append(items, item)
	}
	return response.OK(c, items)
}

func (h *RevenueHandler) CreatePricingRule(c *fiber.Ctx) error {
	var req createPricingRuleRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if req.Name == "" || req.RuleType == "" {
		return response.Error(c, fiber.StatusBadRequest, "name and rule_type are required")
	}
	validTypes := map[string]bool{"seasonal": true, "occupancy": true, "manual": true}
	if !validTypes[req.RuleType] {
		return response.Error(c, fiber.StatusBadRequest, "rule_type must be seasonal, occupancy, or manual")
	}
	ruleID := uuid.New()
	_, err := tenantPool(c, h.pool).Exec(c.Context(), `
		INSERT INTO pricing_rules
			(id, hotel_id, name, rule_type, conditions, adjustment, priority, active, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,now())`,
		ruleID, tenantHotelID(c), req.Name, req.RuleType,
		req.Conditions, req.Adjustment, req.Priority, req.Active,
	)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.Created(c, map[string]interface{}{
		"id":        ruleID,
		"name":      req.Name,
		"rule_type": req.RuleType,
		"active":    req.Active,
	})
}

func (h *RevenueHandler) UpdatePricingRule(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid pricing rule id")
	}
	var req updatePricingRuleRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	setClauses := ""
	args := make([]interface{}, 0, 6)
	argIdx := 1
	if req.Name != "" {
		setClauses += fmt.Sprintf("name = $%d, ", argIdx)
		args = append(args, req.Name)
		argIdx++
	}
	if req.RuleType != "" {
		validTypes := map[string]bool{"seasonal": true, "occupancy": true, "manual": true}
		if !validTypes[req.RuleType] {
			return response.Error(c, fiber.StatusBadRequest, "rule_type must be seasonal, occupancy, or manual")
		}
		setClauses += fmt.Sprintf("rule_type = $%d, ", argIdx)
		args = append(args, req.RuleType)
		argIdx++
	}
	if req.Conditions != nil {
		setClauses += fmt.Sprintf("conditions = $%d, ", argIdx)
		args = append(args, req.Conditions)
		argIdx++
	}
	if req.Adjustment != nil {
		setClauses += fmt.Sprintf("adjustment = $%d, ", argIdx)
		args = append(args, *req.Adjustment)
		argIdx++
	}
	if req.Priority != nil {
		setClauses += fmt.Sprintf("priority = $%d, ", argIdx)
		args = append(args, *req.Priority)
		argIdx++
	}
	if req.Active != nil {
		setClauses += fmt.Sprintf("active = $%d, ", argIdx)
		args = append(args, *req.Active)
		argIdx++
	}
	if setClauses == "" {
		return response.Error(c, fiber.StatusBadRequest, "no fields to update")
	}
	setClauses = strings.TrimSuffix(strings.TrimSpace(setClauses), ",")
	args = append(args, id)
	args = append(args, tenantHotelID(c))
	q := fmt.Sprintf(
		"UPDATE pricing_rules SET %s WHERE id = $%d AND hotel_id = $%d",
		setClauses, argIdx, argIdx+1,
	)
	tag, err := tenantPool(c, h.pool).Exec(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	if tag.RowsAffected() == 0 {
		return response.Error(c, fiber.StatusNotFound, "pricing rule not found")
	}
	return response.OK(c, map[string]string{"status": "updated"})
}

func (h *RevenueHandler) DeletePricingRule(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid pricing rule id")
	}
	tag, err := tenantPool(c, h.pool).Exec(c.Context(),
		"DELETE FROM pricing_rules WHERE id = $1 AND hotel_id = $2",
		id, tenantHotelID(c),
	)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	if tag.RowsAffected() == 0 {
		return response.Error(c, fiber.StatusNotFound, "pricing rule not found")
	}
	return response.OK(c, map[string]string{"status": "deleted"})
}

// ---------------------------------------------------------------------------
// Yield Metrics
// ---------------------------------------------------------------------------

type yieldMetricsResponse struct {
	Date      time.Time `json:"date"`
	RevPAR    float64   `json:"revpar"`
	ADR       float64   `json:"adr"`
	Occupancy float64   `json:"occupancy"`
	GOPPAR    float64   `json:"goppar"`
}

func (h *RevenueHandler) GetYieldMetrics(c *fiber.Ctx) error {
	rows, err := tenantPool(c, h.pool).Query(c.Context(), `
		SELECT
			COALESCE(AVG(r.revpar), 0),
			COALESCE(AVG(r.adr), 0),
			COALESCE(AVG(r.occupancy_pct), 0),
			COALESCE(AVG(r.goppar), 0)
		FROM revenue_daily r
		WHERE r.hotel_id = $1
		  AND r.date >= CURRENT_DATE - INTERVAL '30 days'`,
		tenantHotelID(c),
	)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()

	items := make([]yieldMetricsResponse, 0)
	for rows.Next() {
		var item yieldMetricsResponse
		if err := rows.Scan(
			&item.RevPAR, &item.ADR, &item.Occupancy, &item.GOPPAR,
		); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		items = append(items, item)
	}
	return response.OK(c, items)
}

// ---------------------------------------------------------------------------
// Competitor Rates
// ---------------------------------------------------------------------------

type competitorRateResponse struct {
	CompetitorName string  `json:"competitor_name"`
	RoomType       string  `json:"room_type"`
	OurRate        float64 `json:"our_rate"`
	TheirRate      float64 `json:"their_rate"`
	Difference     float64 `json:"difference"`
}

func (h *RevenueHandler) GetCompetitorRates(c *fiber.Ctx) error {
	rows, err := tenantPool(c, h.pool).Query(c.Context(), `
		SELECT competitor_name, room_type, our_rate, their_rate,
		       (our_rate - their_rate) AS difference
		FROM competitor_rates
		WHERE hotel_id = $1
		ORDER BY competitor_name, room_type`,
		tenantHotelID(c),
	)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()

	items := make([]competitorRateResponse, 0)
	for rows.Next() {
		var item competitorRateResponse
		if err := rows.Scan(
			&item.CompetitorName, &item.RoomType,
			&item.OurRate, &item.TheirRate, &item.Difference,
		); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		items = append(items, item)
	}
	return response.OK(c, items)
}

// ---------------------------------------------------------------------------
// Forecast
// ---------------------------------------------------------------------------

type forecastResponse struct {
	Date         string  `json:"date"`
	OccupancyPct float64 `json:"occupancy_pct"`
	Revenue      float64 `json:"revenue"`
}

func (h *RevenueHandler) GetForecast(c *fiber.Ctx) error {
	rows, err := tenantPool(c, h.pool).Query(c.Context(), `
		SELECT to_char(date, 'YYYY-MM-DD'), occupancy_pct, revenue
		FROM revenue_forecast
		WHERE hotel_id = $1
		ORDER BY date ASC`,
		tenantHotelID(c),
	)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()

	items := make([]forecastResponse, 0)
	for rows.Next() {
		var item forecastResponse
		if err := rows.Scan(
			&item.Date, &item.OccupancyPct, &item.Revenue,
		); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		items = append(items, item)
	}
	return response.OK(c, items)
}

// ApplyAdjustment (POST /api/revenue/apply-adjustment) applies a percentage change
// to room base rates (all room types, or one) and records it in rate_adjustments.
// This is the real action behind the Revenue "Apply %" button, which used to only
// toast. It changes rooms.price_per_night directly, so the caller should confirm.
func (h *RevenueHandler) ApplyAdjustment(c *fiber.Ctx) error {
	var req struct {
		Percent  float64 `json:"percent"`
		RoomType string  `json:"room_type"`
	}
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if req.Percent == 0 {
		return response.Error(c, fiber.StatusUnprocessableEntity, "percent must be non-zero")
	}
	if req.Percent < -90 || req.Percent > 500 {
		return response.Error(c, fiber.StatusUnprocessableEntity, "percent out of range (-90 to 500)")
	}
	factor := 1 + req.Percent/100
	rt := strings.TrimSpace(req.RoomType)
	q := `UPDATE rooms SET price_per_night = ROUND(price_per_night * $1, 2), updated_at = now() WHERE hotel_id = $2`
	args := []interface{}{factor, tenantHotelID(c)}
	if rt != "" && !strings.EqualFold(rt, "all") {
		q += " AND room_type = $3"
		args = append(args, rt)
	}
	tag, err := tenantPool(c, h.pool).Exec(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	affected := int(tag.RowsAffected())
	_, _ = tenantPool(c, h.pool).Exec(c.Context(),
		`INSERT INTO rate_adjustments (id, hotel_id, percent, room_type, rooms_affected, created_at)
		 VALUES ($1,$2,$3,NULLIF($4,''),$5,now())`,
		uuid.New(), tenantHotelID(c), req.Percent, rt, affected)
	return response.OK(c, fiber.Map{"rooms_affected": affected, "percent": req.Percent})
}
