package handler

import (
	"fmt"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hotelharmony/api/pkg/response"
)

type HousekeepingHandler struct {
	pool *pgxpool.Pool
}

func NewHousekeepingHandler(pool *pgxpool.Pool) *HousekeepingHandler {
	return &HousekeepingHandler{pool: pool}
}

func (h *HousekeepingHandler) Register(r fiber.Router) {
	r.Get("/housekeeping/tasks", h.ListTasks)
	r.Post("/housekeeping/tasks", h.CreateTask)
	r.Patch("/housekeeping/tasks/:id", h.UpdateTask)
	r.Get("/housekeeping/lost-items", h.ListLostItems)
	r.Post("/housekeeping/lost-items", h.CreateLostItem)
	r.Patch("/housekeeping/lost-items/:id", h.UpdateLostItem)
	r.Get("/housekeeping/linen", h.LinenInventory)
	r.Post("/housekeeping/linen/issue", h.IssueLinen)
	r.Post("/housekeeping/linen/return", h.ReturnLinen)
}

// ---------------------------------------------------------------------------
// Tasks (housekeeping_assignments)
// ---------------------------------------------------------------------------

type createTaskRequest struct {
	RoomID     string `json:"room_id"`
	AssignedTo string `json:"assigned_to,omitempty"`
	TaskType   string `json:"task_type"`
	Priority   string `json:"priority,omitempty"`
	Notes      string `json:"notes,omitempty"`
}

type updateTaskRequest struct {
	Status     string `json:"status,omitempty"`
	AssignedTo string `json:"assigned_to,omitempty"`
	Priority   string `json:"priority,omitempty"`
	Notes      string `json:"notes,omitempty"`
}

