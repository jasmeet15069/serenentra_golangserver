package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hotelharmony/api/internal/service"
	"github.com/hotelharmony/api/internal/worker"
	"github.com/hotelharmony/api/pkg/response"
)

type BillingHandler struct {
	baseHandler
	pool     *pgxpool.Pool
	emailSvc *service.EmailService

	invoiceSchemaOnce sync.Once
	invoiceSchemaErr  error
}

func NewBillingHandler(pool *pgxpool.Pool, emailSvc *service.EmailService, secret string) *BillingHandler {
	return &BillingHandler{baseHandler: newBase(secret), pool: pool, emailSvc: emailSvc}
}

func (h *BillingHandler) Register(r fiber.Router) {
	// All billing endpoints are staff-only; gate the whole group behind auth.
	g := r.Group("", authGate(h.secret))
	g.Get("/billing/folios", h.ListFolios)
	g.Get("/billing/folios/:id", h.GetFolio)
	g.Post("/billing/folios", h.CreateFolio)
	g.Post("/billing/folios/:id/charges", h.AddCharge)
	g.Post("/billing/folios/:id/payments", h.RecordPayment)
	g.Get("/billing/invoices", h.ListInvoices)
	g.Post("/billing/invoices", h.GenerateInvoice)
	g.Get("/billing/invoices/:id", h.GetInvoice)
	g.Post("/billing/invoices/:id/email", h.EmailInvoice)
	g.Get("/billing/transactions", h.TransactionHistory)
	g.Get("/billing/charges", h.ListAllCharges)
}

func (h *BillingHandler) ListAllCharges(c *fiber.Ctx) error {
	rows, err := tenantPool(c, h.pool).Query(c.Context(), `
		SELECT fc.id, fc.folio_id, fc.description, fc.charge_type, fc.amount, fc.tax_amount, fc.reference_id, fc.posted_at, fc.posted_by,
		       COALESCE(f.status, ''), COALESCE(gs.guest_name, '')
		FROM folio_charges fc
		JOIN folios f ON f.id = fc.folio_id
		JOIN guest_stays gs ON gs.id = f.booking_id
		WHERE fc.hotel_id = $1
		ORDER BY fc.posted_at DESC`, h.hotelID(c))
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to list charges")
	}
	defer rows.Close()

	type chargeItem struct {
		ID          uuid.UUID  `json:"id"`
		FolioID     uuid.UUID  `json:"folio_id"`
		Description string     `json:"description"`
		ChargeType  *string    `json:"charge_type,omitempty"`
		Amount      float64    `json:"amount"`
		TaxAmount   float64    `json:"tax_amount"`
		ReferenceID *uuid.UUID `json:"reference_id,omitempty"`
		PostedAt    time.Time  `json:"posted_at"`
		PostedBy    *uuid.UUID `json:"posted_by,omitempty"`
		FolioStatus string     `json:"folio_status"`
		GuestName   string     `json:"guest_name"`
	}
	out := make([]chargeItem, 0)
	for rows.Next() {
		var ch chargeItem
		if err := rows.Scan(&ch.ID, &ch.FolioID, &ch.Description, &ch.ChargeType,
			&ch.Amount, &ch.TaxAmount, &ch.ReferenceID, &ch.PostedAt, &ch.PostedBy,
			&ch.FolioStatus, &ch.GuestName); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, "failed to scan charge")
		}
		out = append(out, ch)
	}
	return response.OK(c, out)
}

// ---------------------------------------------------------------------------
// Folios
// ---------------------------------------------------------------------------

