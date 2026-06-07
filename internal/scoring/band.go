package scoring

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mathewepstein/velocity/internal/config"
)

// BandResult is the deterministic Stage-2 output for one ticket: a Fibonacci
// band plus the reasoning behind it. It is a *prior*, not a verdict — when
// NeedsInsight is true the deterministic signals are too thin or contradictory
// to trust, and the frontend routes the ticket to a human/LLM pass.
type BandResult struct {
	Points       int    `json:"points"`        // the snapped single-value pick
	Band         string `json:"band"`          // "3", or "2–3" when straddling
	Confidence   string `json:"confidence"`    // low | medium | high
	NeedsInsight bool   `json:"needs_insight"` // route to a human/LLM pass

	QuadrantCell string `json:"quadrant_cell"` // e.g. "short cycle / high LOC"
	QuadrantBand string `json:"quadrant_band"` // the prior band before signal nudges

	Drivers           []string `json:"drivers"`             // top contributing signals, with evidence
	SignalSummary     string   `json:"signal_summary"`      // one-line rollup (mirrors /score-ticket)
	HardestAspectHint string   `json:"hardest_aspect_hint"` // generated; generic ⇒ lower confidence

	RawEffort float64 `json:"raw_effort"` // continuous score before snapping (transparency)
}

// driver pairs a signal's effort contribution with a human-readable line, so the
// engine can rank drivers by impact and explain the band.
type driver struct {
	points float64
	text   string
}

// Band computes the deterministic band for ev under cfg. It is pure: same
// evidence + config always yields the same result.
func Band(ev *TicketEvidence, cfg config.StoryPointsConfig) BandResult {
	scale := cfg.Scale
	if len(scale) == 0 {
		scale = []int{1, 2, 3, 5, 8, 13}
	}

	cycleDays := cycleDays(ev)
	highLOC := ev.NetLOC >= cfg.LOCThreshold
	longCycle := cycleDays >= cfg.CycleDaysThreshold

	base, cell := quadrant(cfg, longCycle, highLOC)

	// Thinking / process nudges. Each is recorded as a driver so the band is
	// explainable and the dominant complexity source can be named.
	var drivers []driver
	thinking := 0.0
	add := func(pts float64, format string, args ...any) {
		if pts <= 0 {
			return
		}
		thinking += pts
		drivers = append(drivers, driver{points: pts, text: fmt.Sprintf(format, args...)})
	}

	if ev.ReworkCount > 0 {
		add(float64(ev.ReworkCount)*cfg.ReworkWeight, "Rework: %d backward bounce(s) from review/QA into dev", ev.ReworkCount)
	}
	if ev.ReviewRounds > 0 {
		add(float64(ev.ReviewRounds)*cfg.ReviewRoundWeight, "Review back-and-forth: %d changes-requested round(s)", ev.ReviewRounds)
	}
	if ev.DeepThreads > 0 {
		add(float64(ev.DeepThreads)*cfg.DeepThreadWeight, "Approach contention: %d deep review thread(s)", ev.DeepThreads)
	}
	switch ev.TouchedAreaRisk {
	case "high":
		add(cfg.HighRiskBonus, "High-risk area: %d hot file(s) touched (%s)", len(ev.HotFiles), hotFilesPreview(ev.HotFiles))
	case "medium":
		add(cfg.MediumRiskBonus, "Moderate-risk area: %d hot file(s) touched", len(ev.HotFiles))
	}
	if len(ev.Repos) > 1 {
		add(cfg.CrossRepoBonus, "Cross-repo change spanning %d repos", len(ev.Repos))
	}

	// Cap the total thinking contribution so one runaway signal can't max the
	// band; reaching the top band requires a high quadrant base plus substantial
	// thinking, not a single inflated count.
	effThinking := thinking
	if cfg.MaxThinkingBonus > 0 && effThinking > cfg.MaxThinkingBonus {
		effThinking = cfg.MaxThinkingBonus
	}
	raw := base + effThinking
	points, band, straddle := snap(raw, scale, cfg.StraddleFraction)

	res := BandResult{
		Points:       points,
		Band:         band,
		QuadrantCell: cell,
		QuadrantBand: nearestLabel(base, scale),
		RawEffort:    round1(raw),
	}

	// Rank drivers by contribution; keep the top 3. Size is a floor signal, so
	// it's only named when no thinking signal stepped forward.
	sort.SliceStable(drivers, func(i, j int) bool { return drivers[i].points > drivers[j].points })
	for i, d := range drivers {
		if i >= 3 {
			break
		}
		res.Drivers = append(res.Drivers, d.text)
	}

	res.HardestAspectHint, res.SignalSummary = explain(ev, drivers, cycleDays, cell)
	res.Confidence, res.NeedsInsight = judge(ev, cfg, points, straddle, thinking, len(drivers) == 0)
	return res
}

// quadrant returns the base effort + a label for the cycle-time × LOC cell.
func quadrant(cfg config.StoryPointsConfig, longCycle, highLOC bool) (float64, string) {
	switch {
	case longCycle && highLOC:
		return cfg.BaseLongHigh, "long cycle / high LOC"
	case longCycle && !highLOC:
		return cfg.BaseLongLow, "long cycle / low LOC"
	case !longCycle && highLOC:
		return cfg.BaseShortHigh, "short cycle / high LOC"
	default:
		return cfg.BaseShortLow, "short cycle / low LOC"
	}
}

