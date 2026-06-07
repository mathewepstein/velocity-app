package analyze

import (
	"math"

	"github.com/mathewepstein/velocity/internal/config"
)

// Elo constants. The starting rating + 400-divisor + K-factor thresholds
// are locked decisions from the contributor-scoring plan.
const (
	eloStartingRating = 1000.0
	eloLogisticBase   = 10.0
	eloLogisticDiv    = 400.0
	// eloKFactorFallback is the K returned by eloKFactor when no tier table
	// is provided. Matches the legacy "established" default so a misconfigured
	// run still produces sensible-looking ratings.
	eloKFactorFallback = 16
)

// eloExpected returns the expected actual-score for a player at rating R
// versus the team mean rating teamMean, mapped to [0, 1] via the standard
// Elo logistic. A dev exactly at the team mean expects 0.5; a dev 400
// points above expects ~0.909.
func eloExpected(R, teamMean float64) float64 {
	return 1.0 / (1.0 + math.Pow(eloLogisticBase, (teamMean-R)/eloLogisticDiv))
}

// eloKFactor returns the K-factor for a dev with periodsPlayed completed
// periods so far. Picks the K of the tier with the largest MinPeriods that
// is still ≤ periodsPlayed.
//
// Tiers let us ramp K smoothly: brand-new devs get a high K so their rating
// moves fast while we're still learning who they are; established devs get
// a lower K so a single hot period can't swing them too far. The default
// table in DefaultScoringConfig ramps 32 → 24 → 20 → 16 across the first 17
// periods played.
//
// The function is order-independent — it walks all tiers and picks the
// best match — so callers don't have to sort. An empty tier list or a
// table with no tier covering periodsPlayed returns eloKFactorFallback;
// neither should happen in production with the default config.
func eloKFactor(periodsPlayed int, tiers []config.KTier) int {
	best := eloKFactorFallback
	bestMin := -1
	for _, t := range tiers {
		if t.MinPeriods <= periodsPlayed && t.MinPeriods > bestMin {
			best = t.K
			bestMin = t.MinPeriods
		}
	}
	return best
}

// updateElo returns the new rating + delta for one period given the dev's
// current rating R, the team mean rating teamMean, the actual outcome
// (normalized to [0, 1]), and the K-factor.
func updateElo(R, teamMean, actual float64, K int) (newR, delta float64) {
	expected := eloExpected(R, teamMean)
	delta = float64(K) * (actual - expected)
	return R + delta, delta
}

// logisticZ maps every value in xs to (0, 1) by first z-scoring against the
// cohort's mean and population stddev, then squashing through the logistic
// curve. The result is the period's `actual` outcome for the Elo update.
//
// Replaces min-max normalization because min-max collapses any cohort with a
// dominant outlier — one 10x performer pulls every mid-pack score toward 0,
// which then dives mid-pack devs' ratings even when they're producing
// perfectly reasonable work. Logistic-z gives mid-pack performers an `actual`
// close to 0.5 (matched the expected, rating barely moves) while still
// rewarding the top and penalizing the bottom proportional to how far they
// sit from the cohort's center.
//
// Degenerate cases mirror minMaxNormalize: empty input → empty output; a
// single dev or a uniform cohort (stddev ≈ 0) returns 0.5 for everyone,
// preserving the "drew the period" semantics that keep ratings stable when
// there's nothing to rank.
func logisticZ(xs []float64) []float64 {
	out := make([]float64, len(xs))
	if len(xs) < 2 {
		for i := range out {
			out[i] = 0.5
		}
		return out
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean := sum / float64(len(xs))
	var sumSq float64
	for _, x := range xs {
		d := x - mean
		sumSq += d * d
	}
	variance := sumSq / float64(len(xs)) // population variance — the cohort IS the population for this period
	if variance < 1e-12 {
		for i := range out {
			out[i] = 0.5
		}
		return out
	}
	stdev := math.Sqrt(variance)
	for i, x := range xs {
		z := (x - mean) / stdev
		out[i] = 1.0 / (1.0 + math.Exp(-z))
	}
	return out
}

// minMaxNormalize maps every value in xs to [0, 1] via (x - min) / (max - min).
// When every value matches (max == min) the result is 0.5 for everyone —
// "drew the period" — which matches the Elo invariant that an exactly
// average outcome shouldn't move ratings.
//
// Kept for analysis tooling and back-compat tests; the production walker
// switched to logisticZ in Phase 7.
func minMaxNormalize(xs []float64) []float64 {
	out := make([]float64, len(xs))
	if len(xs) == 0 {
		return out
	}
	min, max := xs[0], xs[0]
	for _, x := range xs[1:] {
		if x < min {
			min = x
		}
		if x > max {
			max = x
		}
	}
	if max-min < 1e-12 {
		for i := range out {
			out[i] = 0.5
		}
		return out
	}
	span := max - min
	for i, x := range xs {
		out[i] = (x - min) / span
	}
	return out
}
