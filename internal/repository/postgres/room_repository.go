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
	ListRoomsAvailableBetween(ctx context.Context, hotelID uuid.UUID, checkIn, checkOut time.Time) ([]domain.Room, error)
	FindRoomByID(ctx context.Context, hotelID, id uuid.UUID) (*domain.Room, error)
	FindAvailableRoom(ctx context.Context, hotelID uuid.UUID, roomType *string) (*domain.Room, error)
	CreateRoom(ctx context.Context, hotelID uuid.UUID, r *domain.Room) (*domain.Room, error)
	UpdateRoom(ctx context.Context, hotelID, id uuid.UUID, fields map[string]interface{}) (*domain.Room, error)
	DeleteRoom(ctx context.Context, hotelID, id uuid.UUID) error
	UpdateRoomStatus(ctx context.Context, hotelID, id uuid.UUID, status domain.RoomStatus) error

	CreateStay(ctx context.Context, hotelID uuid.UUID, s *domain.GuestStay) (*domain.GuestStay, error)
	FindStayByID(ctx context.Context, hotelID, id uuid.UUID) (*domain.GuestStay, error)
	ListStays(ctx context.Context, hotelID uuid.UUID, filters map[string]interface{}) ([]domain.GuestStay, error)
	RoomHasActiveStay(ctx context.Context, hotelID, roomID uuid.UUID) (bool, error)
	EnsureGuest(ctx context.Context, hotelID uuid.UUID, name, email, phone string) (uuid.UUID, error)
	SetGuestIdentity(ctx context.Context, hotelID, guestID uuid.UUID, idType, idNumber string) error
	EnsureFolioForBooking(ctx context.Context, hotelID, bookingID, guestID uuid.UUID, currency string) (uuid.UUID, error)
	UpdateStay(ctx context.Context, hotelID, id uuid.UUID, fields map[string]interface{}) error
	DeleteStay(ctx context.Context, hotelID, id uuid.UUID) error
	SoftCancelStay(ctx context.Context, hotelID, id uuid.UUID, reason string, fee float64, by *uuid.UUID) error
	RoomIsFree(ctx context.Context, hotelID, roomID uuid.UUID, checkIn, checkOut time.Time, excludeStayID uuid.UUID) (bool, error)
	HotelName(ctx context.Context, hotelID uuid.UUID) (string, error)
	HotelGSTRate(ctx context.Context, hotelID uuid.UUID) (float64, error)

	SmartCheckinLookup(ctx context.Context, hotelID uuid.UUID, guestName, phone string) (*domain.GuestStay, error)
}

type roomRepository struct {
	db *DB
}

func NewRoomRepository(db *DB) RoomRepository {
	return &roomRepository{db: db}
}

// stayColumns is the single definition of which guest_stays columns are read
// and in what order. Every stay query selects it and every scan helper reads it
// in the same order, so adding a column is one edit instead of six that have to
// agree — and a mismatch between them is what makes pgx fail an entire scan.
//
// Qualified with gs. because all the read queries join rooms.
const stayColumns = `gs.id, gs.hotel_id, gs.guest_id, gs.room_id, gs.guest_name, gs.guest_email, gs.guest_phone,
	gs.check_in_date, gs.check_out_date, gs.actual_check_in, gs.actual_check_out,
	gs.total_amount, gs.notes, gs.source, gs.created_by, gs.created_at, gs.updated_at,
	gs.status, gs.confirmation_no, gs.cancelled_at, gs.cancellation_reason, gs.cancellation_fee,
	gs.adults, gs.children, gs.approach_type, gs.discount_amount, gs.tax_amount, gs.promo_code`

// stayColumnsBare is the same list without the table alias, for INSERT ... RETURNING.
const stayColumnsBare = `id, hotel_id, guest_id, room_id, guest_name, guest_email, guest_phone,
	check_in_date, check_out_date, actual_check_in, actual_check_out,
	total_amount, notes, source, created_by, created_at, updated_at,
	status, confirmation_no, cancelled_at, cancellation_reason, cancellation_fee,
	adults, children, approach_type, discount_amount, tax_amount, promo_code`

