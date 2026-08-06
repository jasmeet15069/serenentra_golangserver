package handler

import (
	"strings"
	"testing"
	"time"

	"github.com/hotelharmony/api/internal/domain"
)

func mustDate(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("bad test date %q: %v", s, err)
	}
	return d
}

// The desk books in nights, the schema stores a checkout date, and the original
// API accepted only the latter. Accepting both means either can be sent — but
// only when they agree, because silently preferring one would book dates the
// caller did not ask for.
func TestResolveStayDates(t *testing.T) {
	cases := []struct {
		name     string
		req      createReservationRequest
		wantIn   string
		wantOut  string
		wantErr  string
		checkErr bool
	}{
		{
			name:    "explicit checkout, the original contract",
			req:     createReservationRequest{CheckInDate: "2026-08-10", CheckOutDate: "2026-08-12"},
			wantIn:  "2026-08-10",
			wantOut: "2026-08-12",
		},
		{
			name:    "duration derives the checkout date",
			req:     createReservationRequest{CheckInDate: "2026-08-10", DurationNights: 3},
			wantIn:  "2026-08-10",
			wantOut: "2026-08-13",
		},
		{
			name:    "both supplied and agreeing is accepted",
			req:     createReservationRequest{CheckInDate: "2026-08-10", CheckOutDate: "2026-08-12", DurationNights: 2},
			wantIn:  "2026-08-10",
			wantOut: "2026-08-12",
		},
		{
			name:     "both supplied and disagreeing is refused, not silently resolved",
			req:      createReservationRequest{CheckInDate: "2026-08-10", CheckOutDate: "2026-08-12", DurationNights: 5},
			checkErr: true,
			wantErr:  "does not match duration_nights",
		},
		{
			name:     "neither supplied",
			req:      createReservationRequest{CheckInDate: "2026-08-10"},
			checkErr: true,
			wantErr:  "check_out_date or duration_nights is required",
		},
		{
			name:     "checkout before checkin",
			req:      createReservationRequest{CheckInDate: "2026-08-10", CheckOutDate: "2026-08-09"},
			checkErr: true,
			wantErr:  "check_out must be after check_in",
		},
		{
			name:     "zero-night stay is not a stay",
			req:      createReservationRequest{CheckInDate: "2026-08-10", CheckOutDate: "2026-08-10"},
			checkErr: true,
			wantErr:  "check_out must be after check_in",
		},
		{
			name:     "unparseable check-in",
			req:      createReservationRequest{CheckInDate: "10/08/2026", CheckOutDate: "2026-08-12"},
			checkErr: true,
			wantErr:  "invalid check_in_date",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in, out, err := resolveStayDates(c.req)
			if c.checkErr {
				if err == nil {
					t.Fatalf("expected an error containing %q, got none", c.wantErr)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err.Error(), c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := in.Format("2006-01-02"); got != c.wantIn {
				t.Errorf("check_in = %s, want %s", got, c.wantIn)
			}
			if got := out.Format("2006-01-02"); got != c.wantOut {
				t.Errorf("check_out = %s, want %s", got, c.wantOut)
			}
		})
	}
}

// The stored status is authoritative because it is the only thing that can say
// "cancelled" — that state has no timestamp to derive it from, which is exactly
// why cancelling used to delete the row. The two derived labels are kept
// because the reservations list has always filtered on those names.
func TestDeriveReservationStatus(t *testing.T) {
	past := time.Now().AddDate(0, 0, -2)
	future := time.Now().AddDate(0, 0, 5)
	ts := time.Now()

	cases := []struct {
		name string
		stay domain.GuestStay
		want string
	}{
		{
			name: "cancelled wins over the timestamps",
			stay: domain.GuestStay{Status: domain.ReservationCancelled, CheckInDate: past},
			want: "cancelled",
		},
		{
			name: "no_show wins over an arrival date that has passed",
			stay: domain.GuestStay{Status: domain.ReservationNoShow, CheckInDate: past},
			want: "no_show",
		},
		{
			name: "in_house",
			stay: domain.GuestStay{Status: domain.ReservationInHouse, ActualCheckIn: &ts},
			want: "in_house",
		},
		{
			name: "checked_out",
			stay: domain.GuestStay{Status: domain.ReservationCheckedOut, ActualCheckOut: &ts},
			want: "checked_out",
		},
		{
			name: "legacy row with no stored status, arrival passed",
			stay: domain.GuestStay{Status: domain.ReservationConfirmed, CheckInDate: past},
			want: "pending_checkin",
		},
		{
			name: "legacy row with no stored status, arrival ahead",
			stay: domain.GuestStay{Status: domain.ReservationConfirmed, CheckInDate: future},
			want: "upcoming",
		},
		{
			name: "legacy row predating the status column, checked in",
			stay: domain.GuestStay{CheckInDate: past, ActualCheckIn: &ts},
			want: "in_house",
		},
		{
			name: "legacy row predating the status column, departed",
			stay: domain.GuestStay{CheckInDate: past, ActualCheckIn: &ts, ActualCheckOut: &ts},
			want: "checked_out",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := deriveReservationStatus(c.stay); got != c.want {
				t.Errorf("deriveReservationStatus() = %q, want %q", got, c.want)
			}
		})
	}
}

