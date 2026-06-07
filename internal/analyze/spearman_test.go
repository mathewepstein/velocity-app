package analyze

import (
	"math"
	"testing"

	"github.com/mathewepstein/velocity/internal/config"
)

// dev is a tiny helper that builds a DevWindowMetrics with whatever score
// and rating the test cares about. Nil pointers stand in for "no signal".
func dev(name string, score *ContributorScore, rating *EloRating) DevWindowMetrics {
	return DevWindowMetrics{
		Dev:    config.DevIdentity{DisplayName: name, GitHubLogins: []string{name}},
		Score:  score,
		Rating: rating,
	}
}

func TestSpearmanRhoIdenticalOrderings(t *testing.T) {
	devs := []DevWindowMetrics{
		dev("a", &ContributorScore{Total: 3}, &EloRating{Current: 1300, PeriodsPlayed: 10}),
		dev("b", &ContributorScore{Total: 2}, &EloRating{Current: 1200, PeriodsPlayed: 10}),
		dev("c", &ContributorScore{Total: 1}, &EloRating{Current: 1100, PeriodsPlayed: 10}),
	}
	if got := spearmanRho(devs); math.Abs(got-1.0) > 1e-9 {
		t.Errorf("identical orderings ρ = %v, want 1", got)
	}
}

func TestSpearmanRhoPerfectInversion(t *testing.T) {
	devs := []DevWindowMetrics{
		dev("a", &ContributorScore{Total: 4}, &EloRating{Current: 1000, PeriodsPlayed: 10}),
		dev("b", &ContributorScore{Total: 3}, &EloRating{Current: 1100, PeriodsPlayed: 10}),
		dev("c", &ContributorScore{Total: 2}, &EloRating{Current: 1200, PeriodsPlayed: 10}),
		dev("d", &ContributorScore{Total: 1}, &EloRating{Current: 1300, PeriodsPlayed: 10}),
	}
	if got := spearmanRho(devs); math.Abs(got-(-1.0)) > 1e-9 {
		t.Errorf("perfect inversion ρ = %v, want -1", got)
	}
}

func TestSpearmanRhoTextbookExample(t *testing.T) {
	// Composite ranks: 1,2,3,4,5. Elo ranks: 1,2,4,3,5.
	// d² = 0,0,1,1,0 → Σ = 2. ρ = 1 - 6·2/(5·24) = 0.9.
	devs := []DevWindowMetrics{
		dev("a", &ContributorScore{Total: 5}, &EloRating{Current: 50, PeriodsPlayed: 10}),
		dev("b", &ContributorScore{Total: 4}, &EloRating{Current: 40, PeriodsPlayed: 10}),
		dev("c", &ContributorScore{Total: 3}, &EloRating{Current: 20, PeriodsPlayed: 10}),
		dev("d", &ContributorScore{Total: 2}, &EloRating{Current: 30, PeriodsPlayed: 10}),
		dev("e", &ContributorScore{Total: 1}, &EloRating{Current: 10, PeriodsPlayed: 10}),
	}
	if got := spearmanRho(devs); math.Abs(got-0.9) > 1e-9 {
		t.Errorf("textbook ρ = %v, want 0.9", got)
	}
}

func TestSpearmanRhoMidRankTies(t *testing.T) {
	// Composite: [3, 3, 1] → mid-ranks [1.5, 1.5, 3]. Elo: [5, 4, 1] → ranks [1, 2, 3].
	// d² = 0.25 + 0.25 + 0 = 0.5. ρ = 1 - 6·0.5/(3·8) = 0.875.
	devs := []DevWindowMetrics{
		dev("a", &ContributorScore{Total: 3}, &EloRating{Current: 5, PeriodsPlayed: 10}),
		dev("b", &ContributorScore{Total: 3}, &EloRating{Current: 4, PeriodsPlayed: 10}),
		dev("c", &ContributorScore{Total: 1}, &EloRating{Current: 1, PeriodsPlayed: 10}),
	}
	if got := spearmanRho(devs); math.Abs(got-0.875) > 1e-9 {
		t.Errorf("mid-rank tie ρ = %v, want 0.875", got)
	}
}

func TestSpearmanRhoFiltersIncomplete(t *testing.T) {
	// Only one dev carries both signals + nonzero PeriodsPlayed; rest filtered.
	// n < 2 → ρ = 0.
	devs := []DevWindowMetrics{
		dev("complete", &ContributorScore{Total: 1}, &EloRating{Current: 1000, PeriodsPlayed: 5}),
		dev("no-score", nil, &EloRating{Current: 1000, PeriodsPlayed: 5}),
		dev("no-rating", &ContributorScore{Total: 1}, nil),
		dev("never-played", &ContributorScore{Total: 1}, &EloRating{Current: 1000, PeriodsPlayed: 0}),
	}
	if got := spearmanRho(devs); got != 0 {
		t.Errorf("under-cohort ρ = %v, want 0 (degenerate)", got)
	}
}

func TestSpearmanRhoEmpty(t *testing.T) {
	if got := spearmanRho(nil); got != 0 {
		t.Errorf("empty ρ = %v, want 0", got)
	}
}

func TestMidRanksDescendingNoTies(t *testing.T) {
	got := midRanks([]float64{10, 30, 20})
	want := []float64{3, 1, 2}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("midRanks[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestMidRanksTieBracket(t *testing.T) {
	// Three tied at the top of a 5-item list → average of (1,2,3) = 2 each.
	got := midRanks([]float64{5, 5, 5, 3, 1})
	want := []float64{2, 2, 2, 4, 5}
	for i := range got {
		if math.Abs(got[i]-want[i]) > 1e-9 {
			t.Errorf("midRanks[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestMidRanksAllEqual(t *testing.T) {
	got := midRanks([]float64{7, 7, 7})
	// All three tied → average of (1,2,3) = 2.
	for i, r := range got {
		if math.Abs(r-2.0) > 1e-9 {
			t.Errorf("midRanks[%d] = %v, want 2", i, r)
		}
	}
}
