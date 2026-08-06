package handler

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/hotelharmony/api/internal/repository/postgres"
)

// Promo code resolution, shared by the booking engine's validate endpoint and
// the front-office reservation form so both apply exactly the same rules.
// Previously the rules lived only inside ValidatePromo, which meant any second
// caller would have had to restate them and would inevitably have drifted.
//
// min_nights, min_amount and max_discount are columns that already existed on
// promotions but nothing enforced. They are enforced here, so a code created
// with those limits now behaves the way the form that created it implies.

// promoResult is the outcome of applying a code to a proposed stay.
type promoResult struct {
	ID       uuid.UUID `json:"-"`
	Valid    bool      `json:"valid"`
	Code     string    `json:"code"`
	Name     string    `json:"name,omitempty"`
	Discount float64   `json:"discount"`
	Message  string    `json:"message"`
}

// errPromoNotApplicable is returned to a caller that must not proceed — the
// reservation path refuses the booking rather than quietly charging full price
// for a code the guest was promised.
var errPromoNotApplicable = errors.New("promo code is not applicable")

// resolvePromo looks a code up and computes its discount against base.
//
// It reports invalid codes as a result with Valid=false rather than an error:
// "this code is expired" is a normal answer to a question, not a failure. A
// non-nil error means the lookup itself failed.
//
// nights < 0 means the caller does not know the length of stay, and the
// minimum-nights rule is skipped. The booking engine's validate endpoint is
// given only a total, and it has never enforced that rule — rejecting there now
// would break codes that currently work.
func resolvePromo(ctx context.Context, pool postgres.Querier, hotelID uuid.UUID, code string, base float64, nights int) (promoResult, error) {
	code = strings.TrimSpace(code)
	res := promoResult{Code: code}
	if code == "" {
		res.Message = "no promo code supplied"
		return res, nil
	}

	var (
		promoID                  uuid.UUID
		name, discountType       string
		discountValue, minAmount float64
		maxDiscount              *float64
		minNights                int
		usageLimit, usedCount    int
		validFrom, validTo       time.Time
		active                   bool
	)

	// Codes are matched case-insensitively: they are printed on flyers and
	// re-keyed by hand, so "SUMMER25" and "summer25" must be the same code.
	err := pool.QueryRow(ctx, `
		SELECT id, name, discount_type, discount_value,
		       COALESCE(min_nights, 0), COALESCE(min_amount, 0), max_discount,
		       COALESCE(usage_limit, 0), used_count, valid_from, valid_to, active
		FROM promotions
		WHERE hotel_id = $1 AND upper(code) = upper($2)`,
		hotelID, code,
	).Scan(&promoID, &name, &discountType, &discountValue,
		&minNights, &minAmount, &maxDiscount,
		&usageLimit, &usedCount, &validFrom, &validTo, &active)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			res.Message = "promo code not found"
			return res, nil
		}
		return res, err
	}

	res.ID = promoID
	res.Name = name

	switch {
	case !active:
		res.Message = "promo code is inactive"
		return res, nil
	case time.Now().UTC().Before(validFrom):
		res.Message = "promo code is not yet valid"
		return res, nil
	case time.Now().UTC().After(validTo):
		res.Message = "promo code has expired"
		return res, nil
	case usageLimit > 0 && usedCount >= usageLimit:
		res.Message = "promo code usage limit reached"
		return res, nil
	case minNights > 0 && nights >= 0 && nights < minNights:
		res.Message = "promo code requires a longer stay"
		return res, nil
	case minAmount > 0 && base < minAmount:
		res.Message = "booking total is below the promo code minimum"
		return res, nil
	}

	var discount float64
	if strings.EqualFold(discountType, "percentage") {
		discount = base * discountValue / 100
	} else {
		discount = discountValue
	}
	// A cap of 0 means "uncapped" — the column is nullable and defaults to
	// nothing, so a real cap is always a positive number.
	if maxDiscount != nil && *maxDiscount > 0 && discount > *maxDiscount {
		discount = *maxDiscount
	}
	if discount > base {
		discount = base
	}

	res.Valid = true
	res.Discount = round2(discount)
	res.Message = "promo code applied successfully"
	return res, nil
}

// redeemPromo consumes one use of a code.
//
// The guard is in the UPDATE rather than in a preceding SELECT: two guests
// redeeming the last use of a code at the same moment would both pass a
// read-then-write check and the code would be over-redeemed. Zero rows affected
// means someone else took the last use between resolve and redeem.
func redeemPromo(ctx context.Context, pool postgres.Querier, hotelID, promoID uuid.UUID) error {
	if promoID == uuid.Nil {
		return nil
	}
	tag, err := pool.Exec(ctx, `
		UPDATE promotions
		   SET used_count = used_count + 1
		 WHERE id = $1 AND hotel_id = $2 AND active
		   AND (usage_limit = 0 OR used_count < usage_limit)`,
		promoID, hotelID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errPromoNotApplicable
	}
	return nil
}
