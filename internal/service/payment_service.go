package service

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/hotelharmony/api/internal/cache"
	"github.com/hotelharmony/api/internal/config"
	"github.com/hotelharmony/api/internal/domain"
	"github.com/hotelharmony/api/internal/repository/postgres"
)

// zeroDecimalCurrencies are Stripe currencies that must NOT be multiplied by 100.
var zeroDecimalCurrencies = map[string]bool{
	"BIF": true, "CLP": true, "DJF": true, "GNF": true, "JPY": true,
	"KMF": true, "KRW": true, "MGA": true, "PYG": true, "RWF": true,
	"UGX": true, "VND": true, "VUV": true, "XAF": true, "XOF": true, "XPF": true,
}

// BookingCheckoutRequest contains all inputs for a new booking checkout.
type BookingCheckoutRequest struct {
	RoomID       uuid.UUID `json:"room_id"`
	RoomType     string    `json:"room_type"`
	UserID       uuid.UUID `json:"user_id" validate:"required"`
	Currency     string    `json:"currency"`
	CheckInDate  time.Time `json:"check_in_date" validate:"required"`
	CheckOutDate time.Time `json:"check_out_date" validate:"required"`
	GuestName    string    `json:"guest_name"`
	GuestEmail   string    `json:"guest_email"`
	GuestPhone   string    `json:"guest_phone"`
	Country      string    `json:"country"`
	OriginURL    string    `json:"-"`
}

// CheckoutResult is returned after a successful Stripe checkout session creation.
type CheckoutResult struct {
	CheckoutURL string    `json:"checkout_url"`
	SessionID   string    `json:"session_id"`
	StayID      uuid.UUID `json:"stay_id"`
	PaymentID   uuid.UUID `json:"payment_id,omitempty"`
}

