package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/hotelharmony/api/pkg/response"
)

// This file implements the dine-in Restaurant POS workflow on top of the same
// POSHandler used by the flat pos_orders endpoints. The model is:
//
//	restaurant_tables -> dining_sessions -> kots -> kot_items
//	dining_sessions   -> bills           -> bill_payments
//
// A dining session is the aggregate root for one seating. Each round of ordering
// ("add more items") is a new KOT under the session; the bill consolidates every
// non-cancelled KOT item and is settled by one or more payments (cash/card/upi).

// ---------------------------------------------------------------------------
// money + state-machine helpers
// ---------------------------------------------------------------------------

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// kotTransitions is the allowed KOT status state machine. A move not listed here
// is rejected with 409 before any write happens.
var kotTransitions = map[string][]string{
	"draft":        {"sent", "cancelled"},
	"sent":         {"acknowledged", "preparing", "cancelled"},
	"acknowledged": {"preparing", "cancelled"},
	"preparing":    {"ready"},
	"ready":        {"served"},
}

func canTransition(table map[string][]string, from, to string) bool {
	for _, t := range table[from] {
		if t == to {
			return true
		}
	}
	return false
}

func validPaymentMethod(m string) bool {
	switch m {
	case "cash", "card", "upi", "room_charge", "wallet":
		return true
	}
	return false
}

func genNumber(prefix string) string {
	return fmt.Sprintf("%s-%s-%d", prefix, time.Now().UTC().Format("20060102"), time.Now().UnixMilli()%1000000)
}

// ---------------------------------------------------------------------------
// Tables
// ---------------------------------------------------------------------------

