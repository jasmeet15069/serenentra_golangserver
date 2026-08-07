package domain

import (
	"strings"
	"testing"
)

// rooms.status has no CHECK constraint, so Valid is the only thing standing
// between a typo in a request body and a room that renders as nothing on every
// board that switches on status. It is worth pinning down.
func TestRoomStatusValid(t *testing.T) {
	cases := []struct {
		in   RoomStatus
		want bool
		why  string
	}{
		{RoomStatusAvailable, true, "available is a known status"},
		{RoomStatusOccupied, true, "occupied is a known status"},
		{RoomStatusCleaning, true, "cleaning is a known status"},
		{RoomStatusMaintenance, true, "maintenance is a known status"},
		{RoomStatusOutOfOrder, true, "out_of_order is a known status"},
		{"out-of-order", false, "hyphens are not the stored form"},
		{"ooo", false, "the industry abbreviation is not what the column holds"},
		{"clean", false, "plausible-looking but wrong — the status is 'cleaning'"},
		{"dirty", false, "housekeeping vocabulary, not a room status"},
		{"Available", false, "statuses are lowercase; the DB comparison is exact"},
		{"", false, "an omitted status must not clear the field"},
	}
	for _, c := range cases {
		if got := c.in.Valid(); got != c.want {
			t.Errorf("RoomStatus(%q).Valid() = %v, want %v (%s)", c.in, got, c.want, c.why)
		}
	}
}

// The error message shown to the caller is built from this list, so it has to
// stay in step with the constants rather than drift into a stale hardcoded set.
func TestRoomStatusValuesCoversEveryValidStatus(t *testing.T) {
	values := RoomStatusValues()
	if len(values) != 5 {
		t.Fatalf("RoomStatusValues() returned %d entries, want 5: %v", len(values), values)
	}
	for _, v := range values {
		if !RoomStatus(v).Valid() {
			t.Errorf("RoomStatusValues() lists %q, which Valid() rejects", v)
		}
	}
}

// Sellable decides whether a room can be occupied at all. Occupied and cleaning
// describe the room right now and say nothing about a stay next month — that is
// why availability is decided by overlapping stays, not by status, and why
// treating them as unsellable would hide every room of a full hotel.
func TestRoomStatusSellable(t *testing.T) {
	cases := []struct {
		in   RoomStatus
		want bool
		why  string
	}{
		{RoomStatusAvailable, true, "obviously sellable"},
		{RoomStatusOccupied, true, "occupied tonight, still bookable for next month"},
		{RoomStatusCleaning, true, "being turned over, back in service within the hour"},
		{RoomStatusMaintenance, false, "someone is working on it"},
		{RoomStatusOutOfOrder, false, "withdrawn from sale entirely"},
	}
	for _, c := range cases {
		if got := c.in.Sellable(); got != c.want {
			t.Errorf("RoomStatus(%q).Sellable() = %v, want %v (%s)", c.in, got, c.want, c.why)
		}
	}
}

// The SQL predicate and Sellable() are two expressions of one rule, applied in
// four separate queries. If they disagree, the booking engine offers a room the
// handler would then refuse — so they are pinned to each other here.
func TestNonSellableRoomStatusSQLMatchesSellable(t *testing.T) {
	for _, s := range RoomStatusValues() {
		inSQL := strings.Contains(NonSellableRoomStatusSQL, "'"+s+"'")
		if inSQL == RoomStatus(s).Sellable() {
			t.Errorf("%q: listed in NonSellableRoomStatusSQL=%v but Sellable()=%v — the SQL and the Go rule disagree",
				s, inSQL, RoomStatus(s).Sellable())
		}
	}
}
