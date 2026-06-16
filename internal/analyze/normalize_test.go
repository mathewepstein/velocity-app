package analyze

import (
	"math"
	"testing"

	"github.com/mathewepstein/velocity/internal/config"
)

func defaultNorm() config.NormalizeConfig { return config.DefaultScoringConfig().Normalize }

func TestMatchesGeneratedPattern_ShippedDefaults(t *testing.T) {
	cfg := defaultNorm()
	cases := []struct {
		path string
		want bool
	}{
		// Defaults: *.lock, package-lock.json, yarn.lock, go.sum, *.min.js, *.pb.go,
		// */dist/*, */vendor/*, */node_modules/*, etc.
		{"web/package-lock.json", true},
		{"backend/go.sum", true},
		{"vendor/foo/bar.go", true},
		{"src/x/node_modules/lodash/index.js", true},
		{"web/dist/app.bundle.js", true},
		{"foo/bar.min.js", true},
		{"proto/api.pb.go", true},
		// Source paths shouldn't match.
		{"src/foo.go", false},
		{"web/components/Button.vue", false},
		{"internal/handlers/user.go", false},
		// Case insensitive.
		{"WEB/PACKAGE-LOCK.JSON", true},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got := matchesGeneratedPattern(tc.path, cfg.GeneratedFilePatterns)
			if got != tc.want {
				t.Errorf("matchesGeneratedPattern(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestEffectiveFilesCount_FractionalGeneratedFiles(t *testing.T) {
	cfg := config.NormalizeConfig{
		GeneratedFilePatterns: []string{"*.lock", "*/dist/*"},
		GeneratedFileWeight:   0.25,
	}
	files := map[string]struct{}{
		"src/foo.go":        {}, // 1.0 — source code, no pattern match
		"src/bar.go":        {}, // 1.0 — source code, no pattern match
		"web/dist/app.js":   {}, // 0.25 — matches "*/dist/*"
		"package-lock.json": {}, // 1.0 — doesn't end in `.lock`; users need an explicit pattern
		"yarn.lock":         {}, // 0.25 — basename matches "*.lock"
	}
	got := effectiveFilesCount(files, cfg)
	want := 1.0 + 1.0 + 0.25 + 1.0 + 0.25
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("effectiveFilesCount = %v, want %v", got, want)
	}
}

func TestSpamMultiplier_TriggersAboveThreshold(t *testing.T) {
	cfg := config.NormalizeConfig{SpamThreshold: 1.5, SpamPenalty: 0.25, MultiplierFloor: 0.5}
	cases := []struct {
		name           string
		commits, files int
		wantBelowOne   bool
	}{
		{"healthy-1-to-1", 10, 10, false},     // ratio 1.0 — under threshold
		{"borderline-1.5", 30, 20, false},     // ratio 1.5 — at threshold, no penalty
		{"spammy-2.0", 40, 20, true},          // ratio 2.0 — 0.5 over threshold → 1-0.125=0.875
		{"very-spammy-10", 200, 20, true},     // ratio 10 — pegged to floor
		{"no-files-no-penalty", 50, 0, false}, // can't compute, no dampening
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			totals := Totals{Commits: tc.commits, UniqueFilesTouched: tc.files}
			got := spamMultiplier(totals, cfg, 0.5)
			if tc.wantBelowOne {
				if got >= 1.0 {
					t.Errorf("spamMultiplier = %v, want < 1", got)
				}
			} else if got != 1.0 {
				t.Errorf("spamMultiplier = %v, want 1.0 (under threshold)", got)
			}
			if got < 0.5 {
				t.Errorf("spamMultiplier = %v, below floor 0.5", got)
			}
		})
	}
}

func TestStuffMultiplier_TriggersAboveTeamP90(t *testing.T) {
	cfg := config.NormalizeConfig{StuffPenalty: 0.25, MultiplierFloor: 0.5}
	// Team distribution: most devs at 10 LOC/file, one outlier higher.
	team := []float64{8, 9, 10, 10, 10, 10, 11, 12, 13, 50}
	// p90 of this distribution is ~17.7 (R type-7).
	t.Run("under-p90-no-dampening", func(t *testing.T) {
		dev := Totals{LOCAdded: 50, LOCDeleted: 50, UniqueFilesTouched: 10} // ratio 10
		got := stuffMultiplier(dev, team, cfg, 0.5)
		if got != 1.0 {
			t.Errorf("got %v, want 1.0 for under-p90 dev", got)
		}
	})
	t.Run("above-p90-dampens", func(t *testing.T) {
		dev := Totals{LOCAdded: 500, LOCDeleted: 0, UniqueFilesTouched: 10} // ratio 50
		got := stuffMultiplier(dev, team, cfg, 0.5)
		if got >= 1.0 {
			t.Errorf("got %v, want < 1.0 for above-p90 dev", got)
		}
		if got < 0.5 {
			t.Errorf("got %v, below floor 0.5", got)
		}
	})
	t.Run("no-files-no-dampening", func(t *testing.T) {
		dev := Totals{LOCAdded: 500, UniqueFilesTouched: 0}
		got := stuffMultiplier(dev, team, cfg, 0.5)
		if got != 1.0 {
			t.Errorf("got %v, want 1.0 when no files", got)
		}
	})
}

func TestEffortMultiplierFloor(t *testing.T) {
	// Aggressive penalties + an extreme dev — multiplier must clamp at 0.5.
	cfg := config.NormalizeConfig{
		SpamThreshold:   1.0,
		SpamPenalty:     2.0, // huge penalty
		StuffPenalty:    2.0,
		MultiplierFloor: 0.5,
	}
	dev := Totals{Commits: 100, UniqueFilesTouched: 1, LOCAdded: 10000, LOCDeleted: 5000}
	team := []float64{1, 1, 1, 1, 1}
	got := effortMultiplier(dev, team, cfg)
	if got != 0.5 {
		t.Errorf("effortMultiplier with aggressive penalties = %v, want 0.5 (floor)", got)
	}
}

func TestEffortMultiplierClampsToOneWhenNoSignals(t *testing.T) {
	cfg := defaultNorm()
	dev := Totals{Commits: 5, UniqueFilesTouched: 50, LOCAdded: 100, LOCDeleted: 50}
	team := []float64{5, 6, 7, 8, 9}
	got := effortMultiplier(dev, team, cfg)
	if got != 1.0 {
		t.Errorf("effortMultiplier with healthy ratios = %v, want 1.0", got)
	}
}
