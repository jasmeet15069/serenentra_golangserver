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
//   CREATE TABLE IF NOT EXISTS channel_connections (
//     id uuid PK, hotel_id uuid, channel_name text, channel_type text,
//     api_key text, settings jsonb, connected bool,
//     last_sync_at timestamptz, created_at timestamptz
//   );

type ChannelHandler struct {
	pool *pgxpool.Pool
}

func NewChannelHandler(pool *pgxpool.Pool) *ChannelHandler {
	return &ChannelHandler{pool: pool}
}

func (h *ChannelHandler) Register(r fiber.Router) {
	r.Get("/channel/connections", h.ListConnections)
	r.Post("/channel/connections", h.CreateConnection)
	r.Patch("/channel/connections/:id", h.UpdateConnection)
	r.Delete("/channel/connections/:id", h.DeleteConnection)
	r.Get("/channel/analytics", h.GetChannelAnalytics)
}

// ---------------------------------------------------------------------------
// OTA Connections
// ---------------------------------------------------------------------------

type createConnectionRequest struct {
	ChannelName string      `json:"channel_name"`
	ChannelType string      `json:"channel_type"`
	APIKey      string      `json:"api_key,omitempty"`
	Settings    interface{} `json:"settings,omitempty"`
}

type updateConnectionRequest struct {
	APIKey    string      `json:"api_key,omitempty"`
	Settings  interface{} `json:"settings,omitempty"`
	Connected *bool       `json:"connected,omitempty"`
}

type connectionResponse struct {
	ID          uuid.UUID    `json:"id"`
	HotelID     uuid.UUID    `json:"hotel_id"`
	ChannelName string       `json:"channel_name"`
	ChannelType string       `json:"channel_type"`
	Settings    *interface{} `json:"settings"`
	Connected   bool         `json:"connected"`
	LastSyncAt  *time.Time   `json:"last_sync_at"`
	CreatedAt   time.Time    `json:"created_at"`
}

func (h *ChannelHandler) ListConnections(c *fiber.Ctx) error {
	rows, err := tenantPool(c, h.pool).Query(c.Context(), `
		SELECT id, hotel_id, channel_name, channel_type, settings,
		       connected, last_sync_at, created_at
		FROM channel_connections
		WHERE hotel_id = $1
		ORDER BY channel_name ASC`,
		tenantHotelID(c),
	)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()

	items := make([]connectionResponse, 0)
	for rows.Next() {
		var item connectionResponse
		if err := rows.Scan(
			&item.ID, &item.HotelID, &item.ChannelName, &item.ChannelType,
			&item.Settings, &item.Connected, &item.LastSyncAt, &item.CreatedAt,
		); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		items = append(items, item)
	}
	return response.OK(c, items)
}

func (h *ChannelHandler) CreateConnection(c *fiber.Ctx) error {
	var req createConnectionRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if req.ChannelName == "" || req.ChannelType == "" {
		return response.Error(c, fiber.StatusBadRequest, "channel_name and channel_type are required")
	}
	connID := uuid.New()
	_, err := tenantPool(c, h.pool).Exec(c.Context(), `
		INSERT INTO channel_connections
			(id, hotel_id, channel_name, channel_type, api_key, settings, connected, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,true,now())`,
		connID, tenantHotelID(c), req.ChannelName, req.ChannelType,
		nullableText(req.APIKey), req.Settings,
	)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.Created(c, map[string]interface{}{
		"id":           connID,
		"channel_name": req.ChannelName,
		"connected":    true,
	})
}

func (h *ChannelHandler) UpdateConnection(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid connection id")
	}
	var req updateConnectionRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	setClauses := ""
	args := make([]interface{}, 0, 3)
	argIdx := 1
	if req.APIKey != "" {
		setClauses += fmt.Sprintf("api_key = $%d, ", argIdx)
		args = append(args, req.APIKey)
		argIdx++
	}
	if req.Settings != nil {
		setClauses += fmt.Sprintf("settings = $%d, ", argIdx)
		args = append(args, req.Settings)
		argIdx++
	}
	if req.Connected != nil {
		setClauses += fmt.Sprintf("connected = $%d, ", argIdx)
		args = append(args, *req.Connected)
		argIdx++
	}
	if setClauses == "" {
		return response.Error(c, fiber.StatusBadRequest, "no fields to update")
	}
	// Each fragment is appended with a trailing ", "; strip it so the SET list is
	// valid SQL (otherwise "SET col = $1,  WHERE ..." is a syntax error).
	setClauses = strings.TrimSuffix(strings.TrimSpace(setClauses), ",")
	args = append(args, id)
	args = append(args, tenantHotelID(c))
	q := fmt.Sprintf(
		"UPDATE channel_connections SET %s WHERE id = $%d AND hotel_id = $%d",
		setClauses, argIdx, argIdx+1,
	)
	tag, err := tenantPool(c, h.pool).Exec(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	if tag.RowsAffected() == 0 {
		return response.Error(c, fiber.StatusNotFound, "connection not found")
	}
	return response.OK(c, map[string]string{"status": "updated"})
}

func (h *ChannelHandler) DeleteConnection(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid connection id")
	}
	tag, err := tenantPool(c, h.pool).Exec(c.Context(),
		"DELETE FROM channel_connections WHERE id = $1 AND hotel_id = $2",
		id, tenantHotelID(c),
	)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	if tag.RowsAffected() == 0 {
		return response.Error(c, fiber.StatusNotFound, "connection not found")
	}
	return response.OK(c, map[string]string{"status": "deleted"})
}

// ---------------------------------------------------------------------------
// Channel Analytics
// ---------------------------------------------------------------------------

type channelAnalyticsResponse struct {
	ChannelName string  `json:"channel_name"`
	Bookings    int     `json:"bookings"`
	Revenue     float64 `json:"revenue"`
}

func (h *ChannelHandler) GetChannelAnalytics(c *fiber.Ctx) error {
	rows, err := tenantPool(c, h.pool).Query(c.Context(), `
		SELECT
			COALESCE(channel_name, 'direct') AS channel_name,
			COUNT(*) AS bookings,
			COALESCE(SUM(b.total), 0) AS revenue
		FROM bookings b
		WHERE b.hotel_id = $1
		  AND b.created_at >= CURRENT_DATE - INTERVAL '30 days'
		GROUP BY channel_name
		ORDER BY revenue DESC`,
		tenantHotelID(c),
	)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()

	items := make([]channelAnalyticsResponse, 0)
	for rows.Next() {
		var item channelAnalyticsResponse
		if err := rows.Scan(
			&item.ChannelName, &item.Bookings, &item.Revenue,
		); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		items = append(items, item)
	}
	return response.OK(c, items)
}
