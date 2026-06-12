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

// stdevPop returns the population standard deviation of xs — the cohort IS the
// population for a period. Returns 0 for fewer than two values or a uniform
// cohort. Used as the margin scale in the Phase-4 round-robin outcome so the
// result function is scale-free (a fixed score gap means the same thing whether
// the period's scores are tightly clustered or spread wide).
func stdevPop(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
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
	return math.Sqrt(sumSq / float64(len(xs)))
}

// marginResult returns the pairwise game result for dev i versus dev j on the
// output axis: 0.5 at a tie, →1 as i dominates, →0 as j dominates.
//
// The score gap d = scoreI − scoreJ is first passed through a deadzone of
// half-width `band`: any |d| ≤ band counts as a draw (0.5). This is the
// "near-ties get no win weighting" rule — two devs within a band of each other
// neither gains nor loses from the matchup. Beyond the band, the *excess* gap
// (|d| − band) drives a natural-base logistic of steepness `scale`, so clear
// over-performers stretch away (a dev `band + 2·scale` above a peer scores
// logistic(2)≈0.88). The deadzone collapses the mid-pack toward 0.5 while the
// extremes separate — exactly the hybrid shape requested: clear winners pull
// away, near-ties don't move.
//
// When scale≈0 (a uniform period, nothing to rank) every game is a draw.
func marginResult(scoreI, scoreJ, scale, band float64) float64 {
	if scale < 1e-12 {
		return 0.5
	}
	d := scoreI - scoreJ
	ad := math.Abs(d)
	if ad <= band {
		return 0.5
	}
	excess := ad - band
	if d < 0 {
		excess = -excess
	}
	return 1.0 / (1.0 + math.Exp(-excess/scale))
}

// roundRobinScore returns the averaged pairwise outcome S_i for each dev: the
// mean deadzone+margin game result of dev i against every other active dev on
// the output axis (Phase 4, C3). S_i = 1 when i clearly out-produces the whole
// field, 0.5 for an average / near-tie period, 0 when i clearly trails
// everyone. There is no logisticZ cap, so a dev who sweeps the field keeps
// climbing — fixing the flatten-top plateau. A single-dev or uniform period
// yields 0.5 everywhere ("drew the period"), preserving the stability
// invariant. `scale` is the post-deadzone logistic steepness and `band` the
// deadzone half-width, both in score units (typically multiples of the
// period's stdev — see applyEloPeriod).
func roundRobinScore(scores []float64, scale, band float64) []float64 {
	out := make([]float64, len(scores))
	if len(scores) < 2 {
		for i := range out {
			out[i] = 0.5
		}
		return out
	}
	n := float64(len(scores) - 1)
	for i := range scores {
		var sum float64
		for j := range scores {
			if i == j {
				continue
			}
			sum += marginResult(scores[i], scores[j], scale, band)
		}
		out[i] = sum / n
	}
	return out
}

// roundRobinExpected returns the averaged pairwise expected score E_i: the mean
// Elo win-probability of dev i against every other active dev at their actual
// current rating (Phase 4, C3). This replaces the single shifting-teamMean
// opponent with the real field, so a strong cohort is genuinely harder to climb
// against and the rating becomes a durable cross-period standing rather than
// "vs whoever happened to show up." A single-dev period yields 0.5 (a draw).
func roundRobinExpected(currentR []float64) []float64 {
	out := make([]float64, len(currentR))
	if len(currentR) < 2 {
		for i := range out {
			out[i] = 0.5
		}
		return out
	}
	n := float64(len(currentR) - 1)
	for i := range currentR {
		var sum float64
		for j := range currentR {
			if i == j {
				continue
			}
			sum += eloExpected(currentR[i], currentR[j])
		}
		out[i] = sum / n
	}
	return out
}

// logisticZ maps every value in xs to (0, 1) by first z-scoring against the
// cohort's mean and population stddev, then squashing through the logistic
// curve. It was the period's `actual` outcome for the Elo update through
// Phase 7; Phase 4's round-robin redesign retired it from the walker (the
// outcome is now roundRobinScore). Kept for analysis tooling and tests.
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
