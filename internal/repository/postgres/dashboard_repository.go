package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/hotelharmony/api/internal/domain"
)

// DashboardRepository runs the single aggregated query that powers
// the dashboard stats endpoint. All counters are fetched in one round
// trip using PostgreSQL CTEs to avoid 10 separate queries.
//
// Every query here is scoped to the requesting hotel_id. This matters even
// though most tenants get their own dedicated database (whose tables
// physically hold only that tenant's rows): tenants still in 'shared' or
// 'provisioned' isolation mode (see tenant.Manager) share the primary
// database, and an unscoped query would sum/count every such tenant's rooms,
// orders, complaints, revenue, stock, staff and guest activity into one
// tenant's dashboard.
type DashboardRepository interface {
	GetStats(ctx context.Context, hotelID uuid.UUID) (*domain.DashboardStats, error)
	GetChartData(ctx context.Context, hotelID uuid.UUID) (*domain.DashboardChartData, error)
}

type dashboardRepository struct {
	db *DB
}

func NewDashboardRepository(db *DB) DashboardRepository {
	return &dashboardRepository{db: db}
}

// GetStats executes a single multi-CTE query instead of the original 10
// sequential SQLite queries. On PostgreSQL with proper indexes this runs
// in < 5 ms even at thousands of rooms.
func (r *dashboardRepository) GetStats(ctx context.Context, hotelID uuid.UUID) (*domain.DashboardStats, error) {
	today := time.Now().UTC().Format("2006-01-02")
	const q = `
		WITH
		  room_counts AS (
		    SELECT
		      COUNT(*)                                          AS total_rooms,
		      COUNT(*) FILTER (WHERE status = 'occupied')      AS occupied,
		      COUNT(*) FILTER (WHERE status = 'available')     AS available
		    FROM rooms WHERE hotel_id = $2
		  ),
		  order_count AS (
		    SELECT COUNT(*) AS active_orders
		    FROM orders
		    WHERE hotel_id = $2 AND status IN ('pending','preparing','ready')
		  ),
		  complaint_count AS (
		    SELECT COUNT(*) AS pending_complaints
		    FROM complaints
		    WHERE hotel_id = $2 AND status != 'resolved'
		  ),
		  revenue AS (
		    SELECT COALESCE(SUM(amount), 0) AS revenue_today
		    FROM payments
		    WHERE hotel_id = $2 AND status = 'completed'
		      AND created_at::date = $1::date
		  ),
		  stock AS (
		    SELECT COUNT(*) AS low_stock
		    FROM inventory_items
		    WHERE hotel_id = $2 AND current_stock <= min_stock
		  ),
		  staff AS (
		    SELECT COUNT(*) AS clocked_in
		    FROM staff_shifts
		    WHERE hotel_id = $2 AND clock_out IS NULL
		  ),
		  arrivals AS (
		    SELECT COUNT(*) AS checking_in
		    FROM guest_stays
		    WHERE hotel_id = $2 AND check_in_date::date = $1::date
		  ),
		  departures AS (
		    SELECT COUNT(*) AS checking_out
		    FROM guest_stays
		    WHERE hotel_id = $2 AND check_out_date::date = $1::date
		  )
		SELECT
		  rc.total_rooms, rc.occupied, rc.available,
		  oc.active_orders, cc.pending_complaints,
		  rv.revenue_today, sk.low_stock, sf.clocked_in,
		  ar.checking_in, dp.checking_out
		FROM room_counts rc, order_count oc, complaint_count cc,
		     revenue rv, stock sk, staff sf, arrivals ar, departures dp`

	var (
		totalRooms   int
		occupied     int
		available    int
		activeOrders int
		pendComp     int
		revenueToday float64
		lowStock     int
		staffIn      int
		checkingIn   int
		checkingOut  int
	)

	err := poolFromContext(ctx, r.db.Pool).QueryRow(ctx, q, today, hotelID).Scan(
		&totalRooms, &occupied, &available,
		&activeOrders, &pendComp,
		&revenueToday, &lowStock, &staffIn,
		&checkingIn, &checkingOut,
	)
	if err != nil {
		return nil, fmt.Errorf("dashboardRepo.GetStats: %w", err)
	}

	if totalRooms == 0 {
		totalRooms = 1
	}

	return &domain.DashboardStats{
		OccupancyRate:          float64(occupied) / float64(totalRooms),
		RoomsAvailable:         available,
		RoomsOccupied:          occupied,
		ActiveOrders:           activeOrders,
		PendingComplaints:      pendComp,
		RevenueToday:           revenueToday,
		LowStockItems:          lowStock,
		StaffClockedIn:         staffIn,
		GuestsCheckingInToday:  checkingIn,
		GuestsCheckingOutToday: checkingOut,
	}, nil
}