// One calculation feeds both the quote endpoint and Create, which is the whole
// point: the wizard used to hardcode 18% GST, display a total including it, and
// send nothing, so the guest agreed to one number and the database stored
// another.
func TestPriceStay(t *testing.T) {
	in := mustDate(t, "2026-08-10")
	out := mustDate(t, "2026-08-12")

	t.Run("nights come from the range, not a count sent by the client", func(t *testing.T) {
		q := priceStay(2500, in, out, 0, 0)
		if q.Nights != 2 {
			t.Errorf("Nights = %d, want 2", q.Nights)
		}
		if q.BaseTotal != 5000 {
			t.Errorf("BaseTotal = %.2f, want 5000", q.BaseTotal)
		}
	})

	t.Run("tax applied at the tenant's configured rate", func(t *testing.T) {
		q := priceStay(2500, in, out, 18, 0)
		if q.TaxAmount != 900 {
			t.Errorf("TaxAmount = %.2f, want 900", q.TaxAmount)
		}
		if q.Payable != 5900 {
			t.Errorf("Payable = %.2f, want 5900", q.Payable)
		}
	})

	t.Run("an unconfigured rate quotes no tax rather than guessing 18", func(t *testing.T) {
		q := priceStay(2500, in, out, 0, 0)
		if q.TaxAmount != 0 {
			t.Errorf("TaxAmount = %.2f, want 0", q.TaxAmount)
		}
		if q.Payable != 5000 {
			t.Errorf("Payable = %.2f, want 5000", q.Payable)
		}
	})

	t.Run("tax is charged on the discounted base, not the list price", func(t *testing.T) {
		q := priceStay(2500, in, out, 18, 1000)
		// 5000 - 1000 = 4000 taxable, 18% = 720.
		if q.TaxAmount != 720 {
			t.Errorf("TaxAmount = %.2f, want 720 (tax on the discounted base)", q.TaxAmount)
		}
		if q.Payable != 4720 {
			t.Errorf("Payable = %.2f, want 4720", q.Payable)
		}
	})

	t.Run("a discount cannot exceed the base and make the payable negative", func(t *testing.T) {
		q := priceStay(2500, in, out, 18, 99999)
		if q.Discount != 5000 {
			t.Errorf("Discount = %.2f, want it capped at the 5000 base", q.Discount)
		}
		if q.Payable != 0 {
			t.Errorf("Payable = %.2f, want 0", q.Payable)
		}
	})

	t.Run("money is rounded, not left as a binary fraction", func(t *testing.T) {
		// 1999.99 x 3 nights = 5999.97, at 12% = 719.9964 -> 720.00.
		q := priceStay(1999.99, in, mustDate(t, "2026-08-13"), 12, 0)
		if q.BaseTotal != 5999.97 {
			t.Errorf("BaseTotal = %v, want 5999.97", q.BaseTotal)
		}
		if q.TaxAmount != 720 {
			t.Errorf("TaxAmount = %v, want 720", q.TaxAmount)
		}
		if q.Payable != 6719.97 {
			t.Errorf("Payable = %v, want 6719.97", q.Payable)
		}
	})
}

