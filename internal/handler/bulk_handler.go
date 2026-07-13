package handler

import (
	"fmt"
	"math"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hotelharmony/api/internal/cache"
	"github.com/hotelharmony/api/internal/domain"
	"github.com/hotelharmony/api/internal/tenant"
	"github.com/hotelharmony/api/pkg/response"
)

// BulkHandler ingests CSV-derived JSON rows for the setup wizard / data import.
// The frontend parses the CSV and POSTs {"rows":[{...}]}; each row is inserted
// into the entity's table scoped to the caller's hotel. Per-row failures are
// reported (with the row index) rather than aborting the whole import.
type BulkHandler struct {
	baseHandler
	pool    *pgxpool.Pool
	tenants *tenant.Manager
	cache   cache.Cache
}

func NewBulkHandler(pool *pgxpool.Pool, secret string, tenants *tenant.Manager, c cache.Cache) *BulkHandler {
	return &BulkHandler{baseHandler: newBase(secret), pool: pool, tenants: tenants, cache: c}
}

func (h *BulkHandler) db(c *fiber.Ctx) *pgxpool.Pool {
	if h.tenants == nil {
		return h.pool
	}
	return h.tenants.PoolForHotel(c.Context(), h.hotelID(c))
}

func (h *BulkHandler) Register(r fiber.Router) {
	g := r.Group("", authGate(h.secret))
	g.Post("/bulk/:entity", h.Import)
}

type bulkEntity struct {
	table string
	cols  map[string]bool
}

func colsSet(cols ...string) map[string]bool {
	m := make(map[string]bool, len(cols))
	for _, c := range cols {
		m[c] = true
	}
	return m
}

// bulkEntities whitelists the writable columns per importable entity. id and
// hotel_id are always injected by the server; created_at/updated_at default.
var bulkEntities = map[string]bulkEntity{
	"rooms": {"rooms", colsSet("room_number", "room_type", "floor", "capacity", "price_per_night", "status")},
	"guests": {"guests", colsSet("full_name", "email", "phone", "address", "city", "country",
		"id_type", "id_number", "vip_status")},
	"vendors": {"vendors", colsSet("name", "contact_person", "email", "phone", "address", "category", "rating", "active")},
	"promotions": {"promotions", colsSet("code", "name", "description", "discount_type", "discount_value",
		"min_nights", "min_amount", "max_discount", "usage_limit")},
	"menu-items": {"menu_items", colsSet("category_id", "name", "description", "price", "image_url",
		"is_available", "preparation_time")},
}

func (h *BulkHandler) Import(c *fiber.Ctx) error {
	// hotel_admin is the tenant admin on most clients (the admin role is named
	// hotel_admin / admin / super_admin inconsistently across tenants). It was
	// missing here, so a hotel_admin bulk-uploading rooms/menu got "access denied".
	if !h.requireRoles(c, "admin", "hotel_admin", "super_admin", "food_manager", "platform_admin") {
		return nil
	}
	cfg, ok := bulkEntities[c.Params("entity")]
	if !ok {
		return response.Error(c, fiber.StatusBadRequest, "unsupported import entity")
	}
	var body struct {
		Rows []map[string]interface{} `json:"rows"`
	}
	if err := c.BodyParser(&body); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if len(body.Rows) == 0 {
		return response.Error(c, fiber.StatusUnprocessableEntity, "no rows to import")
	}

	hotelID := h.hotelID(c)
	inserted := 0
	failures := make([]map[string]interface{}, 0)

	for idx, row := range body.Rows {
		cols := []string{"id", "hotel_id"}
		args := []interface{}{uuid.New(), hotelID}
		ph := []string{"$1", "$2"}
		i := 3
		for k, v := range row {
			if !cfg.cols[k] {
				continue
			}
			// JSON numbers arrive as float64; coerce whole numbers to int so they
			// fit integer columns (and remain valid for numeric columns).
			if f, isFloat := v.(float64); isFloat && f == math.Trunc(f) {
				v = int64(f)
			}
			cols = append(cols, k)
			args = append(args, v)
			ph = append(ph, fmt.Sprintf("$%d", i))
			i++
		}
		if len(cols) == 2 {
			failures = append(failures, map[string]interface{}{"row": idx, "error": "no recognised columns"})
			continue
		}
		q := fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s)`, cfg.table, strings.Join(cols, ", "), strings.Join(ph, ", "))
		if _, err := h.db(c).Exec(c.Context(), q, args...); err != nil {
			failures = append(failures, map[string]interface{}{"row": idx, "error": err.Error()})
			continue
		}
		inserted++
	}

	// Invalidate the caches a fresh import would otherwise leave stale (the room
	// list is cached 60s, dashboard stats too) — without this, bulk-uploaded rooms
	// don't appear in Room Management until the TTL lapses.
	if h.cache != nil && inserted > 0 {
		hid := hotelID.String()
		_ = h.cache.Delete(c.Context(),
			cache.KeyDashboardStats(hid),
			cache.KeyRoomList(hid, "all"),
			cache.KeyRoomList(hid, string(domain.RoomStatusAvailable)),
			cache.KeyRoomList(hid, string(domain.RoomStatusOccupied)),
			cache.KeyRoomList(hid, string(domain.RoomStatusCleaning)),
			cache.KeyRoomList(hid, string(domain.RoomStatusMaintenance)),
		)
	}

	return response.OK(c, map[string]interface{}{
		"entity": c.Params("entity"), "received": len(body.Rows),
		"inserted": inserted, "failed": len(failures), "errors": failures,
	})
}
