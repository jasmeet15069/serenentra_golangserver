package domain

import (
	"testing"
	"time"
)

func TestCalendarNightsIgnoresArrivalAndDepartureClockTimes(t *testing.T) {
	checkIn := time.Date(2026, time.August, 10, 14, 0, 0, 0, time.UTC)
	checkOut := time.Date(2026, time.August, 12, 11, 0, 0, 0, time.UTC)

	if got := CalendarNights(checkIn, checkOut); got != 2 {
		t.Fatalf("CalendarNights = %d, want 2", got)
	}
}
