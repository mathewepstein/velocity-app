package scoring

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mathewepstein/velocity/internal/config"
)

// bandSpike scores a PR-less investigation ticket. Spikes have no diff, so the
// standard LOC × cycle quadrant and the "no merged PR → flag low" gate don't
// apply; instead the axis is active-cycle × artifact-density (research links +
// substantive comments), with status churn as a minor nudge. The result reads
// like any other BandResult so downstream (store, /score-ticket, the UI) is
// unchanged. The "no PR" flag is suppressed — a spike legitimately has none.
func bandSpike(ev *TicketEvidence, cfg config.StoryPointsConfig) BandResult {
	scale := cfg.Scale
	if len(scale) == 0 {
		scale = []int{1, 2, 3, 5, 8, 13}
	}
	sc := cfg.Spike

	cycleDays := cycleDays(ev)
	artifacts := ev.ArtifactLinks + ev.SubstantiveComments
	longCycle := cycleDays >= sc.CycleDaysThreshold
	highArtifacts := artifacts >= sc.ArtifactThreshold

	base, cell := spikeQuadrant(sc, longCycle, highArtifacts)

	var drivers []driver
	thinking := 0.0
	add := func(pts float64, format string, args ...any) {
		if pts <= 0 {
			return
		}
		thinking += pts
		drivers = append(drivers, driver{points: pts, text: fmt.Sprintf(format, args...)})
	}

	// Artifacts and status churn are the spike's complexity corroboration. Each
	// distinct planning artifact past the first nudges up modestly; heavy status
	// churn signals a winding investigation.
	if artifacts > sc.ArtifactThreshold {
		add(float64(artifacts-sc.ArtifactThreshold)*0.5, "Investigation depth: %d artifact(s) (%d link, %d substantive comment)", artifacts, ev.ArtifactLinks, ev.SubstantiveComments)
	}
	if ev.StatusFlips >= 4 {
		add(1.0, "Status churn: %d transitions", ev.StatusFlips)
	}

	// Relationship corroboration: follow-up tickets the investigation spawned, and
	// the breadth of its link graph. Both are conservative nudges — the quadrant
	// base still carries the weight — and only fire when the signal is present.
	if ev.SpawnedCount > 0 {
		add(float64(ev.SpawnedCount)*sc.SpawnedWeight, "Spawned %d follow-up ticket(s)", ev.SpawnedCount)
	}
	if sc.BreadthThreshold > 0 && ev.LinkBreadth > sc.BreadthThreshold {
		add(float64(ev.LinkBreadth-sc.BreadthThreshold)*sc.BreadthWeight, "Linked to %d related ticket(s)", ev.LinkBreadth)
	}

	raw := base + thinking
	points, band, straddle := snap(raw, scale, cfg.StraddleFraction)

	res := BandResult{
		Points:       points,
		Band:         band,
		QuadrantCell: cell,
		QuadrantBand: nearestLabel(base, scale),
		RawEffort:    round1(raw),
	}

	sort.SliceStable(drivers, func(i, j int) bool { return drivers[i].points > drivers[j].points })
	for i, d := range drivers {
		if i >= 3 {
			break
		}
		res.Drivers = append(res.Drivers, d.text)
	}

	res.HardestAspectHint, res.SignalSummary = explainSpike(ev, drivers, cycleDays, artifacts, cell)
	res.Confidence, res.NeedsInsight, res.InsightReason = judgeSpike(longCycle, highArtifacts, artifacts, ev.StatusFlips, ev.SpawnedCount, straddle)
	return res
}

// spikeQuadrant returns the base effort + label for the cycle × artifact-density
// cell. Multi-day, well-evidenced investigation is the costliest cell; a quick
// confirm-and-close spike is the cheapest.
func spikeQuadrant(sc config.SpikeConfig, longCycle, highArtifacts bool) (float64, string) {
	switch {
	case longCycle && highArtifacts:
		return sc.BaseLongHigh, "multi-day / high-artifact spike"
	case longCycle && !highArtifacts:
		return sc.BaseLongLow, "multi-day / low-artifact spike"
	case !longCycle && highArtifacts:
		return sc.BaseShortHigh, "short / high-artifact spike"
	default:
		return sc.BaseShortLow, "short / low-artifact spike"
	}
}

// judgeSpike sets confidence + needs-insight for a spike. The "no PR" gate is
// suppressed (a spike has none by design). It flags the one genuinely ambiguous
// case: a multi-day cycle with no recorded artifacts and little churn — the
// elapsed time could be real investigation or just dormancy, and only a human
// can tell. Spawned follow-up work is itself investigation output, so it lifts
// the dormancy doubt and suppresses the flag. A well-evidenced spike is
// confident; a thin short one is medium.
func judgeSpike(longCycle, highArtifacts bool, artifacts, statusFlips, spawned int, straddle bool) (confidence string, needsInsight bool, reason string) {
	if longCycle && artifacts == 0 && statusFlips < 4 && spawned == 0 {
		return "low", true, "Multi-day spike with no recorded artifacts — elapsed time may be dormancy, not investigation; confirm effort"
	}
	if highArtifacts && longCycle {
		return "high", false, ""
	}
	if straddle {
		return "medium", false, ""
	}
	return "medium", false, ""
}

// explainSpike builds the spike hint + one-line summary (no LOC axis).
func explainSpike(ev *TicketEvidence, drivers []driver, cycleDays float64, artifacts int, cell string) (hint, summary string) {
	if len(drivers) > 0 {
		hint = drivers[0].text
	} else {
		hint = fmt.Sprintf("Investigation spike — sized on cycle (%.1fd) and %d artifact(s)", cycleDays, artifacts)
	}
	parts := []string{
		"spike",
		fmt.Sprintf("cycle %.1fd", cycleDays),
		fmt.Sprintf("%d artifact(s) (%d link/%d comment)", artifacts, ev.ArtifactLinks, ev.SubstantiveComments),
		fmt.Sprintf("%d status flips", ev.StatusFlips),
	}
	summary = strings.Join(parts, " · ")
	return hint, summary
}
