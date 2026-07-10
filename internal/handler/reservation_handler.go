package handler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/hotelharmony/api/internal/config"
	"github.com/hotelharmony/api/internal/domain"
	"github.com/hotelharmony/api/internal/repository/postgres"
	"github.com/hotelharmony/api/internal/service"
	"github.com/hotelharmony/api/internal/worker"
	"github.com/hotelharmony/api/pkg/response"
)

type ReservationHandler struct {
	roomRepo postgres.RoomRepository
	cfg      *config.Config
	emailSvc *service.EmailService
	smsSvc   *service.SMSService
}

func NewReservationHandler(roomRepo postgres.RoomRepository, cfg *config.Config, emailSvc *service.EmailService, smsSvc *service.SMSService) *ReservationHandler {
	return &ReservationHandler{roomRepo: roomRepo, cfg: cfg, emailSvc: emailSvc, smsSvc: smsSvc}
}

func (h *ReservationHandler) Register(r fiber.Router) {
	r.Get("/reservations", h.List)
	r.Get("/reservations/calendar", h.Calendar)
	r.Get("/reservations/:id", h.Get)
	r.Post("/reservations", h.Create)
	r.Patch("/reservations/:id", h.Update)
	r.Delete("/reservations/:id", h.Cancel)
	r.Post("/reservations/:id/checkin", h.CheckIn)
	r.Post("/reservations/:id/checkout", h.CheckOut)
}

func (h *ReservationHandler) List(c *fiber.Ctx) error {
	status := c.Query("status")
	search := c.Query("search")
	from := c.Query("from")
	to := c.Query("to")

	allStays, err := h.roomRepo.ListStays(c.Context(), tenantHotelID(c), nil)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to list reservations")
	}

	result := make([]map[string]interface{}, 0, len(allStays))
	for _, s := range allStays {
		resStatus := deriveReservationStatus(s)

		if status != "" && !strings.EqualFold(resStatus, status) {
			continue
		}
		if search != "" {
			searchL := strings.ToLower(search)
			if !strings.Contains(strings.ToLower(s.GuestName), searchL) && !strings.Contains(strings.ToLower(coalesceStr(s.GuestEmail)), searchL) {
				continue
			}
		}
		if from != "" {
			fd, err := time.Parse("2006-01-02", from)
			if err == nil && s.CheckInDate.Before(fd) {
				continue
			}
		}
		if to != "" {
			td, err := time.Parse("2006-01-02", to)
			if err == nil && s.CheckInDate.After(td) {
				continue
			}
		}

		roomNum := ""
		roomType := ""
		if s.Room != nil {
			roomNum = s.Room.RoomNumber
			roomType = s.Room.RoomType
		}

		result = append(result, map[string]interface{}{
			"id":               s.ID,
			"guest_name":       s.GuestName,
			"guest_email":      s.GuestEmail,
			"guest_phone":      s.GuestPhone,
			"check_in_date":    s.CheckInDate.Format(time.RFC3339),
			"check_out_date":   s.CheckOutDate.Format(time.RFC3339),
			"actual_check_in":  formatTimePtr(s.ActualCheckIn),
			"actual_check_out": formatTimePtr(s.ActualCheckOut),
			"room_number":      roomNum,
			"room_type":        roomType,
			"total_amount":     s.TotalAmount,
			"nights":           int(s.CheckOutDate.Sub(s.CheckInDate).Hours() / 24),
			"status":           resStatus,
			"source":           coalesceStr(s.Source),
			"created_at":       s.CreatedAt.Format(time.RFC3339),
		})
	}
	return response.OK(c, result)
}

func (h *ReservationHandler) Get(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid id")
	}
	stay, err := h.roomRepo.FindStayByID(c.Context(), tenantHotelID(c), id)
	if err != nil {
		return response.Error(c, fiber.StatusNotFound, "reservation not found")
	}
	return response.OK(c, stay)
}

type createReservationRequest struct {
	GuestName    string `json:"guest_name"`
	GuestEmail   string `json:"guest_email"`
	GuestPhone   string `json:"guest_phone"`
	RoomID       string `json:"room_id"`
	CheckInDate  string `json:"check_in_date"`
	CheckOutDate string `json:"check_out_date"`
	Source       string `json:"source"`
	Notes        string `json:"notes"`
}

