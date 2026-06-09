package scoring

import "testing"

// spikeEv builds a PR-less spike ticket; callers set cycle + artifact signals.
func spikeEv() *TicketEvidence {
	return &TicketEvidence{Key: "CD-300", Summary: "Spike: investigate report latency", IssueType: "Task"}
}

// A multi-day spike with research doc + substantive comments → mid-band, NOT
// flagged-low (the standard no-PR gate is suppressed on this path).
func TestBandSpike_WellEvidencedMidBand(t *testing.T) {
	ev := spikeEv()
	ev.CycleHours = 96 // 4 days
	ev.ActiveCycleHours = 96
	ev.ArtifactLinks = 2
	ev.SubstantiveComments = 3
	ev.StatusFlips = 5

	got := Band(ev, spCfg())
	if got.NeedsInsight {
		t.Errorf("well-evidenced spike should not be flagged: %+v", got)
	}
	if got.Points < 3 {
		t.Errorf("multi-day evidenced spike points = %d, want >= 3 (raw=%v)", got.Points, got.RawEffort)
	}
	if got.Confidence != "high" {
		t.Errorf("confidence = %q, want high", got.Confidence)
	}
}

// A "confirm X, one comment" spike → low band, confident-enough, not flagged-low
// for lacking a PR.
func TestBandSpike_TrivialLowBand(t *testing.T) {
	ev := spikeEv()
	ev.CycleHours = 6 // short
	ev.ActiveCycleHours = 6
	ev.ArtifactLinks = 0
	ev.SubstantiveComments = 0

	got := Band(ev, spCfg())
	if got.Points > 2 {
		t.Errorf("trivial spike points = %d, want <= 2", got.Points)
	}
	if got.InsightReason == "No merged PR — typing effort can't be assessed from the diff" {
		t.Error("spike path must suppress the no-PR flag reason")
	}
}

// The genuinely ambiguous case: multi-day elapsed, zero artifacts, little churn
// → flagged for a human (could be dormancy, not investigation).
func TestBandSpike_MultiDayNoArtifactsFlagged(t *testing.T) {
	ev := spikeEv()
	ev.CycleHours = 120
	ev.ActiveCycleHours = 120
	ev.ArtifactLinks = 0
	ev.SubstantiveComments = 0
	ev.StatusFlips = 1

	got := Band(ev, spCfg())
	if !got.NeedsInsight {
		t.Errorf("multi-day no-artifact spike should be flagged: %+v", got)
	}
}

// Spawned follow-up work is investigation output: a multi-day, zero-artifact
// spike that spawned tickets must NOT be flagged as possible dormancy, and the
// spawn count surfaces as a driver.
func TestBandSpike_SpawnedWorkSuppressesDormancyFlag(t *testing.T) {
	ev := spikeEv()
	ev.CycleHours = 120 // multi-day
	ev.ActiveCycleHours = 120
	ev.ArtifactLinks = 0
	ev.SubstantiveComments = 0
	ev.StatusFlips = 1
	ev.SpawnedCount = 2

	got := Band(ev, spCfg())
	if got.NeedsInsight {
		t.Errorf("spike that spawned follow-up work should not be flagged as dormancy: %+v", got)
	}
	found := false
	for _, d := range got.Drivers {
		if d == "Spawned 2 follow-up ticket(s)" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a spawned-work driver, got %v", got.Drivers)
	}
}

// A wide link graph nudges effort up past the breadth threshold and surfaces a
// driver.
func TestBandSpike_BreadthNudge(t *testing.T) {
	base := spikeEv()
	base.CycleHours = 6
	base.ActiveCycleHours = 6
	narrow := Band(base, spCfg())

	wide := spikeEv()
	wide.CycleHours = 6
	wide.ActiveCycleHours = 6
	wide.LinkBreadth = 8 // well past the default threshold of 3
	got := Band(wide, spCfg())

	if got.RawEffort <= narrow.RawEffort {
		t.Errorf("broad link graph should raise raw effort: wide=%v narrow=%v", got.RawEffort, narrow.RawEffort)
	}
	found := false
	for _, d := range got.Drivers {
		if d == "Linked to 8 related ticket(s)" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a breadth driver, got %v", got.Drivers)
	}
}

// Substantive comments feed the artifact axis with diminishing returns: a
// talkative spike (many comments) must not out-band one with a handful, because
// the cap bounds the comment contribution. Mirrors the CD-15865 over-band fix.
func TestBandSpike_SubstantiveCommentCap(t *testing.T) {
	mk := func(comments int) *TicketEvidence {
		ev := spikeEv()
		ev.CycleHours = 120 // multi-day
		ev.ActiveCycleHours = 120
		ev.ArtifactLinks = 0
		ev.SubstantiveComments = comments
		ev.StatusFlips = 2
		return ev
	}
	cfg := spCfg() // default cap = 3

	chatty := Band(mk(12), cfg)
	capped := Band(mk(3), cfg) // already at the cap
	if chatty.Points != capped.Points {
		t.Errorf("comment count past the cap should not raise the band: 12-comment=%d 3-comment=%d", chatty.Points, capped.Points)
	}
	// And the cap actually bites: 12 uncapped comments would push raw to
	// 5.0 + (12-2)*0.5 = 10 (→ 13); capped at 3 it is 5.0 + (3-2)*0.5 = 5.5 (→ 5).
	if chatty.Points > 5 {
		t.Errorf("capped 12-comment spike points = %d, want <= 5 (raw=%v)", chatty.Points, chatty.RawEffort)
	}
}

// A ticket with a PR is never routed to the spike scorer even if it looks like a
// spike by summary — it has a diff to score normally.
func TestBand_SpikeRoutingRequiresNoPR(t *testing.T) {
	ev := spikeEv()
	ev.PRs = []PREvidence{{Number: 1, Repo: "org/app"}}
	ev.NetLOC = 50
	ev.CycleHours = 96
	got := Band(ev, spCfg())
	// QuadrantCell on the standard path uses the LOC×cycle labels.
	if got.QuadrantCell == "" || got.SignalSummary == "" {
		t.Fatal("expected standard band result")
	}
	for _, cell := range []string{"long cycle / low LOC", "long cycle / high LOC", "short cycle / low LOC", "short cycle / high LOC"} {
		if got.QuadrantCell == cell {
			return // standard path, good
		}
	}
	t.Errorf("ticket with PR should use standard quadrant, got cell %q", got.QuadrantCell)
}