type taskResponse struct {
	ID          uuid.UUID  `json:"id"`
	RoomID      uuid.UUID  `json:"room_id"`
	AssignedTo  *string    `json:"assigned_to"`
	TaskType    string     `json:"task_type"`
	Priority    string     `json:"priority"`
	Status      string     `json:"status"`
	Notes       *string    `json:"notes"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	Room        *struct {
		RoomNumber string `json:"room_number"`
		RoomType   string `json:"room_type"`
		Floor      int    `json:"floor"`
	} `json:"room,omitempty"`
	AssignedStaff *struct {
		FullName string `json:"full_name"`
	} `json:"assigned_staff,omitempty"`
}

func (h *HousekeepingHandler) ListTasks(c *fiber.Ctx) error {
	q := `SELECT ha.id, ha.room_id, ha.assigned_to, ha.task_type, ha.priority, ha.status,
	             ha.notes, ha.started_at, ha.completed_at, ha.created_at, ha.updated_at,
	             r.room_number, r.room_type, r.floor, p.full_name
	      FROM housekeeping_assignments ha
	      LEFT JOIN rooms r ON r.id = ha.room_id
	      LEFT JOIN profiles p ON p.user_id = ha.assigned_to
	      WHERE ha.hotel_id = $1`
	args := []interface{}{tenantHotelID(c)}
	argIdx := 2

	for _, f := range []struct{ param, col string }{
		{"status", "ha.status"},
		{"priority", "ha.priority"},
		{"assigned_to", "ha.assigned_to"},
		{"room_id", "ha.room_id"},
	} {
		if v := c.Query(f.param); v != "" {
			q += " AND " + f.col + " = $" + fmt.Sprintf("%d", argIdx)
			args = append(args, v)
			argIdx++
		}
	}
	q += " ORDER BY ha.created_at DESC"

	rows, err := tenantPool(c, h.pool).Query(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()

	items := make([]taskResponse, 0)
	for rows.Next() {
		var item taskResponse
		var roomNumber, roomType *string
		var floor *int
		var staffName *string
		if err := rows.Scan(
			&item.ID, &item.RoomID, &item.AssignedTo, &item.TaskType,
			&item.Priority, &item.Status, &item.Notes,
			&item.StartedAt, &item.CompletedAt, &item.CreatedAt, &item.UpdatedAt,
			&roomNumber, &roomType, &floor, &staffName,
		); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		if roomNumber != nil {
			item.Room = &struct {
				RoomNumber string `json:"room_number"`
				RoomType   string `json:"room_type"`
				Floor      int    `json:"floor"`
			}{
				RoomNumber: *roomNumber,
				RoomType:   safeString(roomType),
				Floor:      safeInt(floor),
			}
		}
		if staffName != nil {
			item.AssignedStaff = &struct {
				FullName string `json:"full_name"`
			}{FullName: *staffName}
		}
		items = append(items, item)
	}
	return response.OK(c, items)
}

func (h *HousekeepingHandler) CreateTask(c *fiber.Ctx) error {
	var req createTaskRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	roomID, err := uuid.Parse(req.RoomID)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid room_id")
	}
	taskID := uuid.New()
	priority := req.Priority
	if priority == "" {
		priority = "normal"
	}
	taskType := req.TaskType
	if taskType == "" {
		taskType = "checkout_clean"
	}
	var assignedTo *string
	if req.AssignedTo != "" {
		assignedTo = &req.AssignedTo
	}
	var notes *string
	if req.Notes != "" {
		notes = &req.Notes
	}
	_, err = tenantPool(c, h.pool).Exec(c.Context(), `
		INSERT INTO housekeeping_assignments
			(id, hotel_id, room_id, assigned_to, task_type, priority, status, notes, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,'pending',$7,now(),now())`,
		taskID, tenantHotelID(c), roomID, assignedTo, taskType, priority, notes,
	)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.Created(c, map[string]interface{}{
		"id":        taskID,
		"room_id":   roomID,
		"task_type": taskType,
		"priority":  priority,
		"status":    "pending",
	})
}

func (h *HousekeepingHandler) UpdateTask(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid task id")
	}
	var req updateTaskRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	setClauses := ""
	args := make([]interface{}, 0, 4)
	argIdx := 1
	if req.Status != "" {
		setClauses += fmt.Sprintf("status = $%d, ", argIdx)
		args = append(args, req.Status)
		argIdx++
	}
	if req.AssignedTo != "" {
		setClauses += fmt.Sprintf("assigned_to = $%d, ", argIdx)
		args = append(args, req.AssignedTo)
		argIdx++
	}
	if req.Priority != "" {
		setClauses += fmt.Sprintf("priority = $%d, ", argIdx)
		args = append(args, req.Priority)
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
	setClauses += fmt.Sprintf("updated_at = $%d", argIdx)
	args = append(args, time.Now().UTC())
	argIdx++
	args = append(args, id)
	args = append(args, tenantHotelID(c))
	q := fmt.Sprintf(
		"UPDATE housekeeping_assignments SET %s WHERE id = $%d AND hotel_id = $%d",
		setClauses, argIdx, argIdx+1,
	)
	_, err = tenantPool(c, h.pool).Exec(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.OK(c, map[string]string{"status": "updated"})
}

// ---------------------------------------------------------------------------
// Lost & Found Items
// ---------------------------------------------------------------------------

type createLostItemRequest struct {
	RoomID      string `json:"room_id"`
	GuestName   string `json:"guest_name,omitempty"`
	ItemName    string `json:"item_name"`
	Description string `json:"description,omitempty"`
	FoundBy     string `json:"found_by,omitempty"`
}

type updateLostItemRequest struct {
	Status string `json:"status"`
}

type lostItemResponse struct {
	ID          uuid.UUID  `json:"id"`
	RoomID      *uuid.UUID `json:"room_id"`
	GuestName   *string    `json:"guest_name"`
	ItemName    string     `json:"item_name"`
	Description *string    `json:"description"`
	FoundBy     *string    `json:"found_by"`
	FoundAt     time.Time  `json:"found_at"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	Room        *struct {
		RoomNumber string `json:"room_number"`
	} `json:"room,omitempty"`
}

