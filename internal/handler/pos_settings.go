package handler

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/hotelharmony/api/pkg/response"
)

// Business / GST settings for the tenant hotel. These columns (migration 010)
// drive the restaurant Tax Invoice header when a bill is not tied to a specific
// outlet, and seed defaults for new outlets. Surfaced as /api/settings/business
// for the setup wizard.

type businessSettings struct {
	Name              string  `json:"name"`
	Address           *string `json:"address"`
	Currency          string  `json:"currency"`
	LegalEntityName   *string `json:"legal_entity_name"`
	RestaurantName    *string `json:"restaurant_name"`
	RestaurantAddress *string `json:"restaurant_address"`
	GSTIN             *string `json:"gstin"`
	FSSAI             *string `json:"fssai"`
	GSTState          *string `json:"gst_state"`
	PlaceOfSupply     *string `json:"place_of_supply"`
	HSNCode           string  `json:"hsn_code"`
	GSTRate           float64 `json:"gst_rate"`
}

const businessCols = `name, address, COALESCE(currency,'INR'), legal_entity_name, restaurant_name,
	restaurant_address, gstin, fssai, gst_state, place_of_supply, COALESCE(hsn_code,'996331'), COALESCE(gst_rate,0)`

func (h *POSHandler) GetBusinessSettings(c *fiber.Ctx) error {
	var b businessSettings
	err := h.db(c).QueryRow(c.Context(), `SELECT `+businessCols+` FROM hotels WHERE id = $1`, h.hotelID(c)).
		Scan(&b.Name, &b.Address, &b.Currency, &b.LegalEntityName, &b.RestaurantName, &b.RestaurantAddress,
			&b.GSTIN, &b.FSSAI, &b.GSTState, &b.PlaceOfSupply, &b.HSNCode, &b.GSTRate)
	if err != nil {
		return response.Error(c, fiber.StatusNotFound, "hotel not found")
	}
	return response.OK(c, b)
}

func (h *POSHandler) UpdateBusinessSettings(c *fiber.Ctx) error {
	if !h.requireRoles(c, "admin", "super_admin", "food_manager", "platform_admin") {
		return nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(c.Body(), &raw); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	allowed := map[string]bool{"name": true, "address": true, "currency": true,
		"legal_entity_name": true, "restaurant_name": true, "restaurant_address": true,
		"gstin": true, "fssai": true, "gst_state": true, "place_of_supply": true,
		"hsn_code": true, "gst_rate": true}
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
	args = append(args, h.hotelID(c))
	q := fmt.Sprintf(`UPDATE hotels SET %s WHERE id = $%d RETURNING %s`,
		strings.Join(set, ", "), i, businessCols)
	var b businessSettings
	err := h.db(c).QueryRow(c.Context(), q, args...).
		Scan(&b.Name, &b.Address, &b.Currency, &b.LegalEntityName, &b.RestaurantName, &b.RestaurantAddress,
			&b.GSTIN, &b.FSSAI, &b.GSTState, &b.PlaceOfSupply, &b.HSNCode, &b.GSTRate)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.OK(c, b)
}