type tableRow struct {
	ID          uuid.UUID  `json:"id"`
	OutletID    *uuid.UUID `json:"outlet_id"`
	TableNumber string     `json:"table_number"`
	Section     *string    `json:"section"`
	Seats       int        `json:"seats"`
	Status      string     `json:"status"`
	PosX        *int       `json:"pos_x"`
	PosY        *int       `json:"pos_y"`
	IsActive    bool       `json:"is_active"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

const tableCols = `id, outlet_id, table_number, section, seats, status, pos_x, pos_y, is_active, created_at, updated_at`

func scanTable(row interface{ Scan(...interface{}) error }) (tableRow, error) {
	var t tableRow
	err := row.Scan(&t.ID, &t.OutletID, &t.TableNumber, &t.Section, &t.Seats, &t.Status, &t.PosX, &t.PosY,
		&t.IsActive, &t.CreatedAt, &t.UpdatedAt)
	return t, err
}

func (h *POSHandler) ListTables(c *fiber.Ctx) error {
	q := `SELECT ` + tableCols + ` FROM restaurant_tables WHERE hotel_id = $1`
	args := []interface{}{h.hotelID(c)}
	if status := c.Query("status"); status != "" {
		q += fmt.Sprintf(" AND status = $%d", len(args)+1)
		args = append(args, status)
	}
	if outletID := c.Query("outlet_id"); outletID != "" {
		q += fmt.Sprintf(" AND outlet_id = $%d", len(args)+1)
		args = append(args, outletID)
	}
	q += " ORDER BY table_number"
	rows, err := h.db(c).Query(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to list tables")
	}
	defer rows.Close()
	out := make([]tableRow, 0)
	for rows.Next() {
		t, scanErr := scanTable(rows)
		if scanErr != nil {
			return response.Error(c, fiber.StatusInternalServerError, "failed to scan table")
		}
		out = append(out, t)
	}
	return response.OK(c, out)
}

func (h *POSHandler) CreateTable(c *fiber.Ctx) error {
	if !h.requireRoles(c, "admin", "hotel_admin", "super_admin", "food_manager", "platform_admin") {
		return nil
	}
	var req struct {
		OutletID    *uuid.UUID `json:"outlet_id"`
		TableNumber string     `json:"table_number"`
		Section     *string    `json:"section"`
		Seats       int        `json:"seats"`
		PosX        *int       `json:"pos_x"`
		PosY        *int       `json:"pos_y"`
	}
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if strings.TrimSpace(req.TableNumber) == "" {
		return response.Error(c, fiber.StatusUnprocessableEntity, "table_number is required")
	}
	if req.Seats <= 0 {
		req.Seats = 2
	}
	row := h.db(c).QueryRow(c.Context(), `
		INSERT INTO restaurant_tables (hotel_id, outlet_id, table_number, section, seats, pos_x, pos_y)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING `+tableCols,
		h.hotelID(c), req.OutletID, req.TableNumber, req.Section, req.Seats, req.PosX, req.PosY)
	t, err := scanTable(row)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.Created(c, t)
}

func (h *POSHandler) UpdateTable(c *fiber.Ctx) error {
	if !h.requireRoles(c, "admin", "hotel_admin", "super_admin", "food_manager", "platform_admin") {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid table id")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(c.Body(), &raw); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	allowed := map[string]bool{"table_number": true, "section": true, "seats": true,
		"status": true, "pos_x": true, "pos_y": true, "is_active": true}
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
	q := fmt.Sprintf(`UPDATE restaurant_tables SET %s WHERE id = $%d AND hotel_id = $%d RETURNING %s`,
		strings.Join(set, ", "), i, i+1, tableCols)
	t, scanErr := scanTable(h.db(c).QueryRow(c.Context(), q, args...))
	if scanErr != nil {
		return response.Error(c, fiber.StatusBadRequest, scanErr.Error())
	}
	return response.OK(c, t)
}

// CleanTable moves a table from cleaning back to available for the next guest.
func (h *POSHandler) CleanTable(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid table id")
	}
	ct, err := h.db(c).Exec(c.Context(), `
		UPDATE restaurant_tables SET status = 'available', updated_at = now()
		WHERE id = $1 AND hotel_id = $2 AND status = 'cleaning'`, id, h.hotelID(c))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	if ct.RowsAffected() == 0 {
		return response.Error(c, fiber.StatusConflict, "table is not in cleaning status")
	}
	return response.OK(c, map[string]string{"status": "available"})
}

// ---------------------------------------------------------------------------
// Dining sessions
// ---------------------------------------------------------------------------

func (h *POSHandler) ListSessions(c *fiber.Ctx) error {
	q := `SELECT id, session_number, table_id, covers, status, guest_name, opened_at, billed_at, closed_at
	      FROM dining_sessions WHERE hotel_id = $1`
	args := []interface{}{h.hotelID(c)}
	if status := c.Query("status"); status != "" {
		q += " AND status = $2"
		args = append(args, status)
	}
	q += " ORDER BY opened_at DESC"
	rows, err := h.db(c).Query(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to list sessions")
	}
	defer rows.Close()
	out := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, tableID uuid.UUID
		var num, status string
		var covers int
		var guest *string
		var opened time.Time
		var billed, closed *time.Time
		if err := rows.Scan(&id, &num, &tableID, &covers, &status, &guest, &opened, &billed, &closed); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, "failed to scan session")
		}
		out = append(out, map[string]interface{}{
			"id": id, "session_number": num, "table_id": tableID, "covers": covers,
			"status": status, "guest_name": guest, "opened_at": opened,
			"billed_at": billed, "closed_at": closed,
		})
	}
	return response.OK(c, out)
}

// OpenSession assigns a table to an arriving dine-in customer and opens a session.
func (h *POSHandler) OpenSession(c *fiber.Ctx) error {
	tableID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid table id")
	}
	var req struct {
		Covers       int        `json:"covers"`
		GuestName    *string    `json:"guest_name"`
		CustomerType string     `json:"customer_type"` // walk_in | hotel_guest
		GuestStayID  *uuid.UUID `json:"guest_stay_id"` // set when charging to a room
	}
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if req.CustomerType != "hotel_guest" {
		req.CustomerType = "walk_in"
	}
	if req.Covers <= 0 {
		req.Covers = 1
	}
	hotelID := h.hotelID(c)
	ctx := c.Context()

	tx, err := h.db(c).Begin(ctx)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to start transaction")
	}
	defer tx.Rollback(ctx)

	var tableStatus string
	var tableOutlet *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT status, outlet_id FROM restaurant_tables WHERE id = $1 AND hotel_id = $2 FOR UPDATE`,
		tableID, hotelID).Scan(&tableStatus, &tableOutlet); err != nil {
		return response.Error(c, fiber.StatusNotFound, "table not found")
	}
	if tableStatus != "available" && tableStatus != "reserved" {
		return response.Error(c, fiber.StatusConflict, "table is not available")
	}

	sessionNumber := genNumber("DIN")
	var sessionID uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO dining_sessions (hotel_id, session_number, table_id, outlet_id, covers, guest_name, customer_type, guest_stay_id, opened_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
		hotelID, sessionNumber, tableID, tableOutlet, req.Covers, req.GuestName, req.CustomerType, req.GuestStayID, h.userID(c)).Scan(&sessionID)
	if err != nil {
		return response.Error(c, fiber.StatusConflict, "table already has an active session")
	}
	if _, err := tx.Exec(ctx, `UPDATE restaurant_tables SET status = 'occupied', updated_at = now() WHERE id = $1`, tableID); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to occupy table")
	}
	if err := tx.Commit(ctx); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to open session")
	}
	h.audit(ctx, c, "session.opened", "dining_session", sessionID)
	return response.Created(c, map[string]interface{}{
		"id": sessionID, "session_number": sessionNumber, "table_id": tableID,
		"outlet_id": tableOutlet, "covers": req.Covers, "customer_type": req.CustomerType, "status": "open",
	})
}

