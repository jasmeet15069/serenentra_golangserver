package handler

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hotelharmony/api/internal/cache"
	"github.com/hotelharmony/api/internal/tenant"
	"github.com/hotelharmony/api/pkg/response"
)

// POSHandler persists point-of-sale orders. Unlike the room-service `orders`
// table (which is keyed to guest_stays and menu_items), POS orders carry the
// restaurant model directly — outlet, channel, table, walk-in customer — and
// store their line items as JSONB so the cart shape round-trips verbatim
// without a foreign key to a catalogue row.
type POSHandler struct {
	baseHandler
	pool    *pgxpool.Pool
	cache   cache.Cache
	tenants *tenant.Manager
}

func NewPOSHandler(pool *pgxpool.Pool, c cache.Cache, secret string, tenants *tenant.Manager) *POSHandler {
	return &POSHandler{baseHandler: newBase(secret), pool: pool, cache: c, tenants: tenants}
}

// db returns the pool this request should use: the tenant's dedicated pool when
// it has dedicated isolation, otherwise the shared pool. For all shared/
// provisioned tenants (i.e. everyone today) this is exactly the shared pool, so
// behaviour is unchanged until a tenant is flipped to 'dedicated'.
func (h *POSHandler) db(c *fiber.Ctx) *pgxpool.Pool {
	if h.tenants == nil {
		return h.pool
	}
	return h.tenants.PoolForHotel(c.Context(), h.hotelID(c))
}

// invalidate drops the cached order list for a hotel after any mutation, so the
// next read repopulates from the database.
func (h *POSHandler) invalidate(c *fiber.Ctx) {
	if h.cache != nil {
		_ = h.cache.Delete(c.Context(), cache.KeyPOSOrders(h.hotelID(c).String()))
	}
}

func (h *POSHandler) Register(r fiber.Router) {
	g := r.Group("", authGate(h.secret))
	g.Get("/pos/orders", h.List)
	g.Post("/pos/orders", h.Create)
	g.Patch("/pos/orders/:id", h.Update)
	g.Delete("/pos/orders/:id", h.Delete)

	// Business / GST settings (setup wizard).
	g.Get("/settings/business", h.GetBusinessSettings)
	g.Put("/settings/business", h.UpdateBusinessSettings)

	// Outlets: restaurants/bars under a hotel (or standalone for walk-ins).
	g.Get("/pos/outlets", h.ListOutlets)
	g.Post("/pos/outlets", h.CreateOutlet)
	g.Patch("/pos/outlets/:id", h.UpdateOutlet)

	// Dine-In workflow: tables -> sessions -> KOTs -> bill -> payments.
	g.Get("/pos/tables", h.ListTables)
	g.Post("/pos/tables", h.CreateTable)
	g.Patch("/pos/tables/:id", h.UpdateTable)
	g.Post("/pos/tables/:id/clean", h.CleanTable)

	g.Get("/pos/waitlist", h.ListWaitlist)
	g.Post("/pos/waitlist", h.AddWaitlist)
	g.Patch("/pos/waitlist/:id", h.UpdateWaitlist)
	g.Delete("/pos/waitlist/:id", h.DeleteWaitlist)

	g.Get("/pos/sessions", h.ListSessions)
	g.Post("/pos/tables/:id/sessions", h.OpenSession)
	g.Get("/pos/sessions/:id", h.GetSession)
	g.Post("/pos/sessions/:id/close", h.CloseSession)

	g.Post("/pos/sessions/:id/kots", h.CreateKOT)
	g.Post("/pos/kots/:id/send", h.SendKOT)
	g.Patch("/pos/kots/:id/status", h.UpdateKOTStatus)

	g.Post("/pos/sessions/:id/bill", h.GenerateBill)
	g.Patch("/pos/bills/:id", h.UpdateBill)
	g.Post("/pos/bills/:id/finalize", h.FinalizeBill)
	g.Post("/pos/bills/:id/payments", h.AddBillPayment)
	g.Get("/pos/bills/:id/receipt", h.BillReceipt)
	g.Get("/pos/bills/:id/invoice", h.BillInvoice)
}

const posOrderCols = `id, order_number, outlet, channel, table_label, room_id, customer_name,
	delivery_address, status, total, subtotal, discount, service_charge, tax_rate, tax_mode, tax,
	items, created_at, updated_at`

