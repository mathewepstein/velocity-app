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
