package handler

import (
	"github.com/gofiber/fiber/v2"

	"github.com/hotelharmony/api/pkg/response"
)

// Security overview superadmin endpoint. Surfaces the platform's real security
// posture: who can access the console (platform operators), how many accounts
// exist, and the active auth/rate-limit controls. Read-only, platform-admin
// gated. The "controls" reflect the server's configured policy (see config +
// plan_catalog/rate_limit); MFA and IP allow-listing are reported as not-yet-
// enabled rather than implied.

type securityOperator struct {
	Email         string   `json:"email"`
	FullName      string   `json:"full_name"`
	PlatformAdmin bool     `json:"platform_admin"`
	Roles         []string `json:"roles"`
	CreatedAt     string   `json:"created_at"`
}

// PlatformSecurity (GET /api/platform/security) returns the security overview.
func (h *OperationsHandler) PlatformSecurity(c *fiber.Ctx) error {
	if !h.requirePlatformAdmin(c) {
		return nil
	}

	operators := []securityOperator{}
	rows, err := h.pool.Query(c.Context(), `
		SELECT u.email,
		       COALESCE(p.full_name, ''),
		       u.platform_admin,
		       COALESCE(array_agg(ur.role) FILTER (WHERE ur.role IS NOT NULL), '{}') AS roles,
		       to_char(u.created_at, 'YYYY-MM-DD') AS created
		FROM users u
		LEFT JOIN profiles p ON p.user_id = u.id
		LEFT JOIN user_roles ur ON ur.user_id = u.id
		WHERE u.platform_admin = true
		   OR u.id IN (SELECT user_id FROM user_roles WHERE role IN ('platform_admin','super_admin'))
		GROUP BY u.email, p.full_name, u.platform_admin, u.created_at
		ORDER BY u.created_at`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var op securityOperator
			if err := rows.Scan(&op.Email, &op.FullName, &op.PlatformAdmin, &op.Roles, &op.CreatedAt); err == nil {
				operators = append(operators, op)
			}
		}
	}

	var userCount int
	_ = h.pool.QueryRow(c.Context(), `SELECT count(*) FROM users`).Scan(&userCount)

	return response.OK(c, fiber.Map{
		"operators":  operators,
		"user_count": userCount,
		// Configured policy. Mirrors internal/config defaults + rate_limit.go +
		// plan_catalog.go. Update here if those change.
		"controls": fiber.Map{
			"access_token_ttl_minutes": 15,
			"refresh_token_ttl_hours":  168,
			"bcrypt_cost":              12,
			"rate_limit_per_min":       fiber.Map{"basic": 300, "pro": 1200, "premium": 6000},
			"global_ip_limit_per_min":  240,
			"tls":                      true,
			"cors_allowlist":           true,
			"mfa_enabled":              false,
			"ip_allowlist_enabled":     false,
			"refresh_rotation_enabled": false,
		},
	})
}
