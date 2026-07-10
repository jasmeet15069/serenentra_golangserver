package handler

import (
	"context"

	"github.com/gofiber/fiber/v2"

	"github.com/hotelharmony/api/internal/config"
	"github.com/hotelharmony/api/internal/service"
	"github.com/hotelharmony/api/internal/worker"
	"github.com/hotelharmony/api/pkg/response"
)

type CommunicationsHandler struct {
	emailSvc  *service.EmailService
	smsSvc    *service.SMSService
	secretKey string
}

func NewCommunicationsHandler(emailSvc *service.EmailService, smsSvc *service.SMSService, cfg *config.Config) *CommunicationsHandler {
	secret := ""
	if cfg != nil {
		secret = cfg.Auth.AccessTokenSecret
	}
	return &CommunicationsHandler{emailSvc: emailSvc, smsSvc: smsSvc, secretKey: secret}
}

func (h *CommunicationsHandler) Register(r fiber.Router) {
	r.Post("/email/send", h.SendEmail)
	r.Post("/sms/send", h.SendSMS)
}

type sendEmailRequest struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
	GuestName string `json:"guest_name,omitempty"`
}

func (h *CommunicationsHandler) SendEmail(c *fiber.Ctx) error {
	if !requireAuthenticatedRequest(c, h.secretKey) {
		return nil
	}
	var req sendEmailRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if req.To == "" || req.Subject == "" || req.Body == "" {
		return response.Error(c, fiber.StatusBadRequest, "to, subject, and body are required")
	}
	guestName := req.GuestName
	if guestName == "" {
		guestName = req.To
	}
	worker.SubmitOrRun("email.notification", func(context.Context) error {
		return h.emailSvc.SendNotification(req.To, guestName, req.Subject, req.Body)
	})
	return response.OK(c, map[string]string{"status": "queued"})
}

type sendSMSRequest struct {
	To      string `json:"to"`
	Message string `json:"message"`
	AlertType string `json:"alert_type,omitempty"`
}

func (h *CommunicationsHandler) SendSMS(c *fiber.Ctx) error {
	if !requireAuthenticatedRequest(c, h.secretKey) {
		return nil
	}
	var req sendSMSRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if req.To == "" || req.Message == "" {
		return response.Error(c, fiber.StatusBadRequest, "to and message are required")
	}
	worker.SubmitOrRun("sms.send", func(context.Context) error {
		if req.AlertType != "" {
			return h.smsSvc.SendAlert(req.To, req.AlertType, req.Message)
		}
		return h.smsSvc.Send(req.To, req.Message)
	})
	return response.OK(c, map[string]string{"status": "queued"})
}
