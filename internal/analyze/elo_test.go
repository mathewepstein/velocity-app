package analyze

import (
	"math"
	"testing"

	"github.com/mathewepstein/velocity/internal/config"
)

func TestEloExpectedSymmetry(t *testing.T) {
	// Two devs at the same rating: expected = 0.5.
	if got := eloExpected(1000, 1000); math.Abs(got-0.5) > 1e-9 {
		t.Errorf("eloExpected(1000, 1000) = %v, want 0.5", got)
	}
	// 400 above mean: expected ~= 0.9091.
	above := eloExpected(1400, 1000)
	if math.Abs(above-0.9090909090909091) > 1e-6 {
		t.Errorf("eloExpected(1400, 1000) = %v, want ~0.9091", above)
	}
	// Symmetry: 400 below ↔ 1 - "400 above".
	below := eloExpected(600, 1000)
	if math.Abs(below+above-1) > 1e-9 {
		t.Errorf("eloExpected(600, 1000) + eloExpected(1400, 1000) = %v, want 1", above+below)
	}
}

func TestEloKFactorDefaultTiers(t *testing.T) {
	// The Phase 7 default ramp: 0→3 = 32, 4→8 = 24, 9→16 = 20, 17+ = 16.
	tiers := []config.KTier{
		{MinPeriods: 0, K: 32},
		{MinPeriods: 4, K: 24},
		{MinPeriods: 9, K: 20},
		{MinPeriods: 17, K: 16},
	}
	cases := []struct {
		periods int
		want    int
	}{
		{0, 32}, {3, 32}, // tier 1
		{4, 24}, {8, 24}, // tier 2
		{9, 20}, {16, 20}, // tier 3
		{17, 16}, {100, 16}, // tier 4 (open-ended)
	}
	for _, c := range cases {
		if got := eloKFactor(c.periods, tiers); got != c.want {
			t.Errorf("eloKFactor(%d, defaults) = %d, want %d", c.periods, got, c.want)
		}
	}
}

func TestEloKFactorTwoTierLegacy(t *testing.T) {
	// Legacy-shaped 2-tier table (what applyDefaults synthesizes from old
	// KFactorNew/KFactorEst/NewThreshold config). Should behave identically
	// to the pre-Phase-7 binary lookup.
	tiers := []config.KTier{
		{MinPeriods: 0, K: 32},
		{MinPeriods: 6, K: 16},
	}
	if got := eloKFactor(5, tiers); got != 32 {
		t.Errorf("legacy 2-tier @ 5 = %d, want 32", got)
	}
	if got := eloKFactor(6, tiers); got != 16 {
		t.Errorf("legacy 2-tier @ 6 = %d, want 16", got)
	}
}

func TestEloKFactorEmptyTiersFallback(t *testing.T) {
	// Defensive: a misconfigured run with no tiers shouldn't panic.
	if got := eloKFactor(10, nil); got != eloKFactorFallback {
		t.Errorf("nil tiers = %d, want fallback %d", got, eloKFactorFallback)
	}
	if got := eloKFactor(10, []config.KTier{}); got != eloKFactorFallback {
		t.Errorf("empty tiers = %d, want fallback %d", got, eloKFactorFallback)
	}
}

func TestEloKFactorUnorderedTiers(t *testing.T) {
	// Function is order-independent — input slice order should not matter.
	tiers := []config.KTier{
		{MinPeriods: 9, K: 20},
		{MinPeriods: 0, K: 32},
		{MinPeriods: 17, K: 16},
		{MinPeriods: 4, K: 24},
	}
	if got := eloKFactor(5, tiers); got != 24 {
		t.Errorf("unordered tiers @ 5 = %d, want 24", got)
	}
	if got := eloKFactor(20, tiers); got != 16 {
		t.Errorf("unordered tiers @ 20 = %d, want 16", got)
	}
}

func TestUpdateEloMovesTowardOutcome(t *testing.T) {
	// At rating 1000 against mean 1000: expected=0.5. Actual=1 → delta=+K/2.
	newR, delta := updateElo(1000, 1000, 1.0, 32)
	if math.Abs(delta-16) > 1e-9 {
		t.Errorf("delta = %v, want 16 (K=32, actual=1, expected=0.5)", delta)
	}
	if math.Abs(newR-1016) > 1e-9 {
		t.Errorf("newR = %v, want 1016", newR)
	}
	// Symmetric drop on actual=0.
	_, downDelta := updateElo(1000, 1000, 0.0, 32)
	if math.Abs(downDelta+16) > 1e-9 {
		t.Errorf("delta on actual=0 = %v, want -16", downDelta)
	}
	// Established K halves the swing.
	_, deltaEst := updateElo(1000, 1000, 1.0, 16)
	if math.Abs(deltaEst-8) > 1e-9 {
		t.Errorf("established delta = %v, want 8", deltaEst)
	}
}

