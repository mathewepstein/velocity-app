package analyze

import "sort"

// spearmanRho returns Spearman's rank correlation between the per-dev
// composite score (Score.Total, descending = better) and the Elo rating
// (Rating.Current, descending = better) over devs that carry both signals.
//
// Devs without a Score, without a Rating, or who haven't played a period
// (PeriodsPlayed == 0) are filtered out — they can't contribute to a
// rank-correlation that only makes sense when both columns are populated.
//
// Returns 0 when fewer than two devs qualify (rank correlation is undefined
// for n < 2). Tied values get mid-ranks, so a uniform cohort returns 1.0
// rather than blowing the formula's denominator.
//
// The exact ρ formula 1 − 6·Σd² / (n·(n²−1)) is used; with mid-rank ties
// this is the standard textbook approximation (Pearson-on-ranks would be
// the exact form). Good enough for telemetry — we surface this as a single
// scalar in metrics.json and use it to spot direction-of-change between
// calibration runs, not to do statistical inference.
func spearmanRho(devs []DevWindowMetrics) float64 {
	type pair struct {
		composite float64
		elo       float64
	}
	pairs := make([]pair, 0, len(devs))
	for _, d := range devs {
		if d.Score == nil || d.Rating == nil || d.Rating.PeriodsPlayed == 0 {
			continue
		}
		pairs = append(pairs, pair{composite: d.Score.Total, elo: d.Rating.Current})
	}
	n := len(pairs)
	if n < 2 {
		return 0
	}

	compVals := make([]float64, n)
	eloVals := make([]float64, n)
	for i, p := range pairs {
		compVals[i] = p.composite
		eloVals[i] = p.elo
	}
	compRanks := midRanks(compVals)
	eloRanks := midRanks(eloVals)

	var sumDsq float64
	for i := range pairs {
		d := compRanks[i] - eloRanks[i]
		sumDsq += d * d
	}
	nf := float64(n)
	return 1.0 - (6.0*sumDsq)/(nf*(nf*nf-1.0))
}

// midRanks assigns descending mid-ranks to xs (largest value gets rank 1).
// Ties share the average of their position bracket — e.g. three values tied
// at the top of a 5-item list each get rank 2.0 (mean of 1, 2, 3).
//
// Returned ranks line up index-for-index with xs.
func midRanks(xs []float64) []float64 {
	n := len(xs)
	type idxVal struct {
		i int
		v float64
	}
	sorted := make([]idxVal, n)
	for i, v := range xs {
		sorted[i] = idxVal{i: i, v: v}
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].v > sorted[j].v })

	ranks := make([]float64, n)
	for i := 0; i < n; {
		j := i + 1
		for j < n && sorted[j].v == sorted[i].v {
			j++
		}
		avg := (float64(i+1) + float64(j)) / 2.0
		for k := i; k < j; k++ {
			ranks[sorted[k].i] = avg
		}
		i = j
	}
	return ranks
}
