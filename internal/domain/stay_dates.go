package domain

import "time"

// CalendarNights counts the dates a guest occupies, never elapsed clock hours.
// A 14:00 arrival and 11:00 departure two days later is two nights even though
// the timestamps are only 45 hours apart.
func CalendarNights(checkIn, checkOut time.Time) int {
	inDay := time.Date(checkIn.Year(), checkIn.Month(), checkIn.Day(), 0, 0, 0, 0, time.UTC)
	outDay := time.Date(checkOut.Year(), checkOut.Month(), checkOut.Day(), 0, 0, 0, 0, time.UTC)
	return int(outDay.Sub(inDay).Hours() / 24)
}
