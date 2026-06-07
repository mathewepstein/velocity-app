package cache

import (
	"testing"
	"time"
)

// fixedNow is 2024-03-15 UTC — inside March, day 15 (past grace for Feb).
var fixedNow = time.Date(2024, time.March, 15, 12, 0, 0, 0, time.UTC)

// earlyMarchNow is 2024-03-05 UTC — inside March, day 5 (still in grace for Feb).
var earlyMarchNow = time.Date(2024, time.March, 5, 12, 0, 0, 0, time.UTC)

func makePulledManifest(t *testing.T, src Source, scope string, m Month) *Manifest {
	t.Helper()
	mf := NewManifest()
	// Model a healthy pull: captured shortly after the month closed (3rd of the
	// following month), so the grace/freeze assertions exercise those rules
	// rather than the partial-capture re-pull. A month pulled while still in
	// progress is covered by TestNeedsPull_PartialMonthRepulledPastGrace.
	pulledAt := m.Add(1).Start().AddDate(0, 0, 2)
	mf.Update(src, scope, m, 42, pulledAt)
	return mf
}

func TestNeedsPull_ForceAlwaysTrue(t *testing.T) {
	mf := makePulledManifest(t, SourceJira, "CD", MustParseMonth("2020-01"))
	if !NeedsPull(mf, SourceJira, "CD", MustParseMonth("2020-01"), fixedNow, true /* force */) {
		t.Error("force=true should always return true")
	}
}

func TestNeedsPull_MissingEntryIsTrue(t *testing.T) {
	mf := NewManifest() // empty
	if !NeedsPull(mf, SourceJira, "CD", MustParseMonth("2024-01"), fixedNow, false) {
		t.Error("missing manifest entry should return true")
	}
}

func TestNeedsPull_NilManifestIsTrue(t *testing.T) {
	if !NeedsPull(nil, SourceJira, "CD", MustParseMonth("2024-01"), fixedNow, false) {
		t.Error("nil manifest should return true")
	}
}

func TestNeedsPull_CurrentMonth(t *testing.T) {
	current := MustParseMonth("2024-03")
	mf := makePulledManifest(t, SourceJira, "CD", current)
	if !NeedsPull(mf, SourceJira, "CD", current, fixedNow, false) {
		t.Error("current month should always be pulled, even if cached")
	}
}

func TestNeedsPull_LastClosedMonthInGrace(t *testing.T) {
	lastClosed := MustParseMonth("2024-02")
	mf := makePulledManifest(t, SourceJira, "CD", lastClosed)
	if !NeedsPull(mf, SourceJira, "CD", lastClosed, earlyMarchNow, false) {
		t.Error("last closed month on day 5 should re-pull (grace window)")
	}
}

func TestNeedsPull_LastClosedMonthAfterGrace(t *testing.T) {
	lastClosed := MustParseMonth("2024-02")
	mf := makePulledManifest(t, SourceJira, "CD", lastClosed)
	if NeedsPull(mf, SourceJira, "CD", lastClosed, fixedNow, false) {
		t.Error("last closed month on day 15 should be frozen (past grace)")
	}
}

func TestNeedsPull_LastClosedMonthOnGraceBoundary(t *testing.T) {
	lastClosed := MustParseMonth("2024-02")
	mf := makePulledManifest(t, SourceJira, "CD", lastClosed)
	day7 := time.Date(2024, time.March, 7, 23, 59, 0, 0, time.UTC)
	if !NeedsPull(mf, SourceJira, "CD", lastClosed, day7, false) {
		t.Error("day 7 should still be inside grace")
	}
	day8 := time.Date(2024, time.March, 8, 0, 0, 0, 0, time.UTC)
	if NeedsPull(mf, SourceJira, "CD", lastClosed, day8, false) {
		t.Error("day 8 should be past grace")
	}
}

func TestNeedsPull_PartialMonthRepulledPastGrace(t *testing.T) {
	// February captured on Feb 10 (mid-month → partial). Checked March 15,
	// well past grace. The old grace-only rule froze it incomplete; the
	// pull-anchored rule re-pulls because it was never captured after close.
	// This is the "cron only runs after the 7th" hole.
	feb := MustParseMonth("2024-02")
	mf := NewManifest()
	mf.Update(SourceJira, "CD", feb, 42, time.Date(2024, time.February, 10, 0, 0, 0, 0, time.UTC))
	if !NeedsPull(mf, SourceJira, "CD", feb, fixedNow, false) {
		t.Error("a month pulled mid-month (partial) should re-pull even past grace")
	}
	// After a post-close re-pull (early March), the month freezes.
	mf.Update(SourceJira, "CD", feb, 42, time.Date(2024, time.March, 3, 0, 0, 0, 0, time.UTC))
	if NeedsPull(mf, SourceJira, "CD", feb, fixedNow, false) {
		t.Error("after a post-close re-pull, the month should freeze")
	}
}

func TestNeedsPull_OldMonthFrozen(t *testing.T) {
	old := MustParseMonth("2020-06")
	mf := makePulledManifest(t, SourceJira, "CD", old)
	if NeedsPull(mf, SourceJira, "CD", old, fixedNow, false) {
		t.Error("old cached month should be frozen")
	}
}

func TestNeedsPull_YearBoundaryGrace(t *testing.T) {
	// January 5, 2024 — grace window for December 2023.
	jan5 := time.Date(2024, time.January, 5, 12, 0, 0, 0, time.UTC)
	dec2023 := MustParseMonth("2023-12")
	mf := makePulledManifest(t, SourceJira, "CD", dec2023)
	if !NeedsPull(mf, SourceJira, "CD", dec2023, jan5, false) {
		t.Error("Dec should re-pull on Jan 5 (grace across year boundary)")
	}
	// January 15 — past grace.
	jan15 := time.Date(2024, time.January, 15, 12, 0, 0, 0, time.UTC)
	if NeedsPull(mf, SourceJira, "CD", dec2023, jan15, false) {
		t.Error("Dec should be frozen on Jan 15")
	}
}