func (h *ReservationHandler) Create(c *fiber.Ctx) error {
	var req createReservationRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if req.GuestName == "" {
		return response.Error(c, fiber.StatusBadRequest, "guest_name is required")
	}
	checkIn, err := time.Parse("2006-01-02", req.CheckInDate)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid check_in_date (use YYYY-MM-DD)")
	}
	checkOut, err := time.Parse("2006-01-02", req.CheckOutDate)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid check_out_date (use YYYY-MM-DD)")
	}
	if !checkOut.After(checkIn) {
		return response.Error(c, fiber.StatusBadRequest, "check_out must be after check_in")
	}
	var roomID uuid.UUID
	if req.RoomID != "" {
		roomID, err = uuid.Parse(req.RoomID)
		if err != nil {
			return response.Error(c, fiber.StatusBadRequest, "invalid room_id")
		}
	}
	if roomID == uuid.Nil {
		return response.Error(c, fiber.StatusBadRequest, "room_id is required")
	}

	hotelID := tenantHotelID(c)

	room, err := h.roomRepo.FindRoomByID(c.Context(), hotelID, roomID)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "room not found")
	}

	// Reject overlapping bookings on the same room. Two [checkIn, checkOut)
	// ranges overlap iff existing.checkIn < newCheckOut && existing.checkOut > newCheckIn.
	existing, err := h.roomRepo.ListStays(c.Context(), hotelID, map[string]interface{}{"room_id": roomID})
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to check room availability")
	}
	for _, s := range existing {
		if s.CheckInDate.Before(checkOut) && s.CheckOutDate.After(checkIn) {
			return response.Error(c, fiber.StatusConflict, fmt.Sprintf(
				"room %s is already booked from %s to %s",
				room.RoomNumber, s.CheckInDate.Format("2006-01-02"), s.CheckOutDate.Format("2006-01-02")))
		}
	}

	var notes *string
	if req.Notes != "" {
		notes = &req.Notes
	}

	source := strings.TrimSpace(req.Source)
	if source == "" {
		source = "Direct"
	}

	nights := checkOut.Sub(checkIn).Hours() / 24
	totalAmount := room.PricePerNight * nights

	stay := &domain.GuestStay{
		ID:           uuid.New(),
		GuestName:    req.GuestName,
		GuestEmail:   strPtr(req.GuestEmail),
		GuestPhone:   strPtr(req.GuestPhone),
		RoomID:       roomID,
		CheckInDate:  checkIn,
		CheckOutDate: checkOut,
		TotalAmount:  float64Ptr(totalAmount),
		Notes:        notes,
		Source:       &source,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}

	created, err := h.roomRepo.CreateStay(c.Context(), hotelID, stay)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, fmt.Sprintf("failed to create: %v", err))
	}
	return response.OK(c, created)
}

func (h *ReservationHandler) Update(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid id")
	}
	var req createReservationRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}

	hotelID := tenantHotelID(c)

	// Cross-validate dates against whichever side isn't being changed, so a
	// PATCH of only one date can't silently invert the stay (checkout before checkin).
	if req.CheckInDate != "" || req.CheckOutDate != "" {
		current, err := h.roomRepo.FindStayByID(c.Context(), hotelID, id)
		if err != nil {
			return response.Error(c, fiber.StatusNotFound, "reservation not found")
		}
		effectiveCheckIn := current.CheckInDate
		effectiveCheckOut := current.CheckOutDate
		if req.CheckInDate != "" {
			if d, err := time.Parse("2006-01-02", req.CheckInDate); err == nil {
				effectiveCheckIn = d
			}
		}
		if req.CheckOutDate != "" {
			if d, err := time.Parse("2006-01-02", req.CheckOutDate); err == nil {
				effectiveCheckOut = d
			}
		}
		if !effectiveCheckOut.After(effectiveCheckIn) {
			return response.Error(c, fiber.StatusBadRequest, "check_out must be after check_in")
		}
	}

	fields := make(map[string]interface{})
	if req.GuestName != "" {
		fields["guest_name"] = req.GuestName
	}
	if req.GuestEmail != "" {
		fields["guest_email"] = req.GuestEmail
	}
	if req.GuestPhone != "" {
		fields["guest_phone"] = req.GuestPhone
	}
	if req.CheckInDate != "" {
		if d, err := time.Parse("2006-01-02", req.CheckInDate); err == nil {
			fields["check_in_date"] = d
		}
	}
	if req.CheckOutDate != "" {
		if d, err := time.Parse("2006-01-02", req.CheckOutDate); err == nil {
			fields["check_out_date"] = d
		}
	}
	if req.Notes != "" {
		fields["notes"] = req.Notes
	}
	if req.Source != "" {
		fields["source"] = req.Source
	}
	if len(fields) == 0 {
		return response.Error(c, fiber.StatusBadRequest, "no fields to update")
	}
	if err := h.roomRepo.UpdateStay(c.Context(), hotelID, id, fields); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "update failed")
	}
	stay, _ := h.roomRepo.FindStayByID(c.Context(), hotelID, id)
	return response.OK(c, stay)
}

func (h *ReservationHandler) Cancel(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid id")
	}

	stay, err := h.roomRepo.FindStayByID(c.Context(), tenantHotelID(c), id)
	if err != nil {
		return response.Error(c, fiber.StatusNotFound, "reservation not found")
	}
	if stay.ActualCheckIn != nil {
		return response.Error(c, fiber.StatusBadRequest, "cannot cancel checked-in reservation")
	}

	roomID := stay.RoomID
	if err := h.roomRepo.DeleteStay(c.Context(), tenantHotelID(c), id); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "cancel failed")
	}
	_ = h.roomRepo.UpdateRoomStatus(c.Context(), tenantHotelID(c), roomID, domain.RoomStatusAvailable)
	return response.OK(c, map[string]string{"status": "cancelled"})
}

