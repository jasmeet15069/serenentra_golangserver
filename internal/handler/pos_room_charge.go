package handler

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/hotelharmony/api/internal/repository/postgres"
)

// Restaurant -> Front Office -> Accounting linkage for "charge to room".
//
// A dine-in bill settled as room_charge collects no money: the guest signs for
// it and pays when they check out. The three modules therefore have to agree,
// and they did not:
//
//   - Front office never saw it. Nothing wrote a folio_charge, so the amount
//     never reached the guest's folio and check-out never asked for it. The
//     restaurant had served food that nobody would ever be billed for.
//   - Reporting double-counted it. Settlement mirrored every payment into
//     `payments`, which is what reports/revenue sums as collections — so a
//     room charge showed up as cash received on the day it was signed for, and
//     again when the guest actually settled at the desk.
//   - Accounting had no way to collect it. Posting Dr Accounts Receivable is
//     right, but with no folio line behind it the receivable had no document.
//
// resolveFolioForRoomCharge answers "whose folio does this bill belong to", and
// postFolioCharge writes the line. Both take a Querier so they run inside the
// settlement transaction: a folio charge that exists without its bill payment,
// or the reverse, is exactly the inconsistency this is meant to remove.

// errNoRoomForCharge means the bill cannot be charged to a room because the
// table was never linked to a checked-in stay.
var errNoRoomForCharge = errors.New("charge to room requires the table to be linked to a checked-in guest")

// resolveFolioForRoomCharge finds the open folio for the stay a dining session
// is attached to, opening one if the stay has none yet.
//
// The stay must be checked in and not yet departed. Charging a future booking
// would put restaurant food on a folio nobody is standing behind, and charging
// a departed one would reopen a bill that has already been settled.
func resolveFolioForRoomCharge(ctx context.Context, db postgres.Querier, hotelID, sessionID uuid.UUID) (uuid.UUID, uuid.UUID, error) {
	var stayID uuid.UUID
	err := db.QueryRow(ctx, `
		SELECT gs.id
		  FROM dining_sessions ds
		  JOIN guest_stays gs ON gs.id = ds.guest_stay_id AND gs.hotel_id = ds.hotel_id
		 WHERE ds.id = $1
		   AND ds.hotel_id = $2
		   AND gs.actual_check_in IS NOT NULL
		   AND gs.actual_check_out IS NULL
		   AND gs.status <> 'cancelled'`, sessionID, hotelID).Scan(&stayID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, uuid.Nil, errNoRoomForCharge
		}
		return uuid.Nil, uuid.Nil, fmt.Errorf("room charge: resolve stay: %w", err)
	}

	// The folio is normally opened at check-in. A stay that predates that
	// behaviour has none, so open it here rather than refusing a legitimate
	// charge — the lookup is keyed on booking_id, so this cannot create a
	// second folio for the same stay.
	var folioID uuid.UUID
	err = db.QueryRow(ctx,
		`SELECT id FROM folios WHERE hotel_id = $1 AND booking_id = $2 AND status = 'open' LIMIT 1`,
		hotelID, stayID).Scan(&folioID)
	if err == nil {
		return folioID, stayID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, uuid.Nil, fmt.Errorf("room charge: resolve folio: %w", err)
	}

	// folios.guest_id carries the CRM guests(id) — see EnsureFolioForBooking.
	var guestID *uuid.UUID
	_ = db.QueryRow(ctx, `
		SELECT g.id FROM guest_stays gs
		  JOIN guests g ON g.hotel_id = gs.hotel_id
		   AND (
		     (NULLIF(gs.guest_phone,'') IS NOT NULL AND right(regexp_replace(COALESCE(g.phone,''), '\D', '', 'g'), 10)
		        = right(regexp_replace(gs.guest_phone, '\D', '', 'g'), 10))
		     OR (NULLIF(gs.guest_email,'') IS NOT NULL AND lower(g.email) = lower(gs.guest_email))
		   )
		 WHERE gs.id = $1 AND gs.hotel_id = $2
		 LIMIT 1`, stayID, hotelID).Scan(&guestID)

	folioID = uuid.New()
	if err := db.QueryRow(ctx, `
		INSERT INTO folios (id, hotel_id, booking_id, guest_id, status, currency, created_at)
		VALUES ($1, $2, $3, $4, 'open', 'INR', now())
		RETURNING id`, folioID, hotelID, stayID, guestID).Scan(&folioID); err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("room charge: open folio: %w", err)
	}
	return folioID, stayID, nil
}

// postFolioCharge writes the restaurant bill onto the guest's folio so it
// appears on their bill and is collected at check-out.
//
// reference_id is the bill id, which is what lets the folio line be traced back
// to the KOTs behind it, and what makes the write idempotent: a retried
// settlement finds the charge already there rather than billing the guest
// twice for one meal.
func postFolioCharge(ctx context.Context, db postgres.Querier, hotelID, folioID, billID uuid.UUID,
	description string, amount, taxAmount float64, postedBy *uuid.UUID) error {

	var exists bool
	if err := db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM folio_charges WHERE folio_id = $1 AND reference_id = $2)`,
		folioID, billID).Scan(&exists); err != nil {
		return fmt.Errorf("room charge: idempotency check: %w", err)
	}
	if exists {
		return nil
	}

	if _, err := db.Exec(ctx, `
		INSERT INTO folio_charges (
			id, folio_id, hotel_id, description, charge_type, amount, tax_amount,
			reference_id, posted_at, posted_by
		) VALUES ($1,$2,$3,$4,'restaurant',$5,$6,$7,now(),$8)`,
		uuid.New(), folioID, hotelID, description, amount, taxAmount, billID, postedBy,
	); err != nil {
		return fmt.Errorf("room charge: post folio charge: %w", err)
	}
	return nil
}

// folioOutstanding totals what a folio still owes: charges plus their tax, less
// anything already paid against it.
//
// Check-out reads this. It previously read nothing at all, so a guest could
// walk out owing for every meal they had signed for.
func folioOutstanding(ctx context.Context, db postgres.Querier, hotelID, stayID uuid.UUID) (float64, uuid.UUID, error) {
	var folioID uuid.UUID
	err := db.QueryRow(ctx,
		`SELECT id FROM folios WHERE hotel_id = $1 AND booking_id = $2 AND status = 'open' LIMIT 1`,
		hotelID, stayID).Scan(&folioID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, uuid.Nil, nil
		}
		return 0, uuid.Nil, err
	}

	var charges, paid float64
	if err := db.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount + tax_amount), 0) FROM folio_charges WHERE folio_id = $1`,
		folioID).Scan(&charges); err != nil {
		return 0, folioID, err
	}
	// Payments recorded against this stay at the desk.
	if err := db.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount), 0) FROM payments
		 WHERE hotel_id = $1 AND guest_stay_id = $2 AND status = 'completed'`,
		hotelID, stayID).Scan(&paid); err != nil {
		return 0, folioID, err
	}

	return round2(charges - paid), folioID, nil
}