// GetSession returns the full session aggregate: KOTs with items and the bill.
func (h *POSHandler) GetSession(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid session id")
	}
	hotelID := h.hotelID(c)
	ctx := c.Context()

	var num, status string
	var tableID uuid.UUID
	var covers int
	var guest *string
	var opened time.Time
	if err := h.db(c).QueryRow(ctx, `
		SELECT session_number, table_id, covers, status, guest_name, opened_at
		FROM dining_sessions WHERE id = $1 AND hotel_id = $2`, id, hotelID).
		Scan(&num, &tableID, &covers, &status, &guest, &opened); err != nil {
		return response.Error(c, fiber.StatusNotFound, "session not found")
	}

	kots, err := h.loadKOTs(ctx, c, id)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to load kots")
	}
	bill, _ := h.loadBillBySession(ctx, c, id)

	return response.OK(c, map[string]interface{}{
		"id": id, "session_number": num, "table_id": tableID, "covers": covers,
		"status": status, "guest_name": guest, "opened_at": opened,
		"kots": kots, "bill": bill,
	})
}

// CloseSession finalises a settled session and releases the table for cleaning.
func (h *POSHandler) CloseSession(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid session id")
	}
	hotelID := h.hotelID(c)
	ctx := c.Context()

	tx, err := h.db(c).Begin(ctx)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to start transaction")
	}
	defer tx.Rollback(ctx)

	var status string
	var tableID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT status, table_id FROM dining_sessions WHERE id = $1 AND hotel_id = $2 FOR UPDATE`,
		id, hotelID).Scan(&status, &tableID); err != nil {
		return response.Error(c, fiber.StatusNotFound, "session not found")
	}
	if status != "settled" {
		return response.Error(c, fiber.StatusConflict, "session must be settled (bill paid) before closing")
	}
	if _, err := tx.Exec(ctx, `UPDATE dining_sessions SET status = 'closed', closed_by = $2, closed_at = now(), updated_at = now() WHERE id = $1`,
		id, h.userID(c)); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to close session")
	}
	if _, err := tx.Exec(ctx, `UPDATE restaurant_tables SET status = 'cleaning', updated_at = now() WHERE id = $1`, tableID); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to release table")
	}
	if err := tx.Commit(ctx); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to close session")
	}
	h.audit(ctx, c, "session.closed", "dining_session", id)
	return response.OK(c, map[string]interface{}{"id": id, "status": "closed", "table_status": "cleaning"})
}

// ---------------------------------------------------------------------------
// KOTs
// ---------------------------------------------------------------------------

func (h *POSHandler) loadKOTs(ctx context.Context, c *fiber.Ctx, sessionID uuid.UUID) ([]map[string]interface{}, error) {
	rows, err := h.db(c).Query(ctx, `
		SELECT id, kot_number, round_no, status, station, notes, sent_at, ready_at, served_at, created_at
		FROM kots WHERE dining_session_id = $1 ORDER BY round_no`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	kots := make([]map[string]interface{}, 0)
	ids := make([]uuid.UUID, 0)
	for rows.Next() {
		var id uuid.UUID
		var num, status string
		var round int
		var station, notes *string
		var sentAt, readyAt, servedAt *time.Time
		var created time.Time
		if err := rows.Scan(&id, &num, &round, &status, &station, &notes, &sentAt, &readyAt, &servedAt, &created); err != nil {
			return nil, err
		}
		ids = append(ids, id)
		kots = append(kots, map[string]interface{}{
			"id": id, "kot_number": num, "round_no": round, "status": status,
			"station": station, "notes": notes, "sent_at": sentAt, "ready_at": readyAt,
			"served_at": servedAt, "created_at": created, "items": []interface{}{},
		})
	}
	rows.Close()

	// attach items per KOT
	for idx, k := range kots {
		irows, err := h.db(c).Query(ctx, `
			SELECT id, menu_item_id, item_name, quantity, unit_price, modifiers, line_total, notes, status
			FROM kot_items WHERE kot_id = $1 ORDER BY created_at`, ids[idx])
		if err != nil {
			return nil, err
		}
		items := make([]map[string]interface{}, 0)
		for irows.Next() {
			var iid uuid.UUID
			var menuID *uuid.UUID
			var name, istatus string
			var qty int
			var unit, line float64
			var mods json.RawMessage
			var inotes *string
			if err := irows.Scan(&iid, &menuID, &name, &qty, &unit, &mods, &line, &inotes, &istatus); err != nil {
				irows.Close()
				return nil, err
			}
			if len(mods) == 0 {
				mods = json.RawMessage("[]")
			}
			items = append(items, map[string]interface{}{
				"id": iid, "menu_item_id": menuID, "item_name": name, "quantity": qty,
				"unit_price": unit, "modifiers": mods, "line_total": line, "notes": inotes, "status": istatus,
			})
		}
		irows.Close()
		k["items"] = items
	}
	return kots, nil
}

// CreateKOT records a new round of ordering on a session and queues it as a draft.
func (h *POSHandler) CreateKOT(c *fiber.Ctx) error {
	sessionID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid session id")
	}
	var req struct {
		Station *string `json:"station"`
		Notes   *string `json:"notes"`
		Items   []struct {
			MenuItemID *uuid.UUID      `json:"menu_item_id"`
			ItemName   string          `json:"item_name"`
			Quantity   int             `json:"quantity"`
			UnitPrice  float64         `json:"unit_price"`
			Modifiers  json.RawMessage `json:"modifiers"`
			Notes      *string         `json:"notes"`
		} `json:"items"`
	}
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if len(req.Items) == 0 {
		return response.Error(c, fiber.StatusUnprocessableEntity, "at least one item is required")
	}
	for _, it := range req.Items {
		if strings.TrimSpace(it.ItemName) == "" || it.Quantity <= 0 {
			return response.Error(c, fiber.StatusUnprocessableEntity, "each item needs a name and quantity > 0")
		}
	}
	hotelID := h.hotelID(c)
	ctx := c.Context()

	tx, err := h.db(c).Begin(ctx)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to start transaction")
	}
	defer tx.Rollback(ctx)

	var sStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM dining_sessions WHERE id = $1 AND hotel_id = $2 FOR UPDATE`,
		sessionID, hotelID).Scan(&sStatus); err != nil {
		return response.Error(c, fiber.StatusNotFound, "session not found")
	}
	if sStatus != "open" && sStatus != "billed" {
		return response.Error(c, fiber.StatusConflict, "cannot add items to a "+sStatus+" session")
	}

	var roundNo int
	_ = tx.QueryRow(ctx, `SELECT COALESCE(MAX(round_no),0)+1 FROM kots WHERE dining_session_id = $1`, sessionID).Scan(&roundNo)

	kotNumber := genNumber("KOT")
	var kotID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO kots (hotel_id, dining_session_id, kot_number, round_no, station, notes, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		hotelID, sessionID, kotNumber, roundNo, req.Station, req.Notes, h.userID(c)).Scan(&kotID); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to create kot")
	}

	for _, it := range req.Items {
		mods := it.Modifiers
		if len(mods) == 0 {
			mods = json.RawMessage("[]")
		}
		lineTotal := round2((it.UnitPrice + sumModifierDeltas(mods)) * float64(it.Quantity))
		if _, err := tx.Exec(ctx, `
			INSERT INTO kot_items (hotel_id, kot_id, menu_item_id, item_name, quantity, unit_price, modifiers, line_total, notes)
			VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9)`,
			hotelID, kotID, it.MenuItemID, it.ItemName, it.Quantity, it.UnitPrice, string(mods), lineTotal, it.Notes); err != nil {
			return response.Error(c, fiber.StatusBadRequest, "failed to add kot item: "+err.Error())
		}
	}

	// A late add-on after a bill was generated reopens the session for ordering.
	if sStatus == "billed" {
		_, _ = tx.Exec(ctx, `UPDATE dining_sessions SET status = 'open', updated_at = now() WHERE id = $1`, sessionID)
	}

	if err := tx.Commit(ctx); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to create kot")
	}
	h.audit(ctx, c, "kot.created", "kot", kotID)
	return response.Created(c, map[string]interface{}{
		"id": kotID, "kot_number": kotNumber, "round_no": roundNo, "status": "draft",
	})
}

