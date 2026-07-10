package handler

// config_handler.go — per-client config.json generation and full backup bundle download.
//
// Two endpoints (both platform-admin only):
//   GET /api/platform/tenants/:id/config.json   → download tenant config snapshot as JSON
//   GET /api/platform/tenants/:id/backup/bundle → ZIP of config.json + latest pg dump + latest redis dump

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/hotelharmony/api/pkg/response"
)

// TenantConfigSnapshot is the versioned per-client configuration document.
// It is written to config.json inside every backup bundle and is also
// downloadable on its own via GET /api/platform/tenants/:id/config.json.
//
// Schema version: bump when the shape changes in a breaking way so restore
// tooling can detect the format.
type TenantConfigSnapshot struct {
	Version     string    `json:"version"`
	GeneratedAt time.Time `json:"generated_at"`

	// Identity — mirrors the platform tenant record.
	Tenant tenantSnap `json:"tenant"`

	// RLS — row-level security and isolation topology.
	RLS rlsSnap `json:"rls"`

	// Features — effective module flags and the raw per-tenant overrides.
	Features featureSnap `json:"features"`

	// FeatureMatrix — the full role × module access matrix (default-on + denials).
	FeatureMatrix featureMatrixSnap `json:"feature_matrix"`

	// BackupPolicy — the backup configuration for this client.
	BackupPolicy backupConfig `json:"backup_policy"`
}

type tenantSnap struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	Slug           string  `json:"slug"`
	PlanTier       string  `json:"plan_tier"`
	IsActive       bool    `json:"is_active"`
	Country        *string `json:"country"`
	Currency       *string `json:"currency"`
	RoomsMax       *int    `json:"rooms_max"`
	UsersMax       *int    `json:"users_max"`
	PropertiesMax  *int    `json:"properties_max"`
	CreatedAt      string  `json:"created_at"`
}

type rlsSnap struct {
	IsolationMode  string `json:"isolation_mode"`
	DBName         string `json:"db_name"`
	RedisNamespace string `json:"redis_namespace"`
	ScopedBy       string `json:"scoped_by"`
}

type featureSnap struct {
	// EnabledModules is the effective module visibility (mask + plan gate applied).
	EnabledModules map[string]bool `json:"enabled_modules"`
	// Overrides is the raw per-tenant module mask stored in hotels.modules.
	Overrides map[string]bool `json:"overrides"`
}

type featureMatrixSnap struct {
	Roles  []string                       `json:"roles"`
	Matrix map[string]map[string]bool     `json:"matrix"`
}

