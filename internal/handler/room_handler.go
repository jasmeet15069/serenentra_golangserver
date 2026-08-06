package handler

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/hotelharmony/api/internal/cache"
	"github.com/hotelharmony/api/internal/domain"
	"github.com/hotelharmony/api/internal/repository/postgres"
	"github.com/hotelharmony/api/pkg/response"
)

type RoomHandler struct {
	rooms postgres.RoomRepository
	cache cache.Cache
}

func NewRoomHandler(rooms postgres.RoomRepository, c cache.Cache) *RoomHandler {
	return &RoomHandler{rooms: rooms, cache: c}
}

func (h *RoomHandler) Register(r fiber.Router) {
	r.Get("/rooms", h.List)
	// Registered before any /rooms/:id route so "available" is never taken as an id.
	r.Get("/rooms/available", h.ListAvailable)
	r.Post("/rooms", h.Create)
	r.Patch("/rooms/:id/status", h.UpdateStatus)
	r.Patch("/rooms/:id", h.Update)
	r.Delete("/rooms/:id", h.Delete)
}

// Update patches scalar fields of a room (room_number, room_type, floor,
// capacity, price_per_night, status).
func (h *RoomHandler) Update(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid room id")
	}
	var fields map[string]interface{}
	if err := json.Unmarshal(c.Body(), &fields); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	room, err := h.rooms.UpdateRoom(c.Context(), tenantHotelID(c), id, fields)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	h.invalidateRooms(c)
	return response.OK(c, room)
}

func (h *RoomHandler) Delete(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid room id")
	}
	if err := h.rooms.DeleteRoom(c.Context(), tenantHotelID(c), id); err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	h.invalidateRooms(c)
	return response.OK(c, map[string]string{"status": "deleted"})
}

func (h *RoomHandler) List(c *fiber.Ctx) error {
	var status *domain.RoomStatus
	statusKey := "all"
	if raw := c.Query("status"); raw != "" {
		s := domain.RoomStatus(raw)
		status = &s
		statusKey = raw
	}
	cacheKey := cache.KeyRoomList(tenantHotelID(c).String(), statusKey)
	if cached, err := h.cache.Get(c.Context(), cacheKey); err == nil {
		var rooms []domain.Room
		if json.Unmarshal([]byte(cached), &rooms) == nil {
			c.Set("X-Cache", "HIT")
			return response.OK(c, rooms)
		}
	}
	rooms, err := h.rooms.ListRooms(c.Context(), tenantHotelID(c), status)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	if b, err := json.Marshal(rooms); err == nil {
		_ = h.cache.Set(c.Context(), cacheKey, string(b), cache.TTLRoomList)
	}
	c.Set("X-Cache", "MISS")
	return response.OK(c, rooms)
}

// ListAvailable (GET /api/rooms/available?check_in=&check_out=) returns the
// rooms bookable for a date range.
//
// The booking wizard previously picked rooms out of GET /rooms by status ==
// "available", which is a question about right now rather than about the
// requested dates: a hotel whose rooms were all occupied or being cleaned
// offered nothing to book, even for months ahead, and the wizard could not be
// completed. Dates are required precisely so that mistake cannot recur here.
func (h *RoomHandler) ListAvailable(c *fiber.Ctx) error {
	const layout = "2006-01-02"
	rawIn, rawOut := c.Query("check_in"), c.Query("check_out")
	if rawIn == "" || rawOut == "" {
		return response.Error(c, fiber.StatusBadRequest, "check_in and check_out are required (YYYY-MM-DD)")
	}
	checkIn, err := time.Parse(layout, rawIn)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid check_in, expected YYYY-MM-DD")
	}
	checkOut, err := time.Parse(layout, rawOut)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid check_out, expected YYYY-MM-DD")
	}
	if !checkOut.After(checkIn) {
		return response.Error(c, fiber.StatusBadRequest, "check_out must be after check_in")
	}

	rooms, err := h.rooms.ListRoomsAvailableBetween(c.Context(), tenantHotelID(c), checkIn, checkOut)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, err.Error())
	}
	// Deliberately uncached: availability changes on every booking, and a stale
	// hit here would offer a room that has just been taken.
	return response.OK(c, rooms)
}

type createRoomRequest struct {
	RoomNumber    string   `json:"room_number"`
	RoomType      string   `json:"room_type"`
	Floor         int      `json:"floor"`
	Capacity      int      `json:"capacity"`
	PricePerNight float64  `json:"price_per_night"`
	Status        string   `json:"status"`
	Amenities     []string `json:"amenities"`
}

func (h *RoomHandler) Create(c *fiber.Ctx) error {
	var req createRoomRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	status := domain.RoomStatus(req.Status)
	if status == "" {
		status = domain.RoomStatusAvailable
	}
	room, err := h.rooms.CreateRoom(c.Context(), tenantHotelID(c), &domain.Room{
		RoomNumber:    req.RoomNumber,
		RoomType:      req.RoomType,
		Floor:         req.Floor,
		Capacity:      req.Capacity,
		PricePerNight: req.PricePerNight,
		Status:        status,
		Amenities:     req.Amenities,
	})
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	h.invalidateRooms(c)
	return response.Created(c, room)
}

type updateRoomStatusRequest struct {
	Status string `json:"status"`
}

func (h *RoomHandler) UpdateStatus(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid room id")
	}
	var req updateRoomStatusRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	status := domain.RoomStatus(req.Status)
	if !status.Valid() {
		return response.Error(c, fiber.StatusBadRequest,
			"invalid status, expected one of: "+strings.Join(domain.RoomStatusValues(), ", "))
	}
	if err := h.rooms.UpdateRoomStatus(c.Context(), tenantHotelID(c), id, status); err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			return response.Error(c, fiber.StatusNotFound, "room not found")
		}
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	h.invalidateRooms(c)
	return response.OK(c, map[string]string{"status": "updated"})
}

func (h *RoomHandler) invalidateRooms(c *fiber.Ctx) {
	hid := tenantHotelID(c).String()
	_ = h.cache.Delete(
		c.Context(),
		cache.KeyDashboardStats(hid),
		cache.KeyRoomList(hid, "all"),
		cache.KeyRoomList(hid, string(domain.RoomStatusAvailable)),
		cache.KeyRoomList(hid, string(domain.RoomStatusOccupied)),
		cache.KeyRoomList(hid, string(domain.RoomStatusCleaning)),
		cache.KeyRoomList(hid, string(domain.RoomStatusMaintenance)),
	)
}
