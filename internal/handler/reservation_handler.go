package handler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/hotelharmony/api/internal/config"
	"github.com/hotelharmony/api/internal/domain"
	"github.com/hotelharmony/api/internal/repository/postgres"
	"github.com/hotelharmony/api/internal/service"
	"github.com/hotelharmony/api/internal/worker"
	"github.com/hotelharmony/api/pkg/response"
)

type ReservationHandler struct {
	roomRepo postgres.RoomRepository
	db       *postgres.DB
	cfg      *config.Config
	emailSvc *service.EmailService
	smsSvc   *service.SMSService
}

// NewReservationHandler wires the reservation endpoints.
//
// db is held alongside roomRepo so multi-step writes can run inside one
// transaction via db.WithTenantTx: the repository methods themselves pick the
// transaction up from the context, so they need no transactional variants.
// A nil db is tolerated and simply means each step commits on its own, which is
// the pre-transaction behaviour.
func NewReservationHandler(roomRepo postgres.RoomRepository, db *postgres.DB, cfg *config.Config, emailSvc *service.EmailService, smsSvc *service.SMSService) *ReservationHandler {
	return &ReservationHandler{roomRepo: roomRepo, db: db, cfg: cfg, emailSvc: emailSvc, smsSvc: smsSvc}
}

// inTx runs fn inside a transaction when one is available, and directly
// otherwise, so the handler body reads the same either way.
func (h *ReservationHandler) inTx(ctx context.Context, fn func(context.Context) error) error {
	if h.db == nil {
		return fn(ctx)
	}
	return h.db.WithTenantTx(ctx, fn)
}