// Each payment method needs the reference someone would later use to trace the
// money. The card case is the one that matters most: a full card number must be
// refused outright, not truncated, because a truncated value would mean the
// full number had already been transmitted — into the request logs and from
// there into every database backup.
func TestReservationPaymentValidate(t *testing.T) {
	const payable = 5900

	cases := []struct {
		name    string
		pay     reservationPaymentRequest
		wantErr string
	}{
		{
			name: "upi with both references",
			pay:  reservationPaymentRequest{Method: "upi", UPIID: "guest@okaxis", TransactionRef: "TXN123"},
		},
		{
			name:    "upi without a transaction ref cannot be reconciled",
			pay:     reservationPaymentRequest{Method: "upi", UPIID: "guest@okaxis"},
			wantErr: "upi_id and transaction_ref",
		},
		{
			name:    "upi without a upi id",
			pay:     reservationPaymentRequest{Method: "upi", TransactionRef: "TXN123"},
			wantErr: "upi_id and transaction_ref",
		},
		{
			name: "card with last four and an auth code",
			pay:  reservationPaymentRequest{Method: "card", CardLast4: "4242", AuthCode: "AUTH99"},
		},
		{
			name: "card last four is optional, the auth code is not",
			pay:  reservationPaymentRequest{Method: "card", AuthCode: "AUTH99"},
		},
		{
			name:    "card without an auth code",
			pay:     reservationPaymentRequest{Method: "card", CardLast4: "4242"},
			wantErr: "auth_code",
		},
		{
			name:    "a full card number is refused, never truncated",
			pay:     reservationPaymentRequest{Method: "card", CardLast4: "4242424242424242", AuthCode: "AUTH99"},
			wantErr: "never send a full card number",
		},
		{
			name:    "non-digits in card_last4",
			pay:     reservationPaymentRequest{Method: "card", CardLast4: "42x2", AuthCode: "AUTH99"},
			wantErr: "must be 4 digits",
		},
		{
			name: "cash covering the payable",
			pay:  reservationPaymentRequest{Method: "cash", CashReceived: 6000},
		},
		{
			name: "cash exactly covering the payable",
			pay:  reservationPaymentRequest{Method: "cash", CashReceived: payable},
		},
		{
			name:    "cash short of the payable",
			pay:     reservationPaymentRequest{Method: "cash", CashReceived: 500},
			wantErr: "less than the payable",
		},
		{
			name: "credit collects nothing now",
			pay:  reservationPaymentRequest{Method: "credit"},
		},
		{
			name:    "no method",
			pay:     reservationPaymentRequest{},
			wantErr: "payment.method is required",
		},
		{
			name:    "unknown method",
			pay:     reservationPaymentRequest{Method: "crypto"},
			wantErr: "unsupported payment method",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.pay.validate(payable)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error containing %q, got none", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), c.wantErr)
			}
		})
	}
}

// The confirmation number is what staff and guests quote on the phone, and it
// is covered by a partial unique index on (hotel_id, confirmation_no) — so it
// has to be stable in shape and not collide in practice.
func TestNewConfirmationNo(t *testing.T) {
	checkIn := mustDate(t, "2026-08-10")

	got := newConfirmationNo(checkIn)
	if !strings.HasPrefix(got, "RES-20260810-") {
		t.Errorf("confirmation number %q does not carry the arrival date", got)
	}
	if strings.ToUpper(got) != got {
		t.Errorf("confirmation number %q is not uppercase — it gets read aloud and re-keyed", got)
	}

	seen := make(map[string]bool, 500)
	for i := 0; i < 500; i++ {
		n := newConfirmationNo(checkIn)
		if seen[n] {
			t.Fatalf("duplicate confirmation number %q within one arrival date", n)
		}
		seen[n] = true
	}
}
