package analyze

import (
	"testing"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

// testCI returns the default code-impact coefficients used by tests that
// don't care about scoring. Kept here (not at package scope in production
// code) so production callers must go through config.DefaultScoringConfig.
func testCI() config.CodeImpactConfig {
	return config.DefaultScoringConfig().CodeImpact
}

// testNorm is the parallel helper for the Phase 6.2 normalization knobs.
// Tests that don't exercise the multiplier still need a populated config
// because the new signatures thread it through; this provides the defaults.
func testNorm() config.NormalizeConfig {
	return config.DefaultScoringConfig().Normalize
}

func TestPriorWindow_SameLengthJustBefore(t *testing.T) {
	data := &Loaded{}
	current := cache.MustParseMonth("2026-04")
	curWin := currentWindow(data, current, 4, testCI(), testNorm()) // 2026-01..2026-04
	prior := priorWindow(data, curWin, testCI(), testNorm())
	if prior.Window.Start != "2025-09" || prior.Window.End != "2025-12" {
		t.Errorf("want 2025-09..2025-12, got %s..%s", prior.Window.Start, prior.Window.End)
	}
	if prior.Window.LengthMonths != 4 {
		t.Errorf("length: want 4, got %d", prior.Window.LengthMonths)
	}
}

func TestYoYWindow_TwelveMonthsBack(t *testing.T) {
	data := &Loaded{}
	current := cache.MustParseMonth("2026-04")
	curWin := currentWindow(data, current, 4, testCI(), testNorm())
	yoy := yoyWindow(data, curWin, testCI(), testNorm())
	// 2026-01..2026-04 minus 12 months = 2025-01..2025-04
	if yoy.Window.Start != "2025-01" || yoy.Window.End != "2025-04" {
		t.Errorf("got %s..%s", yoy.Window.Start, yoy.Window.End)
	}
	if yoy.Window.LengthMonths != 4 {
		t.Errorf("length: want 4, got %d", yoy.Window.LengthMonths)
	}
}

func TestQuarterAddAndLabel(t *testing.T) {
	q := quarter{Year: 2026, Q: 2}
	tests := []struct {
		add  int
		want string
	}{
		{0, "2026-Q2"},
		{-1, "2026-Q1"},
		{-2, "2025-Q4"},
		{-5, "2025-Q1"},
		{-6, "2024-Q4"},
		{1, "2026-Q3"},
		{2, "2026-Q4"},
		{3, "2027-Q1"},
	}
	for _, tc := range tests {
		got := q.add(tc.add).label()
		if got != tc.want {
			t.Errorf("q.add(%d) = %s, want %s", tc.add, got, tc.want)
		}
	}
}

func TestQuarterMonthRange(t *testing.T) {
	tests := []struct {
		q              quarter
		wantStart, end string
	}{
		{quarter{2026, 1}, "2026-01", "2026-03"},
		{quarter{2026, 2}, "2026-04", "2026-06"},
		{quarter{2026, 3}, "2026-07", "2026-09"},
		{quarter{2026, 4}, "2026-10", "2026-12"},
	}
	for _, tc := range tests {
		start, end := tc.q.monthRange()
		if start.String() != tc.wantStart || end.String() != tc.end {
			t.Errorf("%s: got %s..%s, want %s..%s", tc.q.label(), start, end, tc.wantStart, tc.end)
		}
	}
}

func TestLastQuarters_ChronologicalOldestFirst(t *testing.T) {
	data := &Loaded{}
	current := cache.MustParseMonth("2026-04") // Q2 2026
	quarters := lastQuarters(data, current, 4, testCI(), testNorm())
	if len(quarters) != 4 {
		t.Fatalf("want 4 quarters, got %d", len(quarters))
	}
	wantLabels := []string{"2025-Q3", "2025-Q4", "2026-Q1", "2026-Q2"}
	for i, q := range quarters {
		if q.Label != wantLabels[i] {
			t.Errorf("quarters[%d].Label = %s, want %s", i, q.Label, wantLabels[i])
		}
	}
}

func TestFullHistory_CoversBackfillToCurrent(t *testing.T) {
	data := &Loaded{}
	start := cache.MustParseMonth("2019-11")
	end := cache.MustParseMonth("2026-04")
	hist := fullHistory(data, start, end, testCI(), testNorm())
	wantMonths := len(cache.MonthsInRange(start, end)) // 78
	if len(hist.Monthly) != wantMonths {
		t.Errorf("full history months: got %d, want %d", len(hist.Monthly), wantMonths)
	}
	if hist.Monthly[0].Month != "2019-11" || hist.Monthly[len(hist.Monthly)-1].Month != "2026-04" {
		t.Errorf("boundaries wrong: %s..%s", hist.Monthly[0].Month, hist.Monthly[len(hist.Monthly)-1].Month)
	}
}
