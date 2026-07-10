package handler

import (
	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/hotelharmony/api/internal/config"
	"github.com/hotelharmony/api/internal/service"
	"github.com/hotelharmony/api/pkg/response"
)

type PaymentHandler struct {
	payments  service.PaymentService
	secretKey string
}

func NewPaymentHandler(payments service.PaymentService, cfg *config.Config) *PaymentHandler {
	secret := ""
	if cfg != nil {
		secret = cfg.Auth.AccessTokenSecret
	}
	return &PaymentHandler{payments: payments, secretKey: secret}
}

func (h *PaymentHandler) Register(r fiber.Router) {
	r.Get("/payment-config", h.Config)
	r.Get("/exchange-rate", h.ExchangeRate)
	r.Post("/bookings/checkout", h.BookingCheckout)
	r.Post("/bookings/hold", h.HoldBooking)
	r.Post("/bookings/razorpay/order", h.RazorpayBookingOrder)
	r.Post("/payments/checkout", h.PaymentCheckout)
	r.Post("/payments/complete", h.CompletePayment)
	r.Post("/payments/razorpay/order", h.RazorpayPaymentOrder)
	r.Post("/payments/razorpay/verify", h.RazorpayVerify)
}

func (h *PaymentHandler) Config(c *fiber.Ctx) error {
	return response.OK(c, h.payments.GetConfig(c.Context()))
}

func (h *PaymentHandler) ExchangeRate(c *fiber.Ctx) error {
	base := c.Query("base", "USD")
	target := c.Query("target", "USD")
	rate, err := h.payments.GetExchangeRate(c.Context(), base, target)
	if err != nil {
		return response.Error(c, fiber.StatusBadGateway, err.Error())
	}
	return response.OK(c, map[string]interface{}{"base": base, "target": target, "rate": rate})
}

type bookingCheckoutRequest struct {
	RoomID       string `json:"room_id"`
	RoomType     string `json:"room_type"`
	UserID       string `json:"user_id"`
	Currency     string `json:"currency"`
	CheckInDate  string `json:"check_in_date"`
	CheckOutDate string `json:"check_out_date"`
	GuestName    string `json:"guest_name"`
	GuestEmail   string `json:"guest_email"`
	GuestPhone   string `json:"guest_phone"`
	Country      string `json:"country"`
}

func parseBookingPayload(c *fiber.Ctx) (bookingCheckoutRequest, service.BookingCheckoutRequest, error) {
	var req bookingCheckoutRequest
	if err := c.BodyParser(&req); err != nil {
		return req, service.BookingCheckoutRequest{}, errors.New("invalid request body")
	}
	var roomID uuid.UUID
	if strings.TrimSpace(req.RoomID) != "" {
		parsedRoomID, err := uuid.Parse(req.RoomID)
		if err != nil {
			return req, service.BookingCheckoutRequest{}, errors.New("invalid room id")
		}
		roomID = parsedRoomID
	}
	if roomID == uuid.Nil && strings.TrimSpace(req.RoomType) == "" {
		return req, service.BookingCheckoutRequest{}, errors.New("room type is required")
	}
	userID, err := uuid.Parse(req.UserID)
	if err != nil {
		return req, service.BookingCheckoutRequest{}, errors.New("invalid user id")
	}
	checkIn, err := parseDate(req.CheckInDate)
	if err != nil {
		return req, service.BookingCheckoutRequest{}, errors.New("invalid check-in date")
	}
	checkOut, err := parseDate(req.CheckOutDate)
	if err != nil {
		return req, service.BookingCheckoutRequest{}, errors.New("invalid check-out date")
	}
	return req, service.BookingCheckoutRequest{
		RoomID:       roomID,
		RoomType:     req.RoomType,
		UserID:       userID,
		Currency:     req.Currency,
		CheckInDate:  checkIn,
		CheckOutDate: checkOut,
		GuestName:    req.GuestName,
		GuestEmail:   req.GuestEmail,
		GuestPhone:   req.GuestPhone,
		Country:      req.Country,
		OriginURL:    origin(c),
	}, nil
}