func (h *ReservationHandler) Register(r fiber.Router) {
	r.Get("/reservations", h.List)
	r.Get("/reservations/calendar", h.Calendar)
	r.Post("/reservations/quote", h.Quote)
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

	// Cancelling used to delete the row, so cancelled bookings have never
	// appeared in this list. Keep that default now that they survive, or every
	// existing screen would suddenly show them. include_cancelled=true opts in,
	// and asking for status=cancelled explicitly still works.
	includeCancelled := strings.EqualFold(c.Query("include_cancelled"), "true")

	result := make([]map[string]interface{}, 0, len(allStays))
	for _, s := range allStays {
		resStatus := deriveReservationStatus(s)

		if s.Status == domain.ReservationCancelled && !includeCancelled &&
			!strings.EqualFold(status, string(domain.ReservationCancelled)) {
			continue
		}
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
			"discount_amount":  s.DiscountAmount,
			"tax_amount":       s.TaxAmount,
			"payable":          s.Payable(),
			"nights":           int(s.CheckOutDate.Sub(s.CheckInDate).Hours() / 24),
			"status":           resStatus,
			"source":           coalesceStr(s.Source),
			"approach_type":    s.ApproachType,
			"confirmation_no":  coalesceStr(s.ConfirmationNo),
			"adults":           s.Adults,
			"children":         s.Children,
			"cancelled_at":     formatTimePtr(s.CancelledAt),
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

	// Added by the front-office module. All optional, so a client sending only
	// the original eight fields above behaves exactly as before.
	Adults         int    `json:"adults"`
	Children       int    `json:"children"`
	ApproachType   string `json:"approach_type"`
	DurationNights int    `json:"duration_nights"`
	PromoCode      string `json:"promo_code"`

	// Identity, stored on the CRM guest record.
	IDType   string `json:"id_type"`
	IDNumber string `json:"id_number"`

	// Payment taken at the desk. Omitting Payment entirely books the
	// reservation without settling it, which is what an unpaid future booking
	// is — the accounting entries are only raised when money actually changes
	// hands.
	Payment *reservationPaymentRequest `json:"payment"`
}

// reservationPaymentRequest is the money taken at the desk when the booking is
// made.
//
// Card: last four digits only. A full card number that reaches this server also
// reaches the request logs and every pg_dump backup, so masking it here would
// be far too late to mean anything — the number must never be sent. The
// payments_card_last4_check constraint enforces the same rule at the column, so
// a writer that bypasses this handler cannot store one either.
type reservationPaymentRequest struct {
	Method         string  `json:"method"`
	Amount         float64 `json:"amount"`
	UPIID          string  `json:"upi_id"`
	TransactionRef string  `json:"transaction_ref"`
	CardLast4      string  `json:"card_last4"`
	AuthCode       string  `json:"auth_code"`
	CashReceived   float64 `json:"cash_received"`
}

// validate enforces the fields each method genuinely needs, so a settled
// booking always carries the reference someone would need to trace it.
func (p *reservationPaymentRequest) validate(payable float64) error {
	method := strings.ToLower(strings.TrimSpace(p.Method))
	if method == "" {
		return fmt.Errorf("payment.method is required")
	}
	switch method {
	case "upi":
		if strings.TrimSpace(p.UPIID) == "" || strings.TrimSpace(p.TransactionRef) == "" {
			return fmt.Errorf("upi payments require upi_id and transaction_ref")
		}
	case "card":
		if strings.TrimSpace(p.AuthCode) == "" {
			return fmt.Errorf("card payments require auth_code")
		}
		last4 := strings.TrimSpace(p.CardLast4)
		if last4 != "" {
			// Rejected, never truncated: a longer value means a full card
			// number was transmitted, and silently trimming it would hide that
			// it had already been logged.
			if len(last4) != 4 {
				return fmt.Errorf("card_last4 must be exactly 4 digits — never send a full card number")
			}
			for _, r := range last4 {
				if r < '0' || r > '9' {
					return fmt.Errorf("card_last4 must be 4 digits")
				}
			}
		}
	case "cash":
		if round2(p.CashReceived) < round2(payable) {
			return fmt.Errorf("cash_received %.2f is less than the payable %.2f", p.CashReceived, payable)
		}
	case "credit", "room", "bill_to_room", "account":
		// Nothing collected now; it becomes a receivable against the customer.
	default:
		return fmt.Errorf("unsupported payment method %q", p.Method)
	}
	return nil
}

// createReservationResponse is the reservation plus what settling it produced.
//
// GuestStay is embedded rather than nested so its JSON fields stay promoted to
// the top level: the endpoint used to return the stay itself, and a client
// reading data.id must keep working. The three additions are pointers with
// omitempty, so an unsettled booking's response is byte-for-byte what it was
// before this module existed.
type createReservationResponse struct {
	*domain.GuestStay
	Quote      *stayQuote        `json:"quote,omitempty"`
	Settlement *settlementResult `json:"settlement,omitempty"`
	Promo      *promoResult      `json:"promo,omitempty"`
}

// resolveStayDates works out the stay window from either an explicit checkout
// date or a duration in nights.
//
// The desk thinks in nights ("two nights"), the schema stores a checkout date,
// and the original API accepted only the latter. Accepting both means the
// wizard can offer a duration field without a second endpoint. Supplying both
// is allowed only when they agree — silently preferring one would let a client
// book dates it did not ask for.
func resolveStayDates(req createReservationRequest) (time.Time, time.Time, error) {
	checkIn, err := time.Parse("2006-01-02", req.CheckInDate)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid check_in_date (use YYYY-MM-DD)")
	}

	var checkOut time.Time
	hasOut := req.CheckOutDate != ""
	if hasOut {
		checkOut, err = time.Parse("2006-01-02", req.CheckOutDate)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid check_out_date (use YYYY-MM-DD)")
		}
	}

	if req.DurationNights > 0 {
		derived := checkIn.AddDate(0, 0, req.DurationNights)
		if hasOut && !derived.Equal(checkOut) {
			return time.Time{}, time.Time{}, fmt.Errorf(
				"check_out_date %s does not match duration_nights %d (expected %s)",
				checkOut.Format("2006-01-02"), req.DurationNights, derived.Format("2006-01-02"))
		}
		checkOut = derived
	} else if !hasOut {
		return time.Time{}, time.Time{}, fmt.Errorf("check_out_date or duration_nights is required")
	}

	if !checkOut.After(checkIn) {
		return time.Time{}, time.Time{}, fmt.Errorf("check_out must be after check_in")
	}
	return checkIn, checkOut, nil
}

// newConfirmationNo produces the reference staff and guests quote on the phone.
// A UUID is unusable for that. Collisions are caught by the partial unique
// index on (hotel_id, confirmation_no) rather than assumed away.
func newConfirmationNo(checkIn time.Time) string {
	suffix := strings.ToUpper(uuid.New().String()[:6])
	return fmt.Sprintf("RES-%s-%s", checkIn.Format("20060102"), suffix)
}