func (h *BillingHandler) ListFolios(c *fiber.Ctx) error {
	status := c.Query("status")
	search := c.Query("search")

	// Payments are linked to a booking (guest_stay_id), not directly to a folio
	// (there is no folio_id column on payments). When a booking has more than one
	// folio, summing the booking's payments onto every sibling folio would
	// double-count them. To avoid that while staying correct without inventing a
	// linkage, the booking's completed payments are attributed to the booking's
	// earliest (canonical) folio only; sibling folios show 0 paid.
	q := `SELECT f.id, f.hotel_id, f.booking_id, f.guest_id, f.status, f.currency,
	         f.created_at, f.closed_at,
	         COALESCE(gs.guest_name, ''), gs.room_id, COALESCE(r.room_number, ''),
	         COALESCE((SELECT SUM(fc.amount + fc.tax_amount) FROM folio_charges fc WHERE fc.folio_id = f.id), 0),
	         COALESCE((
	             SELECT SUM(p.amount) FROM payments p
	             WHERE p.guest_stay_id = f.booking_id AND p.status = 'completed'
	               AND f.id = (
	                   SELECT pf.id FROM folios pf
	                   WHERE pf.booking_id = f.booking_id
	                   ORDER BY pf.created_at, pf.id
	                   LIMIT 1
	               )
	         ), 0)
		  FROM folios f
		  JOIN guest_stays gs ON gs.id = f.booking_id
		  LEFT JOIN rooms r ON r.id = gs.room_id
		  WHERE f.hotel_id = $1`
	args := []interface{}{h.hotelID(c)}
	argIdx := 2

	if status != "" {
		q += fmt.Sprintf(" AND f.status = $%d", argIdx)
		args = append(args, status)
		argIdx++
	}
	if search != "" {
		q += fmt.Sprintf(" AND gs.guest_name ILIKE $%d", argIdx)
		args = append(args, "%"+search+"%")
		argIdx++
	}
	q += " ORDER BY f.created_at DESC"

	rows, err := tenantPool(c, h.pool).Query(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to list folios")
	}
	defer rows.Close()

	type folioItem struct {
		ID           uuid.UUID  `json:"id"`
		BookingID    uuid.UUID  `json:"booking_id"`
		GuestID      uuid.UUID  `json:"guest_id"`
		Status       string     `json:"status"`
		Currency     string     `json:"currency"`
		GuestName    string     `json:"guest_name"`
		RoomID       *uuid.UUID `json:"room_id,omitempty"`
		RoomNumber   string     `json:"room_number"`
		TotalCharges float64    `json:"total_charges"`
		TotalPaid    float64    `json:"total_paid"`
		Balance      float64    `json:"balance"`
		CreatedAt    time.Time  `json:"created_at"`
		ClosedAt     *time.Time `json:"closed_at,omitempty"`
	}
	out := make([]folioItem, 0)

	for rows.Next() {
		var item folioItem
		var hotelID uuid.UUID
		err := rows.Scan(
			&item.ID,
			&hotelID,
			&item.BookingID,
			&item.GuestID,
			&item.Status,
			&item.Currency,
			&item.CreatedAt,
			&item.ClosedAt,
			&item.GuestName,
			&item.RoomID,
			&item.RoomNumber,
			&item.TotalCharges,
			&item.TotalPaid,
		)
		if err != nil {
			return response.Error(c, fiber.StatusInternalServerError, "failed to scan folio row")
		}
		item.Balance = item.TotalCharges - item.TotalPaid
		out = append(out, item)
	}
	return response.OK(c, out)
}

