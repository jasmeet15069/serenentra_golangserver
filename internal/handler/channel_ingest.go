package handler

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hotelharmony/api/internal/domain"
	"github.com/hotelharmony/api/internal/repository/postgres"
	"github.com/hotelharmony/api/pkg/response"
)

// Inbound OTA / booking-engine reservations.
//
// Booking.com, Agoda, OYO, MakeMyTrip, Expedia and a client's own website all
// push reservations rather than being polled. They arrive with no staff session
// and no bearer token, so this endpoint is registered BEFORE the auth gate and
// authenticates itself: the URL names a channel_connection, and the request is
// signed with that connection's api_key.
//
// Registration order is the thing to get right here. api.Use(authGate) in
// router.go gates only what is registered after it, so a webhook registered in
// the staff block would 401 every delivery an OTA ever made — and OTAs treat a
// 401 as a retryable failure, so it would retry forever while nothing worked.
//
// Three properties matter more than the mapping itself:
//
//	stored first    the raw payload is persisted before anything is
//	                interpreted, so a delivery is never lost to a mapping bug
//	                and can be replayed once the mapping is fixed
//	idempotent      OTAs redeliver on any timeout or non-2xx. The unique index
//	                on (hotel_id, channel_name, external_ref) is what stops one
//	                guest becoming four reservations holding four rooms
//	inventory-safe  the room is assigned and held inside the same transaction
//	                that creates the stay, against the same overlap constraint
//	                the front desk writes through, so an OTA and the desk cannot
//	                sell the same room concurrently

// ChannelIngestHandler receives reservations pushed by OTAs and booking engines.
type ChannelIngestHandler struct {
	pool     *pgxpool.Pool
	db       *postgres.DB
	roomRepo postgres.RoomRepository
}

func NewChannelIngestHandler(pool *pgxpool.Pool, db *postgres.DB, roomRepo postgres.RoomRepository) *ChannelIngestHandler {
	return &ChannelIngestHandler{pool: pool, db: db, roomRepo: roomRepo}
}

// Register mounts the webhook. It MUST be called from the public block in
// router.go, before api.Use(authGate) — see the note above.
func (h *ChannelIngestHandler) Register(r fiber.Router) {
	r.Post("/v1/channel-manager/:connectionID/booking", h.Ingest)
	r.Post("/v1/channel-manager/:connectionID/cancel", h.Cancel)
}

// channelBookingPayload is the normalised shape every adapter maps onto.
//
// Field names carry alternates because the OTAs disagree: Booking.com sends
// `reservation_id`, Agoda `booking_id`, OYO `ref`. Rather than one adapter per
// provider before anything works at all, the common aliases are accepted here
// and a provider-specific adapter can be added later without changing this.
type channelBookingPayload struct {
	ExternalRef   string `json:"external_ref"`
	ReservationID string `json:"reservation_id"`
	BookingID     string `json:"booking_id"`
	Ref           string `json:"ref"`

	GuestName  string `json:"guest_name"`
	FirstName  string `json:"first_name"`
	LastName   string `json:"last_name"`
	GuestEmail string `json:"guest_email"`
	Email      string `json:"email"`
	GuestPhone string `json:"guest_phone"`
	Phone      string `json:"phone"`

	CheckIn      string `json:"check_in"`
	CheckInDate  string `json:"check_in_date"`
	CheckOut     string `json:"check_out"`
	CheckOutDate string `json:"check_out_date"`

	RoomType string `json:"room_type"`
	Adults   int    `json:"adults"`
	Children int    `json:"children"`

	Total      float64 `json:"total"`
	TotalPrice float64 `json:"total_price"`
	TaxAmount  float64 `json:"tax_amount"`
	Commission float64 `json:"commission"`
	Currency   string  `json:"currency"`

	Notes string `json:"notes"`
}

// These read through firstNonEmpty (pos_invoice.go), which returns the value
// as sent, so each result is trimmed here — an external reference with a
// trailing newline would otherwise not match its own redelivery and the
// idempotency key would silently stop working.

func (p channelBookingPayload) ref() string {
	return strings.TrimSpace(firstNonEmpty(p.ExternalRef, p.ReservationID, p.BookingID, p.Ref))
}

func (p channelBookingPayload) name() string {
	if n := strings.TrimSpace(p.GuestName); n != "" {
		return n
	}
	return strings.TrimSpace(strings.TrimSpace(p.FirstName) + " " + strings.TrimSpace(p.LastName))
}

