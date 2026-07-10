package handler

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/hotelharmony/api/pkg/response"
)

type platformTenantRequest struct {
	Name     string `json:"name"`
	Slug     string `json:"slug"`
	PlanTier string `json:"plan_tier"`
	Country  string `json:"country"`
	Currency string `json:"currency"`
	// Optional contact details stored on the hotels row.
	HotelEmail string `json:"hotel_email"`
	HotelPhone string `json:"hotel_phone"`
	Timezone   string `json:"timezone"`
	// Optional initial admin for the new tenant. When both are supplied the
	// tenant is provisioned with a ready-to-use login (user + profile + admin
	// role), so a freshly created client instance is usable immediately.
	AdminEmail    string `json:"admin_email"`
	AdminPassword string `json:"admin_password"`
}

type tenantPlanUpdateRequest struct {
	PlanTier string `json:"plan_tier"`
	IsActive *bool  `json:"is_active"`
}

func (h *OperationsHandler) PlatformPlans(c *fiber.Ctx) error {
	if !h.requirePlatformAdmin(c) {
		return nil
	}
	return response.OK(c, planTierSpecs)
}

// quoteIdent safely double-quotes a Postgres identifier coming from the catalog.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// DeletePlatformTenant (DELETE /api/platform/tenants/:id) permanently removes a
// client and ALL of its data: every row scoped to the hotel across all hotel_id
// tables, then the hotel row, in one transaction. FK ordering is resolved with
// per-statement savepoints + retry passes, so it is all-or-nothing — on any
// unresolved failure the whole thing rolls back and nothing is deleted.
//
// No client is "primary": all clients have full CRUD. The ONLY guard is against
// self-lockout — a client cannot be deleted while it owns a platform-admin
// account, because that delete would erase the operator's own login. Reassign
// the admin to another client first.
func (h *OperationsHandler) DeletePlatformTenant(c *fiber.Ctx) error {
	if !h.requirePlatformAdmin(c) {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid tenant id")
	}

	var ownsAdmin bool
	_ = h.pool.QueryRow(c.Context(),
		`SELECT EXISTS (
		   SELECT 1 FROM users
		   WHERE hotel_id = $1
		     AND (platform_admin = true
		          OR id IN (SELECT user_id FROM user_roles WHERE role IN ('platform_admin','super_admin')))
		 )`, id).Scan(&ownsAdmin)
	if ownsAdmin {
		return response.Error(c, fiber.StatusConflict,
			"this client owns a platform-admin account and cannot be deleted (it would lock you out); reassign that admin to another client first")
	}

	var name string
	if err := h.pool.QueryRow(c.Context(), `SELECT name FROM hotels WHERE id = $1`, id).Scan(&name); err != nil {
		return response.Error(c, fiber.StatusNotFound, "client not found")
	}

	// Gather infrastructure state from tenant_registry before deleting DB rows.
	var tenantDomain, dnsSlug, dbName, isolationMode *string
	_ = h.pool.QueryRow(c.Context(),
		`SELECT vercel_domain, dns_record_id, db_name, isolation_mode
		 FROM tenant_registry WHERE hotel_id = $1`, id).
		Scan(&tenantDomain, &dnsSlug, &dbName, &isolationMode)

	// Every table that carries a hotel_id, except hotels itself.
	rows, err := h.pool.Query(c.Context(),
		`SELECT table_name FROM information_schema.columns
		 WHERE column_name = 'hotel_id' AND table_schema = 'public' AND table_name <> 'hotels'`)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to enumerate tenant tables")
	}
	var tables []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err == nil {
			tables = append(tables, t)
		}
	}
	rows.Close()

	tx, err := h.pool.Begin(c.Context())
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to start delete")
	}
	defer tx.Rollback(c.Context())

	// Retry passes: a DELETE blocked by a child FK rolls back to its savepoint and
	// is retried next pass (after its children are gone). Stop when all succeed or
	// a pass makes no progress.
	remaining := tables
	for pass := 0; pass < 10 && len(remaining) > 0; pass++ {
		var failed []string
		for i, t := range remaining {
			sp := fmt.Sprintf("sp_%d_%d", pass, i)
			if _, err := tx.Exec(c.Context(), "SAVEPOINT "+sp); err != nil {
				return response.Error(c, fiber.StatusInternalServerError, "delete failed: "+err.Error())
			}
			if _, err := tx.Exec(c.Context(), "DELETE FROM "+quoteIdent(t)+" WHERE hotel_id = $1", id); err != nil {
				_, _ = tx.Exec(c.Context(), "ROLLBACK TO SAVEPOINT "+sp)
				failed = append(failed, t)
			} else {
				_, _ = tx.Exec(c.Context(), "RELEASE SAVEPOINT "+sp)
			}
		}
		if len(failed) == len(remaining) {
			break // no progress — unresolved dependency
		}
		remaining = failed
	}
	if len(remaining) > 0 {
		return response.Error(c, fiber.StatusConflict,
			"could not remove all tenant data (blocked: "+strings.Join(remaining, ", ")+")")
	}

	if _, err := tx.Exec(c.Context(), `DELETE FROM hotels WHERE id = $1`, id); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to delete client: "+err.Error())
	}
	if err := tx.Commit(c.Context()); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to commit delete")
	}

	if h.featureGate != nil {
		h.featureGate.invalidate(id)
	}

	// Infrastructure cleanup — run asynchronously so HTTP response is immediate.
	// Failures here leave orphaned DNS/nginx/SSL entries but don't break data integrity.
	go func() {
		ctx := context.Background()
		baseDomain := h.provCfg.TenantBaseDomain

		// 1. Remove nginx config + SSL cert via host provisioner.
		if h.provisioner != nil && tenantDomain != nil && *tenantDomain != "" {
			_ = h.provisioner.DeprovisionDomain(ctx, *tenantDomain)
		}

		// 2. Delete GoDaddy DNS A record.
		if h.godaddy != nil && baseDomain != "" && dnsSlug != nil && *dnsSlug != "" {
			_ = h.godaddy.DeleteARecord(ctx, baseDomain, *dnsSlug)
		}

		// 3. Flush Redis namespace (t:{hotel_id}:*).
		if h.cache != nil {
			_, _ = h.cache.FlushNamespace(ctx, "t:"+id.String()+":")
		}

		// 4. Drop dedicated PostgreSQL database if one was provisioned.
		if isolationMode != nil && (*isolationMode == "dedicated" || *isolationMode == "provisioned") &&
			dbName != nil && *dbName != "" {
			// Connect to the postgres maintenance DB to drop the tenant DB.
			mainConn, err := h.pool.Acquire(ctx)
			if err == nil {
				defer mainConn.Release()
				_, _ = mainConn.Exec(ctx,
					"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()",
					*dbName)
				_, _ = mainConn.Exec(ctx, "DROP DATABASE IF EXISTS "+quoteIdent(*dbName))
			}
		}
	}()

	return response.OK(c, fiber.Map{"deleted": true, "id": id.String(), "name": name})
}