func sumModifierDeltas(raw json.RawMessage) float64 {
	var mods []struct {
		PriceDelta float64 `json:"price_delta"`
	}
	if err := json.Unmarshal(raw, &mods); err != nil {
		return 0
	}
	var sum float64
	for _, m := range mods {
		sum += m.PriceDelta
	}
	return sum
}

// SendKOT dispatches a draft KOT to the kitchen.
func (h *POSHandler) SendKOT(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid kot id")
	}
	ct, err := h.db(c).Exec(c.Context(), `
		UPDATE kots SET status = 'sent', sent_at = now(), updated_at = now()
		WHERE id = $1 AND hotel_id = $2 AND status = 'draft'`, id, h.hotelID(c))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	if ct.RowsAffected() == 0 {
		return response.Error(c, fiber.StatusConflict, "kot is not in draft status")
	}
	h.audit(c.Context(), c, "kot.sent", "kot", id)
	return response.OK(c, map[string]interface{}{"id": id, "status": "sent"})
}

// UpdateKOTStatus advances a KOT through the kitchen state machine, mirroring the
// status onto its non-cancelled items for preparing/ready/served transitions.
func (h *POSHandler) UpdateKOTStatus(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid kot id")
	}
	var req struct {
		Status string `json:"status"`
	}
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	hotelID := h.hotelID(c)
	ctx := c.Context()

	tx, err := h.db(c).Begin(ctx)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to start transaction")
	}
	defer tx.Rollback(ctx)

	var current string
	if err := tx.QueryRow(ctx, `SELECT status FROM kots WHERE id = $1 AND hotel_id = $2 FOR UPDATE`,
		id, hotelID).Scan(&current); err != nil {
		return response.Error(c, fiber.StatusNotFound, "kot not found")
	}
	if !canTransition(kotTransitions, current, req.Status) {
		return response.Error(c, fiber.StatusConflict, "invalid transition: "+current+" -> "+req.Status)
	}

	set := "status = $1, updated_at = now()"
	switch req.Status {
	case "ready":
		set += ", ready_at = now()"
	case "served":
		set += ", served_at = now()"
	}
	if _, err := tx.Exec(ctx, `UPDATE kots SET `+set+` WHERE id = $2`, req.Status, id); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to update kot")
	}
	if req.Status == "preparing" || req.Status == "ready" || req.Status == "served" {
		if _, err := tx.Exec(ctx, `UPDATE kot_items SET status = $1, updated_at = now() WHERE kot_id = $2 AND status <> 'cancelled'`,
			req.Status, id); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, "failed to update kot items")
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to update kot")
	}
	h.audit(ctx, c, "kot.status_changed", "kot", id)
	return response.OK(c, map[string]interface{}{"id": id, "status": req.Status})
}

