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

// DemoHandler handles demo-request submissions from the Serenentra landing page.
// Submit is public (before authGate). List requires platform-admin JWT.
type DemoHandler struct {
	pool     *pgxpool.Pool
	emailSvc *service.EmailService
	secret   string
	notifyTo string
}

func NewDemoHandler(pool *pgxpool.Pool, emailSvc *service.EmailService, secret string) *DemoHandler {
	return &DemoHandler{
		pool:     pool,
		emailSvc: emailSvc,
		secret:   secret,
		notifyTo: "sales@serenentra.com",
	}
}

func (h *DemoHandler) isPlatformAdmin(c *fiber.Ctx) bool {
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

func (h *DemoHandler) Register(r fiber.Router) {
	r.Post("/demo-request", h.Submit)
	r.Get("/demo-requests", h.List)          // superadmin-only list (checked inside handler)
	r.Delete("/demo-requests/:id", h.Delete) // superadmin-only (checked inside handler)
}

type demoRequestBody struct {
	Name         string `json:"name"`
	Email        string `json:"email"`
	Phone        string `json:"phone"`
	PropertyName string `json:"property"`
	Rooms        string `json:"rooms"`
	Country      string `json:"country"`
	Message      string `json:"message"`
}

func (h *DemoHandler) Submit(c *fiber.Ctx) error {
	var req demoRequestBody
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}

	req.Name = strings.TrimSpace(req.Name)
	req.Email = strings.TrimSpace(req.Email)
	req.PropertyName = strings.TrimSpace(req.PropertyName)

	if req.Name == "" || req.Email == "" || req.PropertyName == "" || req.Rooms == "" {
		return response.Error(c, fiber.StatusBadRequest, "name, email, property and rooms are required")
	}

	// Persist to DB.
	if h.pool != nil {
		_, err := h.pool.Exec(
			context.Background(),
			`INSERT INTO demo_requests (name, email, phone, property_name, rooms, country, message)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			req.Name, req.Email, req.Phone, req.PropertyName, req.Rooms, req.Country, req.Message,
		)
		if err != nil {
			// Log but don't fail — the lead is not lost (email below still fires).
			_ = err
		}
	}

	// Fire email notification asynchronously so the HTTP response is not held
	// waiting on SMTP. The lead is already persisted in the DB above.
	if h.emailSvc != nil {
		svc := h.emailSvc
		notifyTo := h.notifyTo
		body := fmt.Sprintf(
			"New demo request from %s (%s)\n\nProperty: %s\nRooms: %s\nCountry: %s\nPhone: %s\n\nMessage:\n%s",
			req.Name, req.Email,
			req.PropertyName, req.Rooms,
			req.Country, req.Phone,
			req.Message,
		)
		subject := fmt.Sprintf("New Demo Request — %s (%s rooms)", req.PropertyName, req.Rooms)
		go func() {
			_ = svc.SendNotification(notifyTo, "Serenentra Sales", subject, body)
		}()
	}

	return response.OK(c, fiber.Map{"status": "received"})
}

// List returns all demo requests — platform-admin only.
func (h *DemoHandler) List(c *fiber.Ctx) error {
	if !h.isPlatformAdmin(c) {
		return response.Error(c, fiber.StatusForbidden, "platform admin required")
	}
	if h.pool == nil {
		return response.OK(c, []interface{}{})
	}
	rows, err := h.pool.Query(
		context.Background(),
		`SELECT id, name, email, phone, property_name, rooms, country, message, status, created_at
		 FROM demo_requests ORDER BY created_at DESC LIMIT 200`,
	)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "query failed")
	}
	defer rows.Close()

	type lead struct {
		ID           string    `json:"id"`
		Name         string    `json:"name"`
		Email        string    `json:"email"`
		Phone        *string   `json:"phone"`
		PropertyName string    `json:"property_name"`
		Rooms        string    `json:"rooms"`
		Country      *string   `json:"country"`
		Message      *string   `json:"message"`
		Status       string    `json:"status"`
		CreatedAt    time.Time `json:"created_at"`
	}
	var leads []lead
	for rows.Next() {
		var l lead
		if err := rows.Scan(&l.ID, &l.Name, &l.Email, &l.Phone, &l.PropertyName,
			&l.Rooms, &l.Country, &l.Message, &l.Status, &l.CreatedAt); err != nil {
			continue
		}
		leads = append(leads, l)
	}
	if leads == nil {
		leads = []lead{}
	}
	return response.OK(c, leads)
}

// Delete removes a demo request — platform-admin only.
func (h *DemoHandler) Delete(c *fiber.Ctx) error {
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
	ct, err := h.pool.Exec(context.Background(), `DELETE FROM demo_requests WHERE id = $1`, id)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "delete failed")
	}
	if ct.RowsAffected() == 0 {
		return response.Error(c, fiber.StatusNotFound, "lead not found")
	}
	return response.OK(c, fiber.Map{"status": "deleted"})
}
