package analyze

import (
	"math"
	"sort"

	"github.com/mathewepstein/velocity/internal/config"
)

// scoreMetric names every signal that contributes to the composite score.
// Keys match the TOML config (`[profiles.<p>.scoring.weights]`) and the
// JSON breakdown shape so config, code, and UI stay in sync.
const (
	metricPRsMerged          = "prs_merged"
	metricJiraIssuesResolved = "jira_issues_resolved"
	metricCodeImpact         = "code_impact"
	metricPRsReviewed        = "prs_reviewed"
	metricPRsCreated         = "prs_created"
	metricJiraIssuesProgressed = "jira_issues_progressed"
	metricJiraIssuesCreated  = "jira_issues_created"
	metricActiveWeeks        = "active_weeks"
	metricStoryPoints        = "story_points"
	metricLOCChanged         = "loc_changed"
	metricCommits            = "commits"
)

// allMetrics is the canonical iteration order for breakdown serialization
// and deterministic test output. commits stays at the tail with a default
// weight of zero — a user can still re-enable it via [scoring.weights] for
// experimentation without code changes.
var allMetrics = []string{
	metricPRsMerged,
	metricJiraIssuesResolved,
	metricCodeImpact,
	metricPRsReviewed,
	metricPRsCreated,
	metricJiraIssuesProgressed,
	metricJiraIssuesCreated,
	metricActiveWeeks,
	metricStoryPoints,
	metricLOCChanged,
	metricCommits,
}

// isGameableMetric reports whether the silent Phase 6.2 effort multiplier
// applies. The three metrics most susceptible to commit-spam / LOC-stuffing
// get dampened before z-scoring; other inputs (PR counts, Jira activity,
// active weeks, story points) are left untouched.
func isGameableMetric(m string) bool {
	switch m {
	case metricCommits, metricLOCChanged, metricCodeImpact:
		return true
	}
	return false
}

// isCoverageGatedMetric reports whether a zero value means "no data for this
// dev" rather than "zero output". story_points is the only such metric: story
// points are always positive when present, so an SP sum of 0 means the dev's
// resolved tickets simply haven't been SP-scored yet (CD scores tickets
// post-hoc via score-ticket, often sparsely). A coverage-gated metric builds
// its z-distribution over devs WITH data only and assigns a neutral z=0 to
// devs without, so unscored work never drags a dev's composite down.
func isCoverageGatedMetric(m string) bool {
	return m == metricStoryPoints
}

// rawMetricValue extracts the unscaled per-dev value for one metric.
// loc_changed combines additions + deletions; the p95-cap + sqrt transforms
// happen later in computeContributorScores once team-wide context is known.
// code_impact reads Totals.CodeImpact, which applyCodeImpactCap fills with
// the team-p95-capped sqrt composite — already dampened, so it lands raw
// into z-scoring like the other established metrics.
// story_points returns the AVERAGE SP per *scored* resolved ticket (a
// complexity signal), not the sum — so it doesn't double-count ticket volume,
// which jira_issues_resolved already rewards. 0 when the dev has no scored
// tickets; coverage-gating then reads that 0 as missing data (neutral z), not
// low effort. Coverage depends on the SP field being populated (CD historically
// didn't; score-ticket backfills it). Totals.StoryPoints keeps the raw sum for
// display / macro "SP completed" aggregation.
func rawMetricValue(t Totals, metric string) float64 {
	switch metric {
	case metricPRsMerged:
		return float64(t.PRsMerged)
	case metricJiraIssuesResolved:
		return float64(t.JiraIssuesResolved)
	case metricCodeImpact:
		return t.CodeImpact
	case metricPRsReviewed:
		return float64(t.PRsReviewed)
	case metricPRsCreated:
		return float64(t.PRsCreated)
	case metricJiraIssuesProgressed:
		return float64(t.JiraIssuesProgressed)
	case metricJiraIssuesCreated:
		// B5 planning credit: raw distinct issues the dev filed (reporter-
		// attributed). sqrt-saturated in computeContributorScores so filing
		// volume can't be farmed into rank.
		return float64(t.JiraIssuesCreated)
	case metricActiveWeeks:
		return float64(t.ActiveWeeks)
	case metricStoryPoints:
		if t.ScoredTicketsResolved == 0 {
			return 0
		}
		return t.StoryPoints / float64(t.ScoredTicketsResolved)
	case metricLOCChanged:
		return float64(t.LOCAdded + t.LOCDeleted)
	case metricCommits:
		return float64(t.Commits)
	}
	return 0
}