func (p channelBookingPayload) email() string {
	return strings.TrimSpace(firstNonEmpty(p.GuestEmail, p.Email))
}
func (p channelBookingPayload) phone() string {
	return strings.TrimSpace(firstNonEmpty(p.GuestPhone, p.Phone))
}
func (p channelBookingPayload) checkIn() string {
	return strings.TrimSpace(firstNonEmpty(p.CheckIn, p.CheckInDate))
}
func (p channelBookingPayload) checkOut() string {
	return strings.TrimSpace(firstNonEmpty(p.CheckOut, p.CheckOutDate))
}
func (p channelBookingPayload) total() float64 {
	if p.Total > 0 {
		return p.Total
	}
	return p.TotalPrice
}

// channelConnection is the tenant + secret behind a webhook URL.
type channelConnection struct {
	ID      uuid.UUID
	HotelID uuid.UUID
	Name    string
	APIKey  string
	Active  bool
}

func (h *ChannelIngestHandler) lookupConnection(ctx context.Context, id uuid.UUID) (*channelConnection, error) {
	var cc channelConnection
	err := h.pool.QueryRow(ctx, `
		SELECT id, hotel_id, channel_name, COALESCE(api_key, ''), connected
		  FROM channel_connections WHERE id = $1`, id).
		Scan(&cc.ID, &cc.HotelID, &cc.Name, &cc.APIKey, &cc.Active)
	if err != nil {
		return nil, err
	}
	return &cc, nil
}

// verifySignature checks the HMAC-SHA256 of the raw body against the
// connection's api_key.
//
// Compared with hmac.Equal, not ==: a byte-by-byte comparison that returns early
// leaks, through timing, how much of a forged signature was correct, which is
// enough to reconstruct one.
func verifySignature(body []byte, secret, provided string) bool {
	provided = strings.TrimSpace(provided)
	provided = strings.TrimPrefix(provided, "sha256=")
	if secret == "" || provided == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(provided))
}

// Ingest accepts one pushed reservation.
func (h *ChannelIngestHandler) Ingest(c *fiber.Ctx) error {
	connID, err := uuid.Parse(c.Params("connectionID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid channel connection id")
	}
	ctx := c.Context()

	cc, err := h.lookupConnection(ctx, connID)
	if err != nil {
		// Deliberately the same answer as a bad signature: distinguishing
		// "no such connection" from "wrong secret" tells an unauthenticated
		// caller which connection ids are real.
		return response.Error(c, fiber.StatusUnauthorized, "unauthorized channel")
	}
	body := c.Body()
	if !verifySignature(body, cc.APIKey, c.Get("X-Signature")) {
		return response.Error(c, fiber.StatusUnauthorized, "unauthorized channel")
	}
	if !cc.Active {
		return response.Error(c, fiber.StatusForbidden, "channel connection is disabled")
	}

	var payload channelBookingPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid JSON payload")
	}
	externalRef := payload.ref()
	if externalRef == "" {
		// Without the OTA's own reference there is no idempotency key, so a
		// redelivery would create a second reservation. Refuse rather than
		// accept something that cannot be deduplicated.
		return response.Error(c, fiber.StatusUnprocessableEntity,
			"external_ref (or reservation_id / booking_id / ref) is required")
	}

	// Store the raw delivery first. If mapping or room assignment fails below,
	// the payload is still on record and can be replayed.
	recordID := uuid.New()
	var existingStatus string
	var existingStay *uuid.UUID
	err = h.pool.QueryRow(ctx, `
		INSERT INTO channel_bookings (id, hotel_id, connection_id, channel_name, external_ref, payload, status)
		VALUES ($1,$2,$3,$4,$5,$6,'received')
		ON CONFLICT (hotel_id, channel_name, external_ref)
		DO UPDATE SET status = channel_bookings.status
		RETURNING id, status, guest_stay_id`,
		recordID, cc.HotelID, cc.ID, cc.Name, externalRef, string(body),
	).Scan(&recordID, &existingStatus, &existingStay)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to record delivery")
	}

	// A redelivery of something already processed is a success, not a new
	// booking. Answering 200 stops the OTA retrying.
	if existingStatus == "processed" {
		return response.OK(c, fiber.Map{
			"status":         "duplicate",
			"external_ref":   externalRef,
			"reservation_id": existingStay,
		})
	}

	stayID, commission, procErr := h.process(ctx, cc, payload, externalRef)
	if procErr != nil {
		_, _ = h.pool.Exec(ctx, `
			UPDATE channel_bookings
			   SET status = 'failed', error = $2, processed_at = now()
			 WHERE id = $1`, recordID, procErr.Error())
		log.Printf("channel: ingest FAILED for %s ref %s: %v — payload stored as %s for replay",
			cc.Name, externalRef, procErr, recordID)
		// 422, not 500: the delivery was accepted and stored, and redelivering
		// the same payload will fail the same way. A 5xx would make the OTA
		// retry a request that cannot succeed.
		return response.Error(c, fiber.StatusUnprocessableEntity, procErr.Error())
	}

	if _, err := h.pool.Exec(ctx, `
		UPDATE channel_bookings
		   SET status = 'processed', guest_stay_id = $2, commission = $3,
		       error = NULL, processed_at = now()
		 WHERE id = $1`, recordID, stayID, commission); err != nil {
		log.Printf("channel: booking %s created but delivery %s not marked processed: %v",
			stayID, recordID, err)
	}

	// Record that the channel is live, which is what the connections screen
	// shows. Nothing used to update this except a manual re-sync.
	_, _ = h.pool.Exec(ctx,
		`UPDATE channel_connections SET last_sync_at = now() WHERE id = $1`, cc.ID)

	return response.Created(c, fiber.Map{
		"status":         "processed",
		"external_ref":   externalRef,
		"reservation_id": stayID,
		"commission":     commission,
	})
}