func (h *BillingHandler) GetFolio(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid folio id")
	}

	var folioID, hotelID, bookingID, guestID uuid.UUID
	var status, currency string
	var createdAt time.Time
	var closedAt *time.Time
	var guestName, roomNumber string
	var roomID *uuid.UUID

	err = tenantPool(c, h.pool).QueryRow(c.Context(), `
		SELECT f.id, f.hotel_id, f.booking_id, f.guest_id, f.status, f.currency,
		       f.created_at, f.closed_at,
		       COALESCE(gs.guest_name, ''), gs.room_id,
		       COALESCE(r.room_number, '')
		FROM folios f
		JOIN guest_stays gs ON gs.id = f.booking_id
		LEFT JOIN rooms r ON r.id = gs.room_id
		WHERE f.id = $1 AND f.hotel_id = $2`, id, h.hotelID(c),
	).Scan(&folioID, &hotelID, &bookingID, &guestID, &status, &currency,
		&createdAt, &closedAt, &guestName, &roomID, &roomNumber)
	if err != nil {
		return response.Error(c, fiber.StatusNotFound, "folio not found")
	}

	chargeRows, err := tenantPool(c, h.pool).Query(c.Context(), `
		SELECT id, folio_id, description, charge_type, amount, tax_amount, reference_id, posted_at, posted_by
		FROM folio_charges
		WHERE folio_id = $1
		ORDER BY posted_at`, id)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to load charges")
	}
	defer chargeRows.Close()

	type chargeItem struct {
		ID          uuid.UUID  `json:"id"`
		FolioID     uuid.UUID  `json:"folio_id"`
		Description string     `json:"description"`
		ChargeType  *string    `json:"charge_type,omitempty"`
		Amount      float64    `json:"amount"`
		TaxAmount   float64    `json:"tax_amount"`
		ReferenceID *uuid.UUID `json:"reference_id,omitempty"`
		PostedAt    time.Time  `json:"posted_at"`
		PostedBy    *uuid.UUID `json:"posted_by,omitempty"`
	}
	charges := make([]chargeItem, 0)
	var totalCharges, totalTax float64
	for chargeRows.Next() {
		var ch chargeItem
		if err := chargeRows.Scan(&ch.ID, &ch.FolioID, &ch.Description, &ch.ChargeType,
			&ch.Amount, &ch.TaxAmount, &ch.ReferenceID, &ch.PostedAt, &ch.PostedBy); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, "failed to scan charge")
		}
		totalCharges += ch.Amount
		totalTax += ch.TaxAmount
		charges = append(charges, ch)
	}

	// Payments are linked to the booking (guest_stay_id), not to a single folio.
	// When a booking has multiple folios, the booking's payments must not be
	// counted toward every folio's balance. We attribute the booking's completed
	// payments only to the booking's earliest (canonical) folio; for sibling
	// folios the payments are still listed for reference but excluded from the
	// balance (see isCanonicalFolio below).
	var isCanonicalFolio bool
	if err := tenantPool(c, h.pool).QueryRow(c.Context(), `
		SELECT $1 = (
			SELECT pf.id FROM folios pf
			WHERE pf.booking_id = $2
			ORDER BY pf.created_at, pf.id
			LIMIT 1
		)`, folioID, bookingID).Scan(&isCanonicalFolio); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to resolve folio payments")
	}

	payRows, err := tenantPool(c, h.pool).Query(c.Context(), `
		SELECT id, payment_number, amount, payment_method, status, notes, created_at, processed_by
		FROM payments
		WHERE guest_stay_id = $1 AND hotel_id = $2
		ORDER BY created_at DESC`, bookingID, h.hotelID(c))
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to load payments")
	}
	defer payRows.Close()

	type paymentItem struct {
		ID            uuid.UUID  `json:"id"`
		PaymentNumber string     `json:"payment_number"`
		Amount        float64    `json:"amount"`
		Method        string     `json:"method"`
		Status        string     `json:"status"`
		Notes         *string    `json:"notes,omitempty"`
		ProcessedBy   *uuid.UUID `json:"processed_by,omitempty"`
		CreatedAt     time.Time  `json:"created_at"`
	}
	payments := make([]paymentItem, 0)
	var totalPaid float64
	for payRows.Next() {
		var p paymentItem
		if err := payRows.Scan(&p.ID, &p.PaymentNumber, &p.Amount, &p.Method,
			&p.Status, &p.Notes, &p.CreatedAt, &p.ProcessedBy); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, "failed to scan payment")
		}
		if p.Status == "completed" && isCanonicalFolio {
			totalPaid += p.Amount
		}
		payments = append(payments, p)
	}

	return response.OK(c, map[string]interface{}{
		"id":            folioID,
		"hotel_id":      hotelID,
		"booking_id":    bookingID,
		"guest_id":      guestID,
		"status":        status,
		"currency":      currency,
		"guest_name":    guestName,
		"room_id":       roomID,
		"room_number":   roomNumber,
		"created_at":    createdAt,
		"closed_at":     closedAt,
		"total_charges": totalCharges,
		"total_tax":     totalTax,
		"total_paid":    totalPaid,
		"balance":       totalCharges + totalTax - totalPaid,
		"charges":       charges,
		"payments":      payments,
	})
}

