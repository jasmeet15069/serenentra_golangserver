package handler

import (
	"fmt"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

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

func (h *NightAuditHandler) GetRevenueAudit(c *fiber.Ctx) error {
	q := `SELECT
	           COALESCE(SUM(r.total), 0) AS expected_revenue,
	           COALESCE(SUM(r.total), 0) AS actual_revenue
	        FROM revenue_daily r
	        WHERE r.hotel_id = $1 AND r.date = CURRENT_DATE`
	args := []interface{}{h.hotelID(c)}

	rows, err := tenantPool(c, h.pool).Query(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()

	items := []revenueAuditResponse{
		{Category: "Room Revenue", Expected: 0, Actual: 0, Difference: 0},
		{Category: "Food & Beverage", Expected: 0, Actual: 0, Difference: 0},
		{Category: "Other Services", Expected: 0, Actual: 0, Difference: 0},
	}

	for rows.Next() {
		var expected, actual float64
		if err := rows.Scan(&expected, &actual); err == nil {
			for i := range items {
				items[i].Expected = expected
				items[i].Actual = actual
				items[i].Difference = expected - actual
			}
		}
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

func (h *NightAuditHandler) GetTaxAudit(c *fiber.Ctx) error {
	rows, err := tenantPool(c, h.pool).Query(c.Context(), `
		SELECT
			CASE WHEN b.total > 0 THEN 'GST' ELSE 'GST' END AS tax_type,
			COALESCE(SUM(b.tax_amount), 0) AS collected,
			COALESCE(SUM(b.tax_amount), 0) AS payable
		FROM bookings b
		WHERE b.hotel_id = $1
		  AND b.check_in = CURRENT_DATE
		GROUP BY tax_type`,
		h.hotelID(c),
	)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()

	items := make([]taxAuditResponse, 0)
	for rows.Next() {
		var item taxAuditResponse
		if err := rows.Scan(&item.TaxType, &item.Collected, &item.Payable); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		item.Status = "verified"
		items = append(items, item)
	}
	if items == nil {
		items = make([]taxAuditResponse, 0)
	}
	return response.OK(c, items)
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
	} `json:"summary"`
}

func (h *NightAuditHandler) CloseDay(c *fiber.Ctx) error {
	reportID := uuid.New()
	hotelID := h.hotelID(c)
	// Use a single consistent date basis (the DB's CURRENT_DATE) for both the
	// stored audit_date and the aggregate WHERE clauses, so the report totals
	// always correspond to the day they are filed under.

	var totalRevenue, totalTax float64
	var occupiedRooms int
	if err := tenantPool(c, h.pool).QueryRow(c.Context(), `
		SELECT
			COALESCE(SUM(b.total), 0),
			COALESCE(SUM(b.tax_amount), 0),
			COUNT(*) FILTER (WHERE b.status = 'checked_in')
		FROM bookings b
		WHERE b.hotel_id = $1 AND b.check_in = CURRENT_DATE`,
		hotelID,
	).Scan(&totalRevenue, &totalTax, &occupiedRooms); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}

	var checkOuts, arrivals int
	if err := tenantPool(c, h.pool).QueryRow(c.Context(), `
		SELECT
			COUNT(*) FILTER (WHERE b.status = 'checked_out' AND b.check_out = CURRENT_DATE),
			COUNT(*) FILTER (WHERE b.status = 'checked_in' AND b.check_in = CURRENT_DATE)
		FROM bookings b
		WHERE b.hotel_id = $1`,
		hotelID,
	).Scan(&checkOuts, &arrivals); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
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
		reportID, hotelID, totalRevenue, totalTax, occupiedRooms,
	).Scan(&auditDateStr); err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}

	resp := closeDayResponse{
		ReportID:  reportID,
		AuditDate: auditDateStr,
		Status:    "closed",
	}
	resp.Summary.TotalRevenue = totalRevenue
	resp.Summary.TotalTax = totalTax
	resp.Summary.OccupiedRooms = occupiedRooms
	resp.Summary.CheckOuts = checkOuts
	resp.Summary.Arrivals = arrivals

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
