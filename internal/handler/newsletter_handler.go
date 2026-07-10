package handler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hotelharmony/api/internal/service"
	"github.com/hotelharmony/api/pkg/response"
)

// NewsletterHandler handles newsletter subscription submissions from the Serenentra landing page.
// Subscribe is public (before authGate). List requires platform-admin JWT.
type NewsletterHandler struct {
	pool     *pgxpool.Pool
	emailSvc *service.EmailService
	secret   string
	notifyTo string
}

func NewNewsletterHandler(pool *pgxpool.Pool, emailSvc *service.EmailService, secret string) *NewsletterHandler {
	return &NewsletterHandler{
		pool:     pool,
		emailSvc: emailSvc,
		secret:   secret,
		notifyTo: "sales@serenentra.com",
	}
}

func (h *NewsletterHandler) isPlatformAdmin(c *fiber.Ctx) bool {
	claims, err := jwtClaimsFromRequest(c, h.secret)
	if err != nil {
		return false
	}
	if pa, _ := claims["platform_admin"].(bool); pa {
		return true
	}
	if rawRoles, ok := claims["roles"].([]interface{}); ok {
		for _, rr := range rawRoles {
			if role, _ := rr.(string); role == "platform_admin" || role == "super_admin" {
				return true
			}
		}
	}
	return false
}

func (h *NewsletterHandler) Register(r fiber.Router) {
	r.Post("/newsletter-subscribe", h.Subscribe)
	r.Get("/newsletter-subscribers", h.List)
	r.Delete("/newsletter-subscribers/:id", h.Delete)
}

type newsletterBody struct {
	Email string `json:"email"`
}

func (h *NewsletterHandler) Subscribe(c *fiber.Ctx) error {
	var req newsletterBody
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}

	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	if req.Email == "" || !strings.Contains(req.Email, "@") {
		return response.Error(c, fiber.StatusBadRequest, "valid email is required")
	}

	if h.pool != nil {
		_, err := h.pool.Exec(
			context.Background(),
			`INSERT INTO newsletter_subscribers (email) VALUES ($1)
			 ON CONFLICT (email) DO UPDATE SET updated_at = now()`,
			req.Email,
		)
		if err != nil {
			_ = err
		}
	}

	if h.emailSvc != nil {
		svc := h.emailSvc
		notifyTo := h.notifyTo
		body := fmt.Sprintf("New newsletter subscriber: %s", req.Email)
		go func() {
			_ = svc.SendNotification(notifyTo, "Serenentra Updates", "New Newsletter Subscriber", body)
		}()
	}

	return response.OK(c, fiber.Map{"status": "subscribed"})
}

func (h *NewsletterHandler) List(c *fiber.Ctx) error {
	if !h.isPlatformAdmin(c) {
		return response.Error(c, fiber.StatusForbidden, "platform admin required")
	}
	if h.pool == nil {
		return response.OK(c, []interface{}{})
	}
	rows, err := h.pool.Query(
		context.Background(),
		`SELECT id, email, created_at FROM newsletter_subscribers ORDER BY created_at DESC LIMIT 1000`,
	)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "query failed")
	}
	defer rows.Close()

	type sub struct {
		ID        string    `json:"id"`
		Email     string    `json:"email"`
		CreatedAt time.Time `json:"created_at"`
	}
	var subs []sub
	for rows.Next() {
		var s sub
		if err := rows.Scan(&s.ID, &s.Email, &s.CreatedAt); err != nil {
			continue
		}
		subs = append(subs, s)
	}
	if subs == nil {
		subs = []sub{}
	}
	return response.OK(c, subs)
}

// Delete removes a newsletter subscriber — platform-admin only.
func (h *NewsletterHandler) Delete(c *fiber.Ctx) error {
	if !h.isPlatformAdmin(c) {
		return response.Error(c, fiber.StatusForbidden, "platform admin required")
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid id")
	}
	if h.pool == nil {
		return response.Error(c, fiber.StatusInternalServerError, "database unavailable")
	}
	ct, err := h.pool.Exec(context.Background(), `DELETE FROM newsletter_subscribers WHERE id = $1`, id)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "delete failed")
	}
	if ct.RowsAffected() == 0 {
		return response.Error(c, fiber.StatusNotFound, "subscriber not found")
	}
	return response.OK(c, fiber.Map{"status": "deleted"})
}
