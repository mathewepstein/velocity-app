package analyze

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
)

// Bi-weekly period model: weeks pair (W01+W02), (W03+W04), …, with period
// labels of the form "YYYY-Pnn" where nn is the *starting* (odd-numbered)
// ISO week. The ISO year of the starting week wins for the label.
//
// Periods never cross ISO years. If an ISO year ends on an odd week
// (e.g. ISO years that contain W53), the trailing odd week becomes a
// single-week period — degenerate but consistent. This matches the open
// question in the plan ("if it produces awkward boundaries, revisit"). For
// the current 24-month backfill we don't hit W53 anywhere.

// parseISOWeek parses "YYYY-Www" into (isoYear, weekNum).
func parseISOWeek(label string) (int, int, error) {
	parts := strings.SplitN(label, "-W", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("not an ISO week label: %q", label)
	}
	y, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("bad year in %q: %w", label, err)
	}
	w, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("bad week in %q: %w", label, err)
	}
	if w < 1 || w > 53 {
		return 0, 0, fmt.Errorf("week %d out of range in %q", w, label)
	}
	return y, w, nil
}

// periodForWeek returns the bi-weekly period label for an ISO week label.
// Even weeks fold back to the previous odd week.
func periodForWeek(weekLabel string) (string, error) {
	year, week, err := parseISOWeek(weekLabel)
	if err != nil {
		return "", err
	}
	start := week
	if week%2 == 0 {
		start = week - 1
	}
	return fmt.Sprintf("%04d-P%02d", year, start), nil
}

// parsePeriodLabel parses "YYYY-Pnn" into (isoYear, startWeek).
func parsePeriodLabel(label string) (int, int, error) {
	parts := strings.SplitN(label, "-P", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("not a period label: %q", label)
	}
	y, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("bad year in %q: %w", label, err)
	}
	w, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("bad start week in %q: %w", label, err)
	}
	if w < 1 || w%2 == 0 {
		return 0, 0, fmt.Errorf("period start week %d must be odd in %q", w, label)
	}
	return y, w, nil
}

// isoWeekStartMonday returns the Monday (UTC, 00:00) of ISO week (year, week).
// Implementation: ISO week 1 of any year is the week containing the first
// Thursday of that year. We anchor on Jan 4 (always in week 1), find its
// Monday, then advance (week-1) weeks.
func isoWeekStartMonday(isoYear, week int) time.Time {
	jan4 := time.Date(isoYear, 1, 4, 0, 0, 0, 0, time.UTC)
	wd := int(jan4.Weekday())
	if wd == 0 {
		wd = 7
	}
	mondayOfW1 := jan4.AddDate(0, 0, -(wd - 1))
	return mondayOfW1.AddDate(0, 0, 7*(week-1))
}

// periodEnd returns the end-of-day Sunday closing the *second* week of the
// named period (or the only week, for an orphan trailing period). Used to
// decide whether a period is complete.
func periodEnd(periodLabel string) (time.Time, error) {
	year, start, err := parsePeriodLabel(periodLabel)
	if err != nil {
		return time.Time{}, err
	}
	// The second week of (year, start) is week (start+1). If that crosses
	// into a new ISO year (start was the trailing odd week of the previous
	// year), this is a degenerate single-week period and we use start's
	// Sunday as the end.
	secondMonday := isoWeekStartMonday(year, start+1)
	// Detect roll-over into next ISO year by comparing actual ISO week.
	secY, secW := secondMonday.ISOWeek()
	if secY != year || secW != start+1 {
		// Degenerate: this is a trailing single-week period.
		firstMonday := isoWeekStartMonday(year, start)
		return firstMonday.AddDate(0, 0, 6).Add(24*time.Hour - time.Nanosecond), nil
	}
	return secondMonday.AddDate(0, 0, 6).Add(24*time.Hour - time.Nanosecond), nil
}

// periodStart returns the Monday (UTC, 00:00) opening the named period.
func periodStart(periodLabel string) (time.Time, error) {
	year, start, err := parsePeriodLabel(periodLabel)
	if err != nil {
		return time.Time{}, err
	}
	return isoWeekStartMonday(year, start), nil
}

// biweeklyPeriodsInRange returns the ordered, deduplicated list of period
// labels that any ISO week in [start, end] (months) falls into.
func biweeklyPeriodsInRange(start, end cache.Month) ([]string, error) {
	weeks := isoWeeksInRange(start, end)
	seen := map[string]struct{}{}
	out := make([]string, 0, len(weeks)/2+1)
	for _, w := range weeks {
		p, err := periodForWeek(w)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out, nil
}

// completedPeriodsBetween filters biweeklyPeriodsInRange(start, end) down to
// the periods whose periodEnd is strictly before `now`. The current
// in-progress period is intentionally excluded — Elo only fires after a
// period has fully closed.
func completedPeriodsBetween(start, end cache.Month, now time.Time) ([]string, error) {
	all, err := biweeklyPeriodsInRange(start, end)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(all))
	for _, p := range all {
		pe, err := periodEnd(p)
		if err != nil {
			return nil, err
		}
		if pe.Before(now) {
			out = append(out, p)
		}
	}
	return out, nil
}
