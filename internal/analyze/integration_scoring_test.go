package analyze

import (
	"testing"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

// integScoringDataset: alice authors one reviewed feature PR (feature/x →
// development, keyed) and one integration merge-up (development → master,
// keyless, no review, re-shipping the feature's commits + a merge commit). Both
// merge inside the window.
func integScoringDataset() *Loaded {
	return &Loaded{
		Months: cache.MonthsInRange(cache.MustParseMonth("2026-01"), cache.MustParseMonth("2026-02")),
		PRs: []cache.GitHubPR{
			{
				Number: 1, Repo: "org/app", Author: "alice", Branch: "feature/x", BaseBranch: "development",
				Created: mustTime("2026-01-05T00:00:00Z"), Merged: ptrTime(mustTime("2026-01-10T00:00:00Z")),
				Additions: 80, Deletions: 20, IssueKeys: []string{"CD-1"}, InlineComments: 3,
				Files:   []string{"a.go", "b.go"},
				Commits: []cache.PRCommit{{SHA: "s1", Author: "alice", ParentCount: 1}, {SHA: "s2", Author: "alice", ParentCount: 1}},
			},
			{
				Number: 2, Repo: "org/app", Author: "alice", Branch: "development", BaseBranch: "master",
				Created: mustTime("2026-02-05T00:00:00Z"), Merged: ptrTime(mustTime("2026-02-10T00:00:00Z")),
				Additions: 800, Deletions: 200, // big re-shipped diff
				Files: []string{"a.go", "b.go", "c.go"},
				Commits: []cache.PRCommit{
					{SHA: "s1", Author: "alice", ParentCount: 1},
					{SHA: "s2", Author: "alice", ParentCount: 1},
					{SHA: "m1", Author: "alice", ParentCount: 2},
				},
			},
		},
	}
}

// B-4: the Elo path (periodTotals) must apply the SAME down-weight as the
// composite path, via the shared weighter — else the two scoring axes drift.
func TestPeriodTotals_IntegrationDownweight(t *testing.T) {
	data := integScoringDataset()
	alice := config.DevIdentity{GitHubLogin: "alice"}
	start, end := mustTime("2026-01-01T00:00:00Z"), mustTime("2026-02-28T23:59:59Z")
	iw := newIntegrationWeighter(data, config.IntegrationConfig{Enabled: true, Factor: 0.25, Threshold: 0.5})

	// Disabled (nil weighter): raw totals, no scored override.
	tOff, scoredOff := periodTotals(data, alice, start, end, nil)
	if scoredOff != nil {
		t.Error("nil weighter must yield nil scored override")
	}
	if tOff.PRsMerged != 2 {
		t.Errorf("raw PRsMerged = %d, want 2", tOff.PRsMerged)
	}

	// Enabled: raw totals UNCHANGED (participation/display), scored down-weighted.
	tOn, scoredOn := periodTotals(data, alice, start, end, iw)
	if tOn.PRsMerged != 2 || tOn.LOCAdded+tOn.LOCDeleted != 1100 {
		t.Errorf("raw totals must be unchanged by the feature: merged=%d loc=%d", tOn.PRsMerged, tOn.LOCAdded+tOn.LOCDeleted)
	}
	if scoredOn == nil {
		t.Fatal("enabled weighter must yield a scored override")
	}
	if scoredOn.prsMerged != 1.25 {
		t.Errorf("Elo scored.prsMerged = %v, want 1.25 (1 feature + 0.25 integration)", scoredOn.prsMerged)
	}
	if scoredOn.locChanged != 350 {
		t.Errorf("Elo scored.locChanged = %v, want 350 (100 + 0.25*1000)", scoredOn.locChanged)
	}
	// hasAnyActivity gates on RAW activity, so a down-weighted dev still "plays".
	if !hasAnyActivity(tOn) {
		t.Error("dev with merged PRs must still count as active (raw gating)")
	}
}

func TestBuildOneDev_IntegrationDownweight(t *testing.T) {
	data := integScoringDataset()
	alice := []config.DevIdentity{{GitHubLogin: "alice"}}
	start, end := cache.MustParseMonth("2026-01"), cache.MustParseMonth("2026-02")

	iw := newIntegrationWeighter(data, config.IntegrationConfig{Enabled: true, Factor: 0.25, Threshold: 0.5})
	if iw == nil {
		t.Fatal("weighter should be non-nil when enabled")
	}
	// Sanity: PR #2 must classify as an integration PR for this test to mean
	// anything.
	if iw.weightFor(data.PRs[1]) != 0.25 {
		t.Fatalf("PR #2 should be flagged (weight 0.25), got %v", iw.weightFor(data.PRs[1]))
	}

	on := buildDevWindows(data, alice, nil, nil, start, end, start, end, testCI(), testNorm(), iw)
	off := buildDevWindows(data, alice, nil, nil, start, end, start, end, testCI(), testNorm(), nil)
	if len(on) == 0 || len(off) == 0 {
		t.Fatal("expected alice in both cohorts")
	}
	onA, offA := on[0], off[0]

	// Display stays RAW: both PRs merged in window → raw count 2, unchanged by
	// the feature.
	if onA.Totals.PRsMerged != 2 || offA.Totals.PRsMerged != 2 {
		t.Errorf("raw Totals.PRsMerged must stay 2 (display unchanged); on=%d off=%d", onA.Totals.PRsMerged, offA.Totals.PRsMerged)
	}
	if onA.Totals.LOCAdded+onA.Totals.LOCDeleted != 1100 {
		t.Errorf("raw LOC must stay 1100, got %d", onA.Totals.LOCAdded+onA.Totals.LOCDeleted)
	}
	// IntegrationPRs surfaces the flagged count (display-only), only when on.
	if onA.Totals.IntegrationPRs != 1 {
		t.Errorf("Totals.IntegrationPRs = %d, want 1", onA.Totals.IntegrationPRs)
	}
	if offA.Totals.IntegrationPRs != 0 {
		t.Errorf("disabled run must not set IntegrationPRs, got %d", offA.Totals.IntegrationPRs)
	}

	// Scored override present only when enabled.
	if onA.scored == nil {
		t.Fatal("scored override must be set when integration scoring is enabled")
	}
	if offA.scored != nil {
		t.Fatal("scored override must be nil when disabled (byte-identical path)")
	}
	// Down-weighted prs_merged = 1 (feature) + 0.25 (integration) = 1.25.
	if got := onA.scored.prsMerged; got != 1.25 {
		t.Errorf("scored.prsMerged = %v, want 1.25", got)
	}
	// Down-weighted loc = 100 (feature) + 0.25*1000 (integration) = 350.
	if got := onA.scored.locChanged; got != 350 {
		t.Errorf("scored.locChanged = %v, want 350", got)
	}
	// metricValueForScoring routes through the override when present, raw when not.
	if got := metricValueForScoring(onA, metricPRsMerged); got != 1.25 {
		t.Errorf("metricValueForScoring(on, prs_merged) = %v, want 1.25", got)
	}
	if got := metricValueForScoring(offA, metricPRsMerged); got != 2 {
		t.Errorf("metricValueForScoring(off, prs_merged) = %v, want 2 (raw)", got)
	}
	// A non-overridden metric reads raw even when scored is set.
	if got := metricValueForScoring(onA, metricPRsReviewed); got != rawMetricValue(onA.Totals, metricPRsReviewed) {
		t.Errorf("non-overridden metric must read raw, got %v", got)
	}

	// code_impact is down-weighted in place (its inputs), so the enabled run's
	// code_impact must be strictly below the disabled run's for this dev.
	if !(onA.Totals.CodeImpact < offA.Totals.CodeImpact) {
		t.Errorf("integration down-weight should lower code_impact: on=%v off=%v", onA.Totals.CodeImpact, offA.Totals.CodeImpact)
	}
}
