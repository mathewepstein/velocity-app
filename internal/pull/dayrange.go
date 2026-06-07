package pull

import (
	"fmt"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
)

// dayRange is an inclusive day window in UTC, used for GitHub Search
// qualifiers like `created:YYYY-MM-DD..YYYY-MM-DD` and
// `committer-date:YYYY-MM-DD..YYYY-MM-DD`. Both endpoints are inclusive.
type dayRange struct {
	Start time.Time
	End   time.Time
}

func (r dayRange) String() string {
	return fmt.Sprintf("%s..%s", r.Start.Format("2006-01-02"), r.End.Format("2006-01-02"))
}

// Days returns the inclusive day count; a single-day range returns 1.
func (r dayRange) Days() int {
	return int(r.End.Sub(r.Start).Hours()/24) + 1
}

// Bisect splits r into two non-overlapping, inclusive halves. Caller must
// ensure r.Days() >= 2.
func (r dayRange) Bisect() (dayRange, dayRange) {
	days := r.Days()
	leftDays := days / 2
	midEnd := r.Start.AddDate(0, 0, leftDays-1)
	rightStart := r.Start.AddDate(0, 0, leftDays)
	return dayRange{Start: r.Start, End: midEnd}, dayRange{Start: rightStart, End: r.End}
}

// monthRange returns the inclusive day window covering cache.Month m.
func monthRange(m cache.Month) dayRange {
	start := m.Start()
	end := m.Add(1).Start().AddDate(0, 0, -1)
	return dayRange{Start: start, End: end}
}