func (h *ReservationHandler) CheckIn(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid id")
	}
	now := time.Now().UTC()
	if err := h.roomRepo.UpdateStay(c.Context(), tenantHotelID(c), id, map[string]interface{}{"actual_check_in": now}); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "check-in failed")
	}

	stay, _ := h.roomRepo.FindStayByID(c.Context(), tenantHotelID(c), id)
	if stay != nil {
		_ = h.roomRepo.UpdateRoomStatus(c.Context(), tenantHotelID(c), stay.RoomID, domain.RoomStatusOccupied)
	}

	// Notify on whichever contact channels the guest actually provided — a
	// guest with only an email (no phone), or vice versa, must still be notified.
	if stay != nil {
		roomNumber := ""
		if stay.Room != nil {
			roomNumber = stay.Room.RoomNumber
		}
		checkInDate := stay.CheckInDate.Format("2006-01-02")
		checkOutDate := stay.CheckOutDate.Format("2006-01-02")
		guestName := stay.GuestName
		if stay.GuestEmail != nil {
			guestEmail := *stay.GuestEmail
			worker.SubmitOrRun("email.booking_confirmation", func(context.Context) error {
				return h.emailSvc.SendBookingConfirmation(guestEmail, guestName, "Grand Hotel Mumbai", roomNumber, checkInDate, checkOutDate)
			})
		}
		if stay.GuestPhone != nil {
			guestPhone := *stay.GuestPhone
			worker.SubmitOrRun("sms.booking_confirmation", func(context.Context) error {
				return h.smsSvc.SendBookingConfirmation(guestPhone, guestName, "Grand Hotel Mumbai", roomNumber, checkInDate, checkOutDate)
			})
		}
	}

	return response.OK(c, map[string]string{"status": "checked_in"})
}

func (h *ReservationHandler) CheckOut(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid id")
	}
	now := time.Now().UTC()
	if err := h.roomRepo.UpdateStay(c.Context(), tenantHotelID(c), id, map[string]interface{}{"actual_check_out": now}); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "check-out failed")
	}

	stay, _ := h.roomRepo.FindStayByID(c.Context(), tenantHotelID(c), id)
	if stay != nil {
		_ = h.roomRepo.UpdateRoomStatus(c.Context(), tenantHotelID(c), stay.RoomID, domain.RoomStatusCleaning)
	}

	if stay != nil {
		totalAmount := "0.00"
		if stay.TotalAmount != nil {
			totalAmount = fmt.Sprintf("%.2f", *stay.TotalAmount)
		}
		dueDate := stay.CheckOutDate.Format("2006-01-02")
		guestName := stay.GuestName
		if stay.GuestEmail != nil {
			guestEmail := *stay.GuestEmail
			worker.SubmitOrRun("email.invoice", func(context.Context) error {
				return h.emailSvc.SendInvoice(guestEmail, guestName, "Grand Hotel Mumbai", "N/A", totalAmount, dueDate)
			})
		}
		if stay.GuestPhone != nil {
			guestPhone := *stay.GuestPhone
			worker.SubmitOrRun("sms.checkout_thanks", func(context.Context) error {
				return h.smsSvc.Send(guestPhone, "Thank you for staying at Grand Hotel Mumbai! Your checkout is complete.")
			})
		}
	}

	return response.OK(c, map[string]string{"status": "checked_out"})
}

func (h *ReservationHandler) Calendar(c *fiber.Ctx) error {
	month := c.Query("month", time.Now().Format("2006-01"))
	start, err := time.Parse("2006-01", month)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid month (use YYYY-MM)")
	}
	end := start.AddDate(0, 1, 0)

	allStays, err := h.roomRepo.ListStays(c.Context(), tenantHotelID(c), nil)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to load")
	}

	type dayInfo struct {
		Date      string `json:"date"`
		CheckIns  int    `json:"check_ins"`
		CheckOuts int    `json:"check_outs"`
		Occupied  int    `json:"occupied"`
	}
	var days []dayInfo
	for d := start; d.Before(end); d = d.AddDate(0, 0, 1) {
		ci := 0
		co := 0
		occ := 0
		ds := d.Format("2006-01-02")
		for _, s := range allStays {
			sd := s.CheckInDate.Format("2006-01-02")
			ed := s.CheckOutDate.Format("2006-01-02")
			if sd == ds {
				ci++
			}
			if ed == ds {
				co++
			}
			if sd <= ds && ed > ds {
				occ++
			}
		}
		days = append(days, dayInfo{Date: ds, CheckIns: ci, CheckOuts: co, Occupied: occ})
	}
	return response.OK(c, days)
}

func deriveReservationStatus(s domain.GuestStay) string {
	if s.ActualCheckOut != nil {
		return "checked_out"
	}
	if s.ActualCheckIn != nil {
		return "in_house"
	}
	if s.CheckInDate.Before(time.Now()) || s.CheckInDate.Equal(time.Now()) {
		return "pending_checkin"
	}
	return "upcoming"
}

func coalesceStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func formatTimePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(time.RFC3339)
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func float64Ptr(v float64) *float64 {
	return &v
}