func TestUpdateEloHighRatedDevGetsLessUpsideForExpectedWins(t *testing.T) {
	// Underdog beats favorite: underdog at 800 vs team mean 1200, actual=1.
	// Expected for underdog = 1/(1+10^(400/400)) = 1/11 ≈ 0.0909.
	// Delta = 32 * (1 - 0.0909) ≈ +29.09.
	_, dUnderdog := updateElo(800, 1200, 1.0, 32)
	if dUnderdog < 28 || dUnderdog > 30 {
		t.Errorf("underdog delta = %v, want ~+29", dUnderdog)
	}
	// Favorite carrying the period gains very little: 1200 vs mean 800,
	// actual=1, expected ~= 0.909. Delta ≈ 32 * 0.091 ≈ +2.91.
	_, dFav := updateElo(1200, 800, 1.0, 32)
	if dFav < 2 || dFav > 4 {
		t.Errorf("favorite delta = %v, want ~+3", dFav)
	}
}

func TestLogisticZSymmetry(t *testing.T) {
	// Symmetric inputs around the mean should produce actuals symmetric
	// around 0.5: top and bottom mirror, sum is 1.0.
	got := logisticZ([]float64{1, 5})
	if math.Abs(got[0]+got[1]-1.0) > 1e-9 {
		t.Errorf("symmetric pair should sum to 1.0, got %v + %v = %v", got[0], got[1], got[0]+got[1])
	}
	if got[0] >= 0.5 || got[1] <= 0.5 {
		t.Errorf("smaller value should get <0.5, larger >0.5; got %v, %v", got[0], got[1])
	}
}

func TestLogisticZAllEqualGivesHalf(t *testing.T) {
	got := logisticZ([]float64{7, 7, 7, 7})
	for i, x := range got {
		if math.Abs(x-0.5) > 1e-9 {
			t.Errorf("uniform cohort [%d] = %v, want 0.5", i, x)
		}
	}
}

func TestLogisticZSingleAndEmpty(t *testing.T) {
	if got := logisticZ([]float64{42}); len(got) != 1 || math.Abs(got[0]-0.5) > 1e-9 {
		t.Errorf("single-element logisticZ = %v, want [0.5]", got)
	}
	if got := logisticZ(nil); len(got) != 0 {
		t.Errorf("empty logisticZ returned %d items, want 0", len(got))
	}
}

func TestLogisticZNoMidPackCollapse(t *testing.T) {
	// Cohort with one dominant outlier — min-max collapses everyone else to
	// ~0, but logistic-z should keep mid-pack devs near 0.5. This is the
	// whole point of the swap (Pattern C remediation).
	xs := []float64{1, 2, 3, 4, 5, 100}
	got := logisticZ(xs)
	mm := minMaxNormalize(xs)
	// Mid-pack: index 2 (score 3) sits between min and max; logistic should
	// stay well above 0.1 while min-max squashes it to ~0.02.
	if got[2] < 0.2 {
		t.Errorf("mid-pack under outlier collapses to %v, want > 0.2", got[2])
	}
	if mm[2] > 0.05 {
		t.Errorf("expected min-max to collapse mid-pack to near-zero; got %v", mm[2])
	}
	// Top stays above 0.5; bottom below 0.5.
	if got[5] <= 0.5 {
		t.Errorf("top score logistic = %v, want > 0.5", got[5])
	}
	if got[0] >= 0.5 {
		t.Errorf("bottom score logistic = %v, want < 0.5", got[0])
	}
}

func TestLogisticZAscendingOrderPreserved(t *testing.T) {
	// Monotonicity: same ordering as input, no crossings.
	xs := []float64{-3, -1, 0, 2, 5}
	got := logisticZ(xs)
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Errorf("logisticZ not monotonic: got[%d]=%v <= got[%d]=%v", i, got[i], i-1, got[i-1])
		}
	}
}

func TestMinMaxNormalize(t *testing.T) {
	got := minMaxNormalize([]float64{1, 3, 5, 9})
	want := []float64{0, 0.25, 0.5, 1}
	for i := range got {
		if math.Abs(got[i]-want[i]) > 1e-9 {
			t.Errorf("normalize[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestMinMaxNormalizeAllEqualGivesHalf(t *testing.T) {
	got := minMaxNormalize([]float64{5, 5, 5})
	for _, x := range got {
		if math.Abs(x-0.5) > 1e-9 {
			t.Errorf("all-equal normalize = %v, want 0.5 (drew the period)", x)
		}
	}
}

func TestMinMaxNormalizeEmpty(t *testing.T) {
	got := minMaxNormalize(nil)
	if len(got) != 0 {
		t.Errorf("empty normalize returned %d items, want 0", len(got))
	}
}
