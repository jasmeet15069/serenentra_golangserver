package handler

import (
	"fmt"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hotelharmony/api/pkg/response"
)

// Tables:
//   CREATE TABLE IF NOT EXISTS vendors (
//     id uuid PK, hotel_id uuid, name text, contact_person text,
//     email text, phone text, address text, category text,
//     rating numeric, active bool, created_at timestamptz
//   );
//   CREATE TABLE IF NOT EXISTS purchase_orders (
//     id uuid PK, hotel_id uuid, vendor_id uuid, po_number text,
//     status text, items jsonb, total numeric, notes text,
//     issued_at timestamptz, received_at timestamptz, created_at timestamptz
//   );

type ProcurementHandler struct {
	pool *pgxpool.Pool
}

func NewProcurementHandler(pool *pgxpool.Pool) *ProcurementHandler {
	return &ProcurementHandler{pool: pool}
}

func (h *ProcurementHandler) Register(r fiber.Router) {
	r.Get("/procurement/vendors", h.ListVendors)
	r.Post("/procurement/vendors", h.CreateVendor)
	r.Patch("/procurement/vendors/:id", h.UpdateVendor)
	r.Get("/procurement/purchase-orders", h.ListPurchaseOrders)
	r.Post("/procurement/purchase-orders", h.CreatePurchaseOrder)
	r.Patch("/procurement/purchase-orders/:id/status", h.UpdatePOStatus)
}

// ---------------------------------------------------------------------------
// Vendors
// ---------------------------------------------------------------------------

type createVendorRequest struct {
	Name          string `json:"name"`
	ContactPerson string `json:"contact_person,omitempty"`
	Email         string `json:"email,omitempty"`
	Phone         string `json:"phone,omitempty"`
	Address       string `json:"address,omitempty"`
	Category      string `json:"category,omitempty"`
}

type vendorResponse struct {
	ID            uuid.UUID `json:"id"`
	HotelID       uuid.UUID `json:"hotel_id"`
	Name          string    `json:"name"`
	ContactPerson *string   `json:"contact_person"`
	Email         *string   `json:"email"`
	Phone         *string   `json:"phone"`
	Address       *string   `json:"address"`
	Category      *string   `json:"category"`
	Rating        *float64  `json:"rating"`
	Active        bool      `json:"active"`
	CreatedAt     time.Time `json:"created_at"`
}

func (h *ProcurementHandler) ListVendors(c *fiber.Ctx) error {
	q := `SELECT id, hotel_id, name, contact_person, email, phone, address,
	             category, rating, active, created_at
	      FROM vendors
	      WHERE hotel_id = $1`
	args := []interface{}{tenantHotelID(c)}
	argIdx := 2

	for _, f := range []struct{ param, col string }{
		{"category", "category"},
		{"active", "active"},
	} {
		if v := c.Query(f.param); v != "" {
			q += " AND " + f.col + " = $" + fmt.Sprintf("%d", argIdx)
			args = append(args, v)
			argIdx++
		}
	}
	q += " ORDER BY name ASC"

	rows, err := tenantPool(c, h.pool).Query(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()

	items := make([]vendorResponse, 0)
	for rows.Next() {
		var item vendorResponse
		if err := rows.Scan(
			&item.ID, &item.HotelID, &item.Name,
			&item.ContactPerson, &item.Email, &item.Phone,
			&item.Address, &item.Category, &item.Rating,
			&item.Active, &item.CreatedAt,
		); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		items = append(items, item)
	}
	return response.OK(c, items)
}

func (h *ProcurementHandler) CreateVendor(c *fiber.Ctx) error {
	var req createVendorRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if req.Name == "" {
		return response.Error(c, fiber.StatusBadRequest, "name is required")
	}
	vendorID := uuid.New()
	_, err := tenantPool(c, h.pool).Exec(c.Context(), `
		INSERT INTO vendors
			(id, hotel_id, name, contact_person, email, phone, address, category, active, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,true,now())`,
		vendorID, tenantHotelID(c), req.Name,
		nullableText(req.ContactPerson), nullableText(req.Email),
		nullableText(req.Phone), nullableText(req.Address),
		nullableText(req.Category),
	)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.Created(c, map[string]interface{}{
		"id":   vendorID,
		"name": req.Name,
	})
}

func (h *ProcurementHandler) UpdateVendor(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid vendor id")
	}
	var req struct {
		Name          *string `json:"name,omitempty"`
		ContactPerson *string `json:"contact_person,omitempty"`
		Email         *string `json:"email,omitempty"`
		Phone         *string `json:"phone,omitempty"`
		Address       *string `json:"address,omitempty"`
		Category      *string `json:"category,omitempty"`
		Active        *bool   `json:"active,omitempty"`
	}
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if req.Active != nil && !*req.Active {
		_, err = tenantPool(c, h.pool).Exec(c.Context(), `UPDATE vendors SET active = false WHERE id = $1 AND hotel_id = $2`, id, tenantHotelID(c))
	} else {
		q := "UPDATE vendors SET "
		args := make([]interface{}, 0)
		argIdx := 1
		if req.Name != nil {
			q += fmt.Sprintf("name = $%d, ", argIdx)
			args = append(args, *req.Name)
			argIdx++
		}
		if req.ContactPerson != nil {
			q += fmt.Sprintf("contact_person = $%d, ", argIdx)
			args = append(args, *req.ContactPerson)
			argIdx++
		}
		if req.Email != nil {
			q += fmt.Sprintf("email = $%d, ", argIdx)
			args = append(args, *req.Email)
			argIdx++
		}
		if req.Phone != nil {
			q += fmt.Sprintf("phone = $%d, ", argIdx)
			args = append(args, *req.Phone)
			argIdx++
		}
		if req.Address != nil {
			q += fmt.Sprintf("address = $%d, ", argIdx)
			args = append(args, *req.Address)
			argIdx++
		}
		if req.Category != nil {
			q += fmt.Sprintf("category = $%d, ", argIdx)
			args = append(args, *req.Category)
			argIdx++
		}
		if req.Active != nil {
			q += fmt.Sprintf("active = $%d, ", argIdx)
			args = append(args, *req.Active)
			argIdx++
		}
		q = q[:len(q)-2] + fmt.Sprintf(" WHERE id = $%d AND hotel_id = $%d", argIdx, argIdx+1)
		args = append(args, id, tenantHotelID(c))
		_, err = tenantPool(c, h.pool).Exec(c.Context(), q, args...)
	}
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.OK(c, map[string]string{"status": "updated"})
}

