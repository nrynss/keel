package cost

import (
	"testing"
	"time"
)

// TestNoneIsTheZeroValue pins that an unset period keeps lifetime ceilings,
// so a configuration that never sets one behaves exactly as before.
func TestNoneIsTheZeroValue(t *testing.T) {
	var unset Period
	if unset != None {
		t.Fatalf("zero period = %d, want None", int(unset))
	}
}

// TestPeriodStartPinsWindowBounds checks the calendar math one period at a
// time, including a clock outside UTC and a value the package never defined.
func TestPeriodStartPinsWindowBounds(t *testing.T) {
	ist := time.FixedZone("IST", 5*60*60+30*60)
	cases := []struct {
		name   string
		period Period
		at     time.Time
		want   time.Time
	}{
		{
			"none has no window",
			None,
			time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC),
			time.Time{},
		},
		{
			"daily starts at utc midnight",
			DailyUTC,
			time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC),
			time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			"daily folds a late hour back to midnight",
			DailyUTC,
			time.Date(2024, 3, 1, 23, 59, 59, 0, time.UTC),
			time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			"daily reads a foreign clock in utc",
			DailyUTC,
			time.Date(2024, 3, 2, 4, 0, 0, 0, ist),
			time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			"monthly starts at the first",
			MonthlyUTC,
			time.Date(2024, 3, 15, 12, 0, 0, 0, time.UTC),
			time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			"monthly keeps new year midnight",
			MonthlyUTC,
			time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			"an undefined value has no window",
			Period(99),
			time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC),
			time.Time{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.period.Start(c.at); !got.Equal(c.want) {
				t.Fatalf("start = %v, want %v", got, c.want)
			}
		})
	}
}