// judge decides confidence + whether a human/LLM pass is needed.
//
// NeedsInsight fires when the deterministic band can't be trusted:
//   - no merged PRs → can't assess typing effort at all;
//   - straddle → raw effort sits between two Fibonacci steps;
//   - high band (≥5) driven by the quadrant with negligible thinking signal →
//     likely inflated by cycle time (QA-queue latency, not real complexity);
//   - generic-only driver (size, no thinking signal) on a non-trivial band.
func judge(ev *TicketEvidence, cfg config.StoryPointsConfig, points int, straddle bool, thinking float64, noDrivers bool) (string, bool) {
	switch {
	case len(ev.PRs) == 0:
		return "low", true
	case straddle:
		return "low", true
	case points >= 5 && thinking < cfg.MinThinkingForHighBand:
		// The band is high but nothing corroborates the difficulty.
		return "low", true
	case noDrivers && points >= 3:
		// Sized into a 3+ purely on volume with zero thinking signal.
		return "low", true
	}

	// Confident when a real thinking signal is present and we're not on a boundary.
	if thinking >= cfg.MinThinkingForHighBand && !straddle {
		return "high", false
	}
	return "medium", false
}

// explain builds the hardest-aspect hint + the one-line signal summary.
func explain(ev *TicketEvidence, drivers []driver, cycleDays float64, cell string) (hint, summary string) {
	if len(drivers) > 0 {
		hint = drivers[0].text // already sorted by contribution
	} else {
		hint = fmt.Sprintf("No standout complexity signal — sized on volume (%d net LOC across %d files)", ev.NetLOC, ev.FileCount)
	}

	parts := []string{
		fmt.Sprintf("cycle %.1fd", cycleDays),
		fmt.Sprintf("%d net LOC / %d files", ev.NetLOC, ev.FileCount),
	}
	if len(ev.PRs) != 1 {
		parts = append(parts, fmt.Sprintf("%d PRs", len(ev.PRs)))
	}
	parts = append(parts,
		fmt.Sprintf("%d review round(s)", ev.ReviewRounds),
		fmt.Sprintf("%d rework", ev.ReworkCount),
		fmt.Sprintf("risk %s", emptyTo(ev.TouchedAreaRisk, "low")),
	)
	summary = strings.Join(parts, " · ")
	return hint, summary
}

// cycleDays returns the cycle in days used for the quadrant. It prefers the
// *active* cycle (In-Progress→Done minus QA-queue/wait time) so dead queue time
// doesn't inflate the band; falls back to raw cycle when queue data is absent,
// then to Created→Resolved calendar time when there's no changelog cycle at all.
func cycleDays(ev *TicketEvidence) float64 {
	if ev.ActiveCycleHours > 0 {
		return ev.ActiveCycleHours / 24
	}
	if ev.CycleHours > 0 {
		return ev.CycleHours / 24
	}
	if ev.Resolved != nil && ev.Resolved.After(ev.Created) {
		return ev.Resolved.Sub(ev.Created).Hours() / 24
	}
	return 0
}

// snap maps a continuous effort to the nearest scale step, returning the picked
// value, a band label (a range like "2–3" when straddling the midpoint), and
// whether it straddled. straddleFrac scales the midpoint dead-zone width by the
// gap between the two adjacent steps.
func snap(raw float64, scale []int, straddleFrac float64) (points int, band string, straddle bool) {
	if raw <= float64(scale[0]) {
		return scale[0], itos(scale[0]), false
	}
	last := scale[len(scale)-1]
	if raw >= float64(last) {
		return last, itos(last), false
	}
	for i := 0; i < len(scale)-1; i++ {
		lo, hi := scale[i], scale[i+1]
		if raw < float64(lo) || raw > float64(hi) {
			continue
		}
		mid := float64(lo+hi) / 2
		gap := float64(hi - lo)
		if straddleFrac > 0 && abs(raw-mid) < straddleFrac*gap {
			return hi, fmt.Sprintf("%d–%d", lo, hi), true // pick the higher when in doubt
		}
		if raw < mid {
			return lo, itos(lo), false
		}
		return hi, itos(hi), false
	}
	return last, itos(last), false
}

// nearestLabel snaps without straddle logic — used to report the quadrant prior.
func nearestLabel(raw float64, scale []int) string {
	p, _, _ := snap(raw, scale, 0)
	return itos(p)
}

func hotFilesPreview(files []string) string {
	if len(files) == 0 {
		return "none"
	}
	bases := make([]string, 0, 3)
	for i, f := range files {
		if i >= 3 {
			bases = append(bases, "…")
			break
		}
		bases = append(bases, shortPath(f))
	}
	return strings.Join(bases, ", ")
}

func shortPath(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 && i < len(p)-1 {
		return p[i+1:]
	}
	return p
}

func emptyTo(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func itos(n int) string { return fmt.Sprintf("%d", n) }

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func round1(f float64) float64 {
	return float64(int(f*10+0.5)) / 10
}