// buildTenantConfig assembles the full config snapshot for a tenant by querying
// the hotels, tenant_registry, backup_configurations, and client_role_permissions
// tables. It never returns an error — missing rows degrade gracefully to safe defaults.
func (h *OperationsHandler) buildTenantConfig(ctx context.Context, id uuid.UUID) *TenantConfigSnapshot {
	snap := &TenantConfigSnapshot{
		Version:     "1",
		GeneratedAt: time.Now().UTC(),
	}

	// --- Tenant identity ---
	var name, slug, plan string
	var isActive bool
	var country, currency *string
	var settingsBytes []byte
	var createdAt time.Time

	_ = h.pool.QueryRow(ctx,
		`SELECT name, slug, plan_tier, is_active, country, currency, COALESCE(settings, '{}'), created_at
		   FROM hotels WHERE id = $1`, id).
		Scan(&name, &slug, &plan, &isActive, &country, &currency, &settingsBytes, &createdAt)

	settings := map[string]interface{}{}
	_ = json.Unmarshal(settingsBytes, &settings)

	intFromSettings := func(key string) *int {
		if v, ok := settings[key]; ok {
			switch n := v.(type) {
			case float64:
				i := int(n)
				return &i
			}
		}
		return nil
	}

	snap.Tenant = tenantSnap{
		ID:            id.String(),
		Name:          name,
		Slug:          slug,
		PlanTier:      normalizePlanTier(plan),
		IsActive:      isActive,
		Country:       country,
		Currency:      currency,
		RoomsMax:      intFromSettings("max_rooms"),
		UsersMax:      intFromSettings("max_users"),
		PropertiesMax: intFromSettings("max_properties"),
		CreatedAt:     createdAt.UTC().Format(time.RFC3339),
	}

	// --- RLS / isolation ---
	var isoMode, redisSuffix string
	var dbName *string
	if err := h.pool.QueryRow(ctx,
		`SELECT isolation_mode, COALESCE(db_name, ''), redis_namespace
		   FROM tenant_registry WHERE hotel_id = $1`, id).
		Scan(&isoMode, &dbName, &redisSuffix); err != nil {
		isoMode = "shared"
		redisSuffix = id.String()
	}
	_, _, _, sharedDB := connectionInfoFromEnv()
	effectiveDB := sharedDB
	if isoMode == "dedicated" && dbName != nil && *dbName != "" {
		effectiveDB = *dbName
	}
	scopedBy := "hotel_id row-level (shared database)"
	if isoMode == "dedicated" {
		scopedBy = "dedicated database"
	}
	snap.RLS = rlsSnap{
		IsolationMode:  isoMode,
		DBName:         effectiveDB,
		RedisNamespace: "t:" + id.String() + ":*",
		ScopedBy:       scopedBy,
	}

	// --- Features (module mask) ---
	var rawMods []byte
	_ = h.pool.QueryRow(ctx, `SELECT COALESCE(modules, '{}') FROM hotels WHERE id = $1`, id).Scan(&rawMods)
	overrides := map[string]bool{}
	_ = json.Unmarshal(rawMods, &overrides)

	snap.Features = featureSnap{
		EnabledModules: effectiveModules(overrides),
		Overrides:      overrides,
	}

	// --- Feature matrix (role × module denials) ---
	deny := map[string]map[string]bool{}
	rows, err := h.pool.Query(ctx,
		`SELECT role, feature_key FROM client_role_permissions WHERE hotel_id = $1 AND enabled = false`, id)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var role, fk string
			if rows.Scan(&role, &fk) == nil {
				if deny[role] == nil {
					deny[role] = map[string]bool{}
				}
				deny[role][fk] = true
			}
		}
	}
	snap.FeatureMatrix = featureMatrixSnap{
		Roles:  matrixRoles,
		Matrix: effectiveMatrix(deny),
	}

	// --- Backup policy ---
	bp := backupConfig{
		HotelID: id.String(), Enabled: false, CronExpr: "0 3 * * *",
		Destination: "local", RetentionDays: 30, Encrypt: true,
	}
	var notify *string
	_ = h.pool.QueryRow(ctx,
		`SELECT enabled, cron_expr, destination, retention_days, encrypt, notify_email
		   FROM backup_configurations WHERE hotel_id = $1`, id).
		Scan(&bp.Enabled, &bp.CronExpr, &bp.Destination, &bp.RetentionDays, &bp.Encrypt, &notify)
	if notify != nil {
		bp.NotifyEmail = *notify
	}
	snap.BackupPolicy = bp

	return snap
}

// saveConfigSnapshot persists the latest config snapshot for a tenant to the
// tenant_configs table. Call as a fire-and-forget goroutine after any mutation
// that changes a client's configuration (plan, modules, matrix, backup policy).
func (h *OperationsHandler) saveConfigSnapshot(id uuid.UUID) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	snap := h.buildTenantConfig(ctx, id)
	data, err := json.Marshal(snap)
	if err != nil {
		return
	}
	_, _ = h.pool.Exec(ctx,
		`INSERT INTO tenant_configs (hotel_id, config, updated_at)
		 VALUES ($1, $2::jsonb, now())
		 ON CONFLICT (hotel_id) DO UPDATE SET config = EXCLUDED.config, updated_at = now()`,
		id, string(data))
}

// GetTenantConfigJSON (GET /api/platform/tenants/:id/config) returns the stored
// config snapshot for this client as a JSON API response (not a file download).
// Each client has its own isolated snapshot: dedicated DB name, Redis namespace
// (t:{id}:*), DNS slug, plan tier, enabled modules, role-feature matrix, and
// backup policy. Falls back to a fresh build if no snapshot is stored yet.
func (h *OperationsHandler) GetTenantConfigJSON(c *fiber.Ctx) error {
	if !h.requirePlatformAdmin(c) {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid tenant id")
	}

	var configRaw []byte
	if err := h.pool.QueryRow(c.Context(),
		`SELECT config FROM tenant_configs WHERE hotel_id = $1`, id).
		Scan(&configRaw); err == nil {
		var snap TenantConfigSnapshot
		if json.Unmarshal(configRaw, &snap) == nil {
			return response.OK(c, snap)
		}
	}

	// No stored snapshot yet — build fresh and persist for next time.
	snap := h.buildTenantConfig(c.Context(), id)
	go h.saveConfigSnapshot(id)
	return response.OK(c, snap)
}