// ---------------------------------------------------------------------------
// Purchase Orders
// ---------------------------------------------------------------------------

type createPORequest struct {
	VendorID string      `json:"vendor_id"`
	Items    interface{} `json:"items"`
	Total    float64     `json:"total"`
	Notes    string      `json:"notes,omitempty"`
}

type updatePOStatusRequest struct {
	Status string `json:"status"`
}

type purchaseOrderResponse struct {
	ID         uuid.UUID    `json:"id"`
	HotelID    uuid.UUID    `json:"hotel_id"`
	VendorID   uuid.UUID    `json:"vendor_id"`
	PONumber   string       `json:"po_number"`
	Status     string       `json:"status"`
	Items      *interface{} `json:"items"`
	Total      float64      `json:"total"`
	Notes      *string      `json:"notes"`
	IssuedAt   *time.Time   `json:"issued_at"`
	ReceivedAt *time.Time   `json:"received_at"`
	CreatedAt  time.Time    `json:"created_at"`
}

func (h *ProcurementHandler) ListPurchaseOrders(c *fiber.Ctx) error {
	q := `SELECT id, hotel_id, vendor_id, po_number, status, items, total,
	             notes, issued_at, received_at, created_at
	      FROM purchase_orders
	      WHERE hotel_id = $1`
	args := []interface{}{tenantHotelID(c)}
	argIdx := 2

	for _, f := range []struct{ param, col string }{
		{"status", "status"},
		{"vendor_id", "vendor_id"},
	} {
		if v := c.Query(f.param); v != "" {
			q += " AND " + f.col + " = $" + fmt.Sprintf("%d", argIdx)
			args = append(args, v)
			argIdx++
		}
	}
	q += " ORDER BY created_at DESC"

	rows, err := tenantPool(c, h.pool).Query(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()

	items := make([]purchaseOrderResponse, 0)
	for rows.Next() {
		var item purchaseOrderResponse
		if err := rows.Scan(
			&item.ID, &item.HotelID, &item.VendorID,
			&item.PONumber, &item.Status, &item.Items,
			&item.Total, &item.Notes, &item.IssuedAt,
			&item.ReceivedAt, &item.CreatedAt,
		); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		items = append(items, item)
	}
	return response.OK(c, items)
}

func (h *ProcurementHandler) CreatePurchaseOrder(c *fiber.Ctx) error {
	var req createPORequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if req.VendorID == "" {
		return response.Error(c, fiber.StatusBadRequest, "vendor_id is required")
	}
	vendorID, err := uuid.Parse(req.VendorID)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid vendor_id")
	}
	poID := uuid.New()
	poNumber := "PO-" + time.Now().Format("20060102") + "-" + poID.String()[:8]
	_, err = tenantPool(c, h.pool).Exec(c.Context(), `
		INSERT INTO purchase_orders
			(id, hotel_id, vendor_id, po_number, status, items, total, notes, issued_at, created_at)
		VALUES ($1,$2,$3,$4,'pending',$5,$6,$7,now(),now())`,
		poID, tenantHotelID(c), vendorID, poNumber,
		req.Items, req.Total, nullableText(req.Notes),
	)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.Created(c, map[string]interface{}{
		"id":        poID,
		"po_number": poNumber,
		"status":    "pending",
	})
}

func (h *ProcurementHandler) UpdatePOStatus(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid purchase order id")
	}
	var req updatePOStatusRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	validStatus := map[string]bool{"approved": true, "rejected": true, "received": true, "cancelled": true}
	if !validStatus[req.Status] {
		return response.Error(c, fiber.StatusBadRequest, "status must be approved, rejected, received, or cancelled")
	}
	var q string
	if req.Status == "received" {
		q = "UPDATE purchase_orders SET status = $1, received_at = now() WHERE id = $2 AND hotel_id = $3"
	} else {
		q = "UPDATE purchase_orders SET status = $1 WHERE id = $2 AND hotel_id = $3"
	}
	tag, err := tenantPool(c, h.pool).Exec(c.Context(), q, req.Status, id, tenantHotelID(c))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	if tag.RowsAffected() == 0 {
		return response.Error(c, fiber.StatusNotFound, "purchase order not found")
	}
	return response.OK(c, map[string]string{"status": req.Status})
}