// RazorpayOrderResult is returned to the frontend before opening Razorpay Checkout.
type RazorpayOrderResult struct {
	OrderID     string    `json:"order_id"`
	KeyID       string    `json:"key_id"`
	PaymentID   uuid.UUID `json:"payment_id"`
	StayID      uuid.UUID `json:"stay_id"`
	Amount      int64     `json:"amount"`
	Currency    string    `json:"currency"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
}

// RazorpayVerifyRequest contains the signed Checkout response from Razorpay.
type RazorpayVerifyRequest struct {
	PaymentID         uuid.UUID `json:"payment_id"`
	RazorpayOrderID   string    `json:"razorpay_order_id"`
	RazorpayPaymentID string    `json:"razorpay_payment_id"`
	RazorpaySignature string    `json:"razorpay_signature"`
}

// PaymentService orchestrates Stripe checkout and payment completion.
type PaymentService interface {
	BookingCheckout(ctx context.Context, req BookingCheckoutRequest) (*CheckoutResult, error)
	HoldBooking(ctx context.Context, req BookingCheckoutRequest) (*CheckoutResult, error)
	CreateRazorpayBookingOrder(ctx context.Context, req BookingCheckoutRequest) (*RazorpayOrderResult, error)
	CreateRazorpayPaymentOrder(ctx context.Context, paymentID uuid.UUID, currency, country string) (*RazorpayOrderResult, error)
	VerifyRazorpayPayment(ctx context.Context, req RazorpayVerifyRequest) error
	PaymentCheckout(ctx context.Context, paymentID uuid.UUID, currency, country, originURL string) (*CheckoutResult, error)
	CompletePayment(ctx context.Context, paymentID uuid.UUID, sessionID string) error
	GetConfig(ctx context.Context) map[string]interface{}
	GetExchangeRate(ctx context.Context, base, target string) (float64, error)
}

type paymentService struct {
	roomRepo    postgres.RoomRepository
	paymentRepo postgres.PaymentRepository
	cache       cache.Cache
	cfg         *config.Config
	log         *zap.Logger
	httpClient  *http.Client
	fxMu        sync.Mutex
}

func NewPaymentService(
	roomRepo postgres.RoomRepository,
	paymentRepo postgres.PaymentRepository,
	c cache.Cache,
	cfg *config.Config,
	log *zap.Logger,
) PaymentService {
	return &paymentService{
		roomRepo:    roomRepo,
		paymentRepo: paymentRepo,
		cache:       c,
		cfg:         cfg,
		log:         log,
		httpClient:  &http.Client{Timeout: 15 * time.Second},
	}
}

func normalizedRoomType(roomType string) string {
	return strings.ToLower(strings.TrimSpace(roomType))
}

func bookingRoomLockKey(req BookingCheckoutRequest) (string, error) {
	if req.RoomID != uuid.Nil {
		return req.RoomID.String(), nil
	}
	roomType := normalizedRoomType(req.RoomType)
	if roomType == "" {
		return "", fmt.Errorf("room type is required")
	}
	return "type:" + roomType, nil
}

func bookingNights(checkIn, checkOut time.Time) int {
	nights := int(checkOut.Sub(checkIn).Hours() / 24)
	if nights < 1 {
		return 1
	}
	return nights
}

func roomTypeLabel(roomType string) string {
	parts := strings.Fields(strings.ReplaceAll(strings.TrimSpace(roomType), "_", " "))
	if len(parts) == 0 {
		return "Selected"
	}
	for i, part := range parts {
		lower := strings.ToLower(part)
		parts[i] = strings.ToUpper(lower[:1]) + lower[1:]
	}
	return strings.Join(parts, " ")
}

func (s *paymentService) resolveBookingRoom(ctx context.Context, req BookingCheckoutRequest) (*domain.Room, error) {
	var (
		room *domain.Room
		err  error
	)
	if req.RoomID != uuid.Nil {
		room, err = s.roomRepo.FindRoomByID(ctx, postgres.DemoHotelID, req.RoomID)
	} else {
		roomType := normalizedRoomType(req.RoomType)
		if roomType == "" {
			return nil, fmt.Errorf("room type is required")
		}
		room, err = s.roomRepo.FindAvailableRoom(ctx, postgres.DemoHotelID, &roomType)
	}
	if err != nil {
		return nil, fmt.Errorf("selected room type is no longer available")
	}
	if room.Status != domain.RoomStatusAvailable {
		return nil, fmt.Errorf("selected room type is no longer available")
	}
	return room, nil
}

// BookingCheckout creates a guest_stay, payment record, and Stripe session.
func (s *paymentService) BookingCheckout(ctx context.Context, req BookingCheckoutRequest) (*CheckoutResult, error) {
	if _, err := s.stripeSecretKey(ctx); err != nil {
		return nil, err
	}
	if err := s.ensureStripeChargesEnabled(ctx); err != nil {
		return nil, err
	}
	allocationKey, err := bookingRoomLockKey(req)
	if err != nil {
		return nil, err
	}
	lockKey := fmt.Sprintf("lock:booking:%s:%s:%s:%s", req.UserID, allocationKey, req.CheckInDate.Format("2006-01-02"), req.CheckOutDate.Format("2006-01-02"))
	locked, err := s.cache.SetNX(ctx, lockKey, "1", cache.TTLLock)
	if err != nil {
		s.log.Warn("booking lock unavailable", zap.Error(err))
	} else if !locked {
		return nil, fmt.Errorf("booking is already being processed")
	} else {
		defer func() { _ = s.cache.Delete(context.Background(), lockKey) }()
	}

	currency := strings.ToUpper(req.Currency)
	if currency == "" {
		currency = "USD"
	}

	nights := bookingNights(req.CheckInDate, req.CheckOutDate)
	room, err := s.resolveBookingRoom(ctx, req)
	if err != nil {
		return nil, err
	}

	usdAmount := room.PricePerNight * float64(nights)

	rate, err := s.GetExchangeRate(ctx, "USD", currency)
	if err != nil {
		return nil, fmt.Errorf("unable to price booking in %s: %w", currency, err)
	}
	convertedAmount := roundTo2(usdAmount * rate)

	guestName := req.GuestName
	if guestName == "" {
		guestName = "Guest"
	}
	guestEmail := &req.GuestEmail
	guestPhone := &req.GuestPhone
	notes := fmt.Sprintf("Stripe checkout pending. Country: %s. Currency: %s. Rate: %.6f", req.Country, currency, rate)

	stay, err := s.roomRepo.CreateStay(ctx, postgres.DemoHotelID, &domain.GuestStay{
		GuestID:      &req.UserID,
		RoomID:       room.ID,
		GuestName:    guestName,
		GuestEmail:   guestEmail,
		GuestPhone:   guestPhone,
		CheckInDate:  req.CheckInDate,
		CheckOutDate: req.CheckOutDate,
		TotalAmount:  &usdAmount,
		Notes:        &notes,
		CreatedBy:    &req.UserID,
	})
	if err != nil {
		return nil, fmt.Errorf("create stay: %w", err)
	}

	payNum := fmt.Sprintf("PAY-%s-%s", time.Now().Format("150405"), strings.ToUpper(stay.ID.String()[:6]))
	payMethod := "stripe"
	payNotes := fmt.Sprintf("%s room booking for %d night(s) in %s. Assigned room: %s", roomTypeLabel(room.RoomType), nights, currency, room.RoomNumber)
	payment, err := s.paymentRepo.Create(ctx, &domain.Payment{
		PaymentNumber: payNum,
		GuestStayID:   &stay.ID,
		Amount:        convertedAmount,
		PaymentMethod: payMethod,
		Status:        domain.PaymentStatusPending,
		ProcessedBy:   &req.UserID,
		Notes:         &payNotes,
	})
	if err != nil {
		return nil, fmt.Errorf("create payment: %w", err)
	}

	_ = s.roomRepo.UpdateRoomStatus(ctx, postgres.DemoHotelID, room.ID, domain.RoomStatusOccupied)
	_ = s.cache.Delete(ctx, cache.KeyDashboardStats(postgres.DemoHotelID.String()), cache.KeyRoomList(postgres.DemoHotelID.String(), "all"), cache.KeyRoomList(postgres.DemoHotelID.String(), string(domain.RoomStatusAvailable)), cache.KeyRoomList(postgres.DemoHotelID.String(), string(domain.RoomStatusOccupied)))

	roomType := roomTypeLabel(room.RoomType)
	description := fmt.Sprintf("%s room - %d night(s). Room number assigned after booking.", roomType, nights)
	successURL := fmt.Sprintf("%s/guest?booking=success&stay_id=%s&payment_id=%s&session_id={CHECKOUT_SESSION_ID}",
		req.OriginURL, stay.ID, payment.ID)
	cancelURL := fmt.Sprintf("%s/guest?booking=cancelled&stay_id=%s", req.OriginURL, stay.ID)

	session, err := s.createStripeSession(ctx, stripeSessionParams{
		Currency:       currency,
		Amount:         convertedAmount,
		GuestEmail:     req.GuestEmail,
		StayID:         stay.ID,
		PaymentID:      payment.ID,
		RoomID:         room.ID,
		Country:        req.Country,
		ProductName:    fmt.Sprintf("Hotel %s Room Booking", roomType),
		Description:    description,
		SuccessURL:     successURL,
		CancelURL:      cancelURL,
		IdempotencyKey: stay.ID.String(),
	})
	if err != nil {
		_ = s.paymentRepo.Delete(ctx, payment.ID)
		_ = s.roomRepo.DeleteStay(ctx, postgres.DemoHotelID, stay.ID)
		_ = s.roomRepo.UpdateRoomStatus(ctx, postgres.DemoHotelID, room.ID, domain.RoomStatusAvailable)
		return nil, fmt.Errorf("Stripe checkout failed: %w", err)
	}

	return &CheckoutResult{
		CheckoutURL: session.URL,
		SessionID:   session.ID,
		StayID:      stay.ID,
		PaymentID:   payment.ID,
	}, nil
}

// CreateRazorpayBookingOrder creates the booking/payment records, then creates
// a Razorpay Order. The booking is only confirmed paid after signature verify.
func (s *paymentService) CreateRazorpayBookingOrder(ctx context.Context, req BookingCheckoutRequest) (*RazorpayOrderResult, error) {
	gatewayCfg, keyID, keySecret, err := s.razorpayCredentials(ctx)
	if err != nil {
		return nil, err
	}

	allocationKey, err := bookingRoomLockKey(req)
	if err != nil {
		return nil, err
	}
	lockKey := fmt.Sprintf("lock:razorpay-booking:%s:%s:%s:%s", req.UserID, allocationKey, req.CheckInDate.Format("2006-01-02"), req.CheckOutDate.Format("2006-01-02"))
	locked, err := s.cache.SetNX(ctx, lockKey, "1", cache.TTLLock)
	if err != nil {
		s.log.Warn("razorpay booking lock unavailable", zap.Error(err))
	} else if !locked {
		return nil, fmt.Errorf("booking is already being processed")
	} else {
		defer func() { _ = s.cache.Delete(context.Background(), lockKey) }()
	}

	currency := strings.ToUpper(strings.TrimSpace(req.Currency))
	if currency == "" {
		currency = strings.ToUpper(strings.TrimSpace(gatewayCfg.DefaultCurrency))
	}
	if currency == "" {
		currency = "INR"
	}
	if currency != "INR" {
		return nil, fmt.Errorf("Razorpay checkout is configured for INR. Select India (INR) before paying online")
	}

	room, err := s.resolveBookingRoom(ctx, req)
	if err != nil {
		return nil, err
	}
	nights := bookingNights(req.CheckInDate, req.CheckOutDate)
	usdAmount := room.PricePerNight * float64(nights)
	rate, err := s.GetExchangeRate(ctx, "USD", currency)
	if err != nil {
		return nil, fmt.Errorf("unable to price booking in %s: %w", currency, err)
	}
	convertedAmount := roundTo2(usdAmount * rate)
	minorAmount := razorpayMinorAmount(convertedAmount)

	guestName := strings.TrimSpace(req.GuestName)
	if guestName == "" {
		guestName = "Guest"
	}
	guestEmail := &req.GuestEmail
	guestPhone := &req.GuestPhone
	roomType := roomTypeLabel(room.RoomType)
	stayNotes := fmt.Sprintf("Razorpay checkout pending. Country: %s. Currency: %s. Rate: %.6f. Room number assigned after booking.", req.Country, currency, rate)

	stay, err := s.roomRepo.CreateStay(ctx, postgres.DemoHotelID, &domain.GuestStay{
		GuestID:      &req.UserID,
		RoomID:       room.ID,
		GuestName:    guestName,
		GuestEmail:   guestEmail,
		GuestPhone:   guestPhone,
		CheckInDate:  req.CheckInDate,
		CheckOutDate: req.CheckOutDate,
		TotalAmount:  &usdAmount,
		Notes:        &stayNotes,
		CreatedBy:    &req.UserID,
	})
	if err != nil {
		return nil, fmt.Errorf("create stay: %w", err)
	}

	payNum := fmt.Sprintf("PAY-%s-%s", time.Now().Format("150405"), strings.ToUpper(stay.ID.String()[:6]))
	payNotes := fmt.Sprintf("Razorpay checkout pending. %s room booking for %d night(s) in %s. Assigned room: %s", roomType, nights, currency, room.RoomNumber)
	payment, err := s.paymentRepo.Create(ctx, &domain.Payment{
		PaymentNumber: payNum,
		GuestStayID:   &stay.ID,
		Amount:        convertedAmount,
		PaymentMethod: "razorpay",
		Status:        domain.PaymentStatusPending,
		ProcessedBy:   &req.UserID,
		Notes:         &payNotes,
	})
	if err != nil {
		_ = s.roomRepo.DeleteStay(ctx, postgres.DemoHotelID, stay.ID)
		return nil, fmt.Errorf("create payment: %w", err)
	}

	_ = s.roomRepo.UpdateRoomStatus(ctx, postgres.DemoHotelID, room.ID, domain.RoomStatusOccupied)
	_ = s.cache.Delete(ctx, cache.KeyDashboardStats(postgres.DemoHotelID.String()), cache.KeyRoomList(postgres.DemoHotelID.String(), "all"), cache.KeyRoomList(postgres.DemoHotelID.String(), string(domain.RoomStatusAvailable)), cache.KeyRoomList(postgres.DemoHotelID.String(), string(domain.RoomStatusOccupied)))

	description := fmt.Sprintf("%s room - %d night(s). Room number assigned after booking.", roomType, nights)
	orderID, err := s.createRazorpayOrder(ctx, keyID, keySecret, minorAmount, currency, payment.ID.String(), map[string]string{
		"payment_id": payment.ID.String(),
		"stay_id":    stay.ID.String(),
		"room_id":    room.ID.String(),
		"guest_id":   req.UserID.String(),
	})
	if err != nil {
		_ = s.paymentRepo.Delete(ctx, payment.ID)
		_ = s.roomRepo.DeleteStay(ctx, postgres.DemoHotelID, stay.ID)
		_ = s.roomRepo.UpdateRoomStatus(ctx, postgres.DemoHotelID, room.ID, domain.RoomStatusAvailable)
		return nil, fmt.Errorf("Razorpay order failed: %w", err)
	}

	payNotes = fmt.Sprintf("%s. Razorpay order: %s", payNotes, orderID)
	_ = s.paymentRepo.UpdateAmountAndNotes(ctx, payment.ID, convertedAmount, "razorpay", payNotes)

	return &RazorpayOrderResult{
		OrderID:     orderID,
		KeyID:       keyID,
		PaymentID:   payment.ID,
		StayID:      stay.ID,
		Amount:      minorAmount,
		Currency:    currency,
		Name:        "Hotel Room Booking",
		Description: description,
	}, nil
}

// HoldBooking creates a pending booking without exposing a physical room before the hold exists.
func (s *paymentService) HoldBooking(ctx context.Context, req BookingCheckoutRequest) (*CheckoutResult, error) {
	allocationKey, err := bookingRoomLockKey(req)
	if err != nil {
		return nil, err
	}
	lockKey := fmt.Sprintf("lock:hold:%s:%s:%s:%s", req.UserID, allocationKey, req.CheckInDate.Format("2006-01-02"), req.CheckOutDate.Format("2006-01-02"))
	locked, err := s.cache.SetNX(ctx, lockKey, "1", cache.TTLLock)
	if err != nil {
		s.log.Warn("booking hold lock unavailable", zap.Error(err))
	} else if !locked {
		return nil, fmt.Errorf("booking is already being processed")
	} else {
		defer func() { _ = s.cache.Delete(context.Background(), lockKey) }()
	}

	room, err := s.resolveBookingRoom(ctx, req)
	if err != nil {
		return nil, err
	}

	nights := bookingNights(req.CheckInDate, req.CheckOutDate)
	usdAmount := room.PricePerNight * float64(nights)
	guestName := req.GuestName
	if guestName == "" {
		guestName = "Guest"
	}
	guestEmail := &req.GuestEmail
	guestPhone := &req.GuestPhone
	roomType := roomTypeLabel(room.RoomType)
	notes := fmt.Sprintf("Guest portal hold. Country: %s. Requested type: %s. Room number assigned after booking.", req.Country, roomType)

	stay, err := s.roomRepo.CreateStay(ctx, postgres.DemoHotelID, &domain.GuestStay{
		GuestID:      &req.UserID,
		RoomID:       room.ID,
		GuestName:    guestName,
		GuestEmail:   guestEmail,
		GuestPhone:   guestPhone,
		CheckInDate:  req.CheckInDate,
		CheckOutDate: req.CheckOutDate,
		TotalAmount:  &usdAmount,
		Notes:        &notes,
		CreatedBy:    &req.UserID,
	})
	if err != nil {
		return nil, fmt.Errorf("create stay: %w", err)
	}

	payNum := fmt.Sprintf("PAY-%s-%s", time.Now().Format("150405"), strings.ToUpper(stay.ID.String()[:6]))
	payMethod := "hold"
	payNotes := fmt.Sprintf("%s room hold for %d night(s). Assigned room: %s", roomType, nights, room.RoomNumber)
	payment, err := s.paymentRepo.Create(ctx, &domain.Payment{
		PaymentNumber: payNum,
		GuestStayID:   &stay.ID,
		Amount:        usdAmount,
		PaymentMethod: payMethod,
		Status:        domain.PaymentStatusPending,
		ProcessedBy:   &req.UserID,
		Notes:         &payNotes,
	})
	if err != nil {
		_ = s.roomRepo.DeleteStay(ctx, postgres.DemoHotelID, stay.ID)
		return nil, fmt.Errorf("create payment: %w", err)
	}

	_ = s.roomRepo.UpdateRoomStatus(ctx, postgres.DemoHotelID, room.ID, domain.RoomStatusOccupied)
	_ = s.cache.Delete(ctx, cache.KeyDashboardStats(postgres.DemoHotelID.String()), cache.KeyRoomList(postgres.DemoHotelID.String(), "all"), cache.KeyRoomList(postgres.DemoHotelID.String(), string(domain.RoomStatusAvailable)), cache.KeyRoomList(postgres.DemoHotelID.String(), string(domain.RoomStatusOccupied)))

	return &CheckoutResult{
		StayID:    stay.ID,
		PaymentID: payment.ID,
	}, nil
}

// CreateRazorpayPaymentOrder creates a Razorpay Order for an existing pending payment.
func (s *paymentService) CreateRazorpayPaymentOrder(ctx context.Context, paymentID uuid.UUID, currency, country string) (*RazorpayOrderResult, error) {
	gatewayCfg, keyID, keySecret, err := s.razorpayCredentials(ctx)
	if err != nil {
		return nil, err
	}
	lockKey := "lock:razorpay-payment:" + paymentID.String()
	locked, err := s.cache.SetNX(ctx, lockKey, "1", cache.TTLLock)
	if err != nil {
		s.log.Warn("razorpay payment lock unavailable", zap.Error(err))
	} else if !locked {
		return nil, fmt.Errorf("payment checkout is already being processed")
	} else {
		defer func() { _ = s.cache.Delete(context.Background(), lockKey) }()
	}

	currency = strings.ToUpper(strings.TrimSpace(currency))
	if currency == "" {
		currency = strings.ToUpper(strings.TrimSpace(gatewayCfg.DefaultCurrency))
	}
	if currency == "" {
		currency = "INR"
	}
	if currency != "INR" {
		return nil, fmt.Errorf("Razorpay checkout is configured for INR. Select India (INR) before paying online")
	}

	payment, err := s.paymentRepo.FindByID(ctx, paymentID)
	if err != nil {
		return nil, fmt.Errorf("payment not found")
	}
	if payment.Status == domain.PaymentStatusCompleted {
		return nil, fmt.Errorf("payment is already completed")
	}

	stayID := uuid.Nil
	usdAmount := payment.Amount
	description := "Hotel payment"
	if payment.GuestStayID != nil {
		stayID = *payment.GuestStayID
		stay, err := s.roomRepo.FindStayByID(ctx, postgres.DemoHotelID, *payment.GuestStayID)
		if err != nil {
			return nil, fmt.Errorf("booking not found")
		}
		if stay.TotalAmount != nil {
			usdAmount = *stay.TotalAmount
		}
		roomNumber := ""
		if stay.Room != nil {
			roomNumber = stay.Room.RoomNumber
		}
		description = fmt.Sprintf("Room %s booking payment", roomNumber)
	}

	rate, err := s.GetExchangeRate(ctx, "USD", currency)
	if err != nil {
		return nil, fmt.Errorf("unable to price payment in %s: %w", currency, err)
	}
	convertedAmount := roundTo2(usdAmount * rate)
	minorAmount := razorpayMinorAmount(convertedAmount)
	orderID, err := s.createRazorpayOrder(ctx, keyID, keySecret, minorAmount, currency, paymentID.String(), map[string]string{
		"payment_id": paymentID.String(),
		"stay_id":    stayID.String(),
	})
	if err != nil {
		return nil, fmt.Errorf("Razorpay order failed: %w", err)
	}

	notes := fmt.Sprintf("Razorpay checkout pending. Country: %s. Currency: %s. Rate: %.6f. Razorpay order: %s", country, currency, rate, orderID)
	_ = s.paymentRepo.UpdateAmountAndNotes(ctx, paymentID, convertedAmount, "razorpay", notes)

	return &RazorpayOrderResult{
		OrderID:     orderID,
		KeyID:       keyID,
		PaymentID:   paymentID,
		StayID:      stayID,
		Amount:      minorAmount,
		Currency:    currency,
		Name:        "Hotel Payment",
		Description: description,
	}, nil
}

// PaymentCheckout creates a Stripe session for an existing payment record.
func (s *paymentService) PaymentCheckout(ctx context.Context, paymentID uuid.UUID, currency, country, originURL string) (*CheckoutResult, error) {
	if _, err := s.stripeSecretKey(ctx); err != nil {
		return nil, err
	}
	if err := s.ensureStripeChargesEnabled(ctx); err != nil {
		return nil, err
	}
	lockKey := "lock:payment:" + paymentID.String()
	locked, err := s.cache.SetNX(ctx, lockKey, "1", cache.TTLLock)
	if err != nil {
		s.log.Warn("payment lock unavailable", zap.Error(err))
	} else if !locked {
		return nil, fmt.Errorf("payment checkout is already being processed")
	} else {
		defer func() { _ = s.cache.Delete(context.Background(), lockKey) }()
	}

	currency = strings.ToUpper(currency)
	if currency == "" {
		currency = "USD"
	}

	payment, err := s.paymentRepo.FindByID(ctx, paymentID)
	if err != nil {
		return nil, fmt.Errorf("payment not found")
	}
	if payment.Status == domain.PaymentStatusCompleted {
		return nil, fmt.Errorf("payment is already completed")
	}
	if payment.GuestStayID == nil {
		return nil, fmt.Errorf("booking not found for this payment")
	}

	stay, err := s.roomRepo.FindStayByID(ctx, postgres.DemoHotelID, *payment.GuestStayID)
	if err != nil {
		return nil, fmt.Errorf("booking not found")
	}

	usdAmount := 0.0
	if stay.TotalAmount != nil {
		usdAmount = *stay.TotalAmount
	} else {
		usdAmount = payment.Amount
	}

	rate, err := s.GetExchangeRate(ctx, "USD", currency)
	if err != nil {
		return nil, fmt.Errorf("unable to price payment in %s: %w", currency, err)
	}
	convertedAmount := roundTo2(usdAmount * rate)

	notes := fmt.Sprintf("Stripe checkout pending. Country: %s. Currency: %s. Rate: %.6f", country, currency, rate)
	_ = s.paymentRepo.UpdateAmountAndNotes(ctx, paymentID, convertedAmount, "stripe", notes)

	roomNumber := ""
	if stay.Room != nil {
		roomNumber = stay.Room.RoomNumber
	}
	successURL := fmt.Sprintf("%s/guest?booking=success&stay_id=%s&payment_id=%s&session_id={CHECKOUT_SESSION_ID}",
		originURL, stay.ID, paymentID)
	cancelURL := fmt.Sprintf("%s/guest?booking=cancelled&stay_id=%s&payment_id=%s", originURL, stay.ID, paymentID)

	guestEmail := ""
	if stay.GuestEmail != nil {
		guestEmail = *stay.GuestEmail
	}

	session, err := s.createStripeSession(ctx, stripeSessionParams{
		Currency:       currency,
		Amount:         convertedAmount,
		GuestEmail:     guestEmail,
		StayID:         stay.ID,
		PaymentID:      paymentID,
		Country:        country,
		ProductName:    fmt.Sprintf("Hotel Room %s Booking Payment", roomNumber),
		Description:    fmt.Sprintf("Room %s payment", roomNumber),
		SuccessURL:     successURL,
		CancelURL:      cancelURL,
		IdempotencyKey: "pay-" + paymentID.String(),
	})
	if err != nil {
		return nil, fmt.Errorf("Stripe checkout failed: %w", err)
	}

	return &CheckoutResult{
		CheckoutURL: session.URL,
		SessionID:   session.ID,
		StayID:      stay.ID,
		PaymentID:   paymentID,
	}, nil
}

// CompletePayment verifies a Stripe session and marks the payment completed.
func (s *paymentService) CompletePayment(ctx context.Context, paymentID uuid.UUID, sessionID string) error {
	secretKey, err := s.stripeSecretKey(ctx)
	if err != nil {
		return err
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("https://api.stripe.com/v1/checkout/sessions/%s", sessionID), nil)
	req.Header.Set("Authorization", "Bearer "+secretKey)
	req.Header.Set("User-Agent", "HotelHarmony/2.0")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("unable to verify Stripe payment: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var session struct {
		PaymentStatus string `json:"payment_status"`
	}
	if json.Unmarshal(body, &session) != nil || session.PaymentStatus != "paid" {
		return fmt.Errorf("Stripe payment is not paid yet")
	}

	if err := s.paymentRepo.UpdateStatus(ctx, paymentID, domain.PaymentStatusCompleted, "stripe"); err != nil {
		return err
	}
	_ = s.cache.Delete(ctx, cache.KeyDashboardStats(postgres.DemoHotelID.String()))
	return nil
}

// VerifyRazorpayPayment verifies Razorpay's signed Checkout response server-side.
func (s *paymentService) VerifyRazorpayPayment(ctx context.Context, req RazorpayVerifyRequest) error {
	_, keyID, keySecret, err := s.razorpayCredentials(ctx)
	if err != nil {
		return err
	}
	if req.PaymentID == uuid.Nil || strings.TrimSpace(req.RazorpayOrderID) == "" || strings.TrimSpace(req.RazorpayPaymentID) == "" || strings.TrimSpace(req.RazorpaySignature) == "" {
		return fmt.Errorf("Razorpay payment verification data is incomplete")
	}

	payment, err := s.paymentRepo.FindByID(ctx, req.PaymentID)
	if err != nil {
		return fmt.Errorf("payment not found")
	}
	if payment.Status == domain.PaymentStatusCompleted {
		return nil
	}
	if payment.Notes == nil || !strings.Contains(*payment.Notes, req.RazorpayOrderID) {
		return fmt.Errorf("Razorpay order does not match this payment")
	}

	message := req.RazorpayOrderID + "|" + req.RazorpayPaymentID
	mac := hmac.New(sha256.New, []byte(keySecret))
	_, _ = mac.Write([]byte(message))
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(strings.ToLower(expected)), []byte(strings.ToLower(req.RazorpaySignature))) {
		return fmt.Errorf("Razorpay payment signature is invalid")
	}

	if err := s.verifyRazorpayPaymentCapture(ctx, keyID, keySecret, req, payment); err != nil {
		return err
	}
	if err := s.paymentRepo.UpdateStatus(ctx, req.PaymentID, domain.PaymentStatusCompleted, "razorpay"); err != nil {
		return err
	}
	_ = s.cache.Delete(ctx, cache.KeyDashboardStats(postgres.DemoHotelID.String()))
	return nil
}

// GetConfig returns payment gateway configuration for the frontend.
func (s *paymentService) GetConfig(ctx context.Context) map[string]interface{} {
	secret := s.cfg.Stripe.SecretKey
	pub := s.cfg.Stripe.PublishableKey
	activeGateway := "stripe"
	razorpayConfigured := false
	razorpayKeyID := ""
	defaultCurrency := "USD"
	gatewayMode := ""
	if gatewayCfg, err := s.paymentRepo.FindGatewayConfig(ctx); err == nil {
		activeGateway = gatewayCfg.ActiveGateway
		defaultCurrency = gatewayCfg.DefaultCurrency
		gatewayMode = gatewayCfg.GatewayMode
		if gatewayCfg.StripePublishableKey != nil && strings.TrimSpace(*gatewayCfg.StripePublishableKey) != "" {
			pub = strings.TrimSpace(*gatewayCfg.StripePublishableKey)
		}
		if storedSecret, err := s.decryptSetting(gatewayCfg.StripeSecretKeyEncrypted); err == nil && storedSecret != "" {
			secret = storedSecret
		}
		razorpayConfigured = gatewayCfg.RazorpayEnabled &&
			gatewayCfg.RazorpayKeyID != nil &&
			strings.TrimSpace(*gatewayCfg.RazorpayKeyID) != "" &&
			gatewayCfg.RazorpayKeySecretEncrypted != nil &&
			strings.TrimSpace(*gatewayCfg.RazorpayKeySecretEncrypted) != ""
		if gatewayCfg.RazorpayKeyID != nil {
			razorpayKeyID = strings.TrimSpace(*gatewayCfg.RazorpayKeyID)
		}
	}
	configured := strings.HasPrefix(secret, "sk_")
	mode := ""
	if strings.HasPrefix(secret, "sk_live_") {
		mode = "live"
	} else if strings.HasPrefix(secret, "sk_test_") {
		mode = "test"
	}
	pubMode := ""
	if strings.HasPrefix(pub, "pk_live_") {
		pubMode = "live"
	} else if strings.HasPrefix(pub, "pk_test_") {
		pubMode = "test"
	}
	return map[string]interface{}{
		"stripe_configured":   configured,
		"active_gateway":      activeGateway,
		"default_currency":    defaultCurrency,
		"gateway_mode":        gatewayMode,
		"mode":                mode,
		"publishable_mode":    pubMode,
		"mode_matches":        mode != "" && pubMode != "" && mode == pubMode,
		"razorpay_configured": razorpayConfigured,
		"razorpay_key_id":     razorpayKeyID,
	}
}

// GetExchangeRate fetches a live exchange rate from Frankfurter, cached in Redis.
func (s *paymentService) GetExchangeRate(ctx context.Context, base, target string) (float64, error) {
	base = strings.ToUpper(base)
	target = strings.ToUpper(target)
	if base == target {
		return 1.0, nil
	}

	cacheKey := fmt.Sprintf("fx:%s:%s", base, target)
	if v, err := s.cache.Get(ctx, cacheKey); err == nil {
		var rate float64
		if n, err := fmt.Sscanf(v, "%f", &rate); err == nil && n == 1 {
			return rate, nil
		}
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("https://api.frankfurter.dev/v2/rate/%s/%s", base, target), nil)
	req.Header.Set("User-Agent", "HotelHarmony/2.0")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("exchange rate fetch: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var result struct {
		Rate float64 `json:"rate"`
	}
	if err := json.Unmarshal(body, &result); err != nil || result.Rate == 0 {
		return 0, fmt.Errorf("exchange rate: invalid response")
	}

	_ = s.cache.Set(ctx, cacheKey, fmt.Sprintf("%.6f", result.Rate), 10*time.Minute)
	return result.Rate, nil
}

type stripeSessionParams struct {
	Currency       string
	Amount         float64
	GuestEmail     string
	StayID         uuid.UUID
	PaymentID      uuid.UUID
	RoomID         uuid.UUID
	Country        string
	ProductName    string
	Description    string
	SuccessURL     string
	CancelURL      string
	IdempotencyKey string
}

type stripeSession struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

func (s *paymentService) createRazorpayOrder(ctx context.Context, keyID, keySecret string, amount int64, currency, receipt string, notes map[string]string) (string, error) {
	payload := map[string]interface{}{
		"amount":   amount,
		"currency": currency,
		"receipt":  receipt,
		"notes":    notes,
	}
	body, _ := json.Marshal(payload)

	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(reqCtx, http.MethodPost, "https://api.razorpay.com/v1/orders", bytes.NewReader(body))
	req.SetBasicAuth(keyID, keySecret)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "HotelOps/2.0")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("razorpay error %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		ID       string `json:"id"`
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil || strings.TrimSpace(result.ID) == "" {
		return "", fmt.Errorf("razorpay: invalid order response")
	}
	if result.Amount != amount || strings.ToUpper(result.Currency) != currency {
		return "", fmt.Errorf("razorpay: order amount or currency mismatch")
	}
	return result.ID, nil
}

func (s *paymentService) verifyRazorpayPaymentCapture(ctx context.Context, keyID, keySecret string, req RazorpayVerifyRequest, payment *domain.Payment) error {
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	httpReq, _ := http.NewRequestWithContext(reqCtx, http.MethodGet,
		fmt.Sprintf("https://api.razorpay.com/v1/payments/%s", req.RazorpayPaymentID), nil)
	httpReq.SetBasicAuth(keyID, keySecret)
	httpReq.Header.Set("User-Agent", "HotelOps/2.0")

	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("unable to verify Razorpay payment status: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Razorpay payment status check failed: %s", string(body))
	}

	var result struct {
		ID       string `json:"id"`
		OrderID  string `json:"order_id"`
		Status   string `json:"status"`
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("Razorpay payment status response was invalid")
	}
	if result.OrderID != req.RazorpayOrderID || result.ID != req.RazorpayPaymentID {
		return fmt.Errorf("Razorpay payment does not match the verified order")
	}
	if result.Status != "captured" {
		return fmt.Errorf("Razorpay payment is %s, not captured yet", result.Status)
	}
	expectedAmount := razorpayMinorAmount(payment.Amount)
	if result.Amount != expectedAmount || strings.ToUpper(result.Currency) != "INR" {
		return fmt.Errorf("Razorpay payment amount or currency mismatch")
	}
	return nil
}

func (s *paymentService) createStripeSession(ctx context.Context, p stripeSessionParams) (*stripeSession, error) {
	secretKey, err := s.stripeSecretKey(ctx)
	if err != nil {
		return nil, err
	}

	minorAmount := stripeMinorAmount(p.Amount, p.Currency)

	form := url.Values{}
	form.Set("mode", "payment")
	form.Set("payment_method_types[0]", "card")
	form.Set("success_url", p.SuccessURL)
	form.Set("cancel_url", p.CancelURL)
	form.Set("customer_email", p.GuestEmail)
	form.Set("client_reference_id", p.StayID.String())
	form.Set("line_items[0][quantity]", "1")
	form.Set("line_items[0][price_data][currency]", strings.ToLower(p.Currency))
	form.Set("line_items[0][price_data][unit_amount]", fmt.Sprintf("%d", minorAmount))
	form.Set("line_items[0][price_data][product_data][name]", p.ProductName)
	form.Set("line_items[0][price_data][product_data][description]", p.Description)
	form.Set("metadata[stay_id]", p.StayID.String())
	form.Set("metadata[payment_id]", p.PaymentID.String())
	form.Set("metadata[room_id]", p.RoomID.String())
	form.Set("metadata[currency]", p.Currency)
	form.Set("metadata[country]", p.Country)
	form.Set("payment_intent_data[metadata][stay_id]", p.StayID.String())
	form.Set("payment_intent_data[metadata][payment_id]", p.PaymentID.String())

	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(reqCtx, http.MethodPost,
		"https://api.stripe.com/v1/checkout/sessions",
		strings.NewReader(form.Encode()))
	req.Header.Set("Authorization", "Bearer "+secretKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Idempotency-Key", p.IdempotencyKey)
	req.Header.Set("User-Agent", "HotelHarmony/2.0")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("stripe error %d: %s", resp.StatusCode, string(body))
	}

	var session stripeSession
	if err := json.Unmarshal(body, &session); err != nil {
		return nil, fmt.Errorf("stripe: invalid session response")
	}
	return &session, nil
}

func (s *paymentService) ensureStripeChargesEnabled(ctx context.Context) error {
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(reqCtx, http.MethodGet, "https://api.stripe.com/v1/account", nil)
	req.Header.Set("Authorization", "Bearer "+s.cfg.Stripe.SecretKey)
	req.Header.Set("User-Agent", "HotelHarmony/2.0")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("unable to verify Stripe account status: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Stripe account status check failed: %s", string(body))
	}

	var account struct {
		ChargesEnabled bool `json:"charges_enabled"`
	}
	if err := json.Unmarshal(body, &account); err != nil {
		return fmt.Errorf("Stripe account status response was invalid")
	}
	if !account.ChargesEnabled {
		return fmt.Errorf("Stripe live payments are not enabled on this account yet. Complete Stripe account activation / KYC in the Stripe dashboard before accepting card payments")
	}
	return nil
}

func (s *paymentService) stripeSecretKey(ctx context.Context) (string, error) {
	if gatewayCfg, err := s.paymentRepo.FindGatewayConfig(ctx); err == nil {
		if gatewayCfg.ActiveGateway != "" && gatewayCfg.ActiveGateway != "stripe" && gatewayCfg.ActiveGateway != "none" {
			return "", fmt.Errorf("%s is the active payment gateway; Stripe checkout is not enabled", gatewayCfg.ActiveGateway)
		}
		if gatewayCfg.ActiveGateway == "stripe" || gatewayCfg.StripeEnabled {
			if storedSecret, err := s.decryptSetting(gatewayCfg.StripeSecretKeyEncrypted); err == nil && strings.HasPrefix(storedSecret, "sk_") {
				return storedSecret, nil
			}
		}
	}
	if strings.HasPrefix(s.cfg.Stripe.SecretKey, "sk_") {
		return s.cfg.Stripe.SecretKey, nil
	}
	return "", fmt.Errorf("Stripe secret key is not configured")
}

func (s *paymentService) razorpayCredentials(ctx context.Context) (*domain.PaymentGatewayConfig, string, string, error) {
	gatewayCfg, err := s.paymentRepo.FindGatewayConfig(ctx)
	if err != nil {
		return nil, "", "", fmt.Errorf("Razorpay gateway is not configured")
	}
	if gatewayCfg.ActiveGateway != "razorpay" {
		return nil, "", "", fmt.Errorf("%s is the active payment gateway; Razorpay checkout is not enabled", gatewayCfg.ActiveGateway)
	}
	if !gatewayCfg.RazorpayEnabled {
		return nil, "", "", fmt.Errorf("Razorpay is disabled in payment settings")
	}
	keyID := ""
	if gatewayCfg.RazorpayKeyID != nil {
		keyID = strings.TrimSpace(*gatewayCfg.RazorpayKeyID)
	}
	keySecret, err := s.decryptSetting(gatewayCfg.RazorpayKeySecretEncrypted)
	if err != nil {
		return nil, "", "", fmt.Errorf("Razorpay secret could not be decrypted")
	}
	if keyID == "" || keySecret == "" {
		return nil, "", "", fmt.Errorf("Razorpay key id and secret are required")
	}

	mode := strings.ToLower(strings.TrimSpace(gatewayCfg.GatewayMode))
	switch mode {
	case "", "test":
		if !strings.HasPrefix(keyID, "rzp_test_") {
			return nil, "", "", fmt.Errorf("Razorpay test mode requires an rzp_test key id")
		}
	case "live":
		if !strings.HasPrefix(keyID, "rzp_live_") {
			return nil, "", "", fmt.Errorf("Razorpay live mode requires an rzp_live key id")
		}
	default:
		return nil, "", "", fmt.Errorf("Razorpay gateway mode must be test or live")
	}
	return gatewayCfg, keyID, keySecret, nil
}

func (s *paymentService) decryptSetting(value *string) (string, error) {
	if value == nil || strings.TrimSpace(*value) == "" {
		return "", nil
	}
	parts := strings.Split(strings.TrimSpace(*value), ":")
	if len(parts) != 3 || parts[0] != "v1" {
		return "", fmt.Errorf("unsupported encrypted setting format")
	}
	nonce, err := base64.RawStdEncoding.DecodeString(parts[1])
	if err != nil {
		return "", err
	}
	ciphertext, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return "", err
	}
	keyMaterial := s.cfg.Auth.AccessTokenSecret
	if keyMaterial == "" {
		keyMaterial = "hotelops-local-development-secret"
	}
	key := sha256.Sum256([]byte(keyMaterial))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func stripeMinorAmount(amount float64, currency string) int64 {
	if zeroDecimalCurrencies[strings.ToUpper(currency)] {
		v := int64(amount)
		if v < 1 {
			return 1
		}
		return v
	}
	v := int64(amount * 100)
	if v < 1 {
		return 1
	}
	return v
}

func razorpayMinorAmount(amount float64) int64 {
	v := int64(amount*100 + 0.5)
	if v < 1 {
		return 1
	}
	return v
}

func roundTo2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}
