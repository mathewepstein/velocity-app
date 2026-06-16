package analyze

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

// isoWeek formats a time as "YYYY-Www" using the ISO week-date year. The ISO
// year can differ from the calendar year near year boundaries (e.g., 2025-01-01
// is ISO week 2025-W01, but 2024-12-30 is already ISO week 2025-W01 too).
func isoWeek(t time.Time) string {
	y, w := t.UTC().ISOWeek()
	return fmt.Sprintf("%04d-W%02d", y, w)
}

// monthKey renders a time as "YYYY-MM".
func monthKey(t time.Time) string {
	u := t.UTC()
	return fmt.Sprintf("%04d-%02d", u.Year(), u.Month())
}

// monthInRange reports whether YYYY-MM m lies in [start, end] inclusive.
func monthInRange(m string, start, end cache.Month) bool {
	parsed, err := cache.ParseMonth(m)
	if err != nil {
		return false
	}
	if parsed.Before(start) {
		return false
	}
	if end.Before(parsed) {
		return false
	}
	return true
}

// rollupMonthly aggregates every record that falls inside [start, end] into a
// continuous slice of MonthlyRow (one row per month, zeros filled). Touched
// Jira issues count per cache month (updated month); created/resolved use
// their own timestamps. PR merged + LoC are attributed to merged_at, not
// created_at, since the UI "PRs merged" chart should spike when work ships.
func rollupMonthly(data *Loaded, start, end cache.Month, ci config.CodeImpactConfig) []MonthlyRow {
	months := cache.MonthsInRange(start, end)
	byMonth := make(map[string]*MonthlyRow, len(months))
	filesByMonth := make(map[string]map[string]struct{}, len(months))
	for _, m := range months {
		key := m.String()
		byMonth[key] = &MonthlyRow{Month: key}
		filesByMonth[key] = map[string]struct{}{}
	}
	touch := func(key string, fn func(*MonthlyRow)) {
		row, ok := byMonth[key]
		if !ok {
			return
		}
		fn(row)
	}

	for _, i := range data.Issues {
		touch(monthKey(i.Updated), func(r *MonthlyRow) { r.JiraIssuesTouched++ })
		if monthInRange(monthKey(i.Created), start, end) {
			touch(monthKey(i.Created), func(r *MonthlyRow) { r.JiraIssuesCreated++ })
		}
		if i.Resolved != nil && monthInRange(monthKey(*i.Resolved), start, end) {
			sp := i.StoryPoints
			touch(monthKey(*i.Resolved), func(r *MonthlyRow) {
				r.JiraIssuesResolved++
				// SP is only counted for scored tickets (SP > 0); an SP of 0
				// means "not scored yet", so it neither adds to the sum nor to
				// the scored-ticket denominator used for the average.
				if sp > 0 {
					r.StoryPoints += sp
					r.ScoredTicketsResolved++
				}
			})
		}
	}
	for _, p := range data.PRs {
		if monthInRange(monthKey(p.Created), start, end) {
			touch(monthKey(p.Created), func(r *MonthlyRow) { r.PRsCreated++ })
		}
		if p.Merged != nil && monthInRange(monthKey(*p.Merged), start, end) {
			mk := monthKey(*p.Merged)
			touch(mk, func(r *MonthlyRow) {
				r.PRsMerged++
				r.LOCAdded += p.Additions
				r.LOCDeleted += p.Deletions
			})
			if files, ok := filesByMonth[mk]; ok {
				for _, f := range p.Files {
					files[f] = struct{}{}
				}
			}
		}
	}
	for _, c := range data.Commits {
		touch(monthKey(c.Committed), func(r *MonthlyRow) { r.Commits++ })
	}
	for _, r := range data.Reviews {
		if monthInRange(monthKey(r.Submitted), start, end) {
			touch(monthKey(r.Submitted), func(row *MonthlyRow) { row.PRsReviewed++ })
		}
	}

	out := make([]MonthlyRow, 0, len(months))
	for _, m := range months {
		key := m.String()
		row := byMonth[key]
		row.UniqueFilesTouched = len(filesByMonth[key])
		row.CodeImpact = computeCodeImpactFloat(float64(row.UniqueFilesTouched), weightedLOC(row.LOCAdded, row.LOCDeleted, ci), row.PRsMerged, ci)
		out = append(out, *row)
	}
	return out
}