// ---------------------------------------------------------------------------
// Bills + payments
// ---------------------------------------------------------------------------

type billRow struct {
	ID             uuid.UUID `json:"id"`
	SessionID      uuid.UUID `json:"dining_session_id"`
	BillNumber     string    `json:"bill_number"`
	Status         string    `json:"status"`
	Subtotal       float64   `json:"subtotal"`
	DiscountType   *string   `json:"discount_type"`
	DiscountValue  float64   `json:"discount_value"`
	DiscountAmount float64   `json:"discount_amount"`
	TaxRate        float64   `json:"tax_rate"`
	TaxAmount      float64   `json:"tax_amount"`
	TipType        *string   `json:"tip_type"`
	TipValue       float64   `json:"tip_value"`
	TipAmount      float64   `json:"tip_amount"`
	RoundingAdjust float64   `json:"rounding_adjust"`
	TotalAmount    float64   `json:"total_amount"`
	AmountPaid     float64   `json:"amount_paid"`
	AmountDue      float64   `json:"amount_due"`
	Currency       string    `json:"currency"`
}

const billCols = `id, dining_session_id, bill_number, status, subtotal, discount_type, discount_value,
	discount_amount, tax_rate, tax_amount, tip_type, tip_value, tip_amount, rounding_adjust,
	total_amount, amount_paid, amount_due, currency`

func scanBill(row interface{ Scan(...interface{}) error }) (billRow, error) {
	var b billRow
	err := row.Scan(&b.ID, &b.SessionID, &b.BillNumber, &b.Status, &b.Subtotal, &b.DiscountType,
		&b.DiscountValue, &b.DiscountAmount, &b.TaxRate, &b.TaxAmount, &b.TipType, &b.TipValue,
		&b.TipAmount, &b.RoundingAdjust, &b.TotalAmount, &b.AmountPaid, &b.AmountDue, &b.Currency)
	return b, err
}

// computeTotals recalculates every derived money field from subtotal + the
// discount/tax/tip inputs. Tax and tip are charged on the post-discount base.
func computeTotals(b *billRow) {
	if b.DiscountType != nil && *b.DiscountType == "percent" {
		b.DiscountAmount = round2(b.Subtotal * b.DiscountValue / 100)
	} else if b.DiscountType != nil && *b.DiscountType == "flat" {
		b.DiscountAmount = b.DiscountValue
	} else {
		b.DiscountAmount = 0
	}
	if b.DiscountAmount > b.Subtotal {
		b.DiscountAmount = b.Subtotal
	}
	taxable := b.Subtotal - b.DiscountAmount
	b.TaxAmount = round2(taxable * b.TaxRate / 100)
	if b.TipType != nil && *b.TipType == "percent" {
		b.TipAmount = round2(taxable * b.TipValue / 100)
	} else if b.TipType != nil && *b.TipType == "flat" {
		b.TipAmount = b.TipValue
	} else {
		b.TipAmount = 0
	}
	b.TotalAmount = round2(taxable + b.TaxAmount + b.TipAmount + b.RoundingAdjust)
	b.AmountDue = round2(b.TotalAmount - b.AmountPaid)
}

