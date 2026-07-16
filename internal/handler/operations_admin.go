package handler

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/hotelharmony/api/pkg/response"
)

// System Admin: per-tenant API keys and third-party integrations. These back the
// System Admin screen's previously-fake API-key and integration controls. All are
// hotel-admin only and tenant-pool routed.

// --- API keys (api_keys table, migration 026) ---

type apiKeyResponse struct {
	ID         uuid.UUID  `json:"id"`
	Name       string     `json:"name"`
	KeyPrefix  string     `json:"key_prefix"`
	IsActive   bool       `json:"is_active"`
	LastUsedAt *time.Time `json:"last_used_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

func (h *OperationsHandler) ListAPIKeys(c *fiber.Ctx) error {
	if !requireAnyRoleFromToken(c, h.secretKey, hotelAdminRoles...) {
		return nil
	}
	rows, err := tenantPool(c, h.pool).Query(c.Context(), `
		SELECT id, name, key_prefix, is_active, last_used_at, created_at
		FROM api_keys WHERE hotel_id = $1 ORDER BY created_at DESC`, tenantHotelID(c))
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()
	items := make([]apiKeyResponse, 0)
	for rows.Next() {
		var k apiKeyResponse
		if err := rows.Scan(&k.ID, &k.Name, &k.KeyPrefix, &k.IsActive, &k.LastUsedAt, &k.CreatedAt); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		items = append(items, k)
	}
	return response.OK(c, items)
}

func (h *OperationsHandler) CreateAPIKey(c *fiber.Ctx) error {
	if !requireAnyRoleFromToken(c, h.secretKey, hotelAdminRoles...) {
		return nil
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "API key"
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to generate key")
	}
	fullKey := "sk_live_" + hex.EncodeToString(buf) // 8 + 48 chars
	prefix := fullKey[:14] + "…"
	hash, err := bcrypt.GenerateFromPassword([]byte(fullKey), bcrypt.DefaultCost)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to hash key")
	}
	id := uuid.New()
	if _, err := tenantPool(c, h.pool).Exec(c.Context(), `
		INSERT INTO api_keys (id, hotel_id, name, key_prefix, key_hash, is_active, created_at)
		VALUES ($1,$2,$3,$4,$5,true,now())`,
		id, tenantHotelID(c), name, prefix, string(hash)); err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.Created(c, fiber.Map{
		"id": id, "name": name, "key": fullKey,
		"warning": "This key is shown once and cannot be retrieved again — copy it now.",
	})
}

func (h *OperationsHandler) UpdateAPIKey(c *fiber.Ctx) error {
	if !requireAnyRoleFromToken(c, h.secretKey, hotelAdminRoles...) {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid key id")
	}
	var req struct {
		IsActive *bool `json:"is_active"`
	}
	if err := c.BodyParser(&req); err != nil || req.IsActive == nil {
		return response.Error(c, fiber.StatusBadRequest, "is_active is required")
	}
	tag, err := tenantPool(c, h.pool).Exec(c.Context(),
		`UPDATE api_keys SET is_active = $1 WHERE id = $2 AND hotel_id = $3`, *req.IsActive, id, tenantHotelID(c))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	if tag.RowsAffected() == 0 {
		return response.Error(c, fiber.StatusNotFound, "key not found")
	}
	return response.OK(c, fiber.Map{"id": id, "is_active": *req.IsActive})
}

func (h *OperationsHandler) DeleteAPIKey(c *fiber.Ctx) error {
	if !requireAnyRoleFromToken(c, h.secretKey, hotelAdminRoles...) {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid key id")
	}
	tag, err := tenantPool(c, h.pool).Exec(c.Context(),
		`DELETE FROM api_keys WHERE id = $1 AND hotel_id = $2`, id, tenantHotelID(c))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	if tag.RowsAffected() == 0 {
		return response.Error(c, fiber.StatusNotFound, "key not found")
	}
	return response.OK(c, fiber.Map{"status": "revoked"})
}

// --- Integrations (integrations table, migration 026) ---

type integrationResponse struct {
	Provider      string          `json:"provider"`
	Category      string          `json:"category"`
	IsEnabled     bool            `json:"is_enabled"`
	Status        string          `json:"status"`
	Config        json.RawMessage `json:"config"`
	LastCheckedAt *time.Time      `json:"last_checked_at"`
}

func (h *OperationsHandler) ListIntegrations(c *fiber.Ctx) error {
	if !requireAnyRoleFromToken(c, h.secretKey, hotelAdminRoles...) {
		return nil
	}
	rows, err := tenantPool(c, h.pool).Query(c.Context(), `
		SELECT provider, COALESCE(category,''), is_enabled, status, config, last_checked_at
		FROM integrations WHERE hotel_id = $1 ORDER BY provider ASC`, tenantHotelID(c))
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()
	items := make([]integrationResponse, 0)
	for rows.Next() {
		var it integrationResponse
		if err := rows.Scan(&it.Provider, &it.Category, &it.IsEnabled, &it.Status, &it.Config, &it.LastCheckedAt); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		items = append(items, it)
	}
	return response.OK(c, items)
}

// UpsertIntegration (PATCH /api/admin/integrations/:provider) enables/disables an
// integration and stores its config, creating the row if absent.
func (h *OperationsHandler) UpsertIntegration(c *fiber.Ctx) error {
	if !requireAnyRoleFromToken(c, h.secretKey, hotelAdminRoles...) {
		return nil
	}
	provider := strings.TrimSpace(c.Params("provider"))
	if provider == "" {
		return response.Error(c, fiber.StatusBadRequest, "provider is required")
	}
	var req struct {
		Category  string          `json:"category"`
		IsEnabled *bool           `json:"is_enabled"`
		Config    json.RawMessage `json:"config"`
	}
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	enabled := false
	if req.IsEnabled != nil {
		enabled = *req.IsEnabled
	}
	cfg := req.Config
	if len(cfg) == 0 {
		cfg = json.RawMessage("{}")
	}
	status := "disconnected"
	if enabled {
		status = "connected"
	}
	if _, err := tenantPool(c, h.pool).Exec(c.Context(), `
		INSERT INTO integrations (id, hotel_id, provider, category, is_enabled, config, status, created_at, updated_at)
		VALUES ($1,$2,$3,NULLIF($4,''),$5,$6::jsonb,$7,now(),now())
		ON CONFLICT (hotel_id, provider) DO UPDATE
		  SET is_enabled = EXCLUDED.is_enabled, config = EXCLUDED.config,
		      category = COALESCE(EXCLUDED.category, integrations.category),
		      status = EXCLUDED.status, updated_at = now()`,
		uuid.New(), tenantHotelID(c), provider, req.Category, enabled, string(cfg), status); err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.OK(c, fiber.Map{"provider": provider, "is_enabled": enabled, "status": status})
}

// TestIntegration (POST /api/admin/integrations/:provider/test) performs a real
// check: the integration must exist, be enabled, and have a non-empty config. It
// stamps last_checked_at and returns the resulting status. (Live provider probes
// are a future enhancement; this validates configuration state.)
func (h *OperationsHandler) TestIntegration(c *fiber.Ctx) error {
	if !requireAnyRoleFromToken(c, h.secretKey, hotelAdminRoles...) {
		return nil
	}
	provider := strings.TrimSpace(c.Params("provider"))
	var enabled bool
	var cfg string
	err := tenantPool(c, h.pool).QueryRow(c.Context(),
		`SELECT is_enabled, config::text FROM integrations WHERE hotel_id = $1 AND provider = $2`,
		tenantHotelID(c), provider).Scan(&enabled, &cfg)
	if err != nil {
		return response.Error(c, fiber.StatusNotFound, "integration not configured")
	}
	ok := enabled && cfg != "" && cfg != "{}"
	status := "error"
	msg := "not enabled or missing configuration"
	if ok {
		status = "connected"
		msg = "configuration valid"
	}
	_, _ = tenantPool(c, h.pool).Exec(c.Context(),
		`UPDATE integrations SET status = $1, last_checked_at = now() WHERE hotel_id = $2 AND provider = $3`,
		status, tenantHotelID(c), provider)
	return response.OK(c, fiber.Map{"provider": provider, "status": status, "ok": ok, "message": msg})
}
