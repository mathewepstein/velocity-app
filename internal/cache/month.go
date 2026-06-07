package cache

import (
	"fmt"
	"time"
)

// Month is a calendar-month bucket identified by year and 1-indexed month.
// Using a concrete type (rather than time.Time or a string) catches two classes
// of bug:
//   - "what day does '2024-01' mean?" — there is no day here, so you can't
//     accidentally carry one forward.
//   - "is this month YYYY-MM or MM-YYYY?" — string parsing forces the format.
type Month struct {
	Year  int
	Month time.Month
}

// ParseMonth reads a "YYYY-MM" string into a Month. Rejects anything else,
// including abbreviations like "2024-1".
func ParseMonth(s string) (Month, error) {
	t, err := time.Parse("2006-01", s)
	if err != nil {
		return Month{}, fmt.Errorf("parse month %q: %w", s, err)
	}
	return Month{Year: t.Year(), Month: t.Month()}, nil
}

// MustParseMonth is the test-friendly form of ParseMonth that panics on bad
// input. Use in tests or constants; never with user input.
func MustParseMonth(s string) Month {
	m, err := ParseMonth(s)
	if err != nil {
		panic(err)
	}
	return m
}

// String renders a Month as "YYYY-MM".
func (m Month) String() string {
	return fmt.Sprintf("%04d-%02d", m.Year, m.Month)
}

// Start returns the first instant of the month at UTC.
func (m Month) Start() time.Time {
	return time.Date(m.Year, m.Month, 1, 0, 0, 0, 0, time.UTC)
}

// Add returns a Month n calendar months after m. Negative n goes backward.
// Handles year rollover via time.Date normalization.
func (m Month) Add(n int) Month {
	t := time.Date(m.Year, m.Month+time.Month(n), 1, 0, 0, 0, 0, time.UTC)
	return Month{Year: t.Year(), Month: t.Month()}
}

// Before reports whether m is strictly before other.
func (m Month) Before(other Month) bool {
	if m.Year != other.Year {
		return m.Year < other.Year
	}
	return m.Month < other.Month
}

// Equal reports whether m and other are the same calendar month.
func (m Month) Equal(other Month) bool {
	return m.Year == other.Year && m.Month == other.Month
}

// CurrentMonth returns the Month containing now (UTC). Inject now for
// testability — callers in production pass time.Now().
func CurrentMonth(now time.Time) Month {
	u := now.UTC()
	return Month{Year: u.Year(), Month: u.Month()}
}

// MonthsInRange returns all months from start through end, inclusive.
// Returns nil if end is before start. Order is chronological.
func MonthsInRange(start, end Month) []Month {
	if end.Before(start) {
		return nil
	}
	var out []Month
	for m := start; !end.Before(m); m = m.Add(1) {
		out = append(out, m)
	}
	return out
}
