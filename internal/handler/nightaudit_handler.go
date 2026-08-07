package handler

import (
	"fmt"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hotelharmony/api/internal/domain"
	"github.com/hotelharmony/api/pkg/response"
)

// Tables:
//   CREATE TABLE IF NOT EXISTS night_audit_reports (
//     id uuid PK, hotel_id uuid, audit_date date, status text,
//     expected_revenue jsonb, actual_revenue jsonb, discrepancies jsonb,
//     tax_summary jsonb, notes text, closed_by text,
//     created_at timestamptz
//   );

type NightAuditHandler struct {
	baseHandler
	pool *pgxpool.Pool
}

func NewNightAuditHandler(pool *pgxpool.Pool, secret string) *NightAuditHandler {
	return &NightAuditHandler{baseHandler: newBase(secret), pool: pool}
}

func (h *NightAuditHandler) Register(r fiber.Router) {
	g := r.Group("", authGate(h.secret))
	g.Get("/night-audit/checklist", h.GetChecklist)
	g.Get("/night-audit/revenue-audit", h.GetRevenueAudit)
	g.Get("/night-audit/tax-audit", h.GetTaxAudit)
	g.Post("/night-audit/close-day", h.CloseDay)
	g.Get("/night-audit/reports", h.ListReports)
}

// ---------------------------------------------------------------------------
// Audit Checklist
// ---------------------------------------------------------------------------

type checklistItem struct {
	Task      string `json:"task"`
	Completed bool   `json:"completed"`
}

func (h *NightAuditHandler) GetChecklist(c *fiber.Ctx) error {
	items := []checklistItem{
		{Task: "Verify all checked-in guests have valid registration cards", Completed: false},
		{Task: "Post all outstanding charges to guest folios", Completed: false},
		{Task: "Reconcile restaurant and bar revenue", Completed: false},
		{Task: "Verify housekeeping status for all rooms", Completed: false},
		{Task: "Check for late check-outs and early arrivals", Completed: false},
		{Task: "Verify tax calculations on all transactions", Completed: false},
		{Task: "Run end-of-day revenue summary", Completed: false},
		{Task: "Update room availability for next day", Completed: false},
		{Task: "Backup system data", Completed: false},
		{Task: "Print daily reports", Completed: false},
	}

	var auditCount int
	err := tenantPool(c, h.pool).QueryRow(c.Context(), `
		SELECT COUNT(*) FROM night_audit_reports
		WHERE hotel_id = $1 AND audit_date = CURRENT_DATE`,
		h.hotelID(c),
	).Scan(&auditCount)
	if err == nil && auditCount > 0 {
		for i := range items {
			items[i].Completed = true
		}
	}

	return response.OK(c, items)
}

// ---------------------------------------------------------------------------
// Revenue Audit
// ---------------------------------------------------------------------------

type revenueAuditResponse struct {
	Category   string  `json:"category"`
	Expected   float64 `json:"expected"`
	Actual     float64 `json:"actual"`
	Difference float64 `json:"difference"`
}

