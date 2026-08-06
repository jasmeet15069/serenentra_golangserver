package domain

import "testing"

// Status is now stored and constrained by guest_stays_status_check. Valid is
// what keeps the Go constants and that CHECK from drifting apart — a status
// accepted here but rejected by the constraint fails the write at runtime
// instead of at the boundary.
func TestReservationStatusValid(t *testing.T) {
	cases := []struct {
		in   ReservationStatus
		want bool
		why  string
	}{
		{ReservationTentative, true, "held but not guaranteed"},
		{ReservationConfirmed, true, "the default for a new booking"},
		{ReservationInHouse, true, "checked in, not yet departed"},
		{ReservationCheckedOut, true, "stay complete"},
		{ReservationCancelled, true, "the state that replaced deleting the row"},
		{ReservationNoShow, true, "never arrived, still owes a fee"},
		{"pending_checkin", false, "a presentation label, not a stored state"},
		{"upcoming", false, "a presentation label, not a stored state"},
		{"checked_in", false, "the stored value is in_house"},
		{"Cancelled", false, "statuses are lowercase; the DB comparison is exact"},
		{"", false, "an omitted status must not reach the column"},
	}
	for _, c := range cases {
		if got := c.in.Valid(); got != c.want {
			t.Errorf("ReservationStatus(%q).Valid() = %v, want %v (%s)", c.in, got, c.want, c.why)
		}
	}
}

func TestReservationStatusValuesCoversEveryValidStatus(t *testing.T) {
	values := ReservationStatusValues()
	if len(values) != 6 {
		t.Fatalf("ReservationStatusValues() returned %d entries, want 6: %v", len(values), values)
	}
	for _, v := range values {
		if !ReservationStatus(v).Valid() {
			t.Errorf("ReservationStatusValues() lists %q, which Valid() rejects", v)
		}
	}
}

// Payable is what the guest owes. total_amount deliberately stayed the pre-tax
// accommodation figure so existing readers were unaffected by the discount and
// tax columns added beside it, which makes this the only place the three are
// combined — and the reason the check-out invoice was previously wrong.
func TestGuestStayPayable(t *testing.T) {
	amt := func(v float64) *float64 { return &v }

	cases := []struct {
		name string
		stay GuestStay
		want float64
	}{
		{
			name: "base only, as every pre-existing row reads",
			stay: GuestStay{TotalAmount: amt(5000)},
			want: 5000,
		},
		{
			name: "tax is added, not assumed to be included",
			stay: GuestStay{TotalAmount: amt(5000), TaxAmount: 900},
			want: 5900,
		},
		{
			name: "discount comes off before tax is added on",
			stay: GuestStay{TotalAmount: amt(5000), DiscountAmount: 500, TaxAmount: 810},
			want: 5310,
		},
		{
			name: "a null total is zero, not a nil dereference",
			stay: GuestStay{TotalAmount: nil, TaxAmount: 100},
			want: 100,
		},
	}
	for _, c := range cases {
		if got := c.stay.Payable(); got != c.want {
			t.Errorf("%s: Payable() = %.2f, want %.2f", c.name, got, c.want)
		}
	}
}