func (h *POSHandler) loadBillBySession(ctx context.Context, c *fiber.Ctx, sessionID uuid.UUID) (*billRow, error) {
	row := h.db(c).QueryRow(ctx, `SELECT `+billCols+` FROM bills WHERE dining_session_id = $1 AND status <> 'void'`, sessionID)
	b, err := scanBill(row)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// GenerateBill produces the consolidated bill for a session. It is idempotent:
// calling it again returns the existing open/finalized bill rather than duplicating.
func (h *POSHandler) GenerateBill(c *fiber.Ctx) error {
	sessionID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid session id")
	}
	hotelID := h.hotelID(c)
	ctx := c.Context()

	tx, err := h.db(c).Begin(ctx)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to start transaction")
	}
	defer tx.Rollback(ctx)

	var sStatus string
	var tableID uuid.UUID
	var sessionOutlet *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT status, table_id, outlet_id FROM dining_sessions WHERE id = $1 AND hotel_id = $2 FOR UPDATE`,
		sessionID, hotelID).Scan(&sStatus, &tableID, &sessionOutlet); err != nil {
		return response.Error(c, fiber.StatusNotFound, "session not found")
	}
	if sStatus == "closed" || sStatus == "cancelled" {
		return response.Error(c, fiber.StatusConflict, "session is "+sStatus)
	}

	// Idempotency: return the existing non-void bill if one exists.
	if existing, err := scanBill(tx.QueryRow(ctx, `SELECT `+billCols+` FROM bills WHERE dining_session_id = $1 AND status <> 'void'`, sessionID)); err == nil {
		return response.OK(c, existing)
	}

	var subtotal float64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(ki.line_total),0)
		FROM kot_items ki JOIN kots k ON k.id = ki.kot_id
		WHERE k.dining_session_id = $1 AND ki.status <> 'cancelled'`, sessionID).Scan(&subtotal); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to total items")
	}
	if subtotal <= 0 {
		return response.Error(c, fiber.StatusUnprocessableEntity, "no billable items in this session")
	}

	// Default the GST rate from the outlet (falling back to the hotel's gst_rate),
	// so the bill and its invoice show CGST/SGST without a manual adjustment.
	taxRate := 0.0
	currency := "INR"
	if sessionOutlet != nil {
		_ = tx.QueryRow(ctx, `SELECT default_tax_rate, currency FROM outlets WHERE id = $1`, *sessionOutlet).Scan(&taxRate, &currency)
	} else {
		_ = tx.QueryRow(ctx, `SELECT COALESCE(gst_rate, 0) FROM hotels WHERE id = $1`, hotelID).Scan(&taxRate)
	}

	b := billRow{Subtotal: round2(subtotal), TaxRate: taxRate, Currency: currency}
	computeTotals(&b)
	billNumber := genNumber("BILL")

	row := tx.QueryRow(ctx, `
		INSERT INTO bills (hotel_id, dining_session_id, outlet_id, bill_number, status, subtotal,
			tax_rate, tax_amount, total_amount, amount_due, currency, generated_by)
		VALUES ($1,$2,$3,$4,'open',$5,$6,$7,$8,$9,$10,$11) RETURNING `+billCols,
		hotelID, sessionID, sessionOutlet, billNumber, b.Subtotal, b.TaxRate, b.TaxAmount,
		b.TotalAmount, b.AmountDue, b.Currency, h.userID(c))
	created, err := scanBill(row)
	if err != nil {
		return response.Error(c, fiber.StatusConflict, "a bill already exists for this session")
	}
	if _, err := tx.Exec(ctx, `UPDATE dining_sessions SET status = 'billed', billed_at = now(), updated_at = now() WHERE id = $1`, sessionID); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to mark session billed")
	}
	_, _ = tx.Exec(ctx, `UPDATE restaurant_tables SET status = 'billed', updated_at = now() WHERE id = $1`, tableID)

	if err := tx.Commit(ctx); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to generate bill")
	}
	h.audit(ctx, c, "bill.generated", "bill", created.ID)
	return response.Created(c, created)
}

// UpdateBill sets discount / tip / tax_rate on an open bill and recomputes totals.
func (h *POSHandler) UpdateBill(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid bill id")
	}
	var req struct {
		DiscountType   *string  `json:"discount_type"`
		DiscountValue  *float64 `json:"discount_value"`
		TaxRate        *float64 `json:"tax_rate"`
		TipType        *string  `json:"tip_type"`
		TipValue       *float64 `json:"tip_value"`
		RoundingAdjust *float64 `json:"rounding_adjust"`
	}
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	hotelID := h.hotelID(c)
	ctx := c.Context()

	tx, err := h.db(c).Begin(ctx)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to start transaction")
	}
	defer tx.Rollback(ctx)

	b, err := scanBill(tx.QueryRow(ctx, `SELECT `+billCols+` FROM bills WHERE id = $1 AND hotel_id = $2 FOR UPDATE`, id, hotelID))
	if err != nil {
		return response.Error(c, fiber.StatusNotFound, "bill not found")
	}
	if b.Status != "open" {
		return response.Error(c, fiber.StatusConflict, "bill can only be edited while open")
	}
	if req.DiscountType != nil {
		b.DiscountType = req.DiscountType
	}
	if req.DiscountValue != nil {
		b.DiscountValue = *req.DiscountValue
	}
	if req.TaxRate != nil {
		b.TaxRate = *req.TaxRate
	}
	if req.TipType != nil {
		b.TipType = req.TipType
	}
	if req.TipValue != nil {
		b.TipValue = *req.TipValue
	}
	if req.RoundingAdjust != nil {
		b.RoundingAdjust = *req.RoundingAdjust
	}
	if b.DiscountValue < 0 || b.TipValue < 0 {
		return response.Error(c, fiber.StatusUnprocessableEntity, "discount and tip must be non-negative")
	}
	computeTotals(&b)

	updated, err := scanBill(tx.QueryRow(ctx, `
		UPDATE bills SET discount_type=$1, discount_value=$2, discount_amount=$3, tax_rate=$4, tax_amount=$5,
			tip_type=$6, tip_value=$7, tip_amount=$8, rounding_adjust=$9, total_amount=$10, amount_due=$11, updated_at=now()
		WHERE id=$12 RETURNING `+billCols,
		b.DiscountType, b.DiscountValue, b.DiscountAmount, b.TaxRate, b.TaxAmount,
		b.TipType, b.TipValue, b.TipAmount, b.RoundingAdjust, b.TotalAmount, b.AmountDue, id))
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to update bill")
	}
	if err := tx.Commit(ctx); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to update bill")
	}
	return response.OK(c, updated)
}