// isOverlapViolation reports whether err is the exclusion-constraint violation
// raised by guest_stays_no_overlap (SQLSTATE 23P01).
//
// The handler pre-checks availability for a friendly message, but the database
// is what actually holds the line: the pre-check is a read followed by a write,
// so two concurrent bookings both pass it, and /api/tables/:table bypasses the
// handler altogether. When the constraint fires, answer 409 exactly as the
// pre-check would have.
func isOverlapViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23P01"
}

func (h *ReservationHandler) Create(c *fiber.Ctx) error {
	var req createReservationRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if req.GuestName == "" {
		return response.Error(c, fiber.StatusBadRequest, "guest_name is required")
	}
	// At least one contact detail: without a phone or an email there is nothing
	// to match a returning guest on, so EnsureGuest cannot record them in CRM
	// and no folio can be opened at check-in.
	if strings.TrimSpace(req.GuestPhone) == "" && strings.TrimSpace(req.GuestEmail) == "" {
		return response.Error(c, fiber.StatusBadRequest, "guest_phone or guest_email is required")
	}
	checkIn, checkOut, err := resolveStayDates(req)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
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

	// A room cannot hold more guests than it sleeps. The wizard has always
	// collected occupancy and the API silently dropped it, so this was never
	// checked anywhere.
	adults := req.Adults
	if adults <= 0 {
		adults = 1
	}
	if room.Capacity > 0 && adults+req.Children > room.Capacity {
		return response.Error(c, fiber.StatusBadRequest, fmt.Sprintf(
			"room %s sleeps %d; %d guests requested",
			room.RoomNumber, room.Capacity, adults+req.Children))
	}

	// Friendly pre-check. The guest_stays_no_overlap constraint is what
	// actually prevents the double booking (see isOverlapViolation) — this
	// exists so the common case gets a message naming the conflicting dates
	// rather than a constraint error.
	free, err := h.roomRepo.RoomIsFree(c.Context(), hotelID, roomID, checkIn, checkOut, uuid.Nil)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to check room availability")
	}
	if !free {
		return response.Error(c, fiber.StatusConflict, fmt.Sprintf(
			"room %s is already booked between %s and %s",
			room.RoomNumber, checkIn.Format("2006-01-02"), checkOut.Format("2006-01-02")))
	}

	var notes *string
	if req.Notes != "" {
		notes = &req.Notes
	}

	source := strings.TrimSpace(req.Source)
	if source == "" {
		source = "Direct"
	}
	approach := strings.TrimSpace(req.ApproachType)
	if approach == "" {
		approach = "manual"
	}

	// Priced by the same function the quote endpoint uses, so what the guest
	// was shown is what gets stored. The client's own arithmetic is never
	// trusted — it is display only.
	taxRate, tErr := h.roomRepo.HotelGSTRate(c.Context(), hotelID)
	if tErr != nil {
		taxRate = 0
	}
	nights := int(checkOut.Sub(checkIn).Hours() / 24)

	// Resolve the promo before pricing, so tax is charged on the discounted
	// base. A code that the guest was quoted but which no longer applies fails
	// the booking rather than silently charging them full price.
	var promo promoResult
	if strings.TrimSpace(req.PromoCode) != "" {
		base := priceStay(room.PricePerNight, checkIn, checkOut, taxRate, 0).BaseTotal
		promo, err = resolvePromo(c.Context(), h.db.Querier(c.Context()),
			hotelID, req.PromoCode, base, nights)
		if err != nil {
			return response.Error(c, fiber.StatusInternalServerError, "failed to validate promo code")
		}
		if !promo.Valid {
			return response.Error(c, fiber.StatusBadRequest, promo.Message)
		}
	}

	quote := priceStay(room.PricePerNight, checkIn, checkOut, taxRate, promo.Discount)

	// Validate the payment against the final payable before writing anything.
	if req.Payment != nil {
		if vErr := req.Payment.validate(quote.Payable); vErr != nil {
			return response.Error(c, fiber.StatusBadRequest, vErr.Error())
		}
	}

	confirmationNo := newConfirmationNo(checkIn)

	stay := &domain.GuestStay{
		ID:             uuid.New(),
		GuestName:      req.GuestName,
		GuestEmail:     strPtr(req.GuestEmail),
		GuestPhone:     strPtr(req.GuestPhone),
		RoomID:         roomID,
		CheckInDate:    checkIn,
		CheckOutDate:   checkOut,
		TotalAmount:    float64Ptr(quote.BaseTotal),
		DiscountAmount: quote.Discount,
		TaxAmount:      quote.TaxAmount,
		PromoCode:      strPtr(strings.TrimSpace(req.PromoCode)),
		Notes:          notes,
		Source:         &source,
		Status:         domain.ReservationConfirmed,
		ConfirmationNo: &confirmationNo,
		Adults:         adults,
		Children:       req.Children,
		ApproachType:   approach,
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	}

	// The CRM guest and the stay are written together. Previously the guest was
	// a separate best-effort statement whose error was discarded, so a booking
	// could exist whose guest record had silently failed to be written.
	//
	// The guest id deliberately does NOT go on stay.GuestID. Despite the name,
	// guest_stays.guest_id is a foreign key to users(id) — a guest with a login
	// account — not to guests(id). Putting a CRM guest id there violates the
	// constraint and fails the whole booking. Check-in records it on the folio,
	// which has no such constraint.
	var created *domain.GuestStay
	var settlement settlementResult
	err = h.inTx(c.Context(), func(ctx context.Context) error {
		guestID, gErr := h.roomRepo.EnsureGuest(ctx, hotelID, req.GuestName, req.GuestEmail, req.GuestPhone)
		if gErr != nil {
			return gErr
		}
		// Identity documents belong on the CRM guest, which is where a
		// returning guest's details are looked up from. The columns have
		// existed on guests since the beginning and nothing ever wrote them.
		if guestID != uuid.Nil && (req.IDType != "" || req.IDNumber != "") {
			if idErr := h.roomRepo.SetGuestIdentity(ctx, hotelID, guestID, req.IDType, req.IDNumber); idErr != nil {
				return idErr
			}
		}

		var cErr error
		created, cErr = h.roomRepo.CreateStay(ctx, hotelID, stay)
		if cErr != nil {
			return cErr
		}
		created.Room = &domain.RoomSummary{RoomNumber: room.RoomNumber, RoomType: room.RoomType}

		// Consume the promo only once the booking is certain to exist, and
		// with the guard in the UPDATE so a code cannot be over-redeemed by
		// two desks at the same moment.
		if promo.ID != uuid.Nil {
			if pErr := redeemPromo(ctx, h.db.Querier(ctx), hotelID, promo.ID); pErr != nil {
				return pErr
			}
		}

		// Money changing hands is what raises the accounting entries. A booking
		// taken without payment is simply an unsettled reservation.
		if req.Payment != nil {
			var sErr error
			settlement, sErr = settleReservation(ctx,
				h.db.Querier(ctx), hotelID, created, req.Payment, quote)
			return sErr
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errPromoNotApplicable) {
			return response.Error(c, fiber.StatusConflict, "promo code was just fully redeemed")
		}
		// The constraint caught a booking the pre-check above could not: either
		// a concurrent request took the room between the two, or the write came
		// through a path that never ran the pre-check at all.
		if isOverlapViolation(err) {
			return response.Error(c, fiber.StatusConflict, fmt.Sprintf(
				"room %s was just booked for those dates", room.RoomNumber))
		}
		return response.Error(c, fiber.StatusInternalServerError, fmt.Sprintf("failed to create: %v", err))
	}

	// Confirm the booking now, to whichever channels the guest gave us. This
	// used to be sent at check-in, which meant a guest who booked in advance
	// was told nothing until they arrived.
	h.notifyBookingConfirmed(c.Context(), hotelID, created)

	// The stay is embedded, not nested, so its fields stay exactly where they
	// were — a client reading data.id or data.guest_name is unaffected. The
	// accounting artefacts appear beside them and are omitted entirely when the
	// booking was taken without payment.
	body := createReservationResponse{GuestStay: created, Quote: &quote}
	if req.Payment != nil {
		body.Settlement = &settlement
	}
	if promo.Valid {
		body.Promo = &promo
	}
	return response.Created(c, body)
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

	current, err := h.roomRepo.FindStayByID(c.Context(), hotelID, id)
	if err != nil {
		return response.Error(c, fiber.StatusNotFound, "reservation not found")
	}
	if current.Status == domain.ReservationCancelled {
		return response.Error(c, fiber.StatusConflict, "cannot edit a cancelled reservation")
	}

	fields := make(map[string]interface{})

	// Cross-validate dates against whichever side isn't being changed, so a
	// PATCH of only one date can't silently invert the stay (checkout before
	// checkin).
	if req.CheckInDate != "" || req.CheckOutDate != "" {
		effectiveCheckIn := current.CheckInDate
		effectiveCheckOut := current.CheckOutDate
		if req.CheckInDate != "" {
			d, pErr := time.Parse("2006-01-02", req.CheckInDate)
			if pErr != nil {
				return response.Error(c, fiber.StatusBadRequest, "invalid check_in_date (use YYYY-MM-DD)")
			}
			effectiveCheckIn = d
		}
		if req.CheckOutDate != "" {
			d, pErr := time.Parse("2006-01-02", req.CheckOutDate)
			if pErr != nil {
				return response.Error(c, fiber.StatusBadRequest, "invalid check_out_date (use YYYY-MM-DD)")
			}
			effectiveCheckOut = d
		}
		if !effectiveCheckOut.After(effectiveCheckIn) {
			return response.Error(c, fiber.StatusBadRequest, "check_out must be after check_in")
		}

		// Moving the dates has to answer the same availability question a new
		// booking does. Without this a reservation could be dragged onto dates
		// the room is already sold for, and the room would simply be
		// double-booked.
		free, fErr := h.roomRepo.RoomIsFree(c.Context(), hotelID, current.RoomID, effectiveCheckIn, effectiveCheckOut, id)
		if fErr != nil {
			return response.Error(c, fiber.StatusInternalServerError, "failed to check room availability")
		}
		if !free {
			return response.Error(c, fiber.StatusConflict, fmt.Sprintf(
				"the room is already booked between %s and %s",
				effectiveCheckIn.Format("2006-01-02"), effectiveCheckOut.Format("2006-01-02")))
		}

		// Re-price. total_amount was previously left at whatever the original
		// dates cost, so extending a two-night stay to seven kept the two-night
		// price and the difference was never billed.
		if room, rErr := h.roomRepo.FindRoomByID(c.Context(), hotelID, current.RoomID); rErr == nil {
			nights := effectiveCheckOut.Sub(effectiveCheckIn).Hours() / 24
			fields["total_amount"] = room.PricePerNight * nights
		}
	}
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
		if errors.Is(err, postgres.ErrNotFound) {
			return response.Error(c, fiber.StatusNotFound, "reservation not found")
		}
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

	hotelID := tenantHotelID(c)
	stay, err := h.roomRepo.FindStayByID(c.Context(), hotelID, id)
	if err != nil {
		return response.Error(c, fiber.StatusNotFound, "reservation not found")
	}
	if stay.ActualCheckIn != nil {
		return response.Error(c, fiber.StatusBadRequest, "cannot cancel checked-in reservation")
	}
	if stay.Status == domain.ReservationCancelled {
		return response.Error(c, fiber.StatusConflict, "reservation is already cancelled")
	}

	// Reason and fee are optional so the existing bare DELETE call keeps
	// working; a client that sends neither simply records a cancellation with
	// no reason, which is still infinitely more than the previous hard delete
	// left behind.
	var req cancelReservationRequest
	_ = c.BodyParser(&req)

	roomID := stay.RoomID
	if err := h.roomRepo.SoftCancelStay(c.Context(), hotelID, id, strings.TrimSpace(req.Reason), req.Fee, actorUserID(c)); err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			return response.Error(c, fiber.StatusNotFound, "reservation not found")
		}
		return response.Error(c, fiber.StatusInternalServerError, "cancel failed")
	}
	// Only release the room if nobody is still in it. A room commonly carries a
	// current guest plus later bookings, and cancelling one of those must not
	// advertise an occupied room as free — which then lets it be double-booked.
	if active, err := h.roomRepo.RoomHasActiveStay(c.Context(), hotelID, roomID); err == nil && !active {
		_ = h.roomRepo.UpdateRoomStatus(c.Context(), hotelID, roomID, domain.RoomStatusAvailable)
	}
	return response.OK(c, map[string]interface{}{
		"status":           "cancelled",
		"cancellation_fee": req.Fee,
	})
}

