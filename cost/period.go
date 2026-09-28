package cost

import "time"

// Period bounds a durable budget ceiling to a repeating window. None
// keeps one lifetime ceiling, which is the zero value and the behaviour
// every budget has today. DailyUTC restarts the ceiling at each UTC
// midnight and MonthlyUTC at each UTC month start. A durable store reads
// this value from its own configuration. The in-memory Budget and
// KeyedBudget keep lifetime ceilings, because neither owns a clock.
type Period int

const (
	// None keeps one lifetime ceiling over all spend ever booked. It is
	// the zero value, so a configuration that never sets a period keeps
	// the behaviour every budget has today.
	None Period = iota
	// DailyUTC restarts the ceiling at each UTC midnight, so a limit
	// bounds one calendar day.
	DailyUTC
	// MonthlyUTC restarts the ceiling at each UTC month start, so a
	// limit bounds one calendar month.
	MonthlyUTC
)

// Start returns the instant the window holding t began. A daily window
// begins at its UTC midnight and a monthly one at its first UTC
// midnight. None has no window, so it returns the zero time.
func (p Period) Start(t time.Time) time.Time {
	switch p {
	case DailyUTC:
		year, month, day := t.UTC().Date()
		return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	case MonthlyUTC:
		year, month, _ := t.UTC().Date()
		return time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	default:
		return time.Time{}
	}
}