// rollupWeekly produces a continuous weekly series for [start, end]. We walk
// the range day by day building the ordered list of ISO week labels so weeks
// that straddle month boundaries appear exactly once.
func rollupWeekly(data *Loaded, start, end cache.Month, ci config.CodeImpactConfig) []WeeklyRow {
	weeks := isoWeeksInRange(start, end)
	byWeek := make(map[string]*WeeklyRow, len(weeks))
	filesByWeek := make(map[string]map[string]struct{}, len(weeks))
	for _, w := range weeks {
		byWeek[w] = &WeeklyRow{Week: w}
		filesByWeek[w] = map[string]struct{}{}
	}
	touch := func(key string, fn func(*WeeklyRow)) {
		row, ok := byWeek[key]
		if !ok {
			return
		}
		fn(row)
	}
	inRange := func(t time.Time) bool {
		return !t.Before(start.Start()) && t.Before(end.Add(1).Start())
	}

	for _, i := range data.Issues {
		if inRange(i.Updated) {
			touch(isoWeek(i.Updated), func(r *WeeklyRow) { r.JiraIssuesTouched++ })
		}
		if inRange(i.Created) {
			touch(isoWeek(i.Created), func(r *WeeklyRow) { r.JiraIssuesCreated++ })
		}
		if i.Resolved != nil && inRange(*i.Resolved) {
			sp := i.StoryPoints
			touch(isoWeek(*i.Resolved), func(r *WeeklyRow) {
				r.JiraIssuesResolved++
				if sp > 0 {
					r.StoryPoints += sp
					r.ScoredTicketsResolved++
				}
			})
		}
	}
	for _, p := range data.PRs {
		if inRange(p.Created) {
			touch(isoWeek(p.Created), func(r *WeeklyRow) { r.PRsCreated++ })
		}
		if p.Merged != nil && inRange(*p.Merged) {
			wk := isoWeek(*p.Merged)
			touch(wk, func(r *WeeklyRow) {
				r.PRsMerged++
				r.LOCAdded += p.Additions
				r.LOCDeleted += p.Deletions
			})
			if files, ok := filesByWeek[wk]; ok {
				for _, f := range p.Files {
					files[f] = struct{}{}
				}
			}
		}
	}
	for _, c := range data.Commits {
		if inRange(c.Committed) {
			touch(isoWeek(c.Committed), func(r *WeeklyRow) { r.Commits++ })
		}
	}
	for _, r := range data.Reviews {
		if inRange(r.Submitted) {
			touch(isoWeek(r.Submitted), func(row *WeeklyRow) { row.PRsReviewed++ })
		}
	}

	out := make([]WeeklyRow, 0, len(weeks))
	for _, w := range weeks {
		row := byWeek[w]
		row.UniqueFilesTouched = len(filesByWeek[w])
		row.CodeImpact = computeCodeImpactFloat(float64(row.UniqueFilesTouched), weightedLOC(row.LOCAdded, row.LOCDeleted, ci), row.PRsMerged, ci)
		out = append(out, *row)
	}
	return out
}

// isoWeeksInRange returns the ordered list of unique ISO week labels that
// cover [start, end] (month-level start/end). Walks by week to avoid 365×
// day iteration across full history.
func isoWeeksInRange(start, end cache.Month) []string {
	first := start.Start()
	// Back up to the Monday of the ISO week containing first.
	weekday := int(first.Weekday())
	if weekday == 0 {
		weekday = 7 // Sunday → 7 so Monday lands on 1
	}
	cursor := first.AddDate(0, 0, -(weekday - 1))
	limit := end.Add(1).Start() // exclusive
	var out []string
	seen := map[string]struct{}{}
	for cursor.Before(limit) {
		label := isoWeek(cursor)
		if _, ok := seen[label]; !ok {
			seen[label] = struct{}{}
			out = append(out, label)
		}
		cursor = cursor.AddDate(0, 0, 7)
	}
	// One more check: the last day of `end` might belong to an ISO week whose
	// Monday is *after* our last cursor step if the range length is not a
	// multiple of 7. Covered because we stepped past limit.
	sort.Strings(out)
	return out
}