// GetRevenueAudit reconciles what the operation recorded against what the books
// say, per category.
//
// It previously read revenue_daily — a table nothing populates — and assigned
// the single figure it got to all three categories, so Room Revenue, F&B and
// Other always reported the same number and the difference was always zero. An
// audit that cannot disagree with itself is not an audit.
//
// The two sides are now genuinely independent:
//
//	Expected  the operational record: folio charges raised and restaurant bills
//	          settled today
//	Actual    the general ledger: what was actually credited to the revenue
//	          accounts today
//
// A gap between them is the signal worth having. Settlement posts to the ledger
// *after* committing the sale and only logs on failure ("sale stands, journal
// missing"), so a failed posting leaves revenue in the operation with nothing
// behind it in the books. That is precisely what this now surfaces.
//
// The two sides do not double-count a restaurant bill signed to a room: the
// expected side counts it once, as a bill, and the folio leg is excluded by
// charge_type.
func (h *NightAuditHandler) GetRevenueAudit(c *fiber.Ctx) error {
	hotelID := h.hotelID(c)
	pool := tenantPool(c, h.pool)
	ctx := c.Context()

	items := []revenueAuditResponse{
		{Category: "Room Revenue"},
		{Category: "Food & Beverage"},
		{Category: "Other Services"},
	}

	// --- Expected: the operation ---
	//
	// Accommodation and incidentals are folio charges; a charge_type of
	// 'restaurant' is the room-charged bill, counted on the F&B line instead.
	_ = pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(fc.amount), 0)
		  FROM folio_charges fc
		 WHERE fc.hotel_id = $1
		   AND fc.posted_at::date = CURRENT_DATE
		   AND COALESCE(fc.charge_type, '') <> 'restaurant'`,
		hotelID).Scan(&items[0].Expected)

	_ = pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(b.subtotal), 0)
		  FROM bills b
		 WHERE b.hotel_id = $1
		   AND b.status = 'paid'
		   AND b.paid_at::date = CURRENT_DATE`,
		hotelID).Scan(&items[1].Expected)

	// --- Actual: the ledger ---
	//
	// 4000 is room revenue and 4100 F&B; anything else typed 'revenue' is other
	// services. Credits less debits, so a reversal reduces the day rather than
	// being counted as further income.
	rows, err := pool.Query(ctx, `
		SELECT a.code, COALESCE(SUM(jl.credit - jl.debit), 0)
		  FROM accounting_journal_lines jl
		  JOIN accounting_journal_entries je ON je.id = jl.entry_id
		  JOIN accounting_accounts a ON a.id = jl.account_id
		 WHERE je.hotel_id = $1
		   AND je.entry_date = CURRENT_DATE
		   AND a.type = 'revenue'
		 GROUP BY a.code`,
		hotelID)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()
	for rows.Next() {
		var code string
		var amount float64
		if err := rows.Scan(&code, &amount); err != nil {
			continue
		}
		switch code {
		case "4000":
			items[0].Actual += amount
		case "4100":
			items[1].Actual += amount
		default:
			items[2].Actual += amount
		}
	}

	for i := range items {
		items[i].Expected = round2(items[i].Expected)
		items[i].Actual = round2(items[i].Actual)
		items[i].Difference = round2(items[i].Expected - items[i].Actual)
	}
	return response.OK(c, items)
}

// ---------------------------------------------------------------------------
// Tax Audit
// ---------------------------------------------------------------------------

type taxAuditResponse struct {
	TaxType   string  `json:"tax_type"`
	Collected float64 `json:"collected"`
	Payable   float64 `json:"payable"`
	Status    string  `json:"status"`
}