// FinalizeBill locks the bill so items and adjustments can no longer change.
func (h *POSHandler) FinalizeBill(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid bill id")
	}
	ct, err := h.db(c).Exec(c.Context(), `
		UPDATE bills SET status = 'finalized', finalized_at = now(), updated_at = now()
		WHERE id = $1 AND hotel_id = $2 AND status = 'open'`, id, h.hotelID(c))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	if ct.RowsAffected() == 0 {
		return response.Error(c, fiber.StatusConflict, "bill is not open")
	}
	h.audit(c.Context(), c, "bill.finalized", "bill", id)
	return response.OK(c, map[string]interface{}{"id": id, "status": "finalized"})
}

// AddBillPayment records a payment (cash/card/upi). When the bill is fully paid
// it transitions to paid and settles the session.
func (h *POSHandler) AddBillPayment(c *fiber.Ctx) error {
	if !h.requireRoles(c, "admin", "hotel_admin", "super_admin", "receptionist", "cashier", "food_manager", "platform_admin") {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid bill id")
	}
	var req struct {
		Method       string   `json:"method"`
		Amount       float64  `json:"amount"`
		Tendered     *float64 `json:"tendered"`
		TxnReference *string  `json:"txn_reference"`
		Gateway      *string  `json:"gateway"`
	}
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if !validPaymentMethod(req.Method) {
		return response.Error(c, fiber.StatusUnprocessableEntity, "unsupported payment method")
	}
	if req.Amount <= 0 {
		return response.Error(c, fiber.StatusUnprocessableEntity, "amount must be positive")
	}
	hotelID := h.hotelID(c)
	ctx := c.Context()

	tx, err := h.db(c).Begin(ctx)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to start transaction")
	}
	defer tx.Rollback(ctx)

	b, err := scanBill(tx.QueryRow(ctx, `SELECT `+billCols+` FROM bills WHERE id = $1 AND hotel_id = $2 FOR UPDATE`, id, hotelID))
	if err != nil {
		return response.Error(c, fiber.StatusNotFound, "bill not found")
	}
	if b.Status != "finalized" && b.Status != "partially_paid" {
		return response.Error(c, fiber.StatusConflict, "bill must be finalized before payment")
	}

	changeDue := 0.0
	if req.Method == "cash" && req.Tendered != nil {
		if *req.Tendered < req.Amount {
			return response.Error(c, fiber.StatusUnprocessableEntity, "tendered is less than amount")
		}
		changeDue = round2(*req.Tendered - req.Amount)
	}
	if (req.Method == "card" || req.Method == "upi") && (req.TxnReference == nil || strings.TrimSpace(*req.TxnReference) == "") {
		return response.Error(c, fiber.StatusUnprocessableEntity, "txn_reference is required for card/upi")
	}
	// Non-cash overpayment is rejected; cash overpayment becomes change.
	if req.Method != "cash" && round2(b.AmountPaid+req.Amount) > b.TotalAmount {
		return response.Error(c, fiber.StatusUnprocessableEntity, "payment exceeds amount due")
	}

	paymentNumber := genNumber("PAY")
	var payID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO bill_payments (hotel_id, bill_id, payment_number, method, amount, tendered, change_due, status, txn_reference, gateway, received_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'completed',$8,$9,$10) RETURNING id`,
		hotelID, id, paymentNumber, req.Method, req.Amount, req.Tendered, changeDue, req.TxnReference, req.Gateway, h.userID(c)).Scan(&payID); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "failed to record payment: "+err.Error())
	}

	// Cash change is not "paid toward" the bill, so credit only up to the due amount.
	credited := req.Amount
	if req.Method == "cash" && changeDue > 0 {
		credited = req.Amount // amount already excludes change (tendered - change)
	}

	// Mirror the payment into the main `payments` table so restaurant revenue is
	// visible on the dashboard. The dashboard (dashboard_repository.go) reads
	// revenue from `payments`, NOT `bill_payments`, so dine-in sales were invisible
	// in revenue_today / the revenue trend / the department breakdown. category
	// 'fnb' feeds the F&B slice; status 'completed' makes revenue_today count it.
	// In-tx so it is atomic with the bill payment — a bill payment that recorded
	// must also show as revenue, and vice-versa. No double-count: `bill_payments`
	// is never summed for reporting (only read per-single-bill for the receipt).
	if _, err := tx.Exec(ctx, `
		INSERT INTO payments (id, hotel_id, payment_number, amount, payment_method, status, processed_by, notes, category, created_at)
		VALUES ($1,$2,$3,$4,$5,'completed',$6,$7,'fnb',now())`,
		uuid.New(), hotelID, paymentNumber, credited, req.Method, h.userID(c), "Dine-in bill "+b.BillNumber); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to record revenue: "+err.Error())
	}

	b.AmountPaid = round2(b.AmountPaid + credited)
	b.AmountDue = round2(b.TotalAmount - b.AmountPaid)

	newStatus := "partially_paid"
	if b.AmountDue <= 0 {
		newStatus = "paid"
		b.AmountDue = 0
	}
	if _, err := tx.Exec(ctx, `UPDATE bills SET amount_paid=$1, amount_due=$2, status=$3, paid_at=CASE WHEN $3='paid' THEN now() ELSE paid_at END, updated_at=now() WHERE id=$4`,
		b.AmountPaid, b.AmountDue, newStatus, id); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to update bill")
	}
	if newStatus == "paid" {
		if _, err := tx.Exec(ctx, `UPDATE dining_sessions SET status='settled', updated_at=now() WHERE id=$1`, b.SessionID); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, "failed to settle session")
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to record payment")
	}
	h.audit(ctx, c, "payment.recorded", "bill_payment", payID)
	return response.Created(c, map[string]interface{}{
		"id": payID, "payment_number": paymentNumber, "method": req.Method, "amount": req.Amount,
		"change_due": changeDue, "bill_status": newStatus, "amount_paid": b.AmountPaid, "amount_due": b.AmountDue,
	})
}

// BillReceipt returns the printable receipt payload: bill, line items, payments.
func (h *POSHandler) BillReceipt(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid bill id")
	}
	hotelID := h.hotelID(c)
	ctx := c.Context()

	b, err := scanBill(h.db(c).QueryRow(ctx, `SELECT `+billCols+` FROM bills WHERE id = $1 AND hotel_id = $2`, id, hotelID))
	if err != nil {
		return response.Error(c, fiber.StatusNotFound, "bill not found")
	}

	// line items
	irows, err := h.db(c).Query(ctx, `
		SELECT ki.item_name, ki.quantity, ki.unit_price, ki.line_total
		FROM kot_items ki JOIN kots k ON k.id = ki.kot_id
		WHERE k.dining_session_id = $1 AND ki.status <> 'cancelled' ORDER BY k.round_no, ki.created_at`, b.SessionID)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to load items")
	}
	items := make([]map[string]interface{}, 0)
	for irows.Next() {
		var name string
		var qty int
		var unit, line float64
		if err := irows.Scan(&name, &qty, &unit, &line); err != nil {
			irows.Close()
			return response.Error(c, fiber.StatusInternalServerError, "failed to scan item")
		}
		items = append(items, map[string]interface{}{"name": name, "qty": qty, "unit_price": unit, "line_total": line})
	}
	irows.Close()

	// payments
	prows, err := h.db(c).Query(ctx, `
		SELECT method, amount, tendered, change_due, status, txn_reference FROM bill_payments
		WHERE bill_id = $1 ORDER BY created_at`, id)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to load payments")
	}
	payments := make([]map[string]interface{}, 0)
	for prows.Next() {
		var method, status string
		var amount, change float64
		var tendered *float64
		var ref *string
		if err := prows.Scan(&method, &amount, &tendered, &change, &status, &ref); err != nil {
			prows.Close()
			return response.Error(c, fiber.StatusInternalServerError, "failed to scan payment")
		}
		payments = append(payments, map[string]interface{}{
			"method": method, "amount": amount, "tendered": tendered, "change_due": change,
			"status": status, "txn_reference": ref,
		})
	}
	prows.Close()

	return response.OK(c, map[string]interface{}{"bill": b, "items": items, "payments": payments})
}

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

// audit writes a best-effort row to the shared audit_logs table. Failures are
// swallowed so auditing never blocks an operational write.
func (h *POSHandler) audit(ctx context.Context, c *fiber.Ctx, action, tableName string, recordID uuid.UUID) {
	_, _ = h.db(c).Exec(ctx, `
		INSERT INTO audit_logs (id, user_id, action, table_name, record_id, created_at)
		VALUES ($1,$2,$3,$4,$5, now())`,
		uuid.New(), h.userID(c), action, tableName, recordID)
}
