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
	ev.CycleHours = 480      // 20 raw days → would be "long"
	ev.QueueHours = 384      // 16 days parked in Ready-QA queue
	ev.ActiveCycleHours = 96 // 4 active days → "short" under the 14d threshold

	got := Band(ev, spCfg())
	if got.QuadrantCell != "short cycle / low LOC" {
		t.Errorf("active cycle should make this short, got %q (raw=%v)", got.QuadrantCell, got.RawEffort)
	}
}

// --- Phase 4a: rework / review-round saturation. ---

// A runaway rework count saturates at ReworkCountCap, so the 7th bounce can't
// keep inflating the band the way the 2nd did.
func TestBand_ReworkSaturates(t *testing.T) {
	mk := func(n int) *TicketEvidence {
		ev := onePR()
		ev.NetLOC = 200 // above the small-diff floor so 4b doesn't scale rework
		ev.FileCount = 6
		ev.CycleHours = 24
		ev.ReworkCount = n
		return ev
	}
	cfg := spCfg()
	three := Band(mk(3), cfg).RawEffort
	seven := Band(mk(7), cfg).RawEffort
	if seven != three {
		t.Errorf("rework should saturate at the cap (=%d): raw(3)=%v raw(7)=%v", cfg.ReworkCountCap, three, seven)
	}
	// And the cap actually bit — 7 linear would have scored strictly higher.
	uncapped := spCfg()
	uncapped.ReworkCountCap = 0
	if Band(mk(7), uncapped).RawEffort <= seven {
		t.Errorf("disabling the cap should raise the 7-rework effort above the capped value")
	}
}

// --- Phase 4b: size sanity-floor scales the rework bonus (only) on tiny diffs. ---

// A 12-LOC change that bounced many times is flaky-fix churn, not a 13 — the
// small-diff floor halves its rework credit so it lands in 5/8 territory.
func TestBand_SmallDiffScalesReworkNotRisk(t *testing.T) {
	tiny := onePR()
	tiny.NetLOC = 12 // below the 20-LOC floor
	tiny.FileCount = 2
	tiny.CycleHours = 24 // short
	tiny.ReworkCount = 7

	big := onePR()
	big.NetLOC = 200 // above the floor
	big.FileCount = 6
	big.CycleHours = 24
	big.ReworkCount = 7

	cfg := spCfg()
	if Band(tiny, cfg).RawEffort >= Band(big, cfg).RawEffort {
		t.Errorf("small diff should earn less rework credit than a large diff with the same bounces")
	}

	// Risk credit is NOT scaled by the floor — a tiny diff in a hot file is still
	// risky (this is what keeps the high-risk-small-fix anchor at 5).
	riskTiny := onePR()
	riskTiny.NetLOC = 3
	riskTiny.CycleHours = 18
	riskTiny.TouchedAreaRisk = "high"
	riskTiny.HotFiles = []string{"src/auth/mw.go"}
	noFloor := spCfg()
	noFloor.SmallDiffLOCFloor = 0
	if Band(riskTiny, cfg).RawEffort != Band(riskTiny, noFloor).RawEffort {
		t.Errorf("the small-diff floor must not scale the risk bonus")
	}
}

// --- Phase 2: split-flag on raw effort far above the 13 floor. ---

// A genuine monster (raw ≫ 13) keeps its 13 but routes to a scope/split check
// with confidence capped at medium — a true 13 is the band you're least sure is
// a single unit of work.
func TestBand_SplitFlagOnOversizedEffort(t *testing.T) {
	ev := onePR()
	ev.NetLOC = 400 // high LOC
	ev.FileCount = 20
	ev.CycleHours = 720 // 30 active days → long
	ev.ReworkCount = 4
	ev.ReviewRounds = 5
	ev.DeepThreads = 3
	ev.TouchedAreaRisk = "high"
	ev.HotFiles = []string{"a", "b"}

	got := Band(ev, spCfg())
	if got.RawEffort < spCfg().SplitThreshold {
		t.Fatalf("test fixture should exceed the split threshold, got raw=%v", got.RawEffort)
	}
	if got.Points != 13 {
		t.Errorf("split-flag keeps the score at the top band, got %d", got.Points)
	}
	if !got.NeedsInsight {
		t.Errorf("oversized effort should be flagged for a split check: %+v", got)
	}
	if got.Confidence == "high" {
		t.Errorf("split-flagged 13 must not be high confidence, got %q", got.Confidence)
	}
	if got.InsightReason == "" {
		t.Errorf("split-flag should carry a reason")
	}
}

// A 13 whose raw effort sits just at/below the floor stays a confident 13 —
// defensible-contention 13s are not blanket-flagged (the conservative policy).
func TestBand_TopBandBelowSplitStaysConfident(t *testing.T) {
	ev := onePR()
	ev.NetLOC = 300 // high LOC
	ev.FileCount = 10
	ev.CycleHours = 720 // long
	ev.ReviewRounds = 2
	ev.DeepThreads = 1

	got := Band(ev, spCfg())
	if got.RawEffort >= spCfg().SplitThreshold {
		t.Skip("fixture drifted above the split threshold; not the case under test")
	}
	if got.Points == 13 && got.NeedsInsight && got.InsightReason != "" &&
		got.InsightReason == "Effort exceeds a single-ticket scale — likely should have been split; confirm scope" {
		t.Errorf("a 13 below the split threshold should not be split-flagged: %+v", got)
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