func (h *ReservationHandler) CheckIn(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid id")
	}
	hotelID := tenantHotelID(c)

	// A stay can only be checked in once. Repeating the call used to overwrite
	// actual_check_in with a later timestamp, quietly rewriting when the guest
	// arrived, and a cancelled booking could be checked in as though it were
	// live.
	existing, err := h.roomRepo.FindStayByID(c.Context(), hotelID, id)
	if err != nil {
		return response.Error(c, fiber.StatusNotFound, "reservation not found")
	}
	if existing.Status == domain.ReservationCancelled {
		return response.Error(c, fiber.StatusConflict, "cannot check in a cancelled reservation")
	}
	if existing.ActualCheckIn != nil {
		return response.Error(c, fiber.StatusConflict, "reservation is already checked in")
	}

	now := time.Now().UTC()

	// The stay, the room, the CRM guest and the folio move together. Previously
	// each was a separate statement whose error was discarded, so a check-in
	// could mark a guest in-house with no folio for their charges to post to.
	err = h.inTx(c.Context(), func(ctx context.Context) error {
		if uErr := h.roomRepo.UpdateStay(ctx, hotelID, id, map[string]interface{}{
			"actual_check_in": now,
			"status":          string(domain.ReservationInHouse),
		}); uErr != nil {
			return uErr
		}
		if rErr := h.roomRepo.UpdateRoomStatus(ctx, hotelID, existing.RoomID, domain.RoomStatusOccupied); rErr != nil {
			return rErr
		}
		// Open the folio this stay's charges post to. The CRM guest is resolved
		// from the contact details on the stay, which also covers bookings made
		// before guests were recorded at all. folios.guest_id has no foreign
		// key, so it holds the CRM guests(id) rather than a user account.
		gid, gErr := h.roomRepo.EnsureGuest(ctx, hotelID,
			existing.GuestName, derefStr(existing.GuestEmail), derefStr(existing.GuestPhone))
		if gErr != nil {
			return gErr
		}
		if gid != uuid.Nil {
			if _, fErr := h.roomRepo.EnsureFolioForBooking(ctx, hotelID, id, gid, "INR"); fErr != nil {
				return fErr
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			return response.Error(c, fiber.StatusNotFound, "reservation not found")
		}
		return response.Error(c, fiber.StatusInternalServerError, "check-in failed")
	}

	stay, _ := h.roomRepo.FindStayByID(c.Context(), hotelID, id)
	if stay != nil {
		h.notifyCheckedIn(c.Context(), hotelID, stay)
	}

	return response.OK(c, map[string]string{"status": "checked_in"})
}

func (h *ReservationHandler) CheckOut(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid id")
	}
	hotelID := tenantHotelID(c)

	existing, err := h.roomRepo.FindStayByID(c.Context(), hotelID, id)
	if err != nil {
		return response.Error(c, fiber.StatusNotFound, "reservation not found")
	}
	if existing.ActualCheckIn == nil {
		return response.Error(c, fiber.StatusConflict, "reservation has not been checked in")
	}
	if existing.ActualCheckOut != nil {
		return response.Error(c, fiber.StatusConflict, "reservation is already checked out")
	}

	// A guest must not walk out owing money. Restaurant bills signed to the room
	// land on the folio, and nothing used to read that balance at check-out —
	// so every meal charged to a room was simply never collected.
	//
	// Overridable, because a company-billed stay legitimately departs with an
	// open balance that is invoiced later. The override is explicit rather than
	// the default, so the desk has to mean it.
	outstanding, folioID, balErr := folioOutstanding(c.Context(), h.db.Querier(c.Context()), hotelID, id)
	if balErr != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to read folio balance")
	}
	if outstanding > 0 && !strings.EqualFold(c.Query("allow_outstanding"), "true") {
		return response.Error(c, fiber.StatusConflict, fmt.Sprintf(
			"folio has an outstanding balance of %.2f — settle it, or pass allow_outstanding=true to bill it later",
			outstanding))
	}

	now := time.Now().UTC()
	err = h.inTx(c.Context(), func(ctx context.Context) error {
		if uErr := h.roomRepo.UpdateStay(ctx, hotelID, id, map[string]interface{}{
			"actual_check_out": now,
			"status":           string(domain.ReservationCheckedOut),
		}); uErr != nil {
			return uErr
		}
		if rErr := h.roomRepo.UpdateRoomStatus(ctx, hotelID, existing.RoomID, domain.RoomStatusCleaning); rErr != nil {
			return rErr
		}
		// Close the folio behind the guest so no further charge can be signed
		// to a room whose occupant has left.
		if folioID != uuid.Nil {
			if _, cErr := h.db.Querier(ctx).Exec(ctx,
				`UPDATE folios SET status = 'closed', closed_at = now()
				  WHERE id = $1 AND hotel_id = $2 AND status = 'open'`, folioID, hotelID); cErr != nil {
				return cErr
			}
		}
		// The departure is what housekeeping works from. The room was being set
		// to 'cleaning' with no task raised, so nothing ever appeared on their
		// board and the room sat dirty until someone noticed.
		if _, hErr := h.db.Querier(ctx).Exec(ctx, `
			INSERT INTO housekeeping_assignments
			       (id, hotel_id, room_id, task_type, priority, status, notes, created_at)
			SELECT $1, $2, $3, 'checkout_clean', 'normal', 'pending', $4, now()
			 WHERE NOT EXISTS (
			     SELECT 1 FROM housekeeping_assignments
			      WHERE hotel_id = $2 AND room_id = $3
			        AND task_type = 'checkout_clean' AND status IN ('pending','in_progress')
			 )`,
			uuid.New(), hotelID, existing.RoomID,
			"Departure — "+existing.GuestName); hErr != nil {
			return hErr
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			return response.Error(c, fiber.StatusNotFound, "reservation not found")
		}
		return response.Error(c, fiber.StatusInternalServerError, "check-out failed")
	}

	stay, _ := h.roomRepo.FindStayByID(c.Context(), hotelID, id)

	if stay != nil {
		hotelName := h.hotelDisplayName(c.Context(), hotelID)
		// The amount owed, not just the room rate: discount and tax are part of
		// what the guest actually pays.
		totalAmount := fmt.Sprintf("%.2f", stay.Payable())
		invoiceRef := derefStr(stay.ConfirmationNo)
		if invoiceRef == "" {
			invoiceRef = stay.ID.String()
		}
		dueDate := stay.CheckOutDate.Format("2006-01-02")
		guestName := stay.GuestName
		if stay.GuestEmail != nil {
			guestEmail := *stay.GuestEmail
			worker.SubmitOrRun("email.invoice", func(context.Context) error {
				return h.emailSvc.SendInvoice(guestEmail, guestName, hotelName, invoiceRef, totalAmount, dueDate)
			})
		}
		if stay.GuestPhone != nil {
			guestPhone := *stay.GuestPhone
			worker.SubmitOrRun("sms.checkout_thanks", func(context.Context) error {
				return h.smsSvc.Send(guestPhone, fmt.Sprintf(
					"Thank you for staying at %s! Your checkout is complete.", hotelName))
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

// stayQuote is the priced breakdown of a proposed stay. One calculation, used
// by both the quote endpoint and Create, so the figure the guest is shown and
// the figure written to the reservation cannot drift apart.
type stayQuote struct {
	Nights    int     `json:"nights"`
	RoomRate  float64 `json:"room_rate"`
	BaseTotal float64 `json:"base_total"`
	Discount  float64 `json:"discount"`
	TaxRate   float64 `json:"tax_rate"`
	TaxAmount float64 `json:"tax_amount"`
	Payable   float64 `json:"payable"`
}

// priceStay computes the breakdown for a room over a date range.
//
// Money is rounded through the same round2 the POS billing path uses, so the
// two sides of the system round identically.
//
// Tax is charged on the discounted base, not the list price — discounting after
// tax would overcharge the guest by the tax on the discount.
func priceStay(roomRate float64, checkIn, checkOut time.Time, taxRate, discount float64) stayQuote {
	nights := int(checkOut.Sub(checkIn).Hours() / 24)
	if nights < 1 {
		nights = 1
	}
	base := round2(roomRate * float64(nights))
	if discount > base {
		discount = base
	}
	taxable := base - discount
	tax := round2(taxable * taxRate / 100)
	return stayQuote{
		Nights:    nights,
		RoomRate:  roomRate,
		BaseTotal: base,
		Discount:  round2(discount),
		TaxRate:   taxRate,
		TaxAmount: tax,
		Payable:   round2(taxable + tax),
	}
}

// Quote prices a proposed stay without creating anything, so the booking wizard
// can show a total it did not invent. It is a POST because it takes a body, not
// because it changes state — nothing is written.
func (h *ReservationHandler) Quote(c *fiber.Ctx) error {
	var req createReservationRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	checkIn, checkOut, err := resolveStayDates(req)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	roomID, err := uuid.Parse(req.RoomID)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid room_id")
	}

	hotelID := tenantHotelID(c)
	room, err := h.roomRepo.FindRoomByID(c.Context(), hotelID, roomID)
	if err != nil {
		return response.Error(c, fiber.StatusNotFound, "room not found")
	}

	// A tenant with no configured rate quotes no tax rather than a guessed one.
	taxRate, err := h.roomRepo.HotelGSTRate(c.Context(), hotelID)
	if err != nil {
		taxRate = 0
	}

	return response.OK(c, priceStay(room.PricePerNight, checkIn, checkOut, taxRate, 0))
}

type cancelReservationRequest struct {
	Reason string  `json:"reason"`
	Fee    float64 `json:"cancellation_fee"`
}

// actorUserID is the staff member performing the action, for attribution.
// Nil for a token that carries no subject, which the columns allow.
func actorUserID(c *fiber.Ctx) *uuid.UUID {
	raw, ok := c.Locals("user_id").(string)
	if !ok || strings.TrimSpace(raw) == "" {
		return nil
	}
	id, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil
	}
	return &id
}

// hotelDisplayName is the tenant's own name for guest-facing messages, falling
// back to a neutral phrase rather than another hotel's name. Every guest
// notification used to say "Grand Hotel Mumbai" regardless of which tenant sent
// it.
func (h *ReservationHandler) hotelDisplayName(ctx context.Context, hotelID uuid.UUID) string {
	if name, err := h.roomRepo.HotelName(ctx, hotelID); err == nil && strings.TrimSpace(name) != "" {
		return name
	}
	return "our hotel"
}

// notifyBookingConfirmed tells the guest their booking is held. Sent when the
// reservation is created — it used to be sent at check-in, so a guest booking
// weeks ahead heard nothing until they walked through the door.
func (h *ReservationHandler) notifyBookingConfirmed(ctx context.Context, hotelID uuid.UUID, stay *domain.GuestStay) {
	if stay == nil {
		return
	}
	h.notifyStay(ctx, hotelID, stay, "booking_confirmation")
}

// notifyCheckedIn welcomes the guest once they are in the room.
func (h *ReservationHandler) notifyCheckedIn(ctx context.Context, hotelID uuid.UUID, stay *domain.GuestStay) {
	if stay == nil {
		return
	}
	h.notifyStay(ctx, hotelID, stay, "checkin_welcome")
}

// notifyStay dispatches on whichever channels the guest actually gave us — a
// guest with only an email, or only a phone, must still be reached.
func (h *ReservationHandler) notifyStay(ctx context.Context, hotelID uuid.UUID, stay *domain.GuestStay, kind string) {
	hotelName := h.hotelDisplayName(ctx, hotelID)
	roomNumber := ""
	if stay.Room != nil {
		roomNumber = stay.Room.RoomNumber
	}
	checkInDate := stay.CheckInDate.Format("2006-01-02")
	checkOutDate := stay.CheckOutDate.Format("2006-01-02")
	guestName := stay.GuestName

	if stay.GuestEmail != nil && h.emailSvc != nil {
		guestEmail := *stay.GuestEmail
		worker.SubmitOrRun("email."+kind, func(context.Context) error {
			return h.emailSvc.SendBookingConfirmation(guestEmail, guestName, hotelName, roomNumber, checkInDate, checkOutDate)
		})
	}
	if stay.GuestPhone != nil && h.smsSvc != nil {
		guestPhone := *stay.GuestPhone
		worker.SubmitOrRun("sms."+kind, func(context.Context) error {
			return h.smsSvc.SendBookingConfirmation(guestPhone, guestName, hotelName, roomNumber, checkInDate, checkOutDate)
		})
	}
}

// deriveReservationStatus reports the status the API exposes.
//
// The stored status is authoritative — it is the only thing that can express
// cancelled or no-show. The two derived values below have no stored
// equivalent: they split "confirmed" by whether the arrival date has come,
// which is presentation, not state. They are kept because the reservations list
// and its filters have always used those names.
func deriveReservationStatus(s domain.GuestStay) string {
	switch s.Status {
	case domain.ReservationCancelled, domain.ReservationNoShow,
		domain.ReservationCheckedOut, domain.ReservationInHouse:
		return string(s.Status)
	}
	// Fall back to the timestamps for rows written before status was stored.
	if s.ActualCheckOut != nil {
		return string(domain.ReservationCheckedOut)
	}
	if s.ActualCheckIn != nil {
		return string(domain.ReservationInHouse)
	}
	if !s.CheckInDate.After(time.Now()) {
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