type createFolioRequest struct {
	BookingID string `json:"booking_id"`
	GuestID   string `json:"guest_id"`
	Currency  string `json:"currency"`
}

func (h *BillingHandler) CreateFolio(c *fiber.Ctx) error {
	var req createFolioRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	bookingID, err := uuid.Parse(strings.TrimSpace(req.BookingID))
	if err != nil {
		return response.Error(c, fiber.StatusUnprocessableEntity, "booking_id is required and must be a valid UUID")
	}
	guestID, err := uuid.Parse(strings.TrimSpace(req.GuestID))
	if err != nil {
		return response.Error(c, fiber.StatusUnprocessableEntity, "guest_id is required and must be a valid UUID")
	}
	currency := strings.ToUpper(strings.TrimSpace(req.Currency))
	if currency == "" {
		currency = "USD"
	}

	hotelID := h.hotelID(c)
	folioID := uuid.New()
	_, err = tenantPool(c, h.pool).Exec(c.Context(), `
		INSERT INTO folios (id, hotel_id, booking_id, guest_id, status, currency, created_at)
		VALUES ($1, $2, $3, $4, 'open', $5, now())`,
		folioID, hotelID, bookingID, guestID, currency)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}

	h.audit(c, hotelID, &guestID, "CREATE", "folios", folioID, map[string]interface{}{
		"id":         folioID,
		"booking_id": bookingID,
		"guest_id":   guestID,
		"status":     "open",
		"currency":   currency,
	})

	return response.Created(c, map[string]interface{}{
		"id":         folioID,
		"hotel_id":   hotelID,
		"booking_id": bookingID,
		"guest_id":   guestID,
		"status":     "open",
		"currency":   currency,
	})
}

type addChargeRequest struct {
	Description string  `json:"description"`
	ChargeType  string  `json:"charge_type"`
	Amount      float64 `json:"amount"`
	TaxAmount   float64 `json:"tax_amount"`
	ReferenceID *string `json:"reference_id,omitempty"`
}

func (h *BillingHandler) AddCharge(c *fiber.Ctx) error {
	folioID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid folio id")
	}

	var req addChargeRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if strings.TrimSpace(req.Description) == "" {
		return response.Error(c, fiber.StatusUnprocessableEntity, "description is required")
	}
	if req.Amount <= 0 {
		return response.Error(c, fiber.StatusUnprocessableEntity, "amount must be positive")
	}

	var refID *uuid.UUID
	if req.ReferenceID != nil {
		if parsed, parseErr := uuid.Parse(strings.TrimSpace(*req.ReferenceID)); parseErr == nil {
			refID = &parsed
		}
	}

	chargeTypeStr := strings.TrimSpace(req.ChargeType)
	if chargeTypeStr == "" {
		chargeTypeStr = "other"
	}

	hotelID := h.hotelID(c)
	chargeID := uuid.New()
	_, err = tenantPool(c, h.pool).Exec(c.Context(), `
		INSERT INTO folio_charges (id, folio_id, hotel_id, description, charge_type, amount, tax_amount, reference_id, posted_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())`,
		chargeID, folioID, hotelID, strings.TrimSpace(req.Description),
		chargeTypeStr, req.Amount, req.TaxAmount, refID)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}

	h.audit(c, hotelID, nil, "CREATE", "folio_charges", chargeID, map[string]interface{}{
		"folio_id":    folioID,
		"description": req.Description,
		"charge_type": chargeTypeStr,
		"amount":      req.Amount,
		"tax_amount":  req.TaxAmount,
	})

	return response.Created(c, map[string]interface{}{
		"id":           chargeID,
		"folio_id":     folioID,
		"description":  strings.TrimSpace(req.Description),
		"charge_type":  chargeTypeStr,
		"amount":       req.Amount,
		"tax_amount":   req.TaxAmount,
		"reference_id": refID,
	})
}

