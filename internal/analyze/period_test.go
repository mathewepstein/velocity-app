package analyze

import (
	"testing"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
)

func TestPeriodForWeek(t *testing.T) {
	cases := []struct {
		week string
		want string
	}{
		{"2026-W01", "2026-P01"},
		{"2026-W02", "2026-P01"},
		{"2026-W03", "2026-P03"},
		{"2026-W17", "2026-P17"},
		{"2026-W18", "2026-P17"},
		{"2024-W52", "2024-P51"}, // even week → odd-week start
		{"2025-W01", "2025-P01"}, // year boundary
	}
	for _, tc := range cases {
		got, err := periodForWeek(tc.week)
		if err != nil {
			t.Errorf("periodForWeek(%q): %v", tc.week, err)
			continue
		}
		if got != tc.want {
			t.Errorf("periodForWeek(%q) = %q, want %q", tc.week, got, tc.want)
		}
	}
}

func TestPeriodForWeekRejectsBadInput(t *testing.T) {
	for _, bad := range []string{"", "2026", "2026-W", "2026-W00", "2026-W54", "abc-W01"} {
		if _, err := periodForWeek(bad); err == nil {
			t.Errorf("periodForWeek(%q) should fail", bad)
		}
	}
}

func TestPeriodStartAndEndRoundTrip(t *testing.T) {
	cases := []struct {
		period          string
		wantStart       string // YYYY-MM-DD
		wantEndContains string
	}{
		// 2026-W01 starts Mon 2025-12-29 (ISO-year 2026); W02 ends Sun 2026-01-11.
		{"2026-P01", "2025-12-29", "2026-01-11"},
		// 2024-W19 starts Mon 2024-05-06; W20 ends Sun 2024-05-19.
		{"2024-P19", "2024-05-06", "2024-05-19"},
	}
	for _, tc := range cases {
		start, err := periodStart(tc.period)
		if err != nil {
			t.Errorf("periodStart(%q): %v", tc.period, err)
			continue
		}
		if start.Format("2006-01-02") != tc.wantStart {
			t.Errorf("periodStart(%q) = %s, want %s", tc.period, start.Format("2006-01-02"), tc.wantStart)
		}
		end, err := periodEnd(tc.period)
		if err != nil {
			t.Errorf("periodEnd(%q): %v", tc.period, err)
			continue
		}
		if end.Format("2006-01-02") != tc.wantEndContains {
			t.Errorf("periodEnd(%q) = %s, want %s", tc.period, end.Format("2006-01-02"), tc.wantEndContains)
		}
	}
}

func TestBiweeklyPeriodsInRange(t *testing.T) {
	start := cache.MustParseMonth("2024-05")
	end := cache.MustParseMonth("2024-06")
	got, err := biweeklyPeriodsInRange(start, end)
	if err != nil {
		t.Fatalf("biweeklyPeriodsInRange: %v", err)
	}
	// May 2024 = ISO weeks 18-22; June 2024 = ISO weeks 22-26. Periods: P17
	// (W17+W18), P19, P21, P23, P25.
	want := []string{"2024-P17", "2024-P19", "2024-P21", "2024-P23", "2024-P25"}
	if len(got) != len(want) {
		t.Fatalf("biweeklyPeriodsInRange len = %d, want %d (got %v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("periods[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCompletedPeriodsBetweenExcludesInProgress(t *testing.T) {
	// "Now" sits inside 2024-P21 (which spans 2024-05-20 .. 2024-06-02). Only
	// completed periods through P19 should be returned.
	now := time.Date(2024, 5, 25, 12, 0, 0, 0, time.UTC)
	got, err := completedPeriodsBetween(cache.MustParseMonth("2024-05"), cache.MustParseMonth("2024-06"), now)
	if err != nil {
		t.Fatalf("completedPeriodsBetween: %v", err)
	}
	for _, p := range got {
		if p == "2024-P21" || p == "2024-P23" || p == "2024-P25" {
			t.Errorf("in-progress / future period %q should not be returned (now=%s)", p, now)
		}
	}
	want := []string{"2024-P17", "2024-P19"}
	if len(got) != len(want) {
		t.Fatalf("completed len = %d (%v), want %d (%v)", len(got), got, len(want), want)
	}
}
