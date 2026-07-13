package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/hotelharmony/api/internal/domain"
)

// RoomRepository handles all room and guest-stay persistence.
// All methods take an explicit hotelID so every query is row-scoped to the
// caller's tenant (Phase 4c). Callers pass the JWT's hotel_id; the guest-checkout
// payment path passes DemoHotelID (its single, public tenant).
type RoomRepository interface {
	ListRooms(ctx context.Context, hotelID uuid.UUID, status *domain.RoomStatus) ([]domain.Room, error)
	FindRoomByID(ctx context.Context, hotelID, id uuid.UUID) (*domain.Room, error)
	FindAvailableRoom(ctx context.Context, hotelID uuid.UUID, roomType *string) (*domain.Room, error)
	CreateRoom(ctx context.Context, hotelID uuid.UUID, r *domain.Room) (*domain.Room, error)
	UpdateRoom(ctx context.Context, hotelID, id uuid.UUID, fields map[string]interface{}) (*domain.Room, error)
	DeleteRoom(ctx context.Context, hotelID, id uuid.UUID) error
	UpdateRoomStatus(ctx context.Context, hotelID, id uuid.UUID, status domain.RoomStatus) error

	CreateStay(ctx context.Context, hotelID uuid.UUID, s *domain.GuestStay) (*domain.GuestStay, error)
	FindStayByID(ctx context.Context, hotelID, id uuid.UUID) (*domain.GuestStay, error)
	ListStays(ctx context.Context, hotelID uuid.UUID, filters map[string]interface{}) ([]domain.GuestStay, error)
	UpdateStay(ctx context.Context, hotelID, id uuid.UUID, fields map[string]interface{}) error
	DeleteStay(ctx context.Context, hotelID, id uuid.UUID) error

	SmartCheckinLookup(ctx context.Context, hotelID uuid.UUID, guestName, phone string) (*domain.GuestStay, error)
}

type roomRepository struct {
	db *DB
}

func NewRoomRepository(db *DB) RoomRepository {
	return &roomRepository{db: db}
}

// Rooms

func (r *roomRepository) ListRooms(ctx context.Context, hotelID uuid.UUID, status *domain.RoomStatus) ([]domain.Room, error) {
	q := `SELECT id, hotel_id, room_number, room_type, floor, capacity, price_per_night,
		         status, amenities, created_at, updated_at
		  FROM rooms WHERE hotel_id = $1`
	args := []interface{}{hotelID}
	if status != nil {
		q += " AND status = $2"
		args = append(args, *status)
	}
	q += " ORDER BY floor, room_number"

	rows, err := poolFromContext(ctx, r.db.Pool).Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("roomRepo.ListRooms: %w", err)
	}
	defer rows.Close()
	return scanRooms(rows)
}