// totalsFromMonthly sums MonthlyRows into a Totals. ActiveWeeks is not
// derivable from monthly data and must be supplied by the caller from the
// weekly slice (see windowTotals). UniqueFilesTouched / CodeImpact are NOT
// derivable from row sums — a file touched in multiple months should count
// once across the window, not once per month — so callers must fill them
// via uniqueFilesInWindow + computeCodeImpact (or applyCodeImpactCap when
// team-level capping is in play).
func totalsFromMonthly(rows []MonthlyRow) Totals {
	var t Totals
	for _, r := range rows {
		t.JiraIssuesTouched += r.JiraIssuesTouched
		t.JiraIssuesCreated += r.JiraIssuesCreated
		t.JiraIssuesResolved += r.JiraIssuesResolved
		t.StoryPoints += r.StoryPoints
		t.ScoredTicketsResolved += r.ScoredTicketsResolved
		t.PRsCreated += r.PRsCreated
		t.PRsMerged += r.PRsMerged
		t.PRsReviewed += r.PRsReviewed
		t.Commits += r.Commits
		t.LOCAdded += r.LOCAdded
		t.LOCDeleted += r.LOCDeleted
	}
	return t
}

// uniqueFilesInWindow returns the cardinality of the union of file paths
// across every merged PR in [start, end]. Walking the scoped Loaded directly
// avoids the per-row-sum pitfall (a file touched in two different months
// would otherwise be counted twice).
func uniqueFilesInWindow(data *Loaded, start, end cache.Month) int {
	files := map[string]struct{}{}
	for _, p := range data.PRs {
		if p.Merged == nil {
			continue
		}
		if !monthInRange(monthKey(*p.Merged), start, end) {
			continue
		}
		for _, f := range p.Files {
			files[f] = struct{}{}
		}
	}
	return len(files)
}

// computeCodeImpact is the pure formula `sqrt(α·F + β·L + γ·P)` clamped at
// zero. Used at row scope (uncapped LOC, raw int file cardinality). NaN-safe:
// a fully-zero input returns 0.
func computeCodeImpact(uniqueFiles, locDelta, mergedPRs int, ci config.CodeImpactConfig) float64 {
	return computeCodeImpactFloat(float64(uniqueFiles), float64(locDelta), mergedPRs, ci)
}

// computeCodeImpactFloat is the float-input variant used at window scope so
// the gen-file-weighted effective file count (a float because matched files
// contribute fractionally per Phase 6.2) can flow in without rounding. The
// formula is identical otherwise.
func computeCodeImpactFloat(effectiveFiles, locDelta float64, mergedPRs int, ci config.CodeImpactConfig) float64 {
	return codeImpactFormula(effectiveFiles, locDelta, float64(mergedPRs), ci)
}

// codeImpactFormula is the underlying `sqrt(α·F + β·L + γ·P)` with a float P
// term, so the γ·merged input can carry the integration-down-weighted merged
// count (a flagged PR contributes its factor, not a whole 1). computeCodeImpactFloat
// is the int-P wrapper for callers that don't down-weight.
func codeImpactFormula(effectiveFiles, locDelta, mergedPRs float64, ci config.CodeImpactConfig) float64 {
	sum := ci.Alpha*effectiveFiles + ci.Beta*locDelta + ci.Gamma*mergedPRs
	if sum <= 0 {
		return 0
	}
	return math.Sqrt(sum)
}

// activeWeeksCount counts weekly rows with any non-zero signal. Reviews count
// toward activity so reviewer-heavy devs aren't flagged inactive.
func activeWeeksCount(weekly []WeeklyRow) int {
	n := 0
	for _, w := range weekly {
		if w.JiraIssuesTouched+w.JiraIssuesCreated+w.JiraIssuesResolved+w.PRsCreated+w.PRsMerged+w.PRsReviewed+w.Commits > 0 {
			n++
		}
	}
	return n
}