type recordPaymentRequest struct {
	Amount        float64 `json:"amount"`
	PaymentMethod string  `json:"payment_method"`
	Notes         string  `json:"notes,omitempty"`
}

func (h *BillingHandler) RecordPayment(c *fiber.Ctx) error {
	folioID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid folio id")
	}

	var req recordPaymentRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if req.Amount <= 0 {
		return response.Error(c, fiber.StatusUnprocessableEntity, "amount must be positive")
	}
	method := strings.ToLower(strings.TrimSpace(req.PaymentMethod))
	if method == "" {
		method = "cash"
	}

	var stayID uuid.UUID
	err = tenantPool(c, h.pool).QueryRow(c.Context(), `SELECT booking_id FROM folios WHERE id = $1 AND hotel_id = $2`,
		folioID, tenantHotelID(c)).Scan(&stayID)
	if err != nil {
		return response.Error(c, fiber.StatusNotFound, "folio not found")
	}

	paymentID := uuid.New()
	paymentNumber := fmt.Sprintf("PAY-%s-%d", time.Now().UTC().Format("20060102"), time.Now().UnixMilli()%100000)
	notes := nullableBillingText(req.Notes)
	_, err = tenantPool(c, h.pool).Exec(c.Context(), `
		INSERT INTO payments (id, hotel_id, payment_number, guest_stay_id, amount, payment_method, status, notes, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'completed', $7, now())`,
		paymentID, tenantHotelID(c), paymentNumber, stayID, req.Amount, method, notes)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}

	h.audit(c, tenantHotelID(c), nil, "CREATE", "payments", paymentID, map[string]interface{}{
		"folio_id":       folioID,
		"guest_stay_id":  stayID,
		"amount":         req.Amount,
		"payment_method": method,
		"status":         "completed",
	})

	return response.Created(c, map[string]interface{}{
		"id":             paymentID,
		"payment_number": paymentNumber,
		"folio_id":       folioID,
		"guest_stay_id":  stayID,
		"amount":         req.Amount,
		"payment_method": method,
		"status":         "completed",
		"notes":          notes,
	})
}

// ---------------------------------------------------------------------------
// Invoices
// ---------------------------------------------------------------------------

func (h *BillingHandler) ListInvoices(c *fiber.Ctx) error {
	if err := h.ensureInvoiceSchema(c); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}

	status := c.Query("status")
	search := c.Query("search")

	q := `SELECT i.id, i.hotel_id, i.folio_id, i.invoice_number, i.status,
	         i.subtotal, i.tax_total, i.total, i.currency, i.notes,
	         i.created_at, i.updated_at, i.sent_at, i.paid_at,
	         COALESCE(gs.guest_name, '')
		  FROM invoices i
		  JOIN folios f ON f.id = i.folio_id
		  JOIN guest_stays gs ON gs.id = f.booking_id
		  WHERE i.hotel_id = $1`
	args := []interface{}{tenantHotelID(c)}
	argIdx := 2

	if status != "" {
		q += fmt.Sprintf(" AND i.status = $%d", argIdx)
		args = append(args, status)
		argIdx++
	}
	if search != "" {
		q += fmt.Sprintf(" AND gs.guest_name ILIKE $%d", argIdx)
		args = append(args, "%"+search+"%")
		argIdx++
	}
	q += " ORDER BY i.created_at DESC"

	rows, err := tenantPool(c, h.pool).Query(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to list invoices")
	}
	defer rows.Close()

	type invoiceItem struct {
		ID            uuid.UUID  `json:"id"`
		FolioID       uuid.UUID  `json:"folio_id"`
		InvoiceNumber string     `json:"invoice_number"`
		Status        string     `json:"status"`
		Subtotal      float64    `json:"subtotal"`
		TaxTotal      float64    `json:"tax_total"`
		Total         float64    `json:"total"`
		Currency      string     `json:"currency"`
		Notes         *string    `json:"notes,omitempty"`
		GuestName     string     `json:"guest_name"`
		CreatedAt     time.Time  `json:"created_at"`
		UpdatedAt     time.Time  `json:"updated_at"`
		SentAt        *time.Time `json:"sent_at,omitempty"`
		PaidAt        *time.Time `json:"paid_at,omitempty"`
	}
	out := make([]invoiceItem, 0)

	for rows.Next() {
		var item invoiceItem
		var hotelID uuid.UUID
		if err := rows.Scan(&item.ID, &hotelID, &item.FolioID, &item.InvoiceNumber, &item.Status,
			&item.Subtotal, &item.TaxTotal, &item.Total, &item.Currency, &item.Notes,
			&item.CreatedAt, &item.UpdatedAt, &item.SentAt, &item.PaidAt,
			&item.GuestName); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, "failed to scan invoice row")
		}
		out = append(out, item)
	}
	return response.OK(c, out)
}