// DownloadTenantConfigJSON (GET /api/platform/tenants/:id/config.json) streams a
// freshly assembled config.json for the tenant as a file download and persists
// the snapshot to tenant_configs so the inline viewer stays up to date.
func (h *OperationsHandler) DownloadTenantConfigJSON(c *fiber.Ctx) error {
	if !h.requirePlatformAdmin(c) {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid tenant id")
	}

	snap := h.buildTenantConfig(c.Context(), id)
	go h.saveConfigSnapshot(id) // persist latest
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to generate config")
	}

	slug := snap.Tenant.Slug
	if slug == "" {
		slug = id.String()
	}
	fname := fmt.Sprintf("%s-config.json", slug)
	c.Set("Content-Type", "application/json")
	c.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, fname))
	return c.Send(data)
}

// DownloadPlatformTenantBundle (GET /api/platform/tenants/:id/backup/bundle)
// assembles a ZIP archive containing:
//   - config.json     — current tenant config snapshot
//   - {slug}-{date}.sql.gz  — latest successful postgres dump (if available)
//   - {slug}-{date}-redis.jsonl.gz — latest successful redis dump (if available)
func (h *OperationsHandler) DownloadPlatformTenantBundle(c *fiber.Ctx) error {
	if !h.requirePlatformAdmin(c) {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid tenant id")
	}

	ctx, cancel := context.WithTimeout(c.Context(), 5*time.Minute)
	defer cancel()

	snap := h.buildTenantConfig(ctx, id)
	configJSON, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to generate config")
	}

	slug := snap.Tenant.Slug
	if slug == "" {
		slug = id.String()
	}

	// Find the latest successful postgres dump on disk.
	var pgPath, pgFile string
	_ = h.pool.QueryRow(ctx,
		`SELECT COALESCE(file_path,''), COALESCE(file_path,'')
		   FROM backup_jobs
		  WHERE hotel_id = $1 AND kind = 'postgres' AND status = 'success' AND file_path IS NOT NULL
		  ORDER BY started_at DESC LIMIT 1`, id).
		Scan(&pgPath, &pgFile)

	// Find the latest successful redis dump on disk.
	var redisPath string
	_ = h.pool.QueryRow(ctx,
		`SELECT COALESCE(file_path,'')
		   FROM backup_jobs
		  WHERE hotel_id = $1 AND kind = 'redis' AND status = 'success' AND file_path IS NOT NULL
		  ORDER BY started_at DESC LIMIT 1`, id).
		Scan(&redisPath)

	// Build ZIP in memory.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	// 1. config.json
	w, err := zw.Create("config.json")
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to create zip")
	}
	if _, err := w.Write(configJSON); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to write config to zip")
	}

	// 2. Postgres dump (if on disk).
	if pgPath != "" {
		if data, ferr := os.ReadFile(pgPath); ferr == nil {
			entryName := slug + "-db.sql.gz"
			if wf, ze := zw.Create(entryName); ze == nil {
				_, _ = wf.Write(data)
			}
		}
	}

	// 3. Redis dump (if on disk).
	if redisPath != "" {
		if data, ferr := os.ReadFile(redisPath); ferr == nil {
			entryName := slug + "-redis.jsonl.gz"
			if wf, ze := zw.Create(entryName); ze == nil {
				_, _ = wf.Write(data)
			}
		}
	}

	if err := zw.Close(); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to finalise zip")
	}

	date := time.Now().UTC().Format("20060102")
	zipName := fmt.Sprintf("%s-bundle-%s.zip", slug, date)
	c.Set("Content-Type", "application/zip")
	c.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, zipName))
	return c.Send(buf.Bytes())
}