func (h *HousekeepingHandler) ListLostItems(c *fiber.Ctx) error {
	q := `SELECT li.id, li.room_id, li.guest_name, li.item_name, li.description,
	             li.found_by, li.found_at, li.status, li.created_at, li.updated_at,
	             r.room_number
	      FROM lost_items li
	      LEFT JOIN rooms r ON r.id = li.room_id
	      WHERE li.hotel_id = $1`
	args := []interface{}{tenantHotelID(c)}
	argIdx := 2
	if v := c.Query("status"); v != "" {
		q += " AND li.status = $" + fmt.Sprintf("%d", argIdx)
		args = append(args, v)
		argIdx++
	}
	q += " ORDER BY li.created_at DESC"

	rows, err := tenantPool(c, h.pool).Query(c.Context(), q, args...)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()

	items := make([]lostItemResponse, 0)
	for rows.Next() {
		var item lostItemResponse
		var roomNumber *string
		if err := rows.Scan(
			&item.ID, &item.RoomID, &item.GuestName, &item.ItemName,
			&item.Description, &item.FoundBy, &item.FoundAt, &item.Status,
			&item.CreatedAt, &item.UpdatedAt, &roomNumber,
		); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		if roomNumber != nil {
			item.Room = &struct {
				RoomNumber string `json:"room_number"`
			}{RoomNumber: *roomNumber}
		}
		items = append(items, item)
	}
	return response.OK(c, items)
}

func (h *HousekeepingHandler) CreateLostItem(c *fiber.Ctx) error {
	var req createLostItemRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if req.ItemName == "" {
		return response.Error(c, fiber.StatusBadRequest, "item_name is required")
	}
	itemID := uuid.New()
	now := time.Now().UTC()
	var roomID *uuid.UUID
	if req.RoomID != "" {
		parsed, err := uuid.Parse(req.RoomID)
		if err != nil {
			return response.Error(c, fiber.StatusBadRequest, "invalid room_id")
		}
		roomID = &parsed
	}
	var guestName, description, foundBy *string
	if req.GuestName != "" {
		guestName = &req.GuestName
	}
	if req.Description != "" {
		description = &req.Description
	}
	if req.FoundBy != "" {
		foundBy = &req.FoundBy
	}
	_, err := tenantPool(c, h.pool).Exec(c.Context(), `
		INSERT INTO lost_items
			(id, hotel_id, room_id, guest_name, item_name, description, found_by, found_at, status, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'lost',$9,$9)`,
		itemID, tenantHotelID(c), roomID, guestName, req.ItemName,
		description, foundBy, now, now,
	)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.Created(c, map[string]interface{}{
		"id":        itemID,
		"item_name": req.ItemName,
		"status":    "lost",
		"found_at":  now,
	})
}

func (h *HousekeepingHandler) UpdateLostItem(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid item id")
	}
	var req updateLostItemRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	validStatus := map[string]bool{"claimed": true, "returned": true, "disposed": true}
	if !validStatus[req.Status] {
		return response.Error(c, fiber.StatusBadRequest, "status must be claimed, returned, or disposed")
	}
	_, err = tenantPool(c, h.pool).Exec(c.Context(), `
		UPDATE lost_items SET status = $1, updated_at = $2
		WHERE id = $3 AND hotel_id = $4`,
		req.Status, time.Now().UTC(), id, tenantHotelID(c),
	)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.OK(c, map[string]string{"status": req.Status})
}

// ---------------------------------------------------------------------------
// Linen
// ---------------------------------------------------------------------------