type generateInvoiceRequest struct {
	FolioID string `json:"folio_id"`
	Notes   string `json:"notes,omitempty"`
}

func (h *BillingHandler) GenerateInvoice(c *fiber.Ctx) error {
	if err := h.ensureInvoiceSchema(c); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}

	var req generateInvoiceRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	folioID, err := uuid.Parse(strings.TrimSpace(req.FolioID))
	if err != nil {
		return response.Error(c, fiber.StatusUnprocessableEntity, "folio_id is required and must be a valid UUID")
	}

	var currency string
	var guestID uuid.UUID
	err = tenantPool(c, h.pool).QueryRow(c.Context(),
		`SELECT currency, guest_id FROM folios WHERE id = $1 AND hotel_id = $2`,
		folioID, tenantHotelID(c)).Scan(&currency, &guestID)
	if err != nil {
		return response.Error(c, fiber.StatusNotFound, "folio not found")
	}

	chargeRows, err := tenantPool(c, h.pool).Query(c.Context(), `
		SELECT description, amount, tax_amount
		FROM folio_charges
		WHERE folio_id = $1
		ORDER BY posted_at`, folioID)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to load charges")
	}
	defer chargeRows.Close()

	var subtotal, taxTotal float64
	type invLine struct {
		Description string  `json:"description"`
		Amount      float64 `json:"amount"`
		TaxAmount   float64 `json:"tax_amount"`
	}
	lines := make([]invLine, 0)
	for chargeRows.Next() {
		var line invLine
		if err := chargeRows.Scan(&line.Description, &line.Amount, &line.TaxAmount); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, "failed to scan charge")
		}
		subtotal += line.Amount
		taxTotal += line.TaxAmount
		lines = append(lines, line)
	}

	total := subtotal + taxTotal
	invoiceID := uuid.New()
	invoiceNumber := fmt.Sprintf("INV-%s-%d", time.Now().UTC().Format("20060102"), time.Now().UnixMilli()%100000)

	_, err = tenantPool(c, h.pool).Exec(c.Context(), `
		INSERT INTO invoices (id, hotel_id, folio_id, invoice_number, status, subtotal, tax_total, total, currency, notes, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'draft', $5, $6, $7, $8, $9, now(), now())`,
		invoiceID, tenantHotelID(c), folioID, invoiceNumber,
		subtotal, taxTotal, total, currency, nullableBillingText(req.Notes))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}

	h.audit(c, tenantHotelID(c), &guestID, "CREATE", "invoices", invoiceID, map[string]interface{}{
		"folio_id":       folioID,
		"invoice_number": invoiceNumber,
		"subtotal":       subtotal,
		"tax_total":      taxTotal,
		"total":          total,
		"currency":       currency,
	})

	return response.Created(c, map[string]interface{}{
		"id":             invoiceID,
		"folio_id":       folioID,
		"invoice_number": invoiceNumber,
		"status":         "draft",
		"subtotal":       subtotal,
		"tax_total":      taxTotal,
		"total":          total,
		"currency":       currency,
		"lines":          lines,
		"notes":          nullableBillingText(req.Notes),
	})
}