// computeContributorScores attaches a ContributorScore to every mapped dev
// in devs using the A4 model: weighted sum of per-metric z-scores across
// the team in the same window. The synthetic "unknown" bucket is skipped
// (no rank, no score) so the leaderboard reflects only real, mapped people.
//
// loc_changed gets the documented double-dampening: cap at the team's 95th
// percentile, then sqrt before z-scoring. The cap neutralizes a one-week
// refactor; the sqrt dampens scale further.
//
// commits, loc_changed, and code_impact additionally get the Phase 6.2
// silent effort multiplier applied before z-scoring — dampens contributions
// when patterns suggest commit-spam or LOC-stuffing. The multiplier never
// surfaces in metrics.json; only the dampened z-score contributions land in
// the breakdown.
//
// When stdev is 0 for a metric (e.g., single-dev team, or a metric whose
// column is uniformly zero — story_points until SP coverage exists), every
// dev's z for that metric is 0 — the metric simply doesn't move the score.
//
// story_points is additionally coverage-gated (isCoverageGatedMetric): its
// distribution is built over devs that have SP data, and devs without any
// scored tickets get a neutral z=0 instead of a below-mean negative — an
// unscored ticket is missing data, not zero effort, so it never penalizes.
func computeContributorScores(devs []DevWindowMetrics, weights map[string]float64, norm config.NormalizeConfig) []DevWindowMetrics {
	scoreable := make([]int, 0, len(devs))
	for i := range devs {
		if devs[i].Dev.DisplayName == "unknown" {
			continue
		}
		scoreable = append(scoreable, i)
	}
	if len(scoreable) == 0 {
		return devs
	}

	// Per-dev silent dampening for the gameable inputs. The cohort signal
	// (teamLOCPerFile) is computed once across scoreable devs and reused for
	// each dev's stuff-ratio comparison.
	teamLOCPerFile := teamLOCPerFileDistribution(devs)
	multipliers := make([]float64, len(scoreable))
	for idx, di := range scoreable {
		multipliers[idx] = effortMultiplier(devs[di].Totals, teamLOCPerFile, norm)
	}

	// Build a feature matrix per metric, applying loc_changed's p95-cap +
	// sqrt up front so the z-score computation sees the transformed values.
	values := make(map[string][]float64, len(allMetrics))
	for _, m := range allMetrics {
		col := make([]float64, len(scoreable))
		for idx, di := range scoreable {
			col[idx] = rawMetricValue(devs[di].Totals, m)
			if isGameableMetric(m) {
				col[idx] *= multipliers[idx]
			}
		}
		if m == metricLOCChanged {
			cap95 := percentile(col, 95)
			for idx := range col {
				if col[idx] > cap95 {
					col[idx] = cap95
				}
				col[idx] = math.Sqrt(col[idx])
			}
		}
		// B5: sqrt-saturate jira_issues_created so a heavy ticket-filer's
		// volume advantage compresses (10x filed ≈ 3x credit) — rewards that
		// planning happened, never filing count. No p95 cap; sqrt alone
		// saturates and the low weight keeps it a minor nudge.
		if m == metricJiraIssuesCreated {
			for idx := range col {
				col[idx] = math.Sqrt(col[idx])
			}
		}
		values[m] = col
	}

	// z-score each metric column, then sum weight*z per dev.
	totals := make([]float64, len(scoreable))
	breakdowns := make([]map[string]float64, len(scoreable))
	for idx := range scoreable {
		breakdowns[idx] = make(map[string]float64, len(allMetrics))
	}
	for _, m := range allMetrics {
		col := values[m]
		w := weights[m]

		// Coverage-gated metrics (story_points) compute their distribution over
		// devs WITH data only; devs with a zero value have no data (not zero
		// effort) and get a neutral z=0 below, so missing scores never penalize.
		gated := isCoverageGatedMetric(m)
		statCol := col
		if gated {
			statCol = make([]float64, 0, len(col))
			for _, v := range col {
				if v > 0 {
					statCol = append(statCol, v)
				}
			}
		}
		mean, stdev := meanStdev(statCol)
		// Treat machine-epsilon stdev as zero: when every value matches
		// post-transform (e.g. LOC after p95 cap collapses everyone to the
		// baseline), floating-point summation can leave stdev at ~1e-15
		// and amplify it into spurious unit-scale z-scores.
		stdevIsZero := stdev < 1e-12*math.Max(1, math.Abs(mean))
		for idx := range col {
			var z float64
			switch {
			case gated && col[idx] <= 0:
				z = 0 // no data for this dev → neutral, never a penalty
			case !stdevIsZero:
				z = (col[idx] - mean) / stdev
			}
			contrib := w * z
			totals[idx] += contrib
			breakdowns[idx][m] = contrib
		}
	}

	// Attach scores; ranks are filled in after a sort.
	type ranked struct {
		di    int
		total float64
	}
	ordered := make([]ranked, len(scoreable))
	for idx, di := range scoreable {
		ordered[idx] = ranked{di: di, total: totals[idx]}
		devs[di].Score = &ContributorScore{
			Total:     totals[idx],
			Breakdown: breakdowns[idx],
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].total > ordered[j].total
	})
	for rank, r := range ordered {
		devs[r.di].Score.Rank = rank + 1
	}
	return devs
}

// meanStdev returns the population mean and stdev of xs. stdev is 0 for an
// empty or single-element slice, and intentionally NaN-safe (no NaN in,
// no NaN out).
func meanStdev(xs []float64) (mean, stdev float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean = sum / float64(len(xs))
	if len(xs) == 1 {
		return mean, 0
	}
	var sq float64
	for _, x := range xs {
		d := x - mean
		sq += d * d
	}
	stdev = math.Sqrt(sq / float64(len(xs)))
	return mean, stdev
}

// percentile returns the p-th percentile of xs using linear interpolation
// between the two surrounding ranks (NumPy "linear" / R type-7).
// p is on [0, 100]. Returns 0 for empty input.
func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := make([]float64, len(xs))
	copy(sorted, xs)
	sort.Float64s(sorted)
	if len(sorted) == 1 {
		return sorted[0]
	}
	rank := (p / 100.0) * float64(len(sorted)-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return sorted[lo]
	}
	frac := rank - float64(lo)
	return sorted[lo] + frac*(sorted[hi]-sorted[lo])
}
