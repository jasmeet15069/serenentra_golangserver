package domain

import "testing"

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
	if len(values) != 4 {
		t.Fatalf("RoomStatusValues() returned %d entries, want 4: %v", len(values), values)
	}
	for _, v := range values {
		if !RoomStatus(v).Valid() {
			t.Errorf("RoomStatusValues() lists %q, which Valid() rejects", v)
		}
	}
}
