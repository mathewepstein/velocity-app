package analyze

import (
	"fmt"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
)

// ContributorsForWindow computes the per-dev contributor table for an arbitrary
// scoring window [from, to], on demand — the backend behind the Step 2
// /api/contributors endpoint and the eventual P2.3 range picker.
//
// It reuses the exact dashboard pipeline Run uses for its current-window devs
// (buildDevWindows → applyCodeImpactCap → attachProjectShares →
// computeContributorScores → attachRatings), so calling it with the window Run
// used reproduces Run's in-memory Devs cohort byte-for-byte (the parity gate,
// TestContributorsForWindowMatchesRun). Run still computes that cohort but no
// longer serializes it into metrics.json — this endpoint is now its only
// surface to the frontend.
//
// Hybrid-compute boundary (architecture-evolution E6): the window aggregations
// are recomputed on demand from the corpus, but Elo stays a precompute — this
// reads ratings.json read-only and never advances or saves it. `analyze` (the
// offline step) remains the only writer of Elo state.
//
// from/to are inclusive month bounds. The per-dev full-history series still
// spans backfill_start … current (independent of the scoring window), matching
// Run.
func ContributorsForWindow(opts Options, from, to cache.Month) ([]DevWindowMetrics, error) {
	if opts.Store == nil {
		opts.Store = cache.JSONStore{}
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	current := cache.CurrentMonth(opts.Now)

	if to.Before(from) {
		return nil, fmt.Errorf("window end %s precedes start %s", to, from)
	}

	data, err := LoadAll(opts.Profile, current, opts.Store)
	if err != nil {
		return nil, err
	}
	backfillStart, err := cache.ParseMonth(opts.Profile.Window.BackfillStart)
	if err != nil {
		return nil, fmt.Errorf("invalid backfill_start: %w", err)
	}

	ci := opts.Profile.Scoring.CodeImpact
	norm := opts.Profile.Scoring.Normalize

	// Project detection runs on the full dataset (matches Run) so project-share
	// attribution is identical regardless of the scoring window.
	projects := detectProjects(data, opts.Profile.Surge)

	devs := buildDevWindows(data, opts.Profile.Devs, opts.Profile.Scoring.EffectiveExcludes(), from, to, backfillStart, current, ci, norm)
	applyCodeImpactCap(devs, ci)
	devs = attachProjectShares(devs, buildProjectShares(data, projects, ci))
	devs = computeContributorScores(devs, opts.Profile.Scoring.Weights, norm)

	// Elo is read-only here (precompute boundary). A missing ratings.json just
	// leaves ratings unattached — the window scores still stand on their own.
	ratings, err := cache.LoadRatings()
	if err != nil {
		return nil, fmt.Errorf("load ratings: %w", err)
	}
	devs = attachRatings(devs, ratings.Devs, opts.Profile.Scoring)

	return devs, nil
}