func (h *PaymentHandler) BookingCheckout(c *fiber.Ctx) error {
	if !requireAuthenticatedRequest(c, h.secretKey) {
		return nil
	}

	_, bookingReq, err := parseBookingPayload(c)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	result, err := h.payments.BookingCheckout(c.Context(), bookingReq)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.OK(c, result)
}

func (h *PaymentHandler) HoldBooking(c *fiber.Ctx) error {
	if !requireAuthenticatedRequest(c, h.secretKey) {
		return nil
	}

	_, bookingReq, err := parseBookingPayload(c)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	result, err := h.payments.HoldBooking(c.Context(), bookingReq)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.OK(c, result)
}

func (h *PaymentHandler) RazorpayBookingOrder(c *fiber.Ctx) error {
	if !requireAuthenticatedRequest(c, h.secretKey) {
		return nil
	}

	_, bookingReq, err := parseBookingPayload(c)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	result, err := h.payments.CreateRazorpayBookingOrder(c.Context(), bookingReq)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.OK(c, result)
}

type paymentCheckoutRequest struct {
	PaymentID string `json:"payment_id"`
	Currency  string `json:"currency"`
	Country   string `json:"country"`
}

func (h *PaymentHandler) PaymentCheckout(c *fiber.Ctx) error {
	if !requireAuthenticatedRequest(c, h.secretKey) {
		return nil
	}

	var req paymentCheckoutRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	paymentID, err := uuid.Parse(req.PaymentID)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid payment id")
	}
	result, err := h.payments.PaymentCheckout(c.Context(), paymentID, req.Currency, req.Country, origin(c))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.OK(c, result)
}

func (h *PaymentHandler) RazorpayPaymentOrder(c *fiber.Ctx) error {
	if !requireAuthenticatedRequest(c, h.secretKey) {
		return nil
	}

	var req paymentCheckoutRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	paymentID, err := uuid.Parse(req.PaymentID)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid payment id")
	}
	result, err := h.payments.CreateRazorpayPaymentOrder(c.Context(), paymentID, req.Currency, req.Country)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.OK(c, result)
}

type completePaymentRequest struct {
	PaymentID string `json:"payment_id"`
	SessionID string `json:"session_id"`
}

func (h *PaymentHandler) CompletePayment(c *fiber.Ctx) error {
	if !requireAuthenticatedRequest(c, h.secretKey) {
		return nil
	}

	var req completePaymentRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	paymentID, err := uuid.Parse(req.PaymentID)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid payment id")
	}
	if err := h.payments.CompletePayment(c.Context(), paymentID, req.SessionID); err != nil {
		return response.Error(c, fiber.StatusConflict, err.Error())
	}
	return response.OK(c, map[string]string{"status": "completed"})
}

type razorpayVerifyRequest struct {
	PaymentID         string `json:"payment_id"`
	RazorpayOrderID   string `json:"razorpay_order_id"`
	RazorpayPaymentID string `json:"razorpay_payment_id"`
	RazorpaySignature string `json:"razorpay_signature"`
}

func (h *PaymentHandler) RazorpayVerify(c *fiber.Ctx) error {
	if !requireAuthenticatedRequest(c, h.secretKey) {
		return nil
	}

	var req razorpayVerifyRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	paymentID, err := uuid.Parse(req.PaymentID)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid payment id")
	}
	if err := h.payments.VerifyRazorpayPayment(c.Context(), service.RazorpayVerifyRequest{
		PaymentID:         paymentID,
		RazorpayOrderID:   req.RazorpayOrderID,
		RazorpayPaymentID: req.RazorpayPaymentID,
		RazorpaySignature: req.RazorpaySignature,
	}); err != nil {
		return response.Error(c, fiber.StatusConflict, err.Error())
	}
	return response.OK(c, map[string]string{"status": "completed"})
}

func parseDate(value string) (time.Time, error) {
	if t, err := time.Parse("2006-01-02", value); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, value)
}

func origin(c *fiber.Ctx) string {
	if o := c.Get("Origin"); o != "" {
		return o
	}
	return "http://localhost:8080"
}
