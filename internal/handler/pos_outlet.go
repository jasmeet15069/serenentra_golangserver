package handler

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/hotelharmony/api/pkg/response"
)

// Outlets are restaurants/bars/cafes under a hotel. They own dine-in tables and
// carry their own GST identity for tax invoices. An outlet may be standalone
// (serves walk-in/outsider customers) while still belonging to a tenant hotel.

type outletRow struct {
	ID              uuid.UUID `json:"id"`
	Name            string    `json:"name"`
	Code            *string   `json:"code"`
	Type            string    `json:"type"`
	IsStandalone    bool      `json:"is_standalone"`
	Address         *string   `json:"address"`
	LegalEntityName *string   `json:"legal_entity_name"`
	GSTIN           *string   `json:"gstin"`
	FSSAI           *string   `json:"fssai"`
	PlaceOfSupply   *string   `json:"place_of_supply"`
	HSNCode         string    `json:"hsn_code"`
	DefaultTaxRate  float64   `json:"default_tax_rate"`
	Currency        string    `json:"currency"`
	IsActive        bool      `json:"is_active"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

const outletCols = `id, name, code, type, is_standalone, address, legal_entity_name, gstin, fssai,
	place_of_supply, hsn_code, default_tax_rate, currency, is_active, created_at, updated_at`

func scanOutlet(row interface{ Scan(...interface{}) error }) (outletRow, error) {
	var o outletRow
	err := row.Scan(&o.ID, &o.Name, &o.Code, &o.Type, &o.IsStandalone, &o.Address, &o.LegalEntityName,
		&o.GSTIN, &o.FSSAI, &o.PlaceOfSupply, &o.HSNCode, &o.DefaultTaxRate, &o.Currency,
		&o.IsActive, &o.CreatedAt, &o.UpdatedAt)
	return o, err
}

func (h *POSHandler) ListOutlets(c *fiber.Ctx) error {
	rows, err := h.db(c).Query(c.Context(),
		`SELECT `+outletCols+` FROM outlets WHERE hotel_id = $1 ORDER BY name`, h.hotelID(c))
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to list outlets")
	}
	defer rows.Close()
	out := make([]outletRow, 0)
	for rows.Next() {
		o, scanErr := scanOutlet(rows)
		if scanErr != nil {
			return response.Error(c, fiber.StatusInternalServerError, "failed to scan outlet")
		}
		out = append(out, o)
	}
	return response.OK(c, out)
}

func (h *POSHandler) CreateOutlet(c *fiber.Ctx) error {
	if !h.requireRoles(c, "admin", "hotel_admin", "super_admin", "food_manager", "platform_admin") {
		return nil
	}
	var req struct {
		Name            string   `json:"name"`
		Code            *string  `json:"code"`
		Type            string   `json:"type"`
		IsStandalone    bool     `json:"is_standalone"`
		Address         *string  `json:"address"`
		LegalEntityName *string  `json:"legal_entity_name"`
		GSTIN           *string  `json:"gstin"`
		FSSAI           *string  `json:"fssai"`
		PlaceOfSupply   *string  `json:"place_of_supply"`
		HSNCode         *string  `json:"hsn_code"`
		DefaultTaxRate  *float64 `json:"default_tax_rate"`
		Currency        *string  `json:"currency"`
	}
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if strings.TrimSpace(req.Name) == "" {
		return response.Error(c, fiber.StatusUnprocessableEntity, "name is required")
	}
	if strings.TrimSpace(req.Type) == "" {
		req.Type = "restaurant"
	}
	hsn := "996331"
	if req.HSNCode != nil && strings.TrimSpace(*req.HSNCode) != "" {
		hsn = *req.HSNCode
	}
	taxRate := 5.0
	if req.DefaultTaxRate != nil {
		taxRate = *req.DefaultTaxRate
	}
	currency := "INR"
	if req.Currency != nil && strings.TrimSpace(*req.Currency) != "" {
		currency = *req.Currency
	}
	row := h.db(c).QueryRow(c.Context(), `
		INSERT INTO outlets (hotel_id, name, code, type, is_standalone, address, legal_entity_name,
			gstin, fssai, place_of_supply, hsn_code, default_tax_rate, currency)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		RETURNING `+outletCols,
		h.hotelID(c), req.Name, req.Code, req.Type, req.IsStandalone, req.Address, req.LegalEntityName,
		req.GSTIN, req.FSSAI, req.PlaceOfSupply, hsn, taxRate, currency)
	o, err := scanOutlet(row)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.Created(c, o)
}

func (h *POSHandler) UpdateOutlet(c *fiber.Ctx) error {
	if !h.requireRoles(c, "admin", "hotel_admin", "super_admin", "food_manager", "platform_admin") {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid outlet id")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(c.Body(), &raw); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	allowed := map[string]bool{"name": true, "code": true, "type": true, "is_standalone": true,
		"address": true, "legal_entity_name": true, "gstin": true, "fssai": true,
		"place_of_supply": true, "hsn_code": true, "default_tax_rate": true, "currency": true, "is_active": true}
	set := []string{}
	args := []interface{}{}
	i := 1
	for k, rawVal := range raw {
		if !allowed[k] {
			continue
		}
		var v interface{}
		if err := json.Unmarshal(rawVal, &v); err != nil {
			continue
		}
		set = append(set, fmt.Sprintf("%s = $%d", k, i))
		args = append(args, v)
		i++
	}
	if len(set) == 0 {
		return response.Error(c, fiber.StatusBadRequest, "no fields to update")
	}
	set = append(set, "updated_at = now()")
	args = append(args, id, h.hotelID(c))
	q := fmt.Sprintf(`UPDATE outlets SET %s WHERE id = $%d AND hotel_id = $%d RETURNING %s`,
		strings.Join(set, ", "), i, i+1, outletCols)
	o, scanErr := scanOutlet(h.db(c).QueryRow(c.Context(), q, args...))
	if scanErr != nil {
		return response.Error(c, fiber.StatusBadRequest, scanErr.Error())
	}
	return response.OK(c, o)
}
