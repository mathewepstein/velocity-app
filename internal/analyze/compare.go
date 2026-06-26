package analyze

import (
	"fmt"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

// quartersToShow is the number of most-recent ISO calendar quarters included
// in the Quarters slice. 4 gives a year-over-year view out of the box.
const quartersToShow = 4

// buildWindow computes one WindowMetrics from Loaded data over [start, end].
// label lets callers tag the view ("Current", "Prior", "YoY", "2025-Q4"…).
// CodeImpact on the returned Totals uses uncapped LOC — single-user windows
// have no team distribution to cap against, so this matches the per-row
// convention. Multi-dev windows that need the team-p95 cap apply it via
// applyCodeImpactCap downstream.
func buildWindow(data *Loaded, start, end cache.Month, label string, ci config.CodeImpactConfig, norm config.NormalizeConfig) WindowMetrics {
	monthly := rollupMonthly(data, start, end, ci, norm)
	weekly := rollupWeekly(data, start, end, ci, norm)
	totals := totalsFromMonthly(monthly)
	totals.ActiveWeeks = activeWeeksCount(weekly)
	totals.UniqueFilesTouched = uniqueFilesInWindow(data, start, end)
	// Net generated-file LOC out of the code_impact LOC term (LOCAdded/Deleted
	// on Totals stay raw for display).
	locDelta := totals.LOCAdded + totals.LOCDeleted - excludedLOCInWindow(data, start, end, ci, norm)
	if locDelta < 0 {
		locDelta = 0
	}
	totals.CodeImpact = computeCodeImpact(totals.UniqueFilesTouched, locDelta, totals.PRsMerged, ci)
	return WindowMetrics{
		Label: label,
		Window: WindowRange{
			Start:        start.String(),
			End:          end.String(),
			LengthMonths: len(cache.MonthsInRange(start, end)),
		},
		Totals:  totals,
		Monthly: monthly,
		Weekly:  weekly,
	}
}

// currentWindow returns the default current view: the last `length` months
// ending at currentMonth (inclusive). Length clamps to ≥1 so callers can't
// produce an empty window by feeding in garbage config.
func currentWindow(data *Loaded, currentMonth cache.Month, length int, ci config.CodeImpactConfig, norm config.NormalizeConfig) WindowMetrics {
	if length < 1 {
		length = 1
	}
	start := currentMonth.Add(-(length - 1))
	return buildWindow(data, start, currentMonth, "Current", ci, norm)
}

// priorWindow is the same-length window immediately preceding the current.
// e.g., current = 2026-01..2026-04 → prior = 2025-09..2025-12.
func priorWindow(data *Loaded, current WindowMetrics, ci config.CodeImpactConfig, norm config.NormalizeConfig) WindowMetrics {
	length := current.Window.LengthMonths
	curStart, _ := cache.ParseMonth(current.Window.Start)
	end := curStart.Add(-1)
	start := end.Add(-(length - 1))
	return buildWindow(data, start, end, "Prior", ci, norm)
}

// yoyWindow is the same calendar months, one year earlier.
// e.g., current = 2026-01..2026-04 → yoy = 2025-01..2025-04.
func yoyWindow(data *Loaded, current WindowMetrics, ci config.CodeImpactConfig, norm config.NormalizeConfig) WindowMetrics {
	curStart, _ := cache.ParseMonth(current.Window.Start)
	curEnd, _ := cache.ParseMonth(current.Window.End)
	start := curStart.Add(-12)
	end := curEnd.Add(-12)
	return buildWindow(data, start, end, "YoY", ci, norm)
}

// lastQuarters returns the most recent N ISO calendar quarters, oldest-first.
// The current (possibly partial) quarter is the last entry.
func lastQuarters(data *Loaded, currentMonth cache.Month, n int, ci config.CodeImpactConfig, norm config.NormalizeConfig) []WindowMetrics {
	if n < 1 {
		return nil
	}
	curQ := quarterOf(currentMonth)
	out := make([]WindowMetrics, 0, n)
	// Iterate oldest → newest so the JSON order matches the chart order.
	for i := n - 1; i >= 0; i-- {
		q := curQ.add(-i)
		start, end := q.monthRange()
		out = append(out, buildWindow(data, start, end, q.label(), ci, norm))
	}
	return out
}

// fullHistory sweeps from backfill_start to currentMonth producing the
// monthly sparkline. No weekly data — the full history is too long to hold
// a weekly series usefully (300+ weeks).
func fullHistory(data *Loaded, backfillStart, currentMonth cache.Month, ci config.CodeImpactConfig, norm config.NormalizeConfig) HistoryRollup {
	return HistoryRollup{Monthly: rollupMonthly(data, backfillStart, currentMonth, ci, norm)}
}

// quarter represents an ISO calendar quarter (1-4) of a given year.
type quarter struct {
	Year int
	Q    int // 1..4
}

func quarterOf(m cache.Month) quarter {
	return quarter{Year: m.Year, Q: (int(m.Month)-1)/3 + 1}
}

// add returns the quarter n quarters after q (n may be negative). Year rolls
// over via integer division.
func (q quarter) add(n int) quarter {
	total := q.Year*4 + (q.Q - 1) + n
	year := total / 4
	qi := total%4 + 1
	// total%4 can be negative in Go for negative dividends; normalize.
	if qi < 1 {
		qi += 4
		year--
	}
	return quarter{Year: year, Q: qi}
}

// monthRange returns the first and last month of q as cache.Month values.
func (q quarter) monthRange() (cache.Month, cache.Month) {
	firstMonth := (q.Q-1)*3 + 1
	start := cache.MustParseMonth(fmt.Sprintf("%04d-%02d", q.Year, firstMonth))
	return start, start.Add(2)
}

func (q quarter) label() string {
	return fmt.Sprintf("%04d-Q%d", q.Year, q.Q)
}