func (r *roomRepository) FindRoomByID(ctx context.Context, hotelID, id uuid.UUID) (*domain.Room, error) {
	const q = `SELECT id, hotel_id, room_number, room_type, floor, capacity, price_per_night,
		              status, amenities, created_at, updated_at
		       FROM rooms WHERE hotel_id = $1 AND id = $2`
	rows, err := poolFromContext(ctx, r.db.Pool).Query(ctx, q, hotelID, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rooms, err := scanRooms(rows)
	if err != nil {
		return nil, err
	}
	if len(rooms) == 0 {
		return nil, ErrNotFound
	}
	return &rooms[0], nil
}

// FindAvailableRoom returns the cheapest available room of the given type,
// or any available room if roomType is nil. Used by smart check-in upgrade.
func (r *roomRepository) FindAvailableRoom(ctx context.Context, hotelID uuid.UUID, roomType *string) (*domain.Room, error) {
	q := `SELECT id, hotel_id, room_number, room_type, floor, capacity, price_per_night,
		         status, amenities, created_at, updated_at
		  FROM rooms WHERE hotel_id = $1 AND status = 'available'`
	args := []interface{}{hotelID}
	if roomType != nil {
		q += " AND LOWER(room_type) = LOWER($2)"
		args = append(args, *roomType)
	}
	q += " ORDER BY price_per_night LIMIT 1"

	rows, err := poolFromContext(ctx, r.db.Pool).Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rooms, err := scanRooms(rows)
	if err != nil {
		return nil, err
	}
	if len(rooms) == 0 {
		return nil, ErrNotFound
	}
	return &rooms[0], nil
}

func (r *roomRepository) CreateRoom(ctx context.Context, hotelID uuid.UUID, rm *domain.Room) (*domain.Room, error) {
	const q = `
		INSERT INTO rooms (id, hotel_id, room_number, room_type, floor, capacity, price_per_night,
		                   status, amenities, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10)
		RETURNING id, hotel_id, room_number, room_type, floor, capacity, price_per_night,
		          status, amenities, created_at, updated_at`
	rm.ID = uuid.New()
	rm.HotelID = hotelID
	now := time.Now().UTC()
	rows, err := poolFromContext(ctx, r.db.Pool).Query(ctx, q,
		rm.ID, rm.HotelID, rm.RoomNumber, rm.RoomType, rm.Floor, rm.Capacity,
		rm.PricePerNight, rm.Status, rm.Amenities, now,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rooms, err := scanRooms(rows)
	if err != nil {
		return nil, err
	}
	return &rooms[0], nil
}

// UpdateRoom patches scalar room fields (not amenities) for the given tenant.
func (r *roomRepository) UpdateRoom(ctx context.Context, hotelID, id uuid.UUID, fields map[string]interface{}) (*domain.Room, error) {
	allowed := map[string]bool{"room_number": true, "room_type": true, "floor": true,
		"capacity": true, "price_per_night": true, "status": true}
	set := []string{}
	args := []interface{}{}
	i := 1
	for k, v := range fields {
		if !allowed[k] {
			continue
		}
		set = append(set, fmt.Sprintf("%s = $%d", k, i))
		args = append(args, v)
		i++
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("no fields to update")
	}
	set = append(set, "updated_at = now()")
	args = append(args, hotelID, id)
	q := fmt.Sprintf(`UPDATE rooms SET %s WHERE hotel_id = $%d AND id = $%d
		RETURNING id, hotel_id, room_number, room_type, floor, capacity, price_per_night,
		          status, amenities, created_at, updated_at`, strings.Join(set, ", "), i, i+1)
	rows, err := poolFromContext(ctx, r.db.Pool).Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rooms, err := scanRooms(rows)
	if err != nil {
		return nil, err
	}
	if len(rooms) == 0 {
		return nil, ErrNotFound
	}
	return &rooms[0], nil
}

func (r *roomRepository) DeleteRoom(ctx context.Context, hotelID, id uuid.UUID) error {
	_, err := poolFromContext(ctx, r.db.Pool).Exec(ctx, `DELETE FROM rooms WHERE hotel_id = $1 AND id = $2`, hotelID, id)
	return err
}

func (r *roomRepository) UpdateRoomStatus(ctx context.Context, hotelID, id uuid.UUID, status domain.RoomStatus) error {
	const q = `UPDATE rooms SET status = $1, updated_at = $2 WHERE hotel_id = $3 AND id = $4`
	_, err := poolFromContext(ctx, r.db.Pool).Exec(ctx, q, status, time.Now().UTC(), hotelID, id)
	return err
}

// Guest Stays

func (r *roomRepository) CreateStay(ctx context.Context, hotelID uuid.UUID, s *domain.GuestStay) (*domain.GuestStay, error) {
	const q = `
		INSERT INTO guest_stays (
			id, hotel_id, guest_id, room_id, guest_name, guest_email, guest_phone,
			check_in_date, check_out_date, actual_check_in, actual_check_out,
			total_amount, notes, source, created_by, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$16)
		RETURNING id, hotel_id, guest_id, room_id, guest_name, guest_email, guest_phone,
		          check_in_date, check_out_date, actual_check_in, actual_check_out,
		          total_amount, notes, source, created_by, created_at, updated_at`
	s.ID = uuid.New()
	s.HotelID = hotelID
	now := time.Now().UTC()
	row := poolFromContext(ctx, r.db.Pool).QueryRow(ctx, q,
		s.ID, s.HotelID, s.GuestID, s.RoomID, s.GuestName, s.GuestEmail, s.GuestPhone,
		s.CheckInDate, s.CheckOutDate, s.ActualCheckIn, s.ActualCheckOut,
		s.TotalAmount, s.Notes, s.Source, s.CreatedBy, now,
	)
	return scanSingleStay(row)
}

func (r *roomRepository) FindStayByID(ctx context.Context, hotelID, id uuid.UUID) (*domain.GuestStay, error) {
	const q = `
		SELECT gs.id, gs.hotel_id, gs.guest_id, gs.room_id, gs.guest_name, gs.guest_email, gs.guest_phone,
		       gs.check_in_date, gs.check_out_date, gs.actual_check_in, gs.actual_check_out,
		       gs.total_amount, gs.notes, gs.source, gs.created_by, gs.created_at, gs.updated_at,
		       r.room_number, r.room_type
		FROM guest_stays gs
		LEFT JOIN rooms r ON r.id = gs.room_id
		WHERE gs.hotel_id = $1 AND gs.id = $2`
	row := poolFromContext(ctx, r.db.Pool).QueryRow(ctx, q, hotelID, id)
	return scanEnrichedStay(row)
}

func (r *roomRepository) ListStays(ctx context.Context, hotelID uuid.UUID, filters map[string]interface{}) ([]domain.GuestStay, error) {
	// Build query dynamically based on filters; only safe column names are allowed.
	allowedCols := map[string]bool{"guest_id": true, "room_id": true, "status": true}
	q := `SELECT gs.id, gs.hotel_id, gs.guest_id, gs.room_id, gs.guest_name, gs.guest_email, gs.guest_phone,
		         gs.check_in_date, gs.check_out_date, gs.actual_check_in, gs.actual_check_out,
		         gs.total_amount, gs.notes, gs.source, gs.created_by, gs.created_at, gs.updated_at,
		         r.room_number, r.room_type
		  FROM guest_stays gs LEFT JOIN rooms r ON r.id = gs.room_id
		  WHERE gs.hotel_id = $1`
	args := []interface{}{hotelID}
	i := 2
	for k, v := range filters {
		if !allowedCols[k] {
			continue
		}
		q += " AND"
		q += fmt.Sprintf(" gs.%s = $%d", k, i)
		args = append(args, v)
		i++
	}
	q += " ORDER BY gs.check_in_date DESC"

	rows, err := poolFromContext(ctx, r.db.Pool).Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stays []domain.GuestStay
	for rows.Next() {
		var s domain.GuestStay
		var roomNumber, roomType *string
		if err := rows.Scan(
			&s.ID, &s.HotelID, &s.GuestID, &s.RoomID, &s.GuestName, &s.GuestEmail, &s.GuestPhone,
			&s.CheckInDate, &s.CheckOutDate, &s.ActualCheckIn, &s.ActualCheckOut,
			&s.TotalAmount, &s.Notes, &s.Source, &s.CreatedBy, &s.CreatedAt, &s.UpdatedAt,
			&roomNumber, &roomType,
		); err != nil {
			return nil, err
		}
		if roomNumber != nil {
			s.Room = &domain.RoomSummary{RoomNumber: *roomNumber}
			if roomType != nil {
				s.Room.RoomType = *roomType
			}
		}
		stays = append(stays, s)
	}
	return stays, rows.Err()
}

func (r *roomRepository) UpdateStay(ctx context.Context, hotelID, id uuid.UUID, fields map[string]interface{}) error {
	fields["updated_at"] = time.Now().UTC()
	i := 1
	setClauses := ""
	args := make([]interface{}, 0, len(fields)+1)
	for k, v := range fields {
		if i > 1 {
			setClauses += ", "
		}
		setClauses += fmt.Sprintf("%s = $%d", k, i)
		args = append(args, v)
		i++
	}
	args = append(args, id)
	args = append(args, hotelID)
	q := fmt.Sprintf("UPDATE guest_stays SET %s WHERE id = $%d AND hotel_id = $%d", setClauses, i, i+1)
	_, err := poolFromContext(ctx, r.db.Pool).Exec(ctx, q, args...)
	return err
}

func (r *roomRepository) DeleteStay(ctx context.Context, hotelID, id uuid.UUID) error {
	const q = `DELETE FROM guest_stays WHERE hotel_id = $1 AND id = $2`
	_, err := poolFromContext(ctx, r.db.Pool).Exec(ctx, q, hotelID, id)
	return err
}

func (r *roomRepository) SmartCheckinLookup(ctx context.Context, hotelID uuid.UUID, guestName, phone string) (*domain.GuestStay, error) {
	const q = `
		SELECT gs.id, gs.hotel_id, gs.guest_id, gs.room_id, gs.guest_name, gs.guest_email, gs.guest_phone,
		       gs.check_in_date, gs.check_out_date, gs.actual_check_in, gs.actual_check_out,
		       gs.total_amount, gs.notes, gs.source, gs.created_by, gs.created_at, gs.updated_at,
		       r.room_number, r.room_type
		FROM guest_stays gs
		LEFT JOIN rooms r ON r.id = gs.room_id
		WHERE gs.hotel_id = $1 AND (gs.guest_name ILIKE $2 OR gs.guest_phone ILIKE $3)
		ORDER BY gs.check_in_date DESC
		LIMIT 1`
	row := poolFromContext(ctx, r.db.Pool).QueryRow(ctx, q, hotelID, "%"+guestName+"%", "%"+phone+"%")
	s, err := scanEnrichedStay(row)
	if err != nil && errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return s, err
}

// Scan helpers

func scanRooms(rows pgx.Rows) ([]domain.Room, error) {
	var rooms []domain.Room
	for rows.Next() {
		var rm domain.Room
		if err := rows.Scan(
			&rm.ID, &rm.HotelID, &rm.RoomNumber, &rm.RoomType, &rm.Floor, &rm.Capacity,
			&rm.PricePerNight, &rm.Status, &rm.Amenities, &rm.CreatedAt, &rm.UpdatedAt,
		); err != nil {
			return nil, err
		}
		rooms = append(rooms, rm)
	}
	return rooms, rows.Err()
}

func scanSingleStay(row pgx.Row) (*domain.GuestStay, error) {
	s := &domain.GuestStay{}
	err := row.Scan(
		&s.ID, &s.HotelID, &s.GuestID, &s.RoomID, &s.GuestName, &s.GuestEmail, &s.GuestPhone,
		&s.CheckInDate, &s.CheckOutDate, &s.ActualCheckIn, &s.ActualCheckOut,
		&s.TotalAmount, &s.Notes, &s.Source, &s.CreatedBy, &s.CreatedAt, &s.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return s, nil
}

func scanEnrichedStay(row pgx.Row) (*domain.GuestStay, error) {
	s := &domain.GuestStay{}
	var roomNumber, roomType *string
	err := row.Scan(
		&s.ID, &s.HotelID, &s.GuestID, &s.RoomID, &s.GuestName, &s.GuestEmail, &s.GuestPhone,
		&s.CheckInDate, &s.CheckOutDate, &s.ActualCheckIn, &s.ActualCheckOut,
		&s.TotalAmount, &s.Notes, &s.Source, &s.CreatedBy, &s.CreatedAt, &s.UpdatedAt,
		&roomNumber, &roomType,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if roomNumber != nil {
		s.Room = &domain.RoomSummary{RoomNumber: *roomNumber}
		if roomType != nil {
			s.Room.RoomType = *roomType
		}
	}
	return s, nil
}