// process maps a payload onto a reservation and posts it to the ledger.
//
// Everything happens in one transaction: the room is chosen and taken, the CRM
// guest is recorded, and the receivable is booked together. A booking that
// existed without its ledger entry would understate what the OTA owes.
func (h *ChannelIngestHandler) process(
	ctx context.Context, cc *channelConnection, p channelBookingPayload, externalRef string,
) (uuid.UUID, float64, error) {

	name := p.name()
	if name == "" {
		return uuid.Nil, 0, errors.New("guest name is required")
	}
	if p.email() == "" && p.phone() == "" {
		return uuid.Nil, 0, errors.New("a guest email or phone is required to identify the booker")
	}
	checkIn, err := time.Parse("2006-01-02", p.checkIn())
	if err != nil {
		return uuid.Nil, 0, fmt.Errorf("invalid check-in date %q (use YYYY-MM-DD)", p.checkIn())
	}
	checkOut, err := time.Parse("2006-01-02", p.checkOut())
	if err != nil {
		return uuid.Nil, 0, fmt.Errorf("invalid check-out date %q (use YYYY-MM-DD)", p.checkOut())
	}
	if !checkOut.After(checkIn) {
		return uuid.Nil, 0, errors.New("check-out must be after check-in")
	}

	var stayID uuid.UUID
	commission := round2(p.Commission)

	txErr := h.db.WithTenantTx(ctx, func(txCtx context.Context) error {
		// Pick a room that is genuinely free for the dates. OTAs sell a room
		// TYPE, not a room, so the property decides which one — and it has to be
		// decided inside the transaction or two concurrent deliveries would both
		// be handed the same room.
		rooms, rErr := h.roomRepo.ListRoomsAvailableBetween(txCtx, cc.HotelID, checkIn, checkOut)
		if rErr != nil {
			return fmt.Errorf("could not check availability: %w", rErr)
		}
		wanted := strings.TrimSpace(strings.ToLower(p.RoomType))
		var chosen *domain.Room
		for i := range rooms {
			if wanted == "" || strings.EqualFold(strings.TrimSpace(rooms[i].RoomType), wanted) {
				chosen = &rooms[i]
				break
			}
		}
		if chosen == nil {
			if wanted != "" {
				return fmt.Errorf("no %s room is free for %s to %s",
					p.RoomType, checkIn.Format("2006-01-02"), checkOut.Format("2006-01-02"))
			}
			return fmt.Errorf("no room is free for %s to %s",
				checkIn.Format("2006-01-02"), checkOut.Format("2006-01-02"))
		}

		if _, gErr := h.roomRepo.EnsureGuest(txCtx, cc.HotelID, name, p.email(), p.phone()); gErr != nil {
			return gErr
		}

		// The OTA's price is authoritative — it is what the guest was charged
		// and what the channel will remit against. Only when it sends nothing
		// does the property's own rate card decide.
		taxRate, _ := h.roomRepo.HotelGSTRate(txCtx, cc.HotelID)
		quote := priceStay(chosen.PricePerNight, checkIn, checkOut, taxRate, 0)
		if total := p.total(); total > 0 {
			tax := round2(p.TaxAmount)
			quote.BaseTotal = round2(total - tax)
			quote.TaxAmount = tax
			quote.Discount = 0
			quote.Payable = round2(total)
		}

		adults := p.Adults
		if adults <= 0 {
			adults = 1
		}
		confirmationNo := newConfirmationNo(checkIn)
		source := cc.Name
		notes := strings.TrimSpace(p.Notes)
		var notesPtr *string
		if notes != "" {
			notesPtr = &notes
		}

		stay := &domain.GuestStay{
			GuestName:      name,
			GuestEmail:     strPtr(p.email()),
			GuestPhone:     strPtr(p.phone()),
			RoomID:         chosen.ID,
			CheckInDate:    checkIn,
			CheckOutDate:   checkOut,
			TotalAmount:    float64Ptr(quote.BaseTotal),
			TaxAmount:      quote.TaxAmount,
			Notes:          notesPtr,
			Source:         &source,
			Status:         domain.ReservationConfirmed,
			ConfirmationNo: &confirmationNo,
			Adults:         adults,
			Children:       p.Children,
			ApproachType:   "ota",
		}

		created, cErr := h.roomRepo.CreateStay(txCtx, cc.HotelID, stay)
		if cErr != nil {
			// The overlap constraint fired: the desk took the room between the
			// availability read and the insert.
			if isOverlapViolation(cErr) {
				return fmt.Errorf("room %s was taken for those dates while the booking was being processed",
					chosen.RoomNumber)
			}
			return cErr
		}
		stayID = created.ID
		created.Room = &domain.RoomSummary{RoomNumber: chosen.RoomNumber, RoomType: chosen.RoomType}

		// An OTA booking is money owed by the channel, not money received, so it
		// books to Accounts Receivable rather than Cash or Bank. Settlement
		// happens when the OTA remits.
		db := h.db.Querier(txCtx)
		reference := "FOLIO " + confirmationNo
		if alreadyPosted(txCtx, db, cc.HotelID, reference) {
			return nil
		}

		// The receivable has to belong to somebody: an OTA booking is collected
		// from the guest or the channel later, and an unnamed receivable cannot
		// be chased. Same resolver the POS and front-desk paths use, so one
		// person stays one customer across all three.
		if _, custErr := ensureAccountingCustomer(txCtx, db, cc.HotelID, CustomerDetails{
			Name: name, Phone: p.phone(), Email: p.email(),
		}); custErr != nil {
			return fmt.Errorf("ota customer: %w", custErr)
		}

		if _, jErr := postJournal(txCtx, db, cc.HotelID,
			fmt.Sprintf("%s booking — %s (%s)", cc.Name, name, externalRef), reference,
			[]journalLine{
				{accountCode: "1200", debit: quote.Payable, memo: "Receivable — " + cc.Name},
				{accountCode: "4000", credit: round2(quote.BaseTotal), memo: "Room revenue (" + cc.Name + ")"},
				{accountCode: "2100", credit: quote.TaxAmount, memo: "GST on room revenue"},
			}); jErr != nil {
			return fmt.Errorf("ota ledger: %w", jErr)
		}

		// Commission is an expense owed to the channel, booked separately so the
		// revenue line stays the gross the guest paid. Netting it would
		// understate both revenue and the cost of distribution.
		if commission > 0 {
			if _, cmErr := postJournal(txCtx, db, cc.HotelID,
				fmt.Sprintf("%s commission — %s", cc.Name, externalRef),
				"COMM "+confirmationNo, []journalLine{
					{accountCode: "5100", debit: commission, memo: cc.Name + " commission"},
					{accountCode: "2000", credit: commission, memo: "Payable to " + cc.Name},
				}); cmErr != nil {
				return fmt.Errorf("ota commission: %w", cmErr)
			}
		}
		return nil
	})
	if txErr != nil {
		return uuid.Nil, 0, txErr
	}
	return stayID, commission, nil
}

