package analyze

import (
	"math"
	"testing"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

func TestComputeCodeImpactFormula(t *testing.T) {
	ci := config.CodeImpactConfig{Alpha: 1, Beta: 0.5, Gamma: 2}
	cases := []struct {
		name                          string
		files, loc, merged            int
		want                          float64
	}{
		{"all-zero", 0, 0, 0, 0},
		{"only-files", 16, 0, 0, 4},                // sqrt(16) = 4
		{"only-loc", 0, 200, 0, 10},                // sqrt(0.5*200) = sqrt(100) = 10
		{"only-merged", 0, 0, 8, 4},                // sqrt(2*8) = 4
		{"mixed", 9, 50, 4, math.Sqrt(9 + 25 + 8)}, // 9 + 25 + 8 = 42
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeCodeImpact(tc.files, tc.loc, tc.merged, ci)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("computeCodeImpact(%d, %d, %d) = %v, want %v", tc.files, tc.loc, tc.merged, got, tc.want)
			}
		})
	}
}

func TestComputeCodeImpactNegativeSumClampsToZero(t *testing.T) {
	// Negative coefficient shouldn't surface NaN; the floor at zero keeps
	// scoring deterministic if a user sets an unusual config.
	ci := config.CodeImpactConfig{Alpha: -1, Beta: -1, Gamma: -1}
	if got := computeCodeImpact(5, 5, 5, ci); got != 0 {
		t.Errorf("negative-coefficient input should clamp to 0, got %v", got)
	}
}

func TestApplyCodeImpactCapAppliesTeamP95(t *testing.T) {
	ci := config.CodeImpactConfig{Alpha: 1, Beta: 1, Gamma: 0}
	// One dev with a huge LOC blow-up; nine others at the baseline. The cap
	// should pull the outlier's LOC down to (or near) the baseline; the
	// resulting CodeImpact should be close to the baseline dev's number.
	// effectiveLOC equals raw LOCAdded+LOCDeleted when the code_impact knobs are
	// off — that's what buildOneDev sets in production, and applyCodeImpactCap
	// clamps against it.
	devs := []DevWindowMetrics{
		{
			Dev:            config.DevIdentity{DisplayName: "Outlier"},
			Totals:         Totals{LOCAdded: 10000, LOCDeleted: 5000, UniqueFilesTouched: 4, PRsMerged: 1},
			effectiveFiles: 4, // no gen-file matches in this test
			effectiveLOC:   15000,
		},
	}
	for i := 0; i < 9; i++ {
		devs = append(devs, DevWindowMetrics{
			Dev:            config.DevIdentity{DisplayName: "Baseline" + string(rune('A'+i))},
			Totals:         Totals{LOCAdded: 100, LOCDeleted: 50, UniqueFilesTouched: 4, PRsMerged: 1},
			effectiveFiles: 4,
			effectiveLOC:   150,
		})
	}
	applyCodeImpactCap(devs, ci)

	// Outlier's LOC capped at p95 across [15000, 150, 150, ...]. R type-7 p95
	// with 10 samples sits between rank 9 (150) and rank 10 (15000), at
	// fraction 0.55 → 8317.5. Outlier LOC remains > baseline's, but the
	// CodeImpact must shrink dramatically from the uncapped value.
	uncapped := computeCodeImpact(4, 15000, 1, ci)
	if devs[0].Totals.CodeImpact >= uncapped {
		t.Errorf("outlier CodeImpact = %v, expected < uncapped %v", devs[0].Totals.CodeImpact, uncapped)
	}
	baseline := computeCodeImpact(4, 150, 1, ci)
	for i := 1; i < len(devs); i++ {
		if math.Abs(devs[i].Totals.CodeImpact-baseline) > 1e-9 {
			t.Errorf("baseline dev[%d] CodeImpact = %v, want %v", i, devs[i].Totals.CodeImpact, baseline)
		}
	}
}

func TestApplyCodeImpactCapSkipsUnknownBucket(t *testing.T) {
	ci := config.CodeImpactConfig{Alpha: 1, Beta: 1, Gamma: 1}
	devs := []DevWindowMetrics{
		{Dev: config.DevIdentity{DisplayName: "unknown"}, Totals: Totals{LOCAdded: 9999, UniqueFilesTouched: 5, PRsMerged: 1, CodeImpact: 42}},
	}
	applyCodeImpactCap(devs, ci)
	if devs[0].Totals.CodeImpact != 42 {
		t.Errorf("unknown bucket should be left untouched, got CodeImpact = %v", devs[0].Totals.CodeImpact)
	}
}

func TestRollupMonthlyPopulatesUniqueFilesAndCodeImpact(t *testing.T) {
	ci := config.CodeImpactConfig{Alpha: 1, Beta: 0.5, Gamma: 2}
	data := twoDevDataset()
	// Hydrate PR #1 (merged in 2026-01) with a file list. PR #2 isn't merged,
	// so its files (if any) shouldn't count toward unique_files_touched.
	for i := range data.PRs {
		if data.PRs[i].Number == 1 {
			data.PRs[i].Files = []string{"a.go", "b.go", "c.go"}
		}
		if data.PRs[i].Number == 2 {
			// Bob's PR — not merged. The Files field must not contribute.
			data.PRs[i].Files = []string{"should-not-count.go"}
		}
	}
	start := cache.MustParseMonth("2026-01")
	end := cache.MustParseMonth("2026-04")
	rows := rollupMonthly(data, start, end, ci)

	if rows[0].UniqueFilesTouched != 3 {
		t.Errorf("Jan unique files = %d, want 3", rows[0].UniqueFilesTouched)
	}
	if rows[0].CodeImpact <= 0 {
		t.Errorf("Jan code_impact should be > 0 with files + merged + LOC, got %v", rows[0].CodeImpact)
	}
	// Feb has only unmerged PR #2 → no merged-PR files, no merged count.
	if rows[1].UniqueFilesTouched != 0 {
		t.Errorf("Feb unique files = %d, want 0 (PR #2 not merged)", rows[1].UniqueFilesTouched)
	}
	if rows[1].CodeImpact != 0 {
		t.Errorf("Feb code_impact = %v, want 0", rows[1].CodeImpact)
	}
}