// GetTaxAudit reconciles GST charged on the day's transactions against GST
// posted to the liability account.
//
// It previously summed bookings.tax_amount for rows whose check_in was today.
// Nothing writes bookings, so it returned an empty set for every tenant — and
// even had it held data, `CASE WHEN b.total > 0 THEN 'GST' ELSE 'GST' END`
// yields 'GST' either way, so the branch decided nothing.
//
// Collected is what the operation charged the guest. Payable is what the ledger
// owes the tax authority. They should be equal, and the status says so rather
// than asserting "verified" unconditionally, which is what made the old
// response worthless: it reported success without comparing anything.
func (h *NightAuditHandler) GetTaxAudit(c *fiber.Ctx) error {
	hotelID := h.hotelID(c)
	pool := tenantPool(c, h.pool)
	ctx := c.Context()

	var collected float64
	// Folio charges (accommodation and incidentals) plus restaurant bills
	// settled today. A room-charged bill is excluded from the folio side by
	// charge_type so it is not counted on both.
	_ = pool.QueryRow(ctx, `
		SELECT COALESCE((
		         SELECT SUM(fc.tax_amount) FROM folio_charges fc
		          WHERE fc.hotel_id = $1
		            AND fc.posted_at::date = CURRENT_DATE
		            AND COALESCE(fc.charge_type, '') <> 'restaurant'
		       ), 0)
		     + COALESCE((
		         SELECT SUM(b.tax_amount) FROM bills b
		          WHERE b.hotel_id = $1
		            AND b.status = 'paid'
		            AND b.paid_at::date = CURRENT_DATE
		       ), 0)`,
		hotelID).Scan(&collected)

	// 2100 GST Payable, credits less debits so a reversal reduces the liability.
	var payable float64
	_ = pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(jl.credit - jl.debit), 0)
		  FROM accounting_journal_lines jl
		  JOIN accounting_journal_entries je ON je.id = jl.entry_id
		  JOIN accounting_accounts a ON a.id = jl.account_id
		 WHERE je.hotel_id = $1
		   AND je.entry_date = CURRENT_DATE
		   AND a.code = '2100'`,
		hotelID).Scan(&payable)

	collected, payable = round2(collected), round2(payable)

	// A cent of tolerance: both sides are rounded independently at their own
	// posting sites, so an exact-equality test would report a discrepancy for
	// nothing more than a rounding tail.
	status := "verified"
	if diff := collected - payable; diff > 0.01 || diff < -0.01 {
		status = "discrepancy"
	}

	return response.OK(c, []taxAuditResponse{{
		TaxType:   "GST",
		Collected: collected,
		Payable:   payable,
		Status:    status,
	}})
}

// ---------------------------------------------------------------------------
// Close Day
// ---------------------------------------------------------------------------

type closeDayResponse struct {
	ReportID  uuid.UUID `json:"report_id"`
	AuditDate string    `json:"audit_date"`
	Status    string    `json:"status"`
	Summary   struct {
		TotalRevenue  float64 `json:"total_revenue"`
		TotalTax      float64 `json:"total_tax"`
		OccupiedRooms int     `json:"occupied_rooms"`
		CheckOuts     int     `json:"check_outs"`
		Arrivals      int     `json:"arrivals"`
		// Added alongside the fix for occupancy_rate being stored as a room
		// count. Additive, so existing clients are unaffected.
		OccupancyRate float64 `json:"occupancy_rate"`
	} `json:"summary"`
}

func (h *NightAuditHandler) CloseDay(c *fiber.Ctx) error {
	reportID := uuid.New()
	hotelID := h.hotelID(c)
	// Use a single consistent date basis (the DB's CURRENT_DATE) for both the
	// stored audit_date and the aggregate WHERE clauses, so the report totals
	// always correspond to the day they are filed under.

	// Revenue and tax come from the ledger, which is the authoritative record
	// and the same source the trial balance reads — so the day-close and the
	// accounting module can never disagree.
	//
	// Every figure below used to be aggregated from `bookings`, which nothing
	// writes. A hotel that took all its bookings through the front desk closed
	// its day with zero revenue, zero tax, zero occupancy, zero arrivals and
	// zero departures, and those zeros were persisted as the official record.
	var totalRevenue, totalTax float64
	if err := tenantPool(c, h.pool).QueryRow(c.Context(), `
		SELECT
			COALESCE(SUM(jl.credit - jl.debit) FILTER (WHERE a.type = 'revenue'), 0),
			COALESCE(SUM(jl.credit - jl.debit) FILTER (WHERE a.code = '2100'), 0)
		FROM accounting_journal_lines jl
		JOIN accounting_journal_entries je ON je.id = jl.entry_id
		JOIN accounting_accounts a ON a.id = jl.account_id
		WHERE je.hotel_id = $1 AND je.entry_date = CURRENT_DATE`,
		hotelID,
	).Scan(&totalRevenue, &totalTax); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}

	// Occupancy is who is in the building right now, not who arrived today: a
	// guest on the second night of a three-night stay still occupies a room.
	// Arrivals and departures are the actual movements stamped by check-in and
	// check-out, not the dates the stay was booked for, so a late arrival counts
	// on the day they actually walked in.
	var occupiedRooms, checkOuts, arrivals, sellableRooms int
	if err := tenantPool(c, h.pool).QueryRow(c.Context(), `
		SELECT
			COUNT(*) FILTER (WHERE gs.actual_check_in IS NOT NULL AND gs.actual_check_out IS NULL),
			COUNT(*) FILTER (WHERE gs.actual_check_out::date = CURRENT_DATE),
			COUNT(*) FILTER (WHERE gs.actual_check_in::date  = CURRENT_DATE)
		FROM guest_stays gs
		WHERE gs.hotel_id = $1 AND gs.status <> 'cancelled'`,
		hotelID,
	).Scan(&occupiedRooms, &checkOuts, &arrivals); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}

	// occupancy_rate is a percentage. It was being handed the raw occupied-room
	// count, so a hotel with 8 rooms occupied filed an occupancy of 8%.
	if err := tenantPool(c, h.pool).QueryRow(c.Context(),
		`SELECT COUNT(*) FROM rooms WHERE hotel_id = $1
		   AND status NOT IN (`+domain.NonSellableRoomStatusSQL+`)`,
		hotelID,
	).Scan(&sellableRooms); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	occupancyRate := 0.0
	if sellableRooms > 0 {
		occupancyRate = round2(float64(occupiedRooms) / float64(sellableRooms) * 100)
	}

	// Persist audit_date as CURRENT_DATE so it matches the basis used by the
	// aggregate filters above; return that same value to the caller.
	var auditDateStr string
	if err := tenantPool(c, h.pool).QueryRow(c.Context(), `
		INSERT INTO night_audit_reports
			(id, hotel_id, audit_date, status, expected_revenue, actual_revenue,
			 total_tax, occupancy_rate, closed_by, created_at)
		VALUES ($1,$2,CURRENT_DATE,'closed', $3, $3, $4, $5, NULL, now())
		RETURNING to_char(audit_date, 'YYYY-MM-DD')`,
		reportID, hotelID, round2(totalRevenue), round2(totalTax), occupancyRate,
	).Scan(&auditDateStr); err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}

	resp := closeDayResponse{
		ReportID:  reportID,
		AuditDate: auditDateStr,
		Status:    "closed",
	}
	resp.Summary.TotalRevenue = round2(totalRevenue)
	resp.Summary.TotalTax = round2(totalTax)
	resp.Summary.OccupiedRooms = occupiedRooms
	resp.Summary.CheckOuts = checkOuts
	resp.Summary.Arrivals = arrivals
	resp.Summary.OccupancyRate = occupancyRate

	return response.Created(c, resp)
}

// ---------------------------------------------------------------------------
// Past Reports
// ---------------------------------------------------------------------------

type auditReportResponse struct {
	ID        uuid.UUID `json:"id"`
	AuditDate string    `json:"audit_date"`
	Status    string    `json:"status"`
	ClosedBy  *string   `json:"closed_by"`
	CreatedAt time.Time `json:"created_at"`
}

func (h *NightAuditHandler) ListReports(c *fiber.Ctx) error {
	q := `SELECT id, to_char(audit_date, 'YYYY-MM-DD'), status, closed_by, created_at
	      FROM night_audit_reports
	      WHERE hotel_id = $1`
	args := []interface{}{tenantHotelID(c)}
	argIdx := 2

	if v := c.Query("from"); v != "" {
		q += " AND audit_date >= $" + fmt.Sprintf("%d", argIdx)
		args = append(args, v)
		argIdx++
	}
	if v := c.Query("to"); v != "" {
		q += " AND audit_date <= $" + fmt.Sprintf("%d", argIdx)
		args = append(args, v)
		argIdx++
	}
	q += " ORDER BY audit_date DESC"

	rows, err := tenantPool(c, h.pool).Query(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()

	items := make([]auditReportResponse, 0)
	for rows.Next() {
		var item auditReportResponse
		if err := rows.Scan(
			&item.ID, &item.AuditDate, &item.Status, &item.ClosedBy, &item.CreatedAt,
		); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		items = append(items, item)
	}
	return response.OK(c, items)
}