// Cancel handles an OTA cancelling a reservation it previously pushed.
//
// The reservation is soft-cancelled, which releases the room for resale while
// keeping the record and its ledger entry — an OTA cancellation is exactly the
// history a channel-performance report is built from.
func (h *ChannelIngestHandler) Cancel(c *fiber.Ctx) error {
	connID, err := uuid.Parse(c.Params("connectionID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid channel connection id")
	}
	ctx := c.Context()

	cc, err := h.lookupConnection(ctx, connID)
	if err != nil {
		return response.Error(c, fiber.StatusUnauthorized, "unauthorized channel")
	}
	body := c.Body()
	if !verifySignature(body, cc.APIKey, c.Get("X-Signature")) {
		return response.Error(c, fiber.StatusUnauthorized, "unauthorized channel")
	}

	var payload channelBookingPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid JSON payload")
	}
	externalRef := payload.ref()
	if externalRef == "" {
		return response.Error(c, fiber.StatusUnprocessableEntity, "external_ref is required")
	}

	var stayID *uuid.UUID
	err = h.pool.QueryRow(ctx, `
		SELECT guest_stay_id FROM channel_bookings
		 WHERE hotel_id = $1 AND channel_name = $2 AND external_ref = $3`,
		cc.HotelID, cc.Name, externalRef).Scan(&stayID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return response.Error(c, fiber.StatusNotFound, "no booking with that reference")
		}
		return response.Error(c, fiber.StatusInternalServerError, "lookup failed")
	}
	if stayID == nil {
		return response.Error(c, fiber.StatusConflict, "that delivery never produced a reservation")
	}

	// Read the stay before cancelling, so its room is known for the release
	// check below.
	stay, sErr := h.roomRepo.FindStayByID(ctx, cc.HotelID, *stayID)
	if sErr != nil {
		return response.Error(c, fiber.StatusNotFound, "reservation not found")
	}
	if stay.ActualCheckIn != nil {
		// The guest is already here. Cancelling would release an occupied room
		// for resale, so this needs a human at the desk rather than a webhook.
		return response.Error(c, fiber.StatusConflict,
			"the guest has already checked in — cancel at the front desk")
	}

	if err := h.roomRepo.SoftCancelStay(ctx, cc.HotelID, *stayID,
		cc.Name+" cancellation ("+externalRef+")", 0, nil); err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			return response.OK(c, fiber.Map{"status": "already_cancelled", "external_ref": externalRef})
		}
		return response.Error(c, fiber.StatusInternalServerError, "cancel failed")
	}

	// Release the room only when nobody else is in it: a room routinely carries
	// a current guest plus later bookings, and freeing it on one cancellation
	// would advertise an occupied room as available.
	if active, aErr := h.roomRepo.RoomHasActiveStay(ctx, cc.HotelID, stay.RoomID); aErr == nil && !active {
		_ = h.roomRepo.UpdateRoomStatus(ctx, cc.HotelID, stay.RoomID, domain.RoomStatusAvailable)
	}

	// Reverse the postings, or the cancelled booking leaves a receivable nobody
	// will ever collect sitting on the balance sheet for good — and a commission
	// payable to a channel that earned nothing.
	//
	// Reversed, never deleted: a posted entry is cancelled by an equal-and-
	// opposite one so the audit trail survives and the net effect is zero. Both
	// legs are reversed, the sale and the commission.
	if confirmation := derefStr(stay.ConfirmationNo); confirmation != "" {
		db := h.db.Querier(ctx)
		for _, ref := range []string{"FOLIO " + confirmation, "COMM " + confirmation} {
			if _, revErr := reverseJournalsByReference(ctx, db, cc.HotelID, ref,
				fmt.Sprintf("%s cancellation — %s", cc.Name, externalRef)); revErr != nil {
				// The cancellation itself has already committed and must stand.
				// Log loudly rather than failing it: an unreversed entry is
				// repairable, a room left held for a guest who cancelled is not.
				log.Printf("channel: LEDGER REVERSAL FAILED for %s (%s): %v — booking cancelled, journal still standing",
					ref, externalRef, revErr)
			}
		}
	}

	_, _ = h.pool.Exec(ctx, `
		UPDATE channel_bookings SET status = 'cancelled', processed_at = now()
		 WHERE hotel_id = $1 AND channel_name = $2 AND external_ref = $3`,
		cc.HotelID, cc.Name, externalRef)

	return response.OK(c, fiber.Map{"status": "cancelled", "external_ref": externalRef})
}
