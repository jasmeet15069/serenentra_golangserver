package handler

import (
	"encoding/json"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/hotelharmony/api/internal/cache"
	"github.com/hotelharmony/api/internal/domain"
	"github.com/hotelharmony/api/internal/repository/postgres"
	"github.com/hotelharmony/api/pkg/response"
)

type DashboardHandler struct {
	dashboard postgres.DashboardRepository
	cache     cache.Cache
}

func NewDashboardHandler(dashboard postgres.DashboardRepository, c cache.Cache) *DashboardHandler {
	return &DashboardHandler{dashboard: dashboard, cache: c}
}

func (h *DashboardHandler) Register(r fiber.Router) {
	r.Get("/dashboard/stats", h.Stats)
	r.Get("/dashboard/data", h.FullData)
}

func (h *DashboardHandler) Stats(c *fiber.Ctx) error {
	if cached, err := h.cache.Get(c.Context(), cache.KeyDashboardStats(tenantHotelID(c).String())); err == nil {
		var stats domain.DashboardStats
		if json.Unmarshal([]byte(cached), &stats) == nil {
			c.Set("X-Cache", "HIT")
			return response.OK(c, &stats)
		}
	}

	stats, err := h.dashboard.GetStats(c.Context())
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	if b, err := json.Marshal(stats); err == nil {
		_ = h.cache.Set(c.Context(), cache.KeyDashboardStats(tenantHotelID(c).String()), string(b), 30*time.Second)
	}
	c.Set("X-Cache", "MISS")
	return response.OK(c, stats)
}

func (h *DashboardHandler) FullData(c *fiber.Ctx) error {
	stats, err := h.dashboard.GetStats(c.Context())
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	charts, err := h.dashboard.GetChartData(c.Context())
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	return response.OK(c, map[string]interface{}{
		"stats":  stats,
		"charts": charts,
	})
}
