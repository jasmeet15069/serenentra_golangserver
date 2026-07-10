package handler

import (
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hotelharmony/api/pkg/response"
)

// Tables:
//   CREATE TABLE IF NOT EXISTS assets (
//     id uuid PK, hotel_id uuid, name text, category text,
//     location text, serial_number text, purchase_date date,
//     purchase_cost numeric, warranty_until date, status text,
//     notes text, created_at timestamptz
//   );
//   CREATE TABLE IF NOT EXISTS maintenance_schedule (
//     id uuid PK, hotel_id uuid, asset_id uuid, task_name text,
//     frequency text, last_done date, next_due date,
//     assigned_to text, notes text, completed bool,
//     completed_at timestamptz, created_at timestamptz
//   );

type AssetHandler struct {
	pool *pgxpool.Pool
}

func NewAssetHandler(pool *pgxpool.Pool) *AssetHandler {
	return &AssetHandler{pool: pool}
}

func (h *AssetHandler) Register(r fiber.Router) {
	r.Get("/maintenance/assets", h.ListAssets)
	r.Post("/maintenance/assets", h.CreateAsset)
	r.Patch("/maintenance/assets/:id", h.UpdateAsset)
	r.Get("/maintenance/schedule", h.ListSchedule)
	r.Post("/maintenance/schedule", h.CreateScheduleTask)
	r.Patch("/maintenance/schedule/:id/complete", h.CompleteScheduleTask)
}

// ---------------------------------------------------------------------------
// Assets
// ---------------------------------------------------------------------------

type createAssetRequest struct {
	Name          string  `json:"name"`
	Category      string  `json:"category,omitempty"`
	Location      string  `json:"location,omitempty"`
	SerialNumber  string  `json:"serial_number,omitempty"`
	PurchaseDate  string  `json:"purchase_date,omitempty"`
	PurchaseCost  float64 `json:"purchase_cost,omitempty"`
	WarrantyUntil string  `json:"warranty_until,omitempty"`
	Notes         string  `json:"notes,omitempty"`
}

type updateAssetRequest struct {
	Name     string `json:"name,omitempty"`
	Category string `json:"category,omitempty"`
	Location string `json:"location,omitempty"`
	Status   string `json:"status,omitempty"`
	Notes    string `json:"notes,omitempty"`
}

type assetResponse struct {
	ID            uuid.UUID `json:"id"`
	HotelID       uuid.UUID `json:"hotel_id"`
	Name          string    `json:"name"`
	Category      *string   `json:"category"`
	Location      *string   `json:"location"`
	SerialNumber  *string   `json:"serial_number"`
	PurchaseDate  *string   `json:"purchase_date"`
	PurchaseCost  *float64  `json:"purchase_cost"`
	WarrantyUntil *string   `json:"warranty_until"`
	Status        string    `json:"status"`
	Notes         *string   `json:"notes"`
	CreatedAt     time.Time `json:"created_at"`
}

