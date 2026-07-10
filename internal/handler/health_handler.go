package handler

import (
	"github.com/gofiber/fiber/v2"

	"github.com/hotelharmony/api/internal/cache"
	"github.com/hotelharmony/api/internal/repository/postgres"
	"github.com/hotelharmony/api/pkg/response"
)

type HealthHandler struct {
	db    *postgres.DB
	cache cache.Cache
}

func NewHealthHandler(db *postgres.DB, c cache.Cache) *HealthHandler {
	return &HealthHandler{db: db, cache: c}
}

func (h *HealthHandler) Register(app *fiber.App) {
	app.Get("/health", h.Health)
	app.Get("/ready", h.Health)
}

func (h *HealthHandler) Health(c *fiber.Ctx) error {
	return response.OK(c, map[string]interface{}{
		"status": "ok",
		"db":     h.db.Stats(),
	})
}
