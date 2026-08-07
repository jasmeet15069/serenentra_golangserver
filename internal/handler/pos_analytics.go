package handler

import (
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/hotelharmony/api/pkg/response"
)

// Restaurant analytics for the Restaurant Management screen.
//
// The two charts there — hourly covers and the weekly revenue trend — were
// rendering hardcoded arrays unconditionally, with no check for whether the
// tenant was live. Signed into a real property they showed invented covers and
// invented revenue under the headings "Today" and "Weekly Revenue Trend", which
// is worse than showing nothing: a fabricated number that looks authoritative
// gets acted on.
//
// Both series come from the same records the rest of the module already writes,
// so the chart and the till can never disagree: covers from dining_sessions,
// revenue from settled bills.

type hourlyCover struct {
	Hour   int `json:"hour"`
	Covers int `json:"covers"`
}

type dailyRevenue struct {
	Day     string  `json:"day"`
	Date    string  `json:"date"`
	Revenue float64 `json:"revenue"`
}

type posAnalyticsResponse struct {
	HourlyCovers  []hourlyCover  `json:"hourly_covers"`
	WeeklyRevenue []dailyRevenue `json:"weekly_revenue"`
	// True when the tenant has no restaurant activity at all, so the screen can
	// say "no data yet" rather than draw a flat line that reads as a bad day.
	Empty bool `json:"empty"`
}

func (h *POSHandler) Analytics(c *fiber.Ctx) error {
	hotelID := h.hotelID(c)
	ctx := c.Context()
	db := h.db(c)

	// --- Hourly covers, today ---
	//
	// generate_series so every trading hour appears even with no covers — a gap
	// in the middle of the day is information, and a bar chart that silently
	// skips it misreads as a shorter day.
	out := posAnalyticsResponse{
		HourlyCovers:  make([]hourlyCover, 0, 24),
		WeeklyRevenue: make([]dailyRevenue, 0, 7),
	}

	rows, err := db.Query(ctx, `
		SELECT h::int AS hour,
		       COALESCE(SUM(ds.covers), 0)::int AS covers
		  FROM generate_series(0, 23) AS h
		  LEFT JOIN dining_sessions ds
		         ON ds.hotel_id = $1
		        AND ds.opened_at::date = CURRENT_DATE
		        AND EXTRACT(HOUR FROM ds.opened_at) = h
		 GROUP BY h
		 ORDER BY h`, hotelID)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to read hourly covers")
	}
	totalCovers := 0
	for rows.Next() {
		var hc hourlyCover
		if err := rows.Scan(&hc.Hour, &hc.Covers); err != nil {
			rows.Close()
			return response.Error(c, fiber.StatusInternalServerError, "failed to read hourly covers")
		}
		totalCovers += hc.Covers
		out.HourlyCovers = append(out.HourlyCovers, hc)
	}
	rows.Close()

	// --- Revenue, last 7 days ---
	//
	// Settled bills only. An open or finalised-but-unpaid bill is not revenue,
	// and counting it would overstate the week and disagree with the ledger.
	rows, err = db.Query(ctx, `
		SELECT d::date AS day,
		       COALESCE(SUM(b.total_amount), 0)::numeric AS revenue
		  FROM generate_series(CURRENT_DATE - 6, CURRENT_DATE, '1 day') AS d
		  LEFT JOIN bills b
		         ON b.hotel_id = $1
		        AND b.status = 'paid'
		        AND b.paid_at::date = d::date
		 GROUP BY d
		 ORDER BY d`, hotelID)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to read weekly revenue")
	}
	defer rows.Close()

	totalRevenue := 0.0
	for rows.Next() {
		var day time.Time
		var rev float64
		if err := rows.Scan(&day, &rev); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, "failed to read weekly revenue")
		}
		totalRevenue += rev
		out.WeeklyRevenue = append(out.WeeklyRevenue, dailyRevenue{
			Day:     day.Format("Mon"),
			Date:    day.Format("2006-01-02"),
			Revenue: round2(rev),
		})
	}

	out.Empty = totalCovers == 0 && totalRevenue == 0
	return response.OK(c, out)
}
