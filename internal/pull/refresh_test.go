package pull

import (
	"testing"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

// profileLen4 is the default profile pinned to a 4-month routine window, so
// these window-math tests stay fixed regardless of the config default (which
// the home redesign dropped to 3).
func profileLen4() config.Profile {
	p := config.DefaultProfileConfig()
	p.Window.DefaultLengthMonths = 4
	return p
}

func mustMonth(s string) cache.Month { return cache.MustParseMonth(s) }

func TestEffectiveStart_AnchorsToLastPullOnLapse(t *testing.T) {
	// Last pull was 2024-02; we return in 2024-08 — a gap longer than the
	// 4-month window. effectiveStart must reach back to 2024-02, not stop at
	// the normal window start (2024-05), or 2024-03/04 would be skipped.
	mf := cache.NewManifest()
	mf.Update(cache.SourceJira, "CD", mustMonth("2024-02"), 42,
		time.Date(2024, time.February, 10, 0, 0, 0, 0, time.UTC))
	opts := RefreshOptions{Now: time.Date(2024, time.August, 15, 12, 0, 0, 0, time.UTC)}

	got := effectiveStart(mf, profileLen4(), opts)
	if !got.Equal(mustMonth("2024-02")) {
		t.Errorf("effectiveStart = %s, want 2024-02 (anchored to last pull)", got)
	}
}

func TestEffectiveStart_UsesNormalWindowWhenRecent(t *testing.T) {
	// Last pull is inside the normal window; no catch-up, so the window start
	// (current - 3 = 2024-05) wins over the last pulled month.
	mf := cache.NewManifest()
	mf.Update(cache.SourceJira, "CD", mustMonth("2024-07"), 42,
		time.Date(2024, time.July, 31, 0, 0, 0, 0, time.UTC))
	opts := RefreshOptions{Now: time.Date(2024, time.August, 15, 12, 0, 0, 0, time.UTC)}

	got := effectiveStart(mf, profileLen4(), opts)
	if !got.Equal(mustMonth("2024-05")) {
		t.Errorf("effectiveStart = %s, want 2024-05 (normal window)", got)
	}
}

func TestEffectiveStart_SinceOverrides(t *testing.T) {
	since := mustMonth("2023-01")
	mf := cache.NewManifest()
	mf.Update(cache.SourceJira, "CD", mustMonth("2024-07"), 42,
		time.Date(2024, time.July, 31, 0, 0, 0, 0, time.UTC))
	opts := RefreshOptions{Now: time.Date(2024, time.August, 15, 0, 0, 0, 0, time.UTC), Since: &since}

	got := effectiveStart(mf, profileLen4(), opts)
	if !got.Equal(since) {
		t.Errorf("effectiveStart = %s, want %s (--since override)", got, since)
	}
}

func TestEffectiveStart_EmptyManifestUsesBackfill(t *testing.T) {
	opts := RefreshOptions{Now: time.Date(2024, time.August, 15, 0, 0, 0, 0, time.UTC)}
	got := effectiveStart(cache.NewManifest(), profileLen4(), opts)
	if !got.Equal(mustMonth("2019-11")) {
		t.Errorf("effectiveStart = %s, want 2019-11 (config backfill_start)", got)
	}
}

// TestRefreshPlan_LapseCoverage is the coverage guarantee from the backfill
// plan's P0: pin `now` several months past the last manifest entry and assert
// (a) every gap month is pulled and (b) the last partial month is re-pulled.
//
// Setup: last pull 2024-02, captured on Feb 10 while February was still in
// progress (partial). now = 2024-08-15, 4-month window. The lapse (Feb→Aug)
// exceeds the window, so March/April would be skipped under the old fixed
// offset (Hole 1), and frozen-partial February would never be re-pulled
// (Hole 2). The plan must reach back to Feb, and the per-month pull decision
// (NeedsPull) must elect to pull every month in it.
func TestRefreshPlan_LapseCoverage(t *testing.T) {
	const src, scope = cache.SourceJira, "CD"
	partialFeb := mustMonth("2024-02")
	mf := cache.NewManifest()
	mf.Update(src, scope, partialFeb, 42,
		time.Date(2024, time.February, 10, 0, 0, 0, 0, time.UTC)) // pulled mid-Feb → partial
	now := time.Date(2024, time.August, 15, 12, 0, 0, 0, time.UTC)
	opts := RefreshOptions{Now: now}

	months := refreshPlan(mf, profileLen4(), opts)

	// The plan must span every month from the last pull through the current
	// month — no gap silently dropped (Hole 1).
	want := []string{"2024-02", "2024-03", "2024-04", "2024-05", "2024-06", "2024-07", "2024-08"}
	if got := monthStrings(months); !equalStrings(got, want) {
		t.Fatalf("plan = %v, want %v", got, want)
	}

	// Every month in the plan must actually be pulled: the gap months have no
	// entry, and partial February is re-pulled by the pull-anchored freshness
	// rule (Hole 2).
	for _, m := range months {
		if !cache.NeedsPull(mf, src, scope, m, now, false) {
			t.Errorf("month %s is in the plan but NeedsPull says skip", m)
		}
	}
}

func monthStrings(ms []cache.Month) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.String()
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
