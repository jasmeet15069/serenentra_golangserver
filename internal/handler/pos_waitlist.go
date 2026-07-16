package handler

import (
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/hotelharmony/api/pkg/response"
)

// Restaurant waitlist (waitlists table, migration 025). Backs the restaurant
// floor's waitlist, which was previously local demo state.

type waitlistResponse struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	PartySize  int       `json:"party_size"`
	Section    string    `json:"section"`
	Phone      string    `json:"phone"`
	QuotedWait *int      `json:"quoted_wait"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
}

func (h *POSHandler) ListWaitlist(c *fiber.Ctx) error {
	rows, err := h.db(c).Query(c.Context(), `
		SELECT id, name, party_size, COALESCE(section,''), COALESCE(phone,''), quoted_wait, status, created_at
		FROM waitlists WHERE hotel_id = $1 AND status IN ('waiting','notified')
		ORDER BY created_at ASC`, h.hotelID(c))
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()
	items := make([]waitlistResponse, 0)
	for rows.Next() {
		var w waitlistResponse
		if err := rows.Scan(&w.ID, &w.Name, &w.PartySize, &w.Section, &w.Phone, &w.QuotedWait, &w.Status, &w.CreatedAt); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		items = append(items, w)
	}
	return response.OK(c, items)
}

func (h *POSHandler) AddWaitlist(c *fiber.Ctx) error {
	if !h.requireRoles(c, "admin", "hotel_admin", "super_admin", "receptionist", "food_manager", "waiter", "platform_admin") {
		return nil
	}
	var req struct {
		Name       string `json:"name"`
		PartySize  int    `json:"party_size"`
		Section    string `json:"section"`
		Phone      string `json:"phone"`
		QuotedWait *int   `json:"quoted_wait"`
	}
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if strings.TrimSpace(req.Name) == "" {
		return response.Error(c, fiber.StatusUnprocessableEntity, "guest name is required")
	}
	if req.PartySize < 1 {
		req.PartySize = 1
	}
	id := uuid.New()
	if _, err := h.db(c).Exec(c.Context(), `
		INSERT INTO waitlists (id, hotel_id, name, party_size, section, phone, quoted_wait, status, created_at, updated_at)
		VALUES ($1,$2,$3,$4,NULLIF($5,''),NULLIF($6,''),$7,'waiting',now(),now())`,
		id, h.hotelID(c), strings.TrimSpace(req.Name), req.PartySize, strings.TrimSpace(req.Section), strings.TrimSpace(req.Phone), req.QuotedWait); err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.Created(c, map[string]interface{}{"id": id, "name": strings.TrimSpace(req.Name)})
}

func (h *POSHandler) UpdateWaitlist(c *fiber.Ctx) error {
	if !h.requireRoles(c, "admin", "hotel_admin", "super_admin", "receptionist", "food_manager", "waiter", "platform_admin") {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid waitlist id")
	}
	var req struct {
		Status  *string `json:"status"`
		Section *string `json:"section"`
	}
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	sets := []string{}
	args := []interface{}{}
	idx := 1
	if req.Status != nil {
		valid := map[string]bool{"waiting": true, "notified": true, "seated": true, "left": true}
		if !valid[*req.Status] {
			return response.Error(c, fiber.StatusUnprocessableEntity, "invalid status")
		}
		sets = append(sets, "status = $"+strconv.Itoa(idx))
		args = append(args, *req.Status)
		idx++
	}
	if req.Section != nil {
		sets = append(sets, "section = NULLIF($"+strconv.Itoa(idx)+",'')")
		args = append(args, *req.Section)
		idx++
	}
	if len(sets) == 0 {
		return response.Error(c, fiber.StatusBadRequest, "no fields to update")
	}
	sets = append(sets, "updated_at = now()")
	args = append(args, id, h.hotelID(c))
	q := "UPDATE waitlists SET " + strings.Join(sets, ", ") + " WHERE id = $" + strconv.Itoa(idx) + " AND hotel_id = $" + strconv.Itoa(idx+1)
	tag, err := h.db(c).Exec(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	if tag.RowsAffected() == 0 {
		return response.Error(c, fiber.StatusNotFound, "waitlist entry not found")
	}
	return response.OK(c, map[string]interface{}{"id": id})
}

func (h *POSHandler) DeleteWaitlist(c *fiber.Ctx) error {
	if !h.requireRoles(c, "admin", "hotel_admin", "super_admin", "receptionist", "food_manager", "waiter", "platform_admin") {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid waitlist id")
	}
	tag, err := h.db(c).Exec(c.Context(), `DELETE FROM waitlists WHERE id = $1 AND hotel_id = $2`, id, h.hotelID(c))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	if tag.RowsAffected() == 0 {
		return response.Error(c, fiber.StatusNotFound, "waitlist entry not found")
	}
	return response.OK(c, map[string]string{"status": "removed"})
}
