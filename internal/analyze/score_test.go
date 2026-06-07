package analyze

import (
	"math"
	"testing"

	"github.com/mathewepstein/velocity/internal/config"
)

func TestPercentileLinear(t *testing.T) {
	cases := []struct {
		name string
		xs   []float64
		p    float64
		want float64
	}{
		{"empty", nil, 95, 0},
		{"single", []float64{42}, 95, 42},
		{"two-pts p50", []float64{0, 100}, 50, 50},
		{"unsorted-input", []float64{3, 1, 2}, 50, 2},
		{"p95-of-1-to-10", []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 95, 9.55},
		{"p0", []float64{5, 10, 20}, 0, 5},
		{"p100", []float64{5, 10, 20}, 100, 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := percentile(tc.xs, tc.p)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("percentile(%v, %v) = %v, want %v", tc.xs, tc.p, got, tc.want)
			}
		})
	}
}

func TestMeanStdev(t *testing.T) {
	cases := []struct {
		name      string
		xs        []float64
		wantMean  float64
		wantStdev float64
	}{
		{"empty", nil, 0, 0},
		{"single", []float64{7}, 7, 0},
		{"uniform", []float64{5, 5, 5, 5}, 5, 0},
		{"0-1-2", []float64{0, 1, 2}, 1, math.Sqrt(2.0 / 3.0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotMean, gotStdev := meanStdev(tc.xs)
			if math.Abs(gotMean-tc.wantMean) > 1e-9 {
				t.Errorf("mean(%v) = %v, want %v", tc.xs, gotMean, tc.wantMean)
			}
			if math.Abs(gotStdev-tc.wantStdev) > 1e-9 {
				t.Errorf("stdev(%v) = %v, want %v", tc.xs, gotStdev, tc.wantStdev)
			}
		})
	}
}

func TestComputeContributorScoresRanksMappedDevsOnly(t *testing.T) {
	devs := []DevWindowMetrics{
		{
			Dev:    config.DevIdentity{DisplayName: "Alice"},
			Totals: Totals{PRsMerged: 10, JiraIssuesResolved: 5, PRsReviewed: 8},
		},
		{
			Dev:    config.DevIdentity{DisplayName: "Bob"},
			Totals: Totals{PRsMerged: 5, JiraIssuesResolved: 2, PRsReviewed: 4},
		},
		{
			Dev:    config.DevIdentity{DisplayName: "Carol"},
			Totals: Totals{PRsMerged: 1, JiraIssuesResolved: 1, PRsReviewed: 1},
		},
		{
			Dev:    config.DevIdentity{DisplayName: "unknown"},
			Totals: Totals{PRsMerged: 100, JiraIssuesResolved: 100},
		},
	}
	weights := map[string]float64{
		"prs_merged":           3.0,
		"jira_issues_resolved": 2.0,
		"prs_reviewed":         1.0,
	}
	got := computeContributorScores(devs, weights, testNorm())

	// unknown bucket should never get a score.
	for _, d := range got {
		if d.Dev.DisplayName == "unknown" && d.Score != nil {
			t.Errorf("unknown bucket should be score-less, got %+v", d.Score)
		}
	}

	byName := map[string]*ContributorScore{}
	for _, d := range got {
		if d.Score != nil {
			byName[d.Dev.DisplayName] = d.Score
		}
	}
	if len(byName) != 3 {
		t.Fatalf("expected 3 scored devs, got %d", len(byName))
	}
	if byName["Alice"].Rank != 1 {
		t.Errorf("Alice rank = %d, want 1 (she leads all 3 metrics)", byName["Alice"].Rank)
	}
	if byName["Bob"].Rank != 2 {
		t.Errorf("Bob rank = %d, want 2", byName["Bob"].Rank)
	}
	if byName["Carol"].Rank != 3 {
		t.Errorf("Carol rank = %d, want 3", byName["Carol"].Rank)
	}
	if byName["Alice"].Total <= byName["Bob"].Total {
		t.Errorf("Alice total (%v) should exceed Bob total (%v)", byName["Alice"].Total, byName["Bob"].Total)
	}

	// Breakdown should sum to total.
	for name, s := range byName {
		var sum float64
		for _, v := range s.Breakdown {
			sum += v
		}
		if math.Abs(sum-s.Total) > 1e-9 {
			t.Errorf("%s breakdown sum %v != total %v", name, sum, s.Total)
		}
	}

	// Symmetry: z-scores have mean 0, so weighted-z totals across the team sum to 0.
	var teamSum float64
	for _, s := range byName {
		teamSum += s.Total
	}
	if math.Abs(teamSum) > 1e-9 {
		t.Errorf("team-wide score total = %v, want ~0 (z-scores sum to 0)", teamSum)
	}
}

func TestComputeContributorScoresHandlesSingleDev(t *testing.T) {
	devs := []DevWindowMetrics{
		{
			Dev:    config.DevIdentity{DisplayName: "Alice"},
			Totals: Totals{PRsMerged: 10},
		},
	}
	got := computeContributorScores(devs, map[string]float64{"prs_merged": 3.0}, testNorm())
	if got[0].Score == nil {
		t.Fatal("expected score for single dev")
	}
	if got[0].Score.Total != 0 {
		t.Errorf("single-dev total = %v, want 0 (no comparison group)", got[0].Score.Total)
	}
	if got[0].Score.Rank != 1 {
		t.Errorf("single-dev rank = %d, want 1", got[0].Score.Rank)
	}
}

func TestComputeContributorScoresAppliesLOCCapAndSqrt(t *testing.T) {
	// One outlier in a team of 100. With R type-7 p95, the cap sits at the
	// baseline value (the outlier is past the 95th-percentile rank), so it
	// collapses to the baseline. After sqrt the spread vanishes; everyone's
	// z for loc_changed is 0.
	devs := make([]DevWindowMetrics, 100)
	for i := range devs {
		devs[i] = DevWindowMetrics{
			Dev:    config.DevIdentity{DisplayName: string(rune('A')) + string(rune(i))},
			Totals: Totals{LOCAdded: 100, LOCDeleted: 50}, // baseline = 150
		}
	}
	devs[0].Totals = Totals{LOCAdded: 10000, LOCDeleted: 5000} // 100x outlier

	got := computeContributorScores(devs, map[string]float64{"loc_changed": 1.0}, testNorm())

	outlierZ := got[0].Score.Total
	if math.Abs(outlierZ) > 1e-9 {
		t.Errorf("LOC outlier z = %v, want ~0 after p95-cap collapses spread", outlierZ)
	}
}

func TestComputeContributorScoresIgnoresUnconfiguredWeights(t *testing.T) {
	// Weights map missing keys → metric contributes 0 to score (w=0).
	devs := []DevWindowMetrics{
		{Dev: config.DevIdentity{DisplayName: "Alice"}, Totals: Totals{PRsMerged: 10}},
		{Dev: config.DevIdentity{DisplayName: "Bob"}, Totals: Totals{PRsMerged: 0}},
	}
	got := computeContributorScores(devs, map[string]float64{}, testNorm()) // no weights at all
	for _, d := range got {
		if d.Score.Total != 0 {
			t.Errorf("%s total = %v with empty weights, want 0", d.Dev.DisplayName, d.Score.Total)
		}
	}
}