func (h *BillingHandler) GetInvoice(c *fiber.Ctx) error {
	if err := h.ensureInvoiceSchema(c); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}

	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid invoice id")
	}

	var invoiceID, hotelID, folioID uuid.UUID
	var invoiceNumber, status, currency string
	var subtotal, taxTotal, total float64
	var notes *string
	var createdAt, updatedAt time.Time
	var sentAt, paidAt *time.Time
	var guestName string

	err = tenantPool(c, h.pool).QueryRow(c.Context(), `
		SELECT i.id, i.hotel_id, i.folio_id, i.invoice_number, i.status,
		       i.subtotal, i.tax_total, i.total, i.currency, i.notes,
		       i.created_at, i.updated_at, i.sent_at, i.paid_at,
		       COALESCE(gs.guest_name, '')
		FROM invoices i
		JOIN folios f ON f.id = i.folio_id
		JOIN guest_stays gs ON gs.id = f.booking_id
		WHERE i.id = $1 AND i.hotel_id = $2`, id, tenantHotelID(c),
	).Scan(&invoiceID, &hotelID, &folioID, &invoiceNumber, &status,
		&subtotal, &taxTotal, &total, &currency, &notes,
		&createdAt, &updatedAt, &sentAt, &paidAt, &guestName)
	if err != nil {
		return response.Error(c, fiber.StatusNotFound, "invoice not found")
	}

	return response.OK(c, map[string]interface{}{
		"id":             invoiceID,
		"hotel_id":       hotelID,
		"folio_id":       folioID,
		"invoice_number": invoiceNumber,
		"status":         status,
		"subtotal":       subtotal,
		"tax_total":      taxTotal,
		"total":          total,
		"currency":       currency,
		"notes":          notes,
		"guest_name":     guestName,
		"created_at":     createdAt,
		"updated_at":     updatedAt,
		"sent_at":        sentAt,
		"paid_at":        paidAt,
	})
}

func (h *BillingHandler) EmailInvoice(c *fiber.Ctx) error {
	if err := h.ensureInvoiceSchema(c); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}

	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid invoice id")
	}

	var invoiceNumber, status, guestEmail, guestName string
	var folioID uuid.UUID
	var total float64
	err = tenantPool(c, h.pool).QueryRow(c.Context(), `
		SELECT i.invoice_number, i.status, i.folio_id, i.total,
		       COALESCE(gs.guest_email, ''), COALESCE(gs.guest_name, '')
		FROM invoices i
		JOIN folios f ON f.id = i.folio_id
		JOIN guest_stays gs ON gs.id = f.booking_id
		WHERE i.id = $1 AND i.hotel_id = $2`,
		id, tenantHotelID(c)).Scan(&invoiceNumber, &status, &folioID, &total, &guestEmail, &guestName)
	if err != nil {
		return response.Error(c, fiber.StatusNotFound, "invoice not found")
	}

	_, err = tenantPool(c, h.pool).Exec(c.Context(), `
		UPDATE invoices SET status = 'sent', sent_at = now(), updated_at = now()
		WHERE id = $1`, id)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to update invoice")
	}

	h.audit(c, tenantHotelID(c), nil, "EMAIL", "invoices", id, map[string]interface{}{
		"invoice_number": invoiceNumber,
		"folio_id":       folioID,
		"status_before":  status,
		"status_after":   "sent",
	})

	invoiceTotal := fmt.Sprintf("%.2f", total)
	invoiceDate := time.Now().UTC().Format("2006-01-02")
	worker.SubmitOrRun("email.invoice", func(context.Context) error {
		return h.emailSvc.SendInvoice(guestEmail, guestName, "Grand Hotel Mumbai", invoiceNumber, invoiceTotal, invoiceDate)
	})

	return response.OK(c, map[string]interface{}{
		"message":        "invoice queued for email",
		"invoice_id":     id,
		"invoice_number": invoiceNumber,
		"status":         "sent",
	})
}

// ---------------------------------------------------------------------------
// Transactions (payment history)
// ---------------------------------------------------------------------------

