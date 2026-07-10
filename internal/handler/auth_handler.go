package handler

import (
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hotelharmony/api/internal/cache"
	"github.com/hotelharmony/api/internal/service"
	"github.com/hotelharmony/api/pkg/response"
	"github.com/hotelharmony/api/pkg/validator"
)

type AuthHandler struct {
	auth       service.AuthService
	validate   *validator.Validator
	pool       *pgxpool.Pool
	cache      cache.Cache
	baseDomain string
}

func NewAuthHandler(auth service.AuthService, validate *validator.Validator, pool *pgxpool.Pool, c cache.Cache, baseDomain string) *AuthHandler {
	return &AuthHandler{auth: auth, validate: validate, pool: pool, cache: c, baseDomain: baseDomain}
}

func (h *AuthHandler) Register(r fiber.Router) {
	r.Post("/auth/sign-up", h.SignUp)
	r.Post("/auth/sign-in", h.SignIn)
	r.Post("/auth/sign-out", h.SignOut)
	r.Post("/auth/refresh", h.Refresh)
	r.Patch("/auth/user", h.UpdatePassword)
	r.Post("/auth/impersonate/exchange", h.ImpersonateExchange)
}

type signUpRequest struct {
	Email    string `json:"email" validate:"required,email"`
	Password string `json:"password" validate:"required,min=8"`
	FullName string `json:"full_name"`
}

func (h *AuthHandler) SignUp(c *fiber.Ctx) error {
	var req signUpRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	session, err := h.auth.SignUp(c.Context(), req.Email, req.Password, req.FullName)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	setAuthCookies(c, session)
	return response.OK(c, session)
}

type signInRequest struct {
	Email    string `json:"email" validate:"required,email"`
	Password string `json:"password" validate:"required"`
}

func (h *AuthHandler) SignIn(c *fiber.Ctx) error {
	var req signInRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	session, err := h.auth.SignIn(c.Context(), req.Email, req.Password)
	if err != nil {
		return response.Error(c, fiber.StatusUnauthorized, err.Error())
	}

	// Portal isolation: if the sign-in request comes from a tenant subdomain
	// (e.g. client1.serenentra.com), the user must belong to that tenant.
	// Platform admins are NOT exempt here — master/platform_admin login is
	// restricted to the superadmin console and must not work on client portals.
	if h.pool != nil && h.baseDomain != "" {
		host := strings.ToLower(c.Hostname())
		// Also check X-Forwarded-Host set by nginx when Host isn't forwarded
		if fwd := strings.ToLower(c.Get("X-Forwarded-Host")); fwd != "" {
			host = strings.SplitN(fwd, ",", 2)[0]
		}
		suffix := "." + strings.ToLower(h.baseDomain)
		if strings.HasSuffix(host, suffix) {
			slug := strings.TrimSuffix(host, suffix)
			if slug != "" {
				var tenantHotelID string
				qErr := h.pool.QueryRow(c.Context(),
					"SELECT id::text FROM hotels WHERE slug = $1", slug).
					Scan(&tenantHotelID)
				if qErr == nil && tenantHotelID != "" && session.User.HotelID != tenantHotelID {
					return response.Error(c, fiber.StatusForbidden, "your account is not authorized for this portal")
				}
			}
		}
	}

	setAuthCookies(c, session)
	return response.OK(c, session)
}

type impersonateExchangeRequest struct {
	Ticket string `json:"ticket" validate:"required"`
}

// ImpersonateExchange is the public half of the superadmin "login as client"
// flow: the superadmin console calls a platform-admin-gated endpoint to mint a
// one-time ticket for a specific client's admin account, then opens this
// client-portal-domain endpoint with that ticket. No password is involved —
// the ticket itself (single-use, ~60s TTL, Redis-backed) is the credential.
func (h *AuthHandler) ImpersonateExchange(c *fiber.Ctx) error {
	var req impersonateExchangeRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	ticket := strings.TrimSpace(req.Ticket)
	if ticket == "" || h.cache == nil {
		return response.Error(c, fiber.StatusBadRequest, "a valid ticket is required")
	}

	key := "impersonate:" + ticket
	userIDStr, err := h.cache.Get(c.Context(), key)
	if err != nil || userIDStr == "" {
		return response.Error(c, fiber.StatusUnauthorized, "invalid or expired ticket")
	}
	_ = h.cache.Delete(c.Context(), key) // one-time use

	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		return response.Error(c, fiber.StatusUnauthorized, "invalid ticket")
	}

	session, err := h.auth.ImpersonateSession(c.Context(), userID)
	if err != nil {
		return response.Error(c, fiber.StatusUnauthorized, err.Error())
	}

	setAuthCookies(c, session)
	return response.OK(c, session)
}

func (h *AuthHandler) SignOut(c *fiber.Ctx) error {
	clearAuthCookies(c)
	return response.OK(c, map[string]string{"status": "signed_out"})
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token" validate:"required"`
}

func (h *AuthHandler) Refresh(c *fiber.Ctx) error {
	var req refreshRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	session, err := h.auth.RefreshSession(c.Context(), req.RefreshToken)
	if err != nil {
		return response.Error(c, fiber.StatusUnauthorized, err.Error())
	}
	setAuthCookies(c, session)
	return response.OK(c, session)
}

type updatePasswordRequest struct {
	UserID          string `json:"user_id" validate:"required"`
	Password        string `json:"password" validate:"required,min=8"`
	CurrentPassword string `json:"current_password"`
}

func (h *AuthHandler) UpdatePassword(c *fiber.Ctx) error {
	var req updatePasswordRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	id, err := uuid.Parse(req.UserID)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid user id")
	}
	if req.CurrentPassword == "" {
		return response.Error(c, fiber.StatusBadRequest, "current password is required")
	}
	if err := h.auth.UpdatePasswordWithCurrent(c.Context(), id, req.CurrentPassword, req.Password); err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.OK(c, map[string]string{"status": "updated"})
}

func setAuthCookies(c *fiber.Ctx, session *service.Session) {
	if session == nil {
		return
	}
	secure := isSecureRequest(c)
	maxAge := int(session.ExpiresIn)
	if maxAge <= 0 {
		maxAge = 15 * 60
	}
	c.Cookie(&fiber.Cookie{
		Name:     "hotelops_session",
		Value:    session.AccessToken,
		Path:     "/",
		MaxAge:   maxAge,
		Secure:   secure,
		HTTPOnly: true,
		SameSite: "Lax",
	})
	if session.User != nil {
		c.Cookie(&fiber.Cookie{
			Name:     "hotelops_login_email",
			Value:    url.QueryEscape(session.User.Email),
			Path:     "/",
			MaxAge:   30 * 24 * 60 * 60,
			Secure:   secure,
			HTTPOnly: false,
			SameSite: "Lax",
		})
	}
}

func clearAuthCookies(c *fiber.Ctx) {
	secure := isSecureRequest(c)
	expired := time.Now().Add(-time.Hour)
	for _, name := range []string{"hotelops_session", "hotelops_login_email"} {
		c.Cookie(&fiber.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			Expires:  expired,
			Secure:   secure,
			HTTPOnly: name == "hotelops_session",
			SameSite: "Lax",
		})
	}
}

func isSecureRequest(c *fiber.Ctx) bool {
	return strings.EqualFold(c.Protocol(), "https") ||
		strings.EqualFold(c.Get("X-Forwarded-Proto"), "https") ||
		strings.EqualFold(c.Get("X-Forwarded-Ssl"), "on")
}