// stayScanTargets returns the scan destinations for stayColumns, in order.
func stayScanTargets(s *domain.GuestStay) []any {
	return []any{
		&s.ID, &s.HotelID, &s.GuestID, &s.RoomID, &s.GuestName, &s.GuestEmail, &s.GuestPhone,
		&s.CheckInDate, &s.CheckOutDate, &s.ActualCheckIn, &s.ActualCheckOut,
		&s.TotalAmount, &s.Notes, &s.Source, &s.CreatedBy, &s.CreatedAt, &s.UpdatedAt,
		&s.Status, &s.ConfirmationNo, &s.CancelledAt, &s.CancellationReason, &s.CancellationFee,
		&s.Adults, &s.Children, &s.ApproachType, &s.DiscountAmount, &s.TaxAmount, &s.PromoCode,
	}
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

// ListRoomsAvailableBetween returns the rooms that can be booked for
// [checkIn, checkOut), which is a different question from ListRooms(status).
//
// A room's `status` describes it right now — occupied tonight, being cleaned
// this hour. It says nothing about a stay next month, so selecting rooms by
// status both hides rooms that are bookable later and offers rooms that are
// already reserved for the requested dates.
//
// Availability is therefore the absence of an overlapping stay. Two stays
// overlap when each starts before the other ends; checkout day is free, so the
// comparisons are strict and a guest leaving on the 10th does not block an
// arrival on the 10th. Only `maintenance` excludes a room outright, being the
// one status that means the room cannot be occupied at all.
func (r *roomRepository) ListRoomsAvailableBetween(ctx context.Context, hotelID uuid.UUID, checkIn, checkOut time.Time) ([]domain.Room, error) {
	const q = `
		SELECT id, hotel_id, room_number, room_type, floor, capacity, price_per_night,
		       status, amenities, created_at, updated_at
		FROM rooms rm
		WHERE rm.hotel_id = $1
		  AND rm.status NOT IN (` + domain.NonSellableRoomStatusSQL + `)
		  AND NOT EXISTS (
		      SELECT 1 FROM guest_stays gs
		      WHERE gs.hotel_id = rm.hotel_id
		        AND gs.room_id  = rm.id
		        AND gs.check_in_date  < $3
		        AND gs.check_out_date > $2
		  )
		ORDER BY rm.floor, rm.room_number`
	rows, err := poolFromContext(ctx, r.db.Pool).Query(ctx, q, hotelID, checkIn, checkOut)
	if err != nil {
		return nil, fmt.Errorf("roomRepo.ListRoomsAvailableBetween: %w", err)
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

// UpdateRoomStatus returns ErrNotFound when no room matches, so a caller that
// cares can answer 404 instead of reporting success for a room that does not
// exist (or belongs to another tenant). Callers that update a room they already
// resolved may keep discarding the error.
func (r *roomRepository) UpdateRoomStatus(ctx context.Context, hotelID, id uuid.UUID, status domain.RoomStatus) error {
	const q = `UPDATE rooms SET status = $1, updated_at = $2 WHERE hotel_id = $3 AND id = $4`
	tag, err := poolFromContext(ctx, r.db.Pool).Exec(ctx, q, status, time.Now().UTC(), hotelID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Guest Stays

func (r *roomRepository) CreateStay(ctx context.Context, hotelID uuid.UUID, s *domain.GuestStay) (*domain.GuestStay, error) {
	q := `
		INSERT INTO guest_stays (
			id, hotel_id, guest_id, room_id, guest_name, guest_email, guest_phone,
			check_in_date, check_out_date, actual_check_in, actual_check_out,
			total_amount, notes, source, created_by, created_at, updated_at,
			status, confirmation_no, adults, children, approach_type,
			discount_amount, tax_amount, promo_code
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$16,
		          $17,$18,$19,$20,$21,$22,$23,$24)
		RETURNING ` + stayColumnsBare
	s.ID = uuid.New()
	s.HotelID = hotelID
	now := time.Now().UTC()

	// Defaults for a caller that did not set the newer fields, so an older code
	// path constructing a GuestStay by hand cannot write a zero occupancy or an
	// empty status that the CHECK constraints would reject.
	if s.Status == "" {
		s.Status = domain.ReservationConfirmed
	}
	if s.Adults <= 0 {
		s.Adults = 1
	}
	if s.ApproachType == "" {
		s.ApproachType = "manual"
	}

	row := poolFromContext(ctx, r.db.Pool).QueryRow(ctx, q,
		s.ID, s.HotelID, s.GuestID, s.RoomID, s.GuestName, s.GuestEmail, s.GuestPhone,
		s.CheckInDate, s.CheckOutDate, s.ActualCheckIn, s.ActualCheckOut,
		s.TotalAmount, s.Notes, s.Source, s.CreatedBy, now,
		s.Status, s.ConfirmationNo, s.Adults, s.Children, s.ApproachType,
		s.DiscountAmount, s.TaxAmount, s.PromoCode,
	)
	return scanSingleStay(row)
}

func (r *roomRepository) FindStayByID(ctx context.Context, hotelID, id uuid.UUID) (*domain.GuestStay, error) {
	const q = `
		SELECT ` + stayColumns + `, r.room_number, r.room_type
		FROM guest_stays gs
		LEFT JOIN rooms r ON r.id = gs.room_id
		WHERE gs.hotel_id = $1 AND gs.id = $2`
	row := poolFromContext(ctx, r.db.Pool).QueryRow(ctx, q, hotelID, id)
	return scanEnrichedStay(row)
}

func (r *roomRepository) ListStays(ctx context.Context, hotelID uuid.UUID, filters map[string]interface{}) ([]domain.GuestStay, error) {
	// Build query dynamically based on filters; only safe column names are allowed.
	allowedCols := map[string]bool{"guest_id": true, "room_id": true, "status": true}
	q := `SELECT ` + stayColumns + `, r.room_number, r.room_type
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
		if err := rows.Scan(append(stayScanTargets(&s), &roomNumber, &roomType)...); err != nil {
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
	tag, err := poolFromContext(ctx, r.db.Pool).Exec(ctx, q, args...)
	if err != nil {
		return err
	}
	// No matching stay: the id is unknown or belongs to another tenant. Report
	// it rather than letting the caller treat a zero-row update as a success.
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// EnsureGuest finds or creates the CRM guest behind a booking and returns its
// id, mirroring what the POS path does for accounting customers.
//
// Reservations used to store only a free-text guest_name, leaving
// guest_stays.guest_id null on every row. That broke the chain the front desk
// depends on: a folio requires a guest_id, so check-in could not open one, so
// room charges had nowhere to post and the billing screen stayed empty however
// many guests had stayed. It also meant a returning guest was invisible to CRM.
//
// Matched on phone first, then email; both are the identifiers staff actually
// re-key. With neither, there is nothing to match on and no guest is created —
// the caller keeps a null guest_id rather than accumulating duplicates.
func (r *roomRepository) EnsureGuest(ctx context.Context, hotelID uuid.UUID, name, email, phone string) (uuid.UUID, error) {
	name, email, phone = strings.TrimSpace(name), strings.TrimSpace(email), strings.TrimSpace(phone)
	if phone == "" && email == "" {
		return uuid.Nil, nil
	}
	db := poolFromContext(ctx, r.db.Pool)

	// Compare the last 10 digits, not all of them: "+91 98111 22333" reduces to
	// 919811122333 and "9811122333" to 9811122333, so a whole-string comparison
	// treats one person as two the moment someone types the country code. Ten
	// is the Indian subscriber-number length, which is what both forms share.
	const findByPhone = `
		SELECT id FROM guests
		WHERE hotel_id = $1
		  AND length(regexp_replace($2, '\D', '', 'g')) >= 10
		  AND right(regexp_replace(COALESCE(phone,''), '\D', '', 'g'), 10)
		    = right(regexp_replace($2, '\D', '', 'g'), 10)
		LIMIT 1`
	const findByEmail = `
		SELECT id FROM guests
		WHERE hotel_id = $1 AND lower(COALESCE(email,'')) = lower($2) AND $2 <> ''
		LIMIT 1`

	var id uuid.UUID
	for _, q := range []struct {
		sql string
		arg string
	}{{findByPhone, phone}, {findByEmail, email}} {
		if q.arg == "" {
			continue
		}
		err := db.QueryRow(ctx, q.sql, hotelID, q.arg).Scan(&id)
		if err == nil {
			// Backfill whichever contact detail the record was missing, without
			// overwriting anything already recorded.
			_, _ = db.Exec(ctx, `
				UPDATE guests
				SET email = COALESCE(NULLIF(email,''), NULLIF($2,'')),
				    phone = COALESCE(NULLIF(phone,''), NULLIF($3,'')),
				    updated_at = now()
				WHERE id = $1`, id, email, phone)
			return id, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, fmt.Errorf("roomRepo.EnsureGuest lookup: %w", err)
		}
	}

	if name == "" {
		name = "Guest " + phone
	}
	id = uuid.New()
	if err := db.QueryRow(ctx, `
		INSERT INTO guests (id, hotel_id, full_name, email, phone, created_at, updated_at)
		VALUES ($1, $2, $3, NULLIF($4,''), NULLIF($5,''), now(), now())
		RETURNING id`, id, hotelID, name, email, phone).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("roomRepo.EnsureGuest insert: %w", err)
	}
	return id, nil
}

// SetGuestIdentity records the ID proof captured at the desk.
//
// guests.id_type and id_number have existed since the first migration and
// nothing has ever written them, so no guest in the system carries the identity
// document the front desk is required to record.
//
// Only fills a blank: an ID already on file is not overwritten by a later
// booking where the clerk typed less, and a blank submission never erases one.
func (r *roomRepository) SetGuestIdentity(ctx context.Context, hotelID, guestID uuid.UUID, idType, idNumber string) error {
	const q = `
		UPDATE guests
		   SET id_type   = COALESCE(NULLIF(id_type,''),   NULLIF($3,'')),
		       id_number = COALESCE(NULLIF(id_number,''), NULLIF($4,'')),
		       updated_at = now()
		 WHERE hotel_id = $1 AND id = $2`
	_, err := poolFromContext(ctx, r.db.Pool).Exec(ctx, q,
		hotelID, guestID, strings.TrimSpace(idType), strings.TrimSpace(idNumber))
	return err
}

// EnsureFolioForBooking opens the guest's folio for a booking, or returns the
// existing one. Idempotent, so a repeated check-in cannot open a second folio
// and split one stay's charges across two bills.
func (r *roomRepository) EnsureFolioForBooking(ctx context.Context, hotelID, bookingID, guestID uuid.UUID, currency string) (uuid.UUID, error) {
	if guestID == uuid.Nil {
		return uuid.Nil, nil
	}
	if currency == "" {
		currency = "INR"
	}
	db := poolFromContext(ctx, r.db.Pool)

	var id uuid.UUID
	err := db.QueryRow(ctx,
		`SELECT id FROM folios WHERE hotel_id = $1 AND booking_id = $2 LIMIT 1`,
		hotelID, bookingID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("roomRepo.EnsureFolioForBooking lookup: %w", err)
	}

	id = uuid.New()
	if err := db.QueryRow(ctx, `
		INSERT INTO folios (id, hotel_id, booking_id, guest_id, status, currency, created_at)
		VALUES ($1, $2, $3, $4, 'open', $5, now())
		RETURNING id`, id, hotelID, bookingID, guestID, currency).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("roomRepo.EnsureFolioForBooking insert: %w", err)
	}
	return id, nil
}

// RoomHasActiveStay reports whether someone is currently in the room: a stay
// that has been checked in and not yet checked out. Used before releasing a
// room, so cancelling one booking cannot mark a room occupied by a different
// guest as available.
func (r *roomRepository) RoomHasActiveStay(ctx context.Context, hotelID, roomID uuid.UUID) (bool, error) {
	const q = `
		SELECT EXISTS (
			SELECT 1 FROM guest_stays
			WHERE hotel_id = $1 AND room_id = $2
			  AND actual_check_in IS NOT NULL
			  AND actual_check_out IS NULL
		)`
	var exists bool
	err := poolFromContext(ctx, r.db.Pool).QueryRow(ctx, q, hotelID, roomID).Scan(&exists)
	return exists, err
}

func (r *roomRepository) DeleteStay(ctx context.Context, hotelID, id uuid.UUID) error {
	const q = `DELETE FROM guest_stays WHERE hotel_id = $1 AND id = $2`
	_, err := poolFromContext(ctx, r.db.Pool).Exec(ctx, q, hotelID, id)
	return err
}

// SoftCancelStay marks a reservation cancelled instead of deleting the row.
//
// Cancelling used to DELETE, which destroyed the only record that the booking
// had ever existed: no reason, no fee, no audit trail, and no way to tell a
// cancellation from a no-show or from a booking that was never taken.
// Cancelled-revenue reporting was impossible and the deletion was
// unrecoverable.
//
// Refuses to cancel a stay that is already cancelled, so a repeated request
// cannot overwrite the original reason and timestamp with a later one.
func (r *roomRepository) SoftCancelStay(ctx context.Context, hotelID, id uuid.UUID, reason string, fee float64, by *uuid.UUID) error {
	const q = `
		UPDATE guest_stays
		   SET status = 'cancelled',
		       cancelled_at = now(),
		       cancellation_reason = NULLIF($3, ''),
		       cancellation_fee = $4,
		       cancelled_by = $5,
		       updated_at = now()
		 WHERE hotel_id = $1 AND id = $2 AND status <> 'cancelled'`
	tag, err := poolFromContext(ctx, r.db.Pool).Exec(ctx, q, hotelID, id, reason, fee, by)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RoomIsFree reports whether a room has no other stay overlapping
// [checkIn, checkOut). excludeStayID lets an edit ignore the stay being edited,
// which would otherwise always collide with itself.
//
// Comparisons are strict and cancelled stays are ignored, matching
// ListRoomsAvailableBetween and the guest_stays_no_overlap constraint, so all
// three agree on what "free" means.
func (r *roomRepository) RoomIsFree(ctx context.Context, hotelID, roomID uuid.UUID, checkIn, checkOut time.Time, excludeStayID uuid.UUID) (bool, error) {
	const q = `
		SELECT NOT EXISTS (
			SELECT 1 FROM guest_stays
			WHERE hotel_id = $1
			  AND room_id = $2
			  AND status <> 'cancelled'
			  AND id <> $3
			  AND check_in_date < $5
			  AND check_out_date > $4
		)`
	var free bool
	err := poolFromContext(ctx, r.db.Pool).QueryRow(ctx, q, hotelID, roomID, excludeStayID, checkIn, checkOut).Scan(&free)
	return free, err
}

// HotelGSTRate returns the tenant's configured GST percentage.
//
// This is the authoritative rate — the same column the tax-invoice fields use.
// The booking wizard previously hardcoded 18% on the client, displayed a total
// including it, and then sent nothing, so the figure the guest agreed to and
// the figure stored on the reservation disagreed by that 18% on every booking.
//
// A tenant that has not configured a rate has 0, which is the column default
// and means "quote no tax" rather than "guess".
func (r *roomRepository) HotelGSTRate(ctx context.Context, hotelID uuid.UUID) (float64, error) {
	var rate float64
	err := poolFromContext(ctx, r.db.Pool).
		QueryRow(ctx, `SELECT COALESCE(gst_rate, 0) FROM hotels WHERE id = $1`, hotelID).Scan(&rate)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	return rate, nil
}

// HotelName returns the tenant's own name, for guest-facing messages.
//
// Check-in and check-out notifications hardcoded "Grand Hotel Mumbai", so every
// tenant's guests were told the name of a different hotel.
func (r *roomRepository) HotelName(ctx context.Context, hotelID uuid.UUID) (string, error) {
	var name string
	err := poolFromContext(ctx, r.db.Pool).
		QueryRow(ctx, `SELECT name FROM hotels WHERE id = $1`, hotelID).Scan(&name)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	return name, nil
}

func (r *roomRepository) SmartCheckinLookup(ctx context.Context, hotelID uuid.UUID, guestName, phone string) (*domain.GuestStay, error) {
	const q = `
		SELECT ` + stayColumns + `, r.room_number, r.room_type
		FROM guest_stays gs
		LEFT JOIN rooms r ON r.id = gs.room_id
		WHERE gs.hotel_id = $1 AND (gs.guest_name ILIKE $2 OR gs.guest_phone ILIKE $3)
		  AND gs.status <> 'cancelled'
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
	err := row.Scan(stayScanTargets(s)...)
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
	err := row.Scan(append(stayScanTargets(s), &roomNumber, &roomType)...)
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