func (h *BillingHandler) TransactionHistory(c *fiber.Ctx) error {
	method := c.Query("method")
	statusFilter := c.Query("status")
	search := c.Query("search")

	q := `SELECT p.id, p.payment_number, p.guest_stay_id, p.amount, p.payment_method, p.status,
	         p.notes, p.created_at, p.processed_by,
	         COALESCE(gs.guest_name, ''),
	         COALESCE(r.room_number, '')
		  FROM payments p
		  JOIN guest_stays gs ON gs.id = p.guest_stay_id
		  LEFT JOIN rooms r ON r.id = gs.room_id
		  WHERE p.hotel_id = $1`
	args := []interface{}{tenantHotelID(c)}
	argIdx := 2

	if method != "" {
		q += fmt.Sprintf(" AND p.payment_method = $%d", argIdx)
		args = append(args, method)
		argIdx++
	}
	if statusFilter != "" {
		q += fmt.Sprintf(" AND p.status = $%d", argIdx)
		args = append(args, statusFilter)
		argIdx++
	}
	if search != "" {
		q += fmt.Sprintf(" AND gs.guest_name ILIKE $%d", argIdx)
		args = append(args, "%"+search+"%")
		argIdx++
	}
	q += " ORDER BY p.created_at DESC"

	rows, err := tenantPool(c, h.pool).Query(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to load transactions")
	}
	defer rows.Close()

	type txItem struct {
		ID            uuid.UUID  `json:"id"`
		PaymentNumber string     `json:"payment_number"`
		GuestStayID   *uuid.UUID `json:"guest_stay_id,omitempty"`
		Amount        float64    `json:"amount"`
		PaymentMethod string     `json:"payment_method"`
		Status        string     `json:"status"`
		Notes         *string    `json:"notes,omitempty"`
		ProcessedBy   *uuid.UUID `json:"processed_by,omitempty"`
		GuestName     string     `json:"guest_name"`
		RoomNumber    string     `json:"room_number"`
		CreatedAt     time.Time  `json:"created_at"`
	}
	out := make([]txItem, 0)

	for rows.Next() {
		var t txItem
		if err := rows.Scan(&t.ID, &t.PaymentNumber, &t.GuestStayID, &t.Amount, &t.PaymentMethod,
			&t.Status, &t.Notes, &t.CreatedAt, &t.ProcessedBy, &t.GuestName, &t.RoomNumber); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, "failed to scan transaction")
		}
		out = append(out, t)
	}
	return response.OK(c, out)
}

// ---------------------------------------------------------------------------
// Schema helpers
// ---------------------------------------------------------------------------

func (h *BillingHandler) ensureInvoiceSchema(c *fiber.Ctx) error {
	_, err := tenantPool(c, h.pool).Exec(c.Context(), `
		CREATE TABLE IF NOT EXISTS invoices (
			id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
			hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
			folio_id UUID NOT NULL REFERENCES folios(id) ON DELETE CASCADE,
			invoice_number TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'draft',
			subtotal NUMERIC(12,2) NOT NULL DEFAULT 0,
			tax_total NUMERIC(12,2) NOT NULL DEFAULT 0,
			total NUMERIC(12,2) NOT NULL DEFAULT 0,
			currency TEXT NOT NULL DEFAULT 'USD',
			notes TEXT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			sent_at TIMESTAMPTZ,
			paid_at TIMESTAMPTZ,
			UNIQUE (hotel_id, invoice_number)
		)`)
	return err
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (h *BillingHandler) audit(c *fiber.Ctx, hotelID uuid.UUID, userID *uuid.UUID, action, resource string, resourceID uuid.UUID, newData map[string]interface{}) {
	data, _ := json.Marshal(newData)
	_, _ = tenantPool(c, h.pool).Exec(c.Context(), `
		INSERT INTO audit_logs (
			id, hotel_id, user_id, action, table_name, record_id, resource_type, resource_id,
			new_data, user_agent, ai_triggered, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,false,now())`,
		uuid.New(), hotelID, userID, action, resource, resourceID, resource, resourceID, data, c.Get("User-Agent"),
	)
}

func nullableBillingText(value string) interface{} {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return value
}