type revenueTrendRow struct {
	Date  string  `json:"date"`
	Room  float64 `json:"room"`
	FnB   float64 `json:"fnb"`
	Other float64 `json:"other"`
}

type occupancyTrendRow struct {
	Date      string  `json:"date"`
	Occupied  int     `json:"occupied"`
	Available int     `json:"available"`
	Rate      float64 `json:"rate"`
}

type departmentRevenueRow struct {
	Department string  `json:"department"`
	Current    float64 `json:"current"`
	Previous   float64 `json:"previous"`
}

type arrivalRow struct {
	GuestName string `json:"guest_name"`
	Room      string `json:"room"`
	Status    string `json:"status"`
}

type departureRow struct {
	GuestName string `json:"guest_name"`
	Room      string `json:"room"`
	Status    string `json:"status"`
}

type pendingPaymentRow struct {
	GuestName string  `json:"guest_name"`
	Amount    float64 `json:"amount"`
	DueDate   string  `json:"due_date"`
	Status    string  `json:"status"`
}

type activityRow struct {
	Action    string `json:"action"`
	User      string `json:"user"`
	Details   string `json:"details"`
	CreatedAt string `json:"created_at"`
}

func (r *dashboardRepository) GetChartData(ctx context.Context, hotelID uuid.UUID) (*domain.DashboardChartData, error) {
	today := time.Now().UTC()
	pool := poolFromContext(ctx, r.db.Pool)

	// Date columns are explicitly cast to text because they scan into a Go
	// string field below — scanning a native `date`/`timestamp` OID into
	// *string fails under pgx's cached-statement protocol, and that scan
	// error was silently swallowed by the `if err == nil` row loops, which is
	// exactly how this entire chart section went dark: the query succeeded,
	// every row's Scan quietly failed, and the API returned null with no
	// trace anywhere. (generate_series' '1 day' step makes `d` a timestamp,
	// not a date, so it needs ::date::text — plain ::text would print the
	// full "2026-07-16 00:00:00+00" timestamp instead of a clean date.)
	const revenueTrendQ = `
		SELECT d::date::text AS date,
			COALESCE(SUM(amount) FILTER (WHERE category = 'room'), 0) AS room,
			COALESCE(SUM(amount) FILTER (WHERE category = 'fnb'), 0) AS fnb,
			COALESCE(SUM(amount) FILTER (WHERE category NOT IN ('room','fnb') OR category IS NULL), 0) AS other
		FROM generate_series($1::date - 6, $1::date, '1 day') d
		LEFT JOIN payments ON payments.created_at::date = d AND payments.status = 'completed' AND payments.hotel_id = $2
		GROUP BY d ORDER BY d`

	const occupancyTrendQ = `
		SELECT d::date::text AS date,
			COALESCE(rc.occupied, 0) AS occupied,
			COALESCE(rc.available, 0) AS available
		FROM generate_series($1::date - 6, $1::date, '1 day') d
		LEFT JOIN LATERAL (
			SELECT COUNT(*) FILTER (WHERE status = 'occupied') AS occupied,
				COUNT(*) FILTER (WHERE status = 'available') AS available
			FROM rooms WHERE hotel_id = $2
		) rc ON true
		GROUP BY d, rc.occupied, rc.available ORDER BY d`

	// guest_stays carries guest_name directly (no profiles join needed) and has
	// no status column — status is derived from whether the guest has actually
	// checked in/out yet.
	const arrivalsQ = `
		SELECT gs.guest_name, r.room_number,
			CASE WHEN gs.actual_check_in IS NOT NULL THEN 'checked_in' ELSE 'expected' END
		FROM guest_stays gs
		JOIN rooms r ON r.id = gs.room_id
		WHERE gs.hotel_id = $2 AND gs.check_in_date::date = $1::date
		ORDER BY gs.created_at LIMIT 10`

	const departuresQ = `
		SELECT gs.guest_name, r.room_number,
			CASE WHEN gs.actual_check_out IS NOT NULL THEN 'checked_out' ELSE 'expected' END
		FROM guest_stays gs
		JOIN rooms r ON r.id = gs.room_id
		WHERE gs.hotel_id = $2 AND gs.check_out_date::date = $1::date
		ORDER BY gs.created_at LIMIT 10`

	const pendingPaymentsQ = `
		SELECT COALESCE(gs.guest_name, 'Guest'), pay.amount, pay.created_at::date::text, pay.status
		FROM payments pay
		LEFT JOIN guest_stays gs ON gs.id = pay.guest_stay_id
		WHERE pay.hotel_id = $1 AND pay.status IN ('pending', 'overdue')
		ORDER BY pay.created_at DESC LIMIT 10`

	// audit_logs has no user_name column, only user_id — resolve the display
	// name via profiles, falling back to 'System' for system-triggered entries.
	const activityQ = `
		SELECT al.action, COALESCE(p.full_name, 'System'), al.table_name, al.created_at::text
		FROM audit_logs al
		LEFT JOIN profiles p ON p.user_id = al.user_id
		WHERE al.hotel_id = $1
		ORDER BY al.created_at DESC LIMIT 15`

	const deptRevenueQ = `
		SELECT category, COALESCE(SUM(amount), 0) AS total
		FROM payments
		WHERE hotel_id = $2 AND status = 'completed' AND created_at >= $1
		GROUP BY category ORDER BY total DESC`

	dateStr := today.Format("2006-01-02")
	monthStart := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	prevMonthStart := monthStart.AddDate(0, -1, 0)

	cd := &domain.DashboardChartData{}

	// warn logs a query failure that was previously swallowed silently — every
	// chart section used to fail invisibly (bare `_` on the error), so a broken
	// query looked identical to "no data yet" in the API response.
	warn := func(section string, err error) {
		if err != nil && r.db.logger != nil {
			r.db.logger.Warn("dashboardRepo.GetChartData: query failed", zap.String("section", section), zap.Error(err))
		}
	}

	// Revenue trend
	revRows, err := pool.Query(ctx, revenueTrendQ, dateStr, hotelID)
	warn("revenue_trend", err)
	if revRows != nil {
		defer revRows.Close()
		for revRows.Next() {
			var row revenueTrendRow
			if err := revRows.Scan(&row.Date, &row.Room, &row.FnB, &row.Other); err == nil {
				cd.RevenueTrend = append(cd.RevenueTrend, domain.ChartRevenuePoint{
					Date: row.Date, Room: row.Room, FnB: row.FnB, Other: row.Other,
				})
			} else {
				warn("revenue_trend.scan", err)
			}
		}
	}

	// Occupancy trend
	occRows, err := pool.Query(ctx, occupancyTrendQ, dateStr, hotelID)
	warn("occupancy_trend", err)
	if occRows != nil {
		defer occRows.Close()
		totalRooms := 0
		_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM rooms WHERE hotel_id = $1`, hotelID).Scan(&totalRooms)
		if totalRooms == 0 {
			totalRooms = 1
		}
		for occRows.Next() {
			var row occupancyTrendRow
			if err := occRows.Scan(&row.Date, &row.Occupied, &row.Available); err == nil {
				row.Rate = float64(row.Occupied) / float64(totalRooms) * 100
				cd.OccupancyTrend = append(cd.OccupancyTrend, domain.ChartOccupancyPoint{
					Date: row.Date, Occupied: row.Occupied, Available: row.Available, Rate: row.Rate,
				})
			} else {
				warn("occupancy_trend.scan", err)
			}
		}
	}

	// Department revenue (current month)
	deptCurrent := make(map[string]float64)
	deptRows, err := pool.Query(ctx, deptRevenueQ, monthStart, hotelID)
	warn("department_revenue.current", err)
	if deptRows != nil {
		defer deptRows.Close()
		for deptRows.Next() {
			var cat string
			var total float64
			if err := deptRows.Scan(&cat, &total); err == nil {
				deptCurrent[cat] = total
			} else {
				warn("department_revenue.current.scan", err)
			}
		}
	}

	// Department revenue (previous month)
	deptPrev := make(map[string]float64)
	deptRows2, err := pool.Query(ctx, deptRevenueQ, prevMonthStart, hotelID)
	warn("department_revenue.previous", err)
	if deptRows2 != nil {
		defer deptRows2.Close()
		for deptRows2.Next() {
			var cat string
			var total float64
			if err := deptRows2.Scan(&cat, &total); err == nil {
				deptPrev[cat] = total
			} else {
				warn("department_revenue.previous.scan", err)
			}
		}
	}

	allCats := map[string]bool{}
	for k := range deptCurrent {
		allCats[k] = true
	}
	for k := range deptPrev {
		allCats[k] = true
	}
	for cat := range allCats {
		cd.DepartmentRevenue = append(cd.DepartmentRevenue, domain.DeptRevenueItem{
			Department: cat, Current: deptCurrent[cat], Previous: deptPrev[cat],
		})
	}

	// Arrivals today
	arrRows, err := pool.Query(ctx, arrivalsQ, dateStr, hotelID)
	warn("arrivals_today", err)
	if arrRows != nil {
		defer arrRows.Close()
		for arrRows.Next() {
			var row arrivalRow
			if err := arrRows.Scan(&row.GuestName, &row.Room, &row.Status); err == nil {
				cd.ArrivalsToday = append(cd.ArrivalsToday, domain.GuestStayItem{
					GuestName: row.GuestName, Room: row.Room, Status: row.Status,
				})
			} else {
				warn("arrivals_today.scan", err)
			}
		}
	}

	// Departures today
	depRows, err := pool.Query(ctx, departuresQ, dateStr, hotelID)
	warn("departures_today", err)
	if depRows != nil {
		defer depRows.Close()
		for depRows.Next() {
			var row departureRow
			if err := depRows.Scan(&row.GuestName, &row.Room, &row.Status); err == nil {
				cd.DeparturesToday = append(cd.DeparturesToday, domain.GuestStayItem{
					GuestName: row.GuestName, Room: row.Room, Status: row.Status,
				})
			} else {
				warn("departures_today.scan", err)
			}
		}
	}

	// Pending payments
	payRows, err := pool.Query(ctx, pendingPaymentsQ, hotelID)
	warn("pending_payments", err)
	if payRows != nil {
		defer payRows.Close()
		for payRows.Next() {
			var row pendingPaymentRow
			if err := payRows.Scan(&row.GuestName, &row.Amount, &row.DueDate, &row.Status); err == nil {
				cd.PendingPayments = append(cd.PendingPayments, domain.PendingPaymentItem{
					GuestName: row.GuestName, Amount: row.Amount, DueDate: row.DueDate, Status: row.Status,
				})
			} else {
				warn("pending_payments.scan", err)
			}
		}
	}

	// Recent activity
	actRows, err := pool.Query(ctx, activityQ, hotelID)
	warn("recent_activity", err)
	if actRows != nil {
		defer actRows.Close()
		for actRows.Next() {
			var row activityRow
			if err := actRows.Scan(&row.Action, &row.User, &row.Details, &row.CreatedAt); err == nil {
				cd.RecentActivity = append(cd.RecentActivity, domain.ActivityItem{
					Action: row.Action, User: row.User, Details: row.Details, CreatedAt: row.CreatedAt,
				})
			} else {
				warn("recent_activity.scan", err)
			}
		}
	}

	return cd, nil
}