type linenItemResponse struct {
	ID         uuid.UUID `json:"id"`
	ItemName   string    `json:"item_name"`
	TotalCount int       `json:"total_count"`
	InUse      int       `json:"in_use"`
	InLaundry  int       `json:"in_laundry"`
	Damaged    int       `json:"damaged"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func (h *HousekeepingHandler) LinenInventory(c *fiber.Ctx) error {
	rows, err := tenantPool(c, h.pool).Query(c.Context(), `
		SELECT id, item_name, total_count, in_use, in_laundry, damaged, created_at, updated_at
		FROM linen_inventory
		WHERE hotel_id = $1
		ORDER BY item_name`,
		tenantHotelID(c),
	)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	defer rows.Close()

	items := make([]linenItemResponse, 0)
	for rows.Next() {
		var item linenItemResponse
		if err := rows.Scan(
			&item.ID, &item.ItemName, &item.TotalCount,
			&item.InUse, &item.InLaundry, &item.Damaged,
			&item.CreatedAt, &item.UpdatedAt,
		); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, err.Error())
		}
		items = append(items, item)
	}
	return response.OK(c, items)
}

type linenIssueRequest struct {
	LinenID  string `json:"linen_id"`
	Quantity int    `json:"quantity"`
	IssuedTo string `json:"issued_to"`
	Notes    string `json:"notes,omitempty"`
}

func (h *HousekeepingHandler) IssueLinen(c *fiber.Ctx) error {
	var req linenIssueRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	linenID, err := uuid.Parse(req.LinenID)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid linen_id")
	}
	if req.Quantity <= 0 {
		return response.Error(c, fiber.StatusBadRequest, "quantity must be positive")
	}
	txID := uuid.New()
	_, err = tenantPool(c, h.pool).Exec(c.Context(), `
		INSERT INTO linen_transactions
			(id, hotel_id, linen_id, transaction_type, quantity, issued_to, notes, created_at)
		VALUES ($1,$2,$3,'issue',$4,$5,$6,now())`,
		txID, tenantHotelID(c), linenID, req.Quantity,
		nullableText(req.IssuedTo), nullableText(req.Notes),
	)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	_, err = tenantPool(c, h.pool).Exec(c.Context(), `
		UPDATE linen_inventory
		SET in_use = in_use + $1, total_count = GREATEST(total_count - $1, 0), updated_at = now()
		WHERE id = $2 AND hotel_id = $3`,
		req.Quantity, linenID, tenantHotelID(c),
	)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.Created(c, map[string]interface{}{
		"id":       txID,
		"type":     "issue",
		"quantity": req.Quantity,
	})
}

type linenReturnRequest struct {
	LinenID  string `json:"linen_id"`
	Quantity int    `json:"quantity"`
	Damaged  int    `json:"damaged,omitempty"`
	Notes    string `json:"notes,omitempty"`
}

func (h *HousekeepingHandler) ReturnLinen(c *fiber.Ctx) error {
	var req linenReturnRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	linenID, err := uuid.Parse(req.LinenID)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid linen_id")
	}
	if req.Quantity <= 0 {
		return response.Error(c, fiber.StatusBadRequest, "quantity must be positive")
	}
	txID := uuid.New()
	_, err = tenantPool(c, h.pool).Exec(c.Context(), `
		INSERT INTO linen_transactions
			(id, hotel_id, linen_id, transaction_type, quantity, damaged, notes, created_at)
		VALUES ($1,$2,$3,'return',$4,$5,$6,now())`,
		txID, tenantHotelID(c), linenID, req.Quantity,
		req.Damaged, nullableText(req.Notes),
	)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	cleanQty := req.Quantity - req.Damaged
	if cleanQty < 0 {
		cleanQty = 0
	}
	_, err = tenantPool(c, h.pool).Exec(c.Context(), `
		UPDATE linen_inventory
		SET in_use = GREATEST(in_use - $1, 0),
		    in_laundry = in_laundry + $2,
		    damaged = damaged + $3,
		    updated_at = now()
		WHERE id = $4 AND hotel_id = $5`,
		req.Quantity, cleanQty, req.Damaged, linenID, tenantHotelID(c),
	)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	return response.Created(c, map[string]interface{}{
		"id":       txID,
		"type":     "return",
		"quantity": req.Quantity,
		"damaged":  req.Damaged,
	})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func safeString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func safeInt(i *int) int {
	if i == nil {
		return 0
	}
	return *i
}
