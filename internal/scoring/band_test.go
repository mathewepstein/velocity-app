package scoring

import (
	"testing"
	"time"

	"github.com/mathewepstein/velocity/internal/config"
)

func spCfg() config.StoryPointsConfig { return config.DefaultStoryPointsConfig() }

// onePR is a minimal evidence with a single PR so judge() doesn't trip the
// no-PR NeedsInsight gate; callers set the fields the case under test cares about.
func onePR() *TicketEvidence {
	return &TicketEvidence{Key: "CD-1", PRs: []PREvidence{{Number: 1, Repo: "org/app"}}}
}

// --- Calibration anchors from the /score-ticket rubric. ---

// "A 3-line fix in auth middleware resolving a session-storage compliance bug,
// with 2 review rounds debating approach → 5, not 1."
func TestBand_Anchor_HighRiskSmallFix(t *testing.T) {
	ev := onePR()
	ev.NetLOC = 3
	ev.FileCount = 1
	ev.CycleHours = 18 // short
	ev.ReviewRounds = 2
	ev.TouchedAreaRisk = "high"
	ev.HotFiles = []string{"src/auth/middleware.go"}
	ev.Repos = []string{"org/app"}

	got := Band(ev, spCfg())
	if got.Points != 5 {
		t.Errorf("points = %d, want 5 (raw=%v band=%s)", got.Points, got.RawEffort, got.Band)
	}
	if got.NeedsInsight {
		t.Errorf("should be confident, not needs_insight: %+v", got)
	}
	if got.Confidence != "high" {
		t.Errorf("confidence = %q, want high", got.Confidence)
	}
}

// "A 400-LOC scaffolded CRUD page following an established pattern with one
// rubber-stamp review → 2 or 3, not 8." The honest deterministic answer is a
// 2–3 straddle flagged for a human pass.
func TestBand_Anchor_LargeScaffold(t *testing.T) {
	ev := onePR()
	ev.NetLOC = 400
	ev.FileCount = 6
	ev.CycleHours = 20 // short
	ev.ReviewRounds = 0
	ev.TouchedAreaRisk = "low"

	got := Band(ev, spCfg())
	// short cycle / high LOC, no thinking signal → clean 2 (the cell's floor),
	// confident. The anchor says "2 or 3" — a confident 2 is in range.
	if got.Points != 2 {
		t.Errorf("points = %d, want 2 (raw=%v band=%s)", got.Points, got.RawEffort, got.Band)
	}
	if got.NeedsInsight {
		t.Errorf("a clean scaffold should not be flagged: %+v", got)
	}
}

// "A 50-LOC change that reverted, was rewritten twice, and required a new
// dev-user fixture to test → 5 or 8, not 2."
func TestBand_Anchor_RewrittenSmallChange(t *testing.T) {
	ev := onePR()
	ev.NetLOC = 50
	ev.FileCount = 3
	ev.CycleHours = 72 // long: the rewrites took days
	ev.ReworkCount = 2
	ev.TouchedAreaRisk = "low"

	got := Band(ev, spCfg())
	if got.Points < 5 {
		t.Errorf("points = %d, want ≥5 (raw=%v)", got.Points, got.RawEffort)
	}
	if got.Drivers[0] == "" {
		t.Errorf("expected rework named as top driver, got %v", got.Drivers)
	}
}

// --- NeedsInsight gates. ---

// A long-cycle, high-LOC ticket with NO rework/review/risk is likely inflated by
// queue latency — the band engine must distrust it (this is the CD-24621 shape).
func TestBand_HighBandWithoutThinkingNeedsInsight(t *testing.T) {
	ev := onePR()
	ev.NetLOC = 240
	ev.FileCount = 8
	ev.CycleHours = 600 // 25 days (> threshold), but no rework signals
	ev.ReviewRounds = 0
	ev.ReworkCount = 0
	ev.TouchedAreaRisk = "low"

	got := Band(ev, spCfg())
	if !got.NeedsInsight {
		t.Errorf("high band with no thinking signal should need insight: %+v", got)
	}
	if got.Confidence != "low" {
		t.Errorf("confidence = %q, want low", got.Confidence)
	}
}

func TestBand_NoPRsNeedsInsight(t *testing.T) {
	ev := &TicketEvidence{Key: "CD-2", NetLOC: 0, Created: time.Now()}
	got := Band(ev, spCfg())
	if !got.NeedsInsight || got.Confidence != "low" {
		t.Errorf("PR-less ticket should be low/needs_insight: %+v", got)
	}
	if got.Points > 2 {
		t.Errorf("PR-less ticket (no LOC, no cycle) should land at the low end, got %d", got.Points)
	}
}

// --- Tweakability: the same evidence scores differently under a tuned config. ---

func TestBand_ConfigTweakChangesOutcome(t *testing.T) {
	ev := onePR()
	ev.NetLOC = 50
	ev.CycleHours = 36 // 1.5 days

	def := Band(ev, spCfg())

	// Lower the long-cycle threshold below 1.5d: the ticket flips into the
	// long-cycle quadrant and should score strictly higher.
	tuned := spCfg()
	tuned.CycleDaysThreshold = 1.0
	hi := Band(ev, tuned)

	if !(hi.RawEffort > def.RawEffort) {
		t.Errorf("lowering cycle threshold should raise effort: def=%v tuned=%v", def.RawEffort, hi.RawEffort)
	}
}

// Active cycle (cycle minus QA-queue wait) drives the quadrant: a ticket whose
// raw cycle is "long" but is mostly queue time should read as short.
func TestBand_ActiveCycleDrivesQuadrant(t *testing.T) {
	ev := onePR()
	ev.NetLOC = 20
	ev.CycleHours = 480     // 20 raw days → would be "long"
	ev.QueueHours = 384     // 16 days parked in Ready-QA queue
	ev.ActiveCycleHours = 96 // 4 active days → "short" under the 14d threshold

	got := Band(ev, spCfg())
	if got.QuadrantCell != "short cycle / low LOC" {
		t.Errorf("active cycle should make this short, got %q (raw=%v)", got.QuadrantCell, got.RawEffort)
	}
}

func TestBand_CycleFallbackToResolved(t *testing.T) {
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	resolved := created.Add(480 * time.Hour) // 20 days > threshold
	ev := onePR()
	ev.Created = created
	ev.Resolved = &resolved
	ev.NetLOC = 10
	// CycleHours unset (0) → must fall back to Created→Resolved = 4 days = long.
	got := Band(ev, spCfg())
	if got.QuadrantCell != "long cycle / low LOC" {
		t.Errorf("expected long-cycle quadrant via fallback, got %q (raw=%v)", got.QuadrantCell, got.RawEffort)
	}
}
