package handler

import (
	"github.com/gofiber/fiber/v2"

	"github.com/hotelharmony/api/internal/config"
	"github.com/hotelharmony/api/internal/domain"
	"github.com/hotelharmony/api/internal/repository/postgres"
	"github.com/hotelharmony/api/internal/service"
	"github.com/hotelharmony/api/pkg/response"
)

type AIHandler struct {
	ai        service.AIService
	rooms     postgres.RoomRepository
	dashboard postgres.DashboardRepository
	secretKey string
}

func NewAIHandler(ai service.AIService, rooms postgres.RoomRepository, dashboard postgres.DashboardRepository, cfg *config.Config) *AIHandler {
	secret := ""
	if cfg != nil {
		secret = cfg.Auth.AccessTokenSecret
	}
	return &AIHandler{ai: ai, rooms: rooms, dashboard: dashboard, secretKey: secret}
}

func (h *AIHandler) Register(r fiber.Router) {
	r.Post("/ai/chat", h.Chat)
	r.Post("/functions/ai-menu-suggestions", h.MenuSuggestions)
	r.Post("/functions/ai-complaint-analysis", h.ComplaintAnalysis)
	r.Post("/functions/voice-assistant-token", h.VoiceAssistantToken)
}

type aiChatRequest struct {
	Messages []domain.ChatMessage `json:"messages"`
}

func (h *AIHandler) Chat(c *fiber.Ctx) error {
	if !requireAuthenticatedRequest(c, h.secretKey) {
		return nil
	}

	var req aiChatRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}

	rooms, _ := h.rooms.ListRooms(c.Context(), tenantHotelID(c), nil)
	activeOrders, pendingComplaints := 0, 0
	if stats, err := h.dashboard.GetStats(c.Context()); err == nil {
		activeOrders = stats.ActiveOrders
		pendingComplaints = stats.PendingComplaints
	}

	reply, sources, err := h.ai.Chat(c.Context(), rooms, activeOrders, pendingComplaints, req.Messages)
	if err != nil {
		return response.Error(c, fiber.StatusBadGateway, err.Error())
	}
	return response.OK(c, map[string]interface{}{
		"reply":   reply,
		"sources": sources,
	})
}

func (h *AIHandler) MenuSuggestions(c *fiber.Ctx) error {
	if !requireAuthenticatedRequest(c, h.secretKey) {
		return nil
	}

	var req service.MenuSuggestionsRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	result, err := h.ai.MenuSuggestions(c.Context(), req)
	if err != nil {
		return response.Error(c, fiber.StatusBadGateway, err.Error())
	}
	return response.OK(c, result)
}

func (h *AIHandler) ComplaintAnalysis(c *fiber.Ctx) error {
	if !requireAuthenticatedRequest(c, h.secretKey) {
		return nil
	}

	var req service.ComplaintAnalysisRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	result, err := h.ai.ComplaintAnalysis(c.Context(), req)
	if err != nil {
		return response.Error(c, fiber.StatusBadGateway, err.Error())
	}
	return response.OK(c, result)
}

func (h *AIHandler) VoiceAssistantToken(c *fiber.Ctx) error {
	if !requireAuthenticatedRequest(c, h.secretKey) {
		return nil
	}
	return response.Error(c, fiber.StatusNotImplemented, "voice assistant realtime token is not configured")
}