func (h *AssetHandler) ListAssets(c *fiber.Ctx) error {
	q := `SELECT id, hotel_id, name, category, location, serial_number,
	             purchase_date, purchase_cost, warranty_until, status, notes, created_at
	      FROM assets
	      WHERE hotel_id = $1`
	args := []interface{}{tenantHotelID(c)}
	argIdx := 2

	for _, f := range []struct{ param, col string }{
		{"category", "category"},
		{"status", "status"},
		{"location", "location"},
	} {
		if v := c.Query(f.param); v != "" {
			q += " AND " + f.col + " = $" + fmt.Sprintf("%d", argIdx)
			args = append(args, v)
			argIdx++
		}
	}
	q += " ORDER BY name ASC"

	rows, err := tenantPool(c, h.pool).Query(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()

	items := make([]assetResponse, 0)
	for rows.Next() {
		var item assetResponse
		var purchaseDate, warrantyUntil *time.Time
		if err := rows.Scan(
			&item.ID, &item.HotelID, &item.Name, &item.Category,
			&item.Location, &item.SerialNumber,
			&purchaseDate, &item.PurchaseCost, &warrantyUntil,
			&item.Status, &item.Notes, &item.CreatedAt,
		); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		if purchaseDate != nil {
			s := purchaseDate.Format("2006-01-02")
			item.PurchaseDate = &s
		}
		if warrantyUntil != nil {
			s := warrantyUntil.Format("2006-01-02")
			item.WarrantyUntil = &s
		}
		items = append(items, item)
	}
	return response.OK(c, items)
}

func (h *AssetHandler) CreateAsset(c *fiber.Ctx) error {
	var req createAssetRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if req.Name == "" {
		return response.Error(c, fiber.StatusBadRequest, "name is required")
	}
	assetID := uuid.New()
	_, err := tenantPool(c, h.pool).Exec(c.Context(), `
		INSERT INTO assets
			(id, hotel_id, name, category, location, serial_number,
			 purchase_date, purchase_cost, warranty_until, status, notes, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,
		        CASE WHEN $7::text != '' THEN $7::date ELSE NULL END,
		        $8,
		        CASE WHEN $9::text != '' THEN $9::date ELSE NULL END,
		        'active',$10,now())`,
		assetID, tenantHotelID(c), req.Name,
		nullableText(req.Category), nullableText(req.Location),
		nullableText(req.SerialNumber), req.PurchaseDate, req.PurchaseCost,
		req.WarrantyUntil, nullableText(req.Notes),
	)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.Created(c, map[string]interface{}{
		"id":     assetID,
		"name":   req.Name,
		"status": "active",
	})
}

func (h *AssetHandler) UpdateAsset(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid asset id")
	}
	var req updateAssetRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	setClauses := ""
	args := make([]interface{}, 0, 5)
	argIdx := 1
	if req.Name != "" {
		setClauses += fmt.Sprintf("name = $%d, ", argIdx)
		args = append(args, req.Name)
		argIdx++
	}
	if req.Category != "" {
		setClauses += fmt.Sprintf("category = $%d, ", argIdx)
		args = append(args, req.Category)
		argIdx++
	}
	if req.Location != "" {
		setClauses += fmt.Sprintf("location = $%d, ", argIdx)
		args = append(args, req.Location)
		argIdx++
	}
	if req.Status != "" {
		validStatus := map[string]bool{"active": true, "maintenance": true, "retired": true, "broken": true}
		if !validStatus[req.Status] {
			return response.Error(c, fiber.StatusBadRequest, "status must be active, maintenance, retired, or broken")
		}
		setClauses += fmt.Sprintf("status = $%d, ", argIdx)
		args = append(args, req.Status)
		argIdx++
	}
	if req.Notes != "" {
		setClauses += fmt.Sprintf("notes = $%d, ", argIdx)
		args = append(args, req.Notes)
		argIdx++
	}
	if setClauses == "" {
		return response.Error(c, fiber.StatusBadRequest, "no fields to update")
	}
	// Each fragment is appended with a trailing ", "; strip it so the SET list is
	// valid SQL (otherwise "SET col = $1,  WHERE ..." is a syntax error).
	setClauses = strings.TrimSuffix(strings.TrimSpace(setClauses), ",")
	args = append(args, id)
	args = append(args, tenantHotelID(c))
	q := fmt.Sprintf(
		"UPDATE assets SET %s WHERE id = $%d AND hotel_id = $%d",
		setClauses, argIdx, argIdx+1,
	)
	tag, err := tenantPool(c, h.pool).Exec(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	if tag.RowsAffected() == 0 {
		return response.Error(c, fiber.StatusNotFound, "asset not found")
	}
	return response.OK(c, map[string]string{"status": "updated"})
}

// ---------------------------------------------------------------------------
// Preventive Maintenance Schedule
// ---------------------------------------------------------------------------

type createScheduleTaskRequest struct {
	AssetID    string `json:"asset_id"`
	TaskName   string `json:"task_name"`
	Frequency  string `json:"frequency"`
	AssignedTo string `json:"assigned_to,omitempty"`
	Notes      string `json:"notes,omitempty"`
}

type scheduleTaskResponse struct {
	ID          uuid.UUID  `json:"id"`
	HotelID     uuid.UUID  `json:"hotel_id"`
	AssetID     uuid.UUID  `json:"asset_id"`
	TaskName    string     `json:"task_name"`
	Frequency   string     `json:"frequency"`
	LastDone    *string    `json:"last_done"`
	NextDue     *string    `json:"next_due"`
	AssignedTo  *string    `json:"assigned_to"`
	Notes       *string    `json:"notes"`
	Completed   bool       `json:"completed"`
	CompletedAt *time.Time `json:"completed_at"`
	CreatedAt   time.Time  `json:"created_at"`
	AssetName   *string    `json:"asset_name,omitempty"`
}

func (h *AssetHandler) ListSchedule(c *fiber.Ctx) error {
	q := `SELECT ms.id, ms.hotel_id, ms.asset_id, ms.task_name, ms.frequency,
	             ms.last_done, ms.next_due, ms.assigned_to, ms.notes,
	             ms.completed, ms.completed_at, ms.created_at,
	             a.name AS asset_name
	      FROM maintenance_schedule ms
	      LEFT JOIN assets a ON a.id = ms.asset_id
	      WHERE ms.hotel_id = $1`
	args := []interface{}{tenantHotelID(c)}
	argIdx := 2

	for _, f := range []struct{ param, col string }{
		{"completed", "ms.completed"},
		{"asset_id", "ms.asset_id"},
		{"frequency", "ms.frequency"},
	} {
		if v := c.Query(f.param); v != "" {
			q += " AND " + f.col + " = $" + fmt.Sprintf("%d", argIdx)
			args = append(args, v)
			argIdx++
		}
	}
	q += " ORDER BY ms.next_due ASC NULLS LAST, ms.created_at DESC"

	rows, err := tenantPool(c, h.pool).Query(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()

	items := make([]scheduleTaskResponse, 0)
	for rows.Next() {
		var item scheduleTaskResponse
		var lastDone, nextDue *time.Time
		if err := rows.Scan(
			&item.ID, &item.HotelID, &item.AssetID, &item.TaskName,
			&item.Frequency, &lastDone, &nextDue,
			&item.AssignedTo, &item.Notes,
			&item.Completed, &item.CompletedAt, &item.CreatedAt,
			&item.AssetName,
		); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		if lastDone != nil {
			s := lastDone.Format("2006-01-02")
			item.LastDone = &s
		}
		if nextDue != nil {
			s := nextDue.Format("2006-01-02")
			item.NextDue = &s
		}
		items = append(items, item)
	}
	return response.OK(c, items)
}

func (h *AssetHandler) CreateScheduleTask(c *fiber.Ctx) error {
	var req createScheduleTaskRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if req.AssetID == "" || req.TaskName == "" || req.Frequency == "" {
		return response.Error(c, fiber.StatusBadRequest, "asset_id, task_name, and frequency are required")
	}
	assetID, err := uuid.Parse(req.AssetID)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid asset_id")
	}
	taskID := uuid.New()

	var nextDue *time.Time
	now := time.Now().UTC()
	switch req.Frequency {
	case "daily":
		d := now.AddDate(0, 0, 1)
		nextDue = &d
	case "weekly":
		d := now.AddDate(0, 0, 7)
		nextDue = &d
	case "monthly":
		d := now.AddDate(0, 1, 0)
		nextDue = &d
	case "quarterly":
		d := now.AddDate(0, 3, 0)
		nextDue = &d
	case "yearly":
		d := now.AddDate(1, 0, 0)
		nextDue = &d
	}

	_, err = tenantPool(c, h.pool).Exec(c.Context(), `
		INSERT INTO maintenance_schedule
			(id, hotel_id, asset_id, task_name, frequency, next_due,
			 assigned_to, notes, completed, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,false,now())`,
		taskID, tenantHotelID(c), assetID, req.TaskName, req.Frequency,
		nextDue, nullableText(req.AssignedTo), nullableText(req.Notes),
	)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.Created(c, map[string]interface{}{
		"id":        taskID,
		"task_name": req.TaskName,
		"frequency": req.Frequency,
		"completed": false,
	})
}

func (h *AssetHandler) CompleteScheduleTask(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid schedule task id")
	}

	now := time.Now().UTC()
	tag, err := tenantPool(c, h.pool).Exec(c.Context(), `
		UPDATE maintenance_schedule
		SET completed = true,
		    completed_at = $1,
		    last_done = CURRENT_DATE,
		    next_due = CASE
		        WHEN frequency = 'daily'   THEN CURRENT_DATE + 1
		        WHEN frequency = 'weekly'  THEN CURRENT_DATE + 7
		        WHEN frequency = 'monthly' THEN CURRENT_DATE + INTERVAL '1 month'
		        WHEN frequency = 'quarterly' THEN CURRENT_DATE + INTERVAL '3 months'
		        WHEN frequency = 'yearly'  THEN CURRENT_DATE + INTERVAL '1 year'
		        ELSE CURRENT_DATE + 30
		    END
		WHERE id = $2 AND hotel_id = $3`,
		now, id, tenantHotelID(c),
	)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	if tag.RowsAffected() == 0 {
		return response.Error(c, fiber.StatusNotFound, "schedule task not found")
	}
	return response.OK(c, map[string]interface{}{
		"status":       "completed",
		"completed_at": now,
	})
}
