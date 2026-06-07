package analyze

import (
	"testing"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

// TestStoryPointsRollupCreditedAtResolution verifies SP is summed onto the
// resolution month (mirroring jira_issues_resolved) and that unresolved or
// out-of-window issues contribute nothing.
func TestStoryPointsRollupCreditedAtResolution(t *testing.T) {
	data := &Loaded{
		Issues: []cache.JiraIssue{
			// Resolved in-window (March) → 5 SP credited to March.
			{
				Key: "CD-1", Created: mustTime("2026-01-05T00:00:00Z"),
				Updated:     mustTime("2026-03-01T00:00:00Z"),
				Resolved:    ptrTime(mustTime("2026-03-10T00:00:00Z")),
				StoryPoints: 5,
			},
			// Resolved in-window (March) → another 3 SP to March.
			{
				Key: "CD-2", Created: mustTime("2026-02-01T00:00:00Z"),
				Updated:     mustTime("2026-03-02T00:00:00Z"),
				Resolved:    ptrTime(mustTime("2026-03-20T00:00:00Z")),
				StoryPoints: 3,
			},
			// Unresolved but carries SP → must NOT count.
			{
				Key: "CD-3", Created: mustTime("2026-02-01T00:00:00Z"),
				Updated:     mustTime("2026-02-05T00:00:00Z"),
				StoryPoints: 99,
			},
			// Resolved OUT of window → must NOT count.
			{
				Key: "CD-4", Created: mustTime("2026-01-01T00:00:00Z"),
				Updated:     mustTime("2026-09-01T00:00:00Z"),
				Resolved:    ptrTime(mustTime("2026-09-15T00:00:00Z")),
				StoryPoints: 13,
			},
		},
	}

	start := cache.MustParseMonth("2026-01")
	end := cache.MustParseMonth("2026-04")
	rows := rollupMonthly(data, start, end, testCI())

	// rows[2] == 2026-03
	if rows[2].Month != "2026-03" {
		t.Fatalf("expected rows[2] to be 2026-03, got %s", rows[2].Month)
	}
	if rows[2].StoryPoints != 8 {
		t.Errorf("March StoryPoints = %v, want 8 (5+3)", rows[2].StoryPoints)
	}
	// No other month should carry SP.
	for i, r := range rows {
		if i == 2 {
			continue
		}
		if r.StoryPoints != 0 {
			t.Errorf("%s StoryPoints = %v, want 0", r.Month, r.StoryPoints)
		}
	}

	// CD-1 + CD-2 are both scored (SP > 0) → 2 scored tickets in March.
	if rows[2].ScoredTicketsResolved != 2 {
		t.Errorf("March ScoredTicketsResolved = %d, want 2", rows[2].ScoredTicketsResolved)
	}

	totals := totalsFromMonthly(rows)
	if totals.StoryPoints != 8 {
		t.Errorf("Totals.StoryPoints = %v, want 8", totals.StoryPoints)
	}
	if totals.ScoredTicketsResolved != 2 {
		t.Errorf("Totals.ScoredTicketsResolved = %d, want 2", totals.ScoredTicketsResolved)
	}
	// rawMetricValue exposes the AVERAGE (8 SP / 2 scored tickets = 4), not the sum.
	if got := rawMetricValue(totals, metricStoryPoints); got != 4 {
		t.Errorf("rawMetricValue(story_points) = %v, want 4 (avg per scored ticket)", got)
	}
}

// TestStoryPointsCoverageGatingDoesNotPunishUnscored is the core P5.2 fairness
// guarantee: a dev whose resolved tickets haven't been SP-scored (SP sum 0)
// must get a NEUTRAL story_points contribution (z=0), never a below-mean
// penalty. Devs that DO have SP data are ranked only against each other.
func TestStoryPointsCoverageGatingDoesNotPunishUnscored(t *testing.T) {
	// rawMetricValue uses the average SP per scored ticket, so set sum + count:
	// High avg = 40/5 = 8, Low avg = 6/3 = 2, Unscored has no scored tickets.
	devs := []DevWindowMetrics{
		{Dev: config.DevIdentity{DisplayName: "Scored-High"}, Totals: Totals{StoryPoints: 40, ScoredTicketsResolved: 5}},
		{Dev: config.DevIdentity{DisplayName: "Scored-Low"}, Totals: Totals{StoryPoints: 6, ScoredTicketsResolved: 3}},
		{Dev: config.DevIdentity{DisplayName: "Unscored"}, Totals: Totals{StoryPoints: 0, ScoredTicketsResolved: 0}},
	}
	got := computeContributorScores(devs, map[string]float64{"story_points": 1.0}, testNorm())

	byName := map[string]*ContributorScore{}
	for _, d := range got {
		byName[d.Dev.DisplayName] = d.Score
	}

	// Unscored dev: neutral, NOT punished.
	if c := byName["Unscored"].Breakdown["story_points"]; c != 0 {
		t.Errorf("Unscored story_points contribution = %v, want 0 (neutral, no data)", c)
	}
	// Scored devs are compared only against each other: high > 0 > low.
	hi := byName["Scored-High"].Breakdown["story_points"]
	lo := byName["Scored-Low"].Breakdown["story_points"]
	if hi <= 0 {
		t.Errorf("Scored-High story_points contribution = %v, want > 0", hi)
	}
	if lo >= 0 {
		t.Errorf("Scored-Low story_points contribution = %v, want < 0", lo)
	}
	// Unscored must outrank the data-bearing low scorer on this dimension:
	// missing data (0) beats a genuine low score (negative).
	if byName["Unscored"].Breakdown["story_points"] <= lo {
		t.Error("unscored dev should not score below a data-bearing low scorer on SP")
	}
}

// TestStoryPointsAllUnscoredIsInert confirms the live-today case: when NO dev
// has SP data, the dimension contributes exactly 0 to every composite.
func TestStoryPointsAllUnscoredIsInert(t *testing.T) {
	devs := []DevWindowMetrics{
		{Dev: config.DevIdentity{DisplayName: "A"}, Totals: Totals{PRsMerged: 5}},
		{Dev: config.DevIdentity{DisplayName: "B"}, Totals: Totals{PRsMerged: 3}},
	}
	got := computeContributorScores(devs, map[string]float64{"story_points": 1.0, "prs_merged": 3.0}, testNorm())
	for _, d := range got {
		if c := d.Score.Breakdown["story_points"]; c != 0 {
			t.Errorf("%s story_points contribution = %v with no SP coverage, want 0", d.Dev.DisplayName, c)
		}
	}
}

// TestRawMetricValueStoryPoints guards the revival of the previously-dead
// story_points dimension: rawMetricValue must surface Totals.StoryPoints, not
// the hardcoded 0 it returned before P5.2.
func TestRawMetricValueStoryPoints(t *testing.T) {
	// 21 SP across 3 scored tickets → average 7 per scored ticket.
	tot := Totals{StoryPoints: 21, ScoredTicketsResolved: 3}
	if got := rawMetricValue(tot, metricStoryPoints); got != 7 {
		t.Errorf("rawMetricValue(story_points) = %v, want 7 (21/3 avg)", got)
	}
	// No scored tickets → 0 (missing data, not a divide-by-zero).
	if got := rawMetricValue(Totals{}, metricStoryPoints); got != 0 {
		t.Errorf("rawMetricValue(story_points) on empty Totals = %v, want 0", got)
	}
}
