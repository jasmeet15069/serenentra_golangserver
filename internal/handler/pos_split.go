package handler

import (
	"context"
	"math"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/hotelharmony/api/pkg/response"
)

// Split billing: the cashier toggles "split" on a bill, names the customers,
// and each customer gets their own GST tax invoice for their share
// (GET /pos/bills/:id/invoice?split=<split_id>). Shares default to an equal
// split of the bill total; custom amounts are accepted as long as they sum to
// the total. Stored shares are renormalized against the CURRENT bill total at
// read/render time, so a discount or tip applied after the split was saved
// still yields per-customer invoices that add up to exactly what was charged.

type billSplitRow struct {
	ID           uuid.UUID `json:"id"`
	SplitNo      int       `json:"split_no"`
	CustomerName string    `json:"customer_name"`
	Amount       float64   `json:"amount"`
}

// loadBillSplits returns the bill's splits ordered by split_no with Amount
// renormalized to the given bill total (last split absorbs rounding so the
// amounts always sum to exactly the total).
func (h *POSHandler) loadBillSplits(ctx context.Context, c *fiber.Ctx, billID uuid.UUID, total float64) ([]billSplitRow, error) {
	rows, err := h.db(c).Query(ctx, `
		SELECT id, split_no, customer_name, share_amount
		FROM bill_splits WHERE bill_id = $1 ORDER BY split_no`, billID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []billSplitRow
	var sumShares float64
	shares := []float64{}
	for rows.Next() {
		var s billSplitRow
		var share float64
		if err := rows.Scan(&s.ID, &s.SplitNo, &s.CustomerName, &share); err != nil {
			return nil, err
		}
		sumShares += share
		shares = append(shares, share)
		out = append(out, s)
	}
	if len(out) == 0 {
		return out, nil
	}
	// Renormalize: amount_i = total * share_i / sum(shares), last takes remainder.
	allocated := 0.0
	for i := range out {
		if i == len(out)-1 {
			out[i].Amount = round2(total - allocated)
			break
		}
		amt := 0.0
		if sumShares > 0 {
			amt = round2(total * shares[i] / sumShares)
		}
		out[i].Amount = amt
		allocated = round2(allocated + amt)
	}
	return out, nil
}

// GetBillSplits returns {enabled, splits} for the bill.
func (h *POSHandler) GetBillSplits(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid bill id")
	}
	ctx := c.Context()
	b, err := scanBill(h.db(c).QueryRow(ctx, `SELECT `+billCols+` FROM bills WHERE id = $1 AND hotel_id = $2`, id, h.hotelID(c)))
	if err != nil {
		return response.Error(c, fiber.StatusNotFound, "bill not found")
	}
	splits, err := h.loadBillSplits(ctx, c, id, b.TotalAmount)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to load splits")
	}
	if splits == nil {
		splits = []billSplitRow{}
	}
	return response.OK(c, map[string]interface{}{"enabled": len(splits) > 0, "splits": splits})
}

// SetBillSplits replaces the bill's splits. enabled=false deletes them (the
// cashier unticked the split checkbox); enabled=true requires >= 2 named
// customers. Amounts are optional — omit all for an equal split, or provide
// one per customer summing to the bill total.
func (h *POSHandler) SetBillSplits(c *fiber.Ctx) error {
	if !h.requireRoles(c, "admin", "hotel_admin", "super_admin", "receptionist", "cashier", "food_manager", "platform_admin") {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid bill id")
	}
	var req struct {
		Enabled   bool `json:"enabled"`
		Customers []struct {
			Name   string   `json:"name"`
			Amount *float64 `json:"amount"`
		} `json:"customers"`
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
	if b.Status == "void" {
		return response.Error(c, fiber.StatusConflict, "bill is void")
	}

	if _, err := tx.Exec(ctx, `DELETE FROM bill_splits WHERE bill_id = $1`, id); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to reset splits")
	}

	if !req.Enabled {
		if err := tx.Commit(ctx); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, "failed to save splits")
		}
		h.audit(ctx, c, "bill.split_disabled", "bill", id)
		return response.OK(c, map[string]interface{}{"enabled": false, "splits": []billSplitRow{}})
	}

	n := len(req.Customers)
	if n < 2 {
		return response.Error(c, fiber.StatusUnprocessableEntity, "split needs at least 2 customers")
	}
	withAmount := 0
	amountSum := 0.0
	names := make([]string, n)
	for i, cu := range req.Customers {
		names[i] = strings.TrimSpace(cu.Name)
		if names[i] == "" {
			return response.Error(c, fiber.StatusUnprocessableEntity, "every customer needs a name")
		}
		if cu.Amount != nil {
			if *cu.Amount <= 0 {
				return response.Error(c, fiber.StatusUnprocessableEntity, "split amounts must be positive")
			}
			withAmount++
			amountSum += *cu.Amount
		}
	}
	if withAmount != 0 && withAmount != n {
		return response.Error(c, fiber.StatusUnprocessableEntity, "provide amounts for all customers or none")
	}
	if withAmount == n && math.Abs(round2(amountSum)-b.TotalAmount) > 0.01 {
		return response.Error(c, fiber.StatusUnprocessableEntity, "split amounts must sum to the bill total")
	}

	shares := make([]float64, n)
	if withAmount == n {
		for i, cu := range req.Customers {
			shares[i] = round2(*cu.Amount)
		}
	} else {
		// Equal split; last customer absorbs the rounding remainder.
		each := round2(b.TotalAmount / float64(n))
		for i := 0; i < n-1; i++ {
			shares[i] = each
		}
		shares[n-1] = round2(b.TotalAmount - each*float64(n-1))
	}

	out := make([]billSplitRow, 0, n)
	for i := 0; i < n; i++ {
		var sid uuid.UUID
		if err := tx.QueryRow(ctx, `
			INSERT INTO bill_splits (hotel_id, bill_id, split_no, customer_name, share_amount)
			VALUES ($1,$2,$3,$4,$5) RETURNING id`,
			hotelID, id, i+1, names[i], shares[i]).Scan(&sid); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, "failed to save split")
		}
		out = append(out, billSplitRow{ID: sid, SplitNo: i + 1, CustomerName: names[i], Amount: shares[i]})
	}
	if err := tx.Commit(ctx); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to save splits")
	}
	h.audit(ctx, c, "bill.split_set", "bill", id)
	return response.OK(c, map[string]interface{}{"enabled": true, "splits": out})
}