// findTenantAdminUser returns the id + email of the tenant's primary admin
// login. Tenants have accumulated admin-ish roles under different names over
// time ('hotel_admin' is canonical; 'super_admin' on the legacy demo tenant;
// 'admin' from CreatePlatformTenant's optional initial-admin flow), so this
// checks all three, preferring hotel_admin, then falls back to the
// earliest-created user among them.
func (h *OperationsHandler) findTenantAdminUser(ctx context.Context, hotelID uuid.UUID) (uuid.UUID, string, error) {
	var userID uuid.UUID
	var email string
	err := h.pool.QueryRow(ctx, `
		SELECT u.id, u.email
		FROM users u
		JOIN user_roles ur ON ur.user_id = u.id
		WHERE ur.hotel_id = $1 AND ur.role IN ('hotel_admin', 'super_admin', 'admin')
		ORDER BY
			CASE ur.role WHEN 'hotel_admin' THEN 0 WHEN 'super_admin' THEN 1 WHEN 'admin' THEN 2 ELSE 3 END,
			u.created_at ASC
		LIMIT 1`, hotelID).Scan(&userID, &email)
	return userID, email, err
}

// ensureTenantAdmin returns the tenant's admin (creating a default one if none
// exists) so superadmin "Login as client" / "Reset password" work for EVERY
// client automatically — including blank tenants provisioned without an admin.
// The created account is a client-scoped hotel_admin (NOT platform_admin, so it
// cannot cross tenants), with a random password: access is via the password-less
// impersonation ticket, and the password can be revealed/reset from the console.
// Idempotent and unique per tenant (email keyed to the slug).
func (h *OperationsHandler) ensureTenantAdmin(ctx context.Context, hotelID uuid.UUID, slug string) (uuid.UUID, string, error) {
	if uid, email, err := h.findTenantAdminUser(ctx, hotelID); err == nil {
		return uid, email, nil
	}
	baseDomain := strings.TrimSpace(h.provCfg.TenantBaseDomain)
	if baseDomain == "" {
		baseDomain = "serenentra.com"
	}
	email := "admin@" + slug + "." + baseDomain

	pw, err := generateRandomPassword(18)
	if err != nil {
		return uuid.Nil, "", err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return uuid.Nil, "", err
	}

	userID := uuid.New()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, "", err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		INSERT INTO users (id, hotel_id, email, password_hash, platform_admin, created_at, updated_at)
		VALUES ($1,$2,$3,$4,false,now(),now())`,
		userID, hotelID, email, string(hash)); err != nil {
		return uuid.Nil, "", err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO profiles (id, hotel_id, user_id, full_name, created_at, updated_at)
		VALUES ($1,$2,$3,$4,now(),now()) ON CONFLICT (user_id) DO NOTHING`,
		uuid.New(), hotelID, userID, "Client Admin"); err != nil {
		return uuid.Nil, "", err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO user_roles (id, hotel_id, user_id, role, created_at)
		VALUES ($1,$2,$3,'hotel_admin',now()) ON CONFLICT (user_id, role) DO NOTHING`,
		uuid.New(), hotelID, userID); err != nil {
		return uuid.Nil, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, "", err
	}
	return userID, email, nil
}

func generateTicket() (string, error) {
	b := make([]byte, 32)
	if _, err := crand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// generateRandomPassword returns a random password drawn from a mixed
// alphanumeric+symbol charset. Not for anything beyond one-time admin resets
// shown once to the platform operator.
func generateRandomPassword(n int) (string, error) {
	const charset = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789!@#$%^&*"
	b := make([]byte, n)
	if _, err := crand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, n)
	for i, v := range b {
		out[i] = charset[int(v)%len(charset)]
	}
	return string(out), nil
}

// PlatformTenantImpersonate (POST /api/platform/tenants/:id/impersonate) lets a
// platform_admin obtain one-time access to a client's own admin account,
// without ever knowing or resetting their password. It mints a short-lived
// (60s), single-use Redis ticket bound to the tenant's admin user id and
// returns a client-portal URL that exchanges it for a real session via
// POST /api/auth/impersonate/exchange (see auth_handler.go).
func (h *OperationsHandler) PlatformTenantImpersonate(c *fiber.Ctx) error {
	if !h.requirePlatformAdmin(c) {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid tenant id")
	}

	var slug string
	if err := h.pool.QueryRow(c.Context(), `SELECT slug FROM hotels WHERE id = $1`, id).Scan(&slug); err != nil {
		return response.Error(c, fiber.StatusNotFound, "client not found")
	}

	adminID, adminEmail, err := h.findTenantAdminUser(c.Context(), id)
	if err != nil {
		// No admin yet (e.g. a blank-provisioned client) — create a default one
		// so "Login as client" works for every client automatically.
		adminID, adminEmail, err = h.ensureTenantAdmin(c.Context(), id, slug)
		if err != nil {
			return response.Error(c, fiber.StatusInternalServerError, "could not prepare an admin account for this client")
		}
	}

	if h.cache == nil {
		return response.Error(c, fiber.StatusServiceUnavailable, "impersonation is not available")
	}
	ticket, err := generateTicket()
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to generate ticket")
	}
	// 5 minutes: still short-lived + single-use, but forgiving of the link being
	// relayed (copy/paste, chat, a slow click) rather than clicked instantly.
	const ticketTTL = 5 * time.Minute
	if err := h.cache.Set(c.Context(), "impersonate:"+ticket, adminID.String(), ticketTTL); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to store ticket")
	}

	baseDomain := strings.TrimSpace(h.provCfg.TenantBaseDomain)
	url := fmt.Sprintf("https://%s.%s/impersonate?ticket=%s", slug, baseDomain, ticket)

	return response.OK(c, fiber.Map{
		"url":         url,
		"admin_email": adminEmail,
		"expires_in":  int(ticketTTL.Seconds()),
	})
}

// PlatformTenantResetAdminPassword (POST /api/platform/tenants/:id/reset-admin-password)
// generates a brand-new password for the client's admin account and returns it
// once. Existing passwords are one-way bcrypt hashes and can never be
// recovered/displayed — this is the only way to hand a client a working
// credential again (e.g. after a lockout or support request).
func (h *OperationsHandler) PlatformTenantResetAdminPassword(c *fiber.Ctx) error {
	if !h.requirePlatformAdmin(c) {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid tenant id")
	}

	var slug string
	if err := h.pool.QueryRow(c.Context(), `SELECT slug FROM hotels WHERE id = $1`, id).Scan(&slug); err != nil {
		return response.Error(c, fiber.StatusNotFound, "client not found")
	}
	adminID, adminEmail, err := h.findTenantAdminUser(c.Context(), id)
	if err != nil {
		// No admin yet — create a default one so a fresh credential can be issued.
		adminID, adminEmail, err = h.ensureTenantAdmin(c.Context(), id, slug)
		if err != nil {
			return response.Error(c, fiber.StatusInternalServerError, "could not prepare an admin account for this client")
		}
	}

	newPassword, err := generateRandomPassword(14)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to generate password")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to hash password")
	}
	if _, err := h.pool.Exec(c.Context(),
		`UPDATE users SET password_hash = $1, updated_at = now() WHERE id = $2`,
		string(hash), adminID,
	); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to reset password")
	}

	return response.OK(c, fiber.Map{
		"admin_email":  adminEmail,
		"new_password": newPassword,
		"warning":      "This password is shown once and cannot be retrieved again. Share it securely with the client.",
	})
}

func (h *OperationsHandler) PlatformTenants(c *fiber.Ctx) error {
	if !h.requirePlatformAdmin(c) {
		return nil
	}

	rows, err := h.pool.Query(c.Context(), `
		SELECT
			h.id, h.name, h.slug, h.plan_tier, h.is_active, h.settings,
			h.country, h.currency, h.created_at, h.updated_at,
			(SELECT COUNT(*) FROM rooms r WHERE r.hotel_id = h.id) AS rooms_used,
			(SELECT COUNT(*) FROM users u WHERE u.hotel_id = h.id) AS users_used,
			(SELECT COUNT(*) FROM properties p WHERE p.hotel_id = h.id) AS properties_used,
			COALESCE(tr.provision_status, 'pending') AS provision_status,
			COALESCE(tr.isolation_mode, 'shared') AS isolation_mode,
			tr.vercel_domain,
			(SELECT u.email FROM users u
			   JOIN user_roles ur ON ur.user_id = u.id
			  WHERE ur.hotel_id = h.id AND ur.role IN ('hotel_admin', 'super_admin', 'admin')
			  ORDER BY
			    CASE ur.role WHEN 'hotel_admin' THEN 0 WHEN 'super_admin' THEN 1 WHEN 'admin' THEN 2 ELSE 3 END,
			    u.created_at ASC
			  LIMIT 1) AS admin_email
		FROM hotels h
		LEFT JOIN tenant_registry tr ON tr.hotel_id = h.id
		ORDER BY h.created_at DESC, h.name ASC`)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()

	tenants := []map[string]interface{}{}
	for rows.Next() {
		var id uuid.UUID
		var name, slug, plan string
		var isActive bool
		var settingsBytes []byte
		var country, currency *string
		var createdAt, updatedAt time.Time
		var roomsUsed, usersUsed, propertiesUsed int
		var provisionStatus, isolationMode string
		var vercelDomain *string
		var adminEmail *string
		if err := rows.Scan(
			&id, &name, &slug, &plan, &isActive, &settingsBytes,
			&country, &currency, &createdAt, &updatedAt,
			&roomsUsed, &usersUsed, &propertiesUsed,
			&provisionStatus, &isolationMode, &vercelDomain, &adminEmail,
		); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		settings := map[string]interface{}{}
		_ = json.Unmarshal(settingsBytes, &settings)
		spec := planTierByID(plan)
		tenants = append(tenants, map[string]interface{}{
			"id":               id,
			"name":             name,
			"slug":             slug,
			"plan_tier":        normalizePlanTier(plan),
			"plan_name":        spec.Name,
			"is_active":        isActive,
			"country":          country,
			"currency":         currency,
			"settings":         settings,
			"rooms_used":       roomsUsed,
			"rooms_max":        settings["max_rooms"],
			"users_used":       usersUsed,
			"users_max":        settings["max_users"],
			"properties_used":  propertiesUsed,
			"properties_max":   settings["max_properties"],
			"database_name":    settings["database_name"],
			"provision_status": provisionStatus,
			"isolation_mode":   isolationMode,
			"vercel_domain":    vercelDomain,
			"admin_email":      adminEmail,
			"created_at":       createdAt,
			"updated_at":       updatedAt,
		})
	}
	if err := rows.Err(); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}

	return response.OK(c, tenants)
}

func (h *OperationsHandler) CreatePlatformTenant(c *fiber.Ctx) error {
	if !h.requirePlatformAdmin(c) {
		return nil
	}

	var req platformTenantRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		return response.Error(c, fiber.StatusUnprocessableEntity, "client hotel name is required")
	}
	slug := normalizeSlug(req.Slug)
	if slug == "" {
		slug = normalizeSlug(name)
	}
	if slug == "" {
		return response.Error(c, fiber.StatusUnprocessableEntity, "client slug is required")
	}
	plan := normalizePlanTier(req.PlanTier)
	currency := strings.ToUpper(strings.TrimSpace(req.Currency))
	if currency == "" {
		currency = "USD"
	}
	if len(currency) != 3 {
		return response.Error(c, fiber.StatusUnprocessableEntity, "currency must be a 3-letter code")
	}
	country := strings.TrimSpace(req.Country)
	timezone := strings.TrimSpace(req.Timezone)
	if timezone == "" {
		timezone = "UTC"
	}
	hotelEmail := strings.ToLower(strings.TrimSpace(req.HotelEmail))
	hotelPhone := strings.TrimSpace(req.HotelPhone)
	settings, _ := json.Marshal(settingsForPlanTier(plan, slug))
	hotelID := uuid.New()

	tx, err := h.pool.Begin(c.Context())
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer tx.Rollback(c.Context())

	primaryColor, clientColor, adminColor := planBrandColors(plan)

	if _, err := tx.Exec(c.Context(), `
		INSERT INTO hotels (
			id, name, slug, plan_tier, is_active, settings,
			country, timezone, currency, primary_color, active_payment_gateway,
			email, phone, created_at, updated_at
		) VALUES ($1,$2,$3,$4,true,$5::jsonb,NULLIF($6,''),$7,$8,$9,'none',NULLIF($10,''),NULLIF($11,''),now(),now())`,
		hotelID, name, slug, plan, string(settings), country, timezone, currency, primaryColor, hotelEmail, hotelPhone,
	); err != nil {
		return response.Error(c, fiber.StatusConflict, err.Error())
	}

	if _, err := tx.Exec(c.Context(), `
		INSERT INTO hotel_branding (
			hotel_id, primary_color, client_primary_color, admin_primary_color,
			welcome_message, footer_text, updated_at
		) VALUES ($1,$2,$3,$4,$5,'Powered by Serenentra',now())
		ON CONFLICT (hotel_id) DO NOTHING`,
		hotelID, primaryColor, clientColor, adminColor, "Welcome to "+name,
	); err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}

	if _, err := tx.Exec(c.Context(), `
		INSERT INTO payment_configs (hotel_id, active_gateway, default_currency, gateway_mode)
		VALUES ($1,'none',$2,'test')
		ON CONFLICT (hotel_id) DO NOTHING`,
		hotelID, currency,
	); err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}

	// Optionally provision an initial admin login for the tenant.
	adminEmail := strings.ToLower(strings.TrimSpace(req.AdminEmail))
	adminProvisioned := false
	if adminEmail != "" && strings.TrimSpace(req.AdminPassword) != "" {
		if len(req.AdminPassword) < 8 {
			return response.Error(c, fiber.StatusUnprocessableEntity, "admin password must be at least 8 characters")
		}
		hash, herr := bcrypt.GenerateFromPassword([]byte(req.AdminPassword), bcrypt.DefaultCost)
		if herr != nil {
			return response.Error(c, fiber.StatusInternalServerError, "failed to hash admin password")
		}
		adminUserID := uuid.New()
		if _, err := tx.Exec(c.Context(), `
			INSERT INTO users (id, hotel_id, email, password_hash, platform_admin, created_at, updated_at)
			VALUES ($1,$2,$3,$4,false,now(),now())`,
			adminUserID, hotelID, adminEmail, string(hash),
		); err != nil {
			return response.Error(c, fiber.StatusConflict, "admin email already exists or is invalid")
		}
		if _, err := tx.Exec(c.Context(), `
			INSERT INTO profiles (id, hotel_id, user_id, full_name, created_at, updated_at)
			VALUES ($1,$2,$3,$4,now(),now())
			ON CONFLICT (user_id) DO NOTHING`,
			uuid.New(), hotelID, adminUserID, name+" Admin",
		); err != nil {
			return response.Error(c, fiber.StatusBadRequest, err.Error())
		}
		if _, err := tx.Exec(c.Context(), `
			INSERT INTO user_roles (id, hotel_id, user_id, role, created_at)
			VALUES ($1,$2,$3,'admin',now())
			ON CONFLICT (user_id, role) DO NOTHING`,
			uuid.New(), hotelID, adminUserID,
		); err != nil {
			return response.Error(c, fiber.StatusBadRequest, err.Error())
		}
		adminProvisioned = true
	}

	// Register the tenant as shared (row-level isolation) with a Redis namespace.
	if _, err := tx.Exec(c.Context(), `
		INSERT INTO tenant_registry (hotel_id, isolation_mode, db_name, redis_namespace)
		VALUES ($1,'shared',NULL,$2)
		ON CONFLICT (hotel_id) DO NOTHING`,
		hotelID, hotelID.String(),
	); err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}

	if err := tx.Commit(c.Context()); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}

	if err := h.ensureRolePortalSettingsForHotel(c, hotelID); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}

	// Provision a dedicated database for this tenant so each client has
	// physically isolated storage. On success, seed the essential hotel
	// metadata into the dedicated DB so handler queries resolve correctly,
	// then flip to 'dedicated' mode. On failure the tenant stays in shared
	// (row-level) mode — the admin can retry via provision-db later.
	isolationMode := "shared"
	var dbProvisioned bool
	if h.tenants != nil {
		dbName := deriveTenantDBName(slug)
		if pErr := h.tenants.Provision(c.Context(), dbName); pErr == nil {
			dbProvisioned = true
			tPool, tErr := h.tenants.PoolFor(c.Context(), "dedicated", dbName)
			if tErr == nil {
				if sErr := seedTenantDB(c.Context(), tPool, hotelID, name, slug, plan, currency, country, string(settings)); sErr == nil {
					isolationMode = "dedicated"
					_, _ = h.pool.Exec(c.Context(), `
						UPDATE tenant_registry
						SET isolation_mode = 'dedicated', db_name = $2, updated_at = now()
						WHERE hotel_id = $1`, hotelID, dbName)
				} else {
					isolationMode = "provisioned"
					_, _ = h.pool.Exec(c.Context(), `
						UPDATE tenant_registry
						SET isolation_mode = 'provisioned', db_name = $2, updated_at = now()
						WHERE hotel_id = $1`, hotelID, dbName)
				}
			} else {
				isolationMode = "provisioned"
				_, _ = h.pool.Exec(c.Context(), `
					UPDATE tenant_registry
					SET isolation_mode = 'provisioned', db_name = $2, updated_at = now()
					WHERE hotel_id = $1`, hotelID, dbName)
			}
		}
	}

	// Insert a provisioning_jobs row, then kick off the async DNS + Vercel pipeline.
	var jobID uuid.UUID
	_ = h.pool.QueryRow(c.Context(),
		`INSERT INTO provisioning_jobs (hotel_id, status, steps)
		 VALUES ($1,'running','[]'::jsonb) RETURNING id`, hotelID).Scan(&jobID)

	go h.runProvisioningJob(context.Background(), jobID, hotelID, slug)
	go h.saveConfigSnapshot(hotelID)
	if h.cache != nil {
		cacheCtx := context.Background()
		cacheID := hotelID.String()
		go func() {
			_ = h.cache.Set(cacheCtx, "t:"+cacheID+":plan", plan, 0)
			_ = h.cache.Set(cacheCtx, "t:"+cacheID+":currency", currency, 0)
		}()
	}

	return response.Created(c, map[string]interface{}{
		"id":                hotelID,
		"name":              name,
		"slug":              slug,
		"plan_tier":         plan,
		"currency":          currency,
		"country":           nullableText(country),
		"settings":          settingsForPlanTier(plan, slug),
		"is_active":         true,
		"isolation_mode":    isolationMode,
		"db_provisioned":    dbProvisioned,
		"admin_email":       nullableText(adminEmail),
		"admin_provisioned": adminProvisioned,
		"provision_job_id":  jobID,
		"created_at":        time.Now().UTC(),
	})
}

func (h *OperationsHandler) UpdateTenantPlan(c *fiber.Ctx) error {
	if !h.requirePlatformAdmin(c) {
		return nil
	}
	hotelID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid tenant id")
	}

	var req tenantPlanUpdateRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	plan := normalizePlanTier(req.PlanTier)

	var slug string
	err = h.pool.QueryRow(c.Context(), `SELECT slug FROM hotels WHERE id = $1`, hotelID).Scan(&slug)
	if err != nil {
		return response.Error(c, fiber.StatusNotFound, "client tenant not found")
	}

	settings, _ := json.Marshal(settingsForPlanTier(plan, slug))
	if req.IsActive == nil {
		_, err = h.pool.Exec(c.Context(), `
			UPDATE hotels
			SET plan_tier = $1, settings = $2::jsonb, updated_at = now()
			WHERE id = $3`,
			plan, string(settings), hotelID,
		)
	} else {
		_, err = h.pool.Exec(c.Context(), `
			UPDATE hotels
			SET plan_tier = $1, settings = $2::jsonb, is_active = $3, updated_at = now()
			WHERE id = $4`,
			plan, string(settings), *req.IsActive, hotelID,
		)
	}
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}

	go h.saveConfigSnapshot(hotelID)
	return response.OK(c, map[string]interface{}{
		"id":        hotelID,
		"plan_tier": plan,
		"settings":  settingsForPlanTier(plan, slug),
	})
}

type tenantEditRequest struct {
	Name     *string `json:"name"`
	Country  *string `json:"country"`
	Currency *string `json:"currency"`
}

// UpdatePlatformTenant (PUT /api/platform/tenants/:id) edits a client's profile
// fields (name / country / currency). Only provided fields change. Plan + active
// state are handled by UpdateTenantPlan; this is the "update" half of CRUD.
func (h *OperationsHandler) UpdatePlatformTenant(c *fiber.Ctx) error {
	if !h.requirePlatformAdmin(c) {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid tenant id")
	}
	var req tenantEditRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}

	sets, args := []string{}, []interface{}{}
	n := 1
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			return response.Error(c, fiber.StatusUnprocessableEntity, "name cannot be empty")
		}
		sets = append(sets, fmt.Sprintf("name = $%d", n))
		args = append(args, name)
		n++
	}
	if req.Country != nil {
		sets = append(sets, fmt.Sprintf("country = $%d", n))
		args = append(args, strings.TrimSpace(*req.Country))
		n++
	}
	if req.Currency != nil {
		sets = append(sets, fmt.Sprintf("currency = $%d", n))
		args = append(args, strings.ToUpper(strings.TrimSpace(*req.Currency)))
		n++
	}
	if len(sets) == 0 {
		return response.Error(c, fiber.StatusBadRequest, "no fields to update")
	}
	args = append(args, id)
	q := fmt.Sprintf("UPDATE hotels SET %s, updated_at = now() WHERE id = $%d", strings.Join(sets, ", "), n)
	tag, err := h.pool.Exec(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to update client")
	}
	if tag.RowsAffected() == 0 {
		return response.Error(c, fiber.StatusNotFound, "client not found")
	}
	go h.saveConfigSnapshot(id)
	return response.OK(c, fiber.Map{"id": id.String(), "updated": true})
}

// seedTenantDB inserts the essential hotel rows into a newly provisioned
// dedicated database so handlers can resolve the hotel and its settings
// without querying the shared pool. Users/auth records are intentionally
// NOT seeded here — they remain in the shared DB for auth routing.
// Also seeds 10 default rooms, 5 menu categories, 3 payment methods,
// a default restaurant outlet (pro/premium), and an initial config snapshot
// so the client portal is fully populated on first login.
func seedTenantDB(ctx context.Context, pool *pgxpool.Pool, hotelID uuid.UUID, name, slug, plan, currency, country, settings string) error {
	primaryColor, clientColor, adminColor := planBrandColors(plan)

	if _, err := pool.Exec(ctx, `
		INSERT INTO hotels (id, name, slug, plan_tier, is_active, settings, country, timezone, currency,
		                    primary_color, active_payment_gateway, created_at, updated_at)
		VALUES ($1,$2,$3,$4,true,$5::jsonb,NULLIF($6,''),'UTC',$7,$8,'none',now(),now())
		ON CONFLICT (id) DO NOTHING`,
		hotelID, name, slug, plan, settings, country, currency, primaryColor); err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO hotel_branding (hotel_id, primary_color, client_primary_color, admin_primary_color,
		                            welcome_message, footer_text, updated_at)
		VALUES ($1,$2,$3,$4,$5,'Powered by Serenentra',now())
		ON CONFLICT (hotel_id) DO NOTHING`,
		hotelID, primaryColor, clientColor, adminColor, "Welcome to "+name); err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO payment_configs (hotel_id, active_gateway, default_currency, gateway_mode)
		VALUES ($1,'none',$2,'test')
		ON CONFLICT (hotel_id) DO NOTHING`,
		hotelID, currency); err != nil {
		return err
	}

	// Intentionally seed NO operational content. Every new client starts with a
	// completely blank slate — zero rooms, menu categories, payment methods, and
	// outlets — and builds their own from the portal. Only the hotel record,
	// branding, payment config, and the tenant_configs snapshot below are created.

	// --- initial tenant_configs snapshot so first config fetch never 404s ---
	cfgJSON, _ := json.Marshal(map[string]interface{}{
		"hotel_name": name,
		"currency":   currency,
		"plan":       plan,
		"seeded":     true,
		"seeded_at":  time.Now().UTC().Format(time.RFC3339),
	})
	_, err := pool.Exec(ctx, `
		INSERT INTO tenant_configs (hotel_id, config, updated_at)
		VALUES ($1,$2::jsonb,now())
		ON CONFLICT (hotel_id) DO UPDATE SET config = EXCLUDED.config, updated_at = now()`,
		hotelID, string(cfgJSON))
	return err
}

// planBrandColors returns primary, client-accent, and admin-accent hex colors
// matching the client's plan tier so freshly provisioned portals have a
// distinctive look out of the box.
func planBrandColors(plan string) (primary, client, admin string) {
	switch normalizePlanTier(plan) {
	case "pro":
		return "#7C3AED", "#6D28D9", "#5B21B6"
	case "premium":
		return "#D97706", "#B45309", "#92400E"
	default:
		return "#2563EB", "#1E40AF", "#1D4ED8"
	}
}

func (h *OperationsHandler) requirePlatformAdmin(c *fiber.Ctx) bool {
	claims, err := jwtClaimsFromRequest(c, h.secretKey)
	if err != nil {
		_ = response.Error(c, fiber.StatusUnauthorized, "platform admin token is required")
		return false
	}
	if platformAdmin, _ := claims["platform_admin"].(bool); platformAdmin {
		return true
	}
	rawRoles, ok := claims["roles"].([]interface{})
	if !ok {
		_ = response.Error(c, fiber.StatusForbidden, "platform admin role is required")
		return false
	}
	for _, rawRole := range rawRoles {
		if role, _ := rawRole.(string); role == "platform_admin" {
			return true
		}
	}
	_ = response.Error(c, fiber.StatusForbidden, "platform admin role is required")
	return false
}