type posOrderRow struct {
	ID              uuid.UUID       `json:"id"`
	OrderNumber     string          `json:"order_number"`
	Outlet          string          `json:"outlet"`
	Channel         *string         `json:"channel"`
	TableLabel      *string         `json:"table_label"`
	RoomID          *string         `json:"room_id"`
	CustomerName    *string         `json:"customer_name"`
	DeliveryAddress *string         `json:"delivery_address"`
	Status          string          `json:"status"`
	Total           float64         `json:"total"`
	Subtotal        float64         `json:"subtotal"`
	Discount        float64         `json:"discount"`
	ServiceCharge   float64         `json:"service_charge"`
	TaxRate         float64         `json:"tax_rate"`
	TaxMode         string          `json:"tax_mode"`
	Tax             float64         `json:"tax"`
	Items           json.RawMessage `json:"items"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

func scanPOSOrder(row interface {
	Scan(...interface{}) error
}) (posOrderRow, error) {
	var o posOrderRow
	err := row.Scan(&o.ID, &o.OrderNumber, &o.Outlet, &o.Channel, &o.TableLabel, &o.RoomID,
		&o.CustomerName, &o.DeliveryAddress, &o.Status, &o.Total, &o.Subtotal, &o.Discount,
		&o.ServiceCharge, &o.TaxRate, &o.TaxMode, &o.Tax, &o.Items, &o.CreatedAt, &o.UpdatedAt)
	if len(o.Items) == 0 {
		o.Items = json.RawMessage("[]")
	}
	return o, err
}

func (h *POSHandler) List(c *fiber.Ctx) error {
	status := c.Query("status")
	cacheKey := cache.KeyPOSOrders(h.hotelID(c).String())

	// Only the unfiltered list (what the POS UI polls) is cached; status-filtered
	// reads are rare and bypass the cache to keep invalidation a single key.
	if status == "" && h.cache != nil {
		if cached, err := h.cache.Get(c.Context(), cacheKey); err == nil {
			var out []posOrderRow
			if json.Unmarshal([]byte(cached), &out) == nil {
				c.Set("X-Cache", "HIT")
				return response.OK(c, out)
			}
		}
	}

	q := `SELECT ` + posOrderCols + ` FROM pos_orders WHERE hotel_id = $1`
	args := []interface{}{h.hotelID(c)}
	if status != "" {
		q += " AND status = $2"
		args = append(args, status)
	}
	q += " ORDER BY created_at DESC"

	rows, err := h.db(c).Query(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to list pos orders")
	}
	defer rows.Close()

	out := make([]posOrderRow, 0)
	for rows.Next() {
		o, scanErr := scanPOSOrder(rows)
		if scanErr != nil {
			return response.Error(c, fiber.StatusInternalServerError, "failed to scan pos order")
		}
		out = append(out, o)
	}

	if status == "" && h.cache != nil {
		if b, err := json.Marshal(out); err == nil {
			_ = h.cache.Set(c.Context(), cacheKey, string(b), cache.TTLPOSOrders)
		}
		c.Set("X-Cache", "MISS")
	}
	return response.OK(c, out)
}

type posOrderRequest struct {
	Outlet          string          `json:"outlet"`
	Channel         *string         `json:"channel"`
	TableLabel      *string         `json:"table_label"`
	RoomID          *string         `json:"room_id"`
	CustomerName    *string         `json:"customer_name"`
	DeliveryAddress *string         `json:"delivery_address"`
	Status          string          `json:"status"`
	Total           float64         `json:"total"`
	Subtotal        float64         `json:"subtotal"`
	Discount        float64         `json:"discount"`
	ServiceCharge   float64         `json:"service_charge"`
	TaxRate         float64         `json:"tax_rate"`
	TaxMode         string          `json:"tax_mode"`
	Tax             float64         `json:"tax"`
	Items           json.RawMessage `json:"items"`
}

// posOrderLineItem is the minimal shape of a POS cart line used to validate the
// order and recompute its money fields; the full item JSON is still stored
// verbatim so unrelated fields (name, note, seat) round-trip unchanged.
type posOrderLineItem struct {
	Qty   float64 `json:"qty"`
	Price float64 `json:"price"`
}

func (h *POSHandler) Create(c *fiber.Ctx) error {
	var req posOrderRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if strings.TrimSpace(req.Outlet) == "" {
		return response.Error(c, fiber.StatusUnprocessableEntity, "outlet is required")
	}
	if req.Status == "" {
		req.Status = "Open"
	}
	if req.TaxMode == "" {
		req.TaxMode = "gst"
	}
	// tax_mode must be one of the supported GST modes (mirrors CreatePosOrderBody
	// in the admin portal's types.ts).
	if req.TaxMode != "gst" && req.TaxMode != "igst" {
		return response.Error(c, fiber.StatusBadRequest, "tax_mode must be 'gst' or 'igst'")
	}

	// Line items are required and each must be individually sane.
	var items []posOrderLineItem
	if len(req.Items) > 0 {
		if err := json.Unmarshal(req.Items, &items); err != nil {
			return response.Error(c, fiber.StatusBadRequest, "invalid items")
		}
	}
	if len(items) == 0 {
		return response.Error(c, fiber.StatusBadRequest, "at least one item is required")
	}
	for _, it := range items {
		if it.Qty <= 0 {
			return response.Error(c, fiber.StatusBadRequest, "item quantity must be greater than zero")
		}
		if it.Price < 0 {
			return response.Error(c, fiber.StatusBadRequest, "item price cannot be negative")
		}
	}

	// Server-side sanity guard against tampered/garbage money fields (tax-audit
	// risk). The POS UI owns the pricing math — per-item discounts, promotions and
	// tax-inclusive modes mean we must NOT re-derive the subtotal from qty×price
	// (that would false-reject legitimate carts). Instead we enforce two things any
	// correct breakdown must satisfy: no negative money, and the client's own
	// fields are internally consistent — total = subtotal − discount + service
	// charge + tax — within a ₹1 rounding epsilon.
	if req.Subtotal < 0 || req.Discount < 0 || req.ServiceCharge < 0 || req.Tax < 0 || req.Total < 0 {
		return response.Error(c, fiber.StatusBadRequest, "monetary fields cannot be negative")
	}
	expectedTotal := req.Subtotal - req.Discount + req.ServiceCharge + req.Tax
	if math.Abs(req.Total-expectedTotal) > 1.0 {
		return response.Error(c, fiber.StatusBadRequest, "order total is inconsistent with its subtotal, discount, service charge and tax")
	}

	itemsJSON := "[]"
	if len(req.Items) > 0 {
		itemsJSON = string(req.Items)
	}

	hotelID := h.hotelID(c)
	id := uuid.New()
	orderNumber := fmt.Sprintf("ORD-%s-%d", time.Now().UTC().Format("20060102"), time.Now().UnixMilli()%100000)

	row := h.db(c).QueryRow(c.Context(), `
		INSERT INTO pos_orders (id, hotel_id, order_number, outlet, channel, table_label, room_id,
			customer_name, delivery_address, status, total, subtotal, discount, service_charge,
			tax_rate, tax_mode, tax, items, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18::jsonb,now(),now())
		RETURNING `+posOrderCols,
		id, hotelID, orderNumber, req.Outlet, req.Channel, req.TableLabel, req.RoomID,
		req.CustomerName, req.DeliveryAddress, req.Status, req.Total, req.Subtotal, req.Discount,
		req.ServiceCharge, req.TaxRate, req.TaxMode, req.Tax, itemsJSON,
	)
	o, err := scanPOSOrder(row)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	h.invalidate(c)
	return response.Created(c, o)
}

func (h *POSHandler) Update(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid order id")
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(c.Body(), &raw); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}

	// Only these columns may be patched from the client.
	allowed := map[string]bool{
		"outlet": true, "channel": true, "table_label": true, "room_id": true,
		"customer_name": true, "delivery_address": true, "status": true, "total": true, "items": true,
		"subtotal": true, "discount": true, "service_charge": true, "tax_rate": true,
		"tax_mode": true, "tax": true,
	}

	set := []string{}
	args := []interface{}{}
	i := 1
	for k, rawVal := range raw {
		if !allowed[k] {
			continue
		}
		if k == "items" {
			set = append(set, fmt.Sprintf("items = $%d::jsonb", i))
			args = append(args, string(rawVal))
			i++
			continue
		}
		// Decode the scalar so pgx receives a Go value of the right kind.
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

	q := fmt.Sprintf(`UPDATE pos_orders SET %s WHERE id = $%d AND hotel_id = $%d RETURNING %s`,
		strings.Join(set, ", "), i, i+1, posOrderCols)
	o, scanErr := scanPOSOrder(h.db(c).QueryRow(c.Context(), q, args...))
	if scanErr != nil {
		return response.Error(c, fiber.StatusBadRequest, scanErr.Error())
	}
	h.invalidate(c)
	return response.OK(c, o)
}

func (h *POSHandler) Delete(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid order id")
	}
	if _, err := h.db(c).Exec(c.Context(), `DELETE FROM pos_orders WHERE id = $1 AND hotel_id = $2`, id, h.hotelID(c)); err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	h.invalidate(c)
	return response.OK(c, map[string]string{"status": "deleted"})
}
