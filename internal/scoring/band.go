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
	Points        int    `json:"points"`                   // the snapped single-value pick
	Band          string `json:"band"`                     // "3", or "2–3" when straddling
	Confidence    string `json:"confidence"`               // low | medium | high
	NeedsInsight  bool   `json:"needs_insight"`            // route to a human/LLM pass
	InsightReason string `json:"insight_reason,omitempty"` // why it was flagged (empty when confident)

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
	// PR-less investigation tickets are routed to the spike scorer (cycle ×
	// artifact-density), bypassing the LOC quadrant and the "no PR → flag low"
	// gate that would otherwise mis-score them.
	if IsSpike(ev) && len(ev.PRs) == 0 {
		return bandSpike(ev, cfg)
	}

	scale := EffectiveScale(cfg)

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

	// Size sanity-floor (Phase 4b): a sub-floor diff can't claim the full rework
	// bonus — a 1-line change that bounced many times is flaky-fix churn, not big
	// work, so its rework credit is scaled down. Risk, review-round, and
	// deep-thread credit are left at full weight: a tiny diff in a hot file is
	// genuinely risky, and a small diff with real approach debate is genuinely
	// hard (this is the locked high-risk-small-fix anchor → 5).
	reworkScale := 1.0
	// Bug-aware small-diff floor: the size floor treats a sub-floor diff's rework
	// as flaky churn and downscales it. For bug/regression tickets that's wrong —
	// a 2-line fix that bounced is real diagnosis effort, not big work — so the
	// downscale is suppressed (scale stays 1.0) for bug types.
	if !isBugType(ev) && cfg.SmallDiffLOCFloor > 0 && ev.NetLOC > 0 && ev.NetLOC < cfg.SmallDiffLOCFloor && cfg.SmallDiffBonusScale > 0 {
		reworkScale = cfg.SmallDiffBonusScale
	}

	reworkWeight := cfg.ReworkWeight
	if isBugType(ev) && cfg.Bug.ReworkWeight > 0 {
		reworkWeight = cfg.Bug.ReworkWeight
	}
	if ev.ReworkCount > 0 {
		// Saturate the count (Phase 4a): the 1st/2nd bounce carry full signal, a
		// runaway 7th barely moves the band — capped per-signal so normal sums
		// still land on clean Fibonacci steps.
		n := saturate(ev.ReworkCount, cfg.ReworkCountCap)
		add(float64(n)*reworkWeight*reworkScale, "Rework: %d backward bounce(s) from review/QA into dev", ev.ReworkCount)
	}
	if ev.ReviewRounds > 0 {
		n := saturate(ev.ReviewRounds, cfg.ReviewRoundCap)
		add(float64(n)*cfg.ReviewRoundWeight, "Review back-and-forth: %d changes-requested round(s)", ev.ReviewRounds)
	}
	if ev.DeepThreads > 0 {
		add(float64(ev.DeepThreads)*cfg.DeepThreadWeight, "Approach contention: %d deep review thread(s)", ev.DeepThreads)
	}
	highRiskBonus := cfg.HighRiskBonus
	if isBugType(ev) && cfg.Bug.HighRiskBonus > 0 {
		highRiskBonus = cfg.Bug.HighRiskBonus
	}
	switch ev.TouchedAreaRisk {
	case "high":
		if ev.RiskReason != "" {
			add(highRiskBonus, "High-risk area: domain match %q", ev.RiskReason)
		} else {
			add(highRiskBonus, "High-risk area: %d hot file(s) touched (%s)", len(ev.HotFiles), hotFilesPreview(ev.HotFiles))
		}
	case "medium":
		if ev.RiskReason != "" {
			add(cfg.MediumRiskBonus, "Moderate-risk area: domain match %q", ev.RiskReason)
		} else {
			add(cfg.MediumRiskBonus, "Moderate-risk area: %d hot file(s) touched", len(ev.HotFiles))
		}
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
	res.Confidence, res.NeedsInsight, res.InsightReason = judge(ev, cfg, points, straddle, thinking, raw, len(drivers) == 0)

	// Phase 2 split-flag: raw effort far above the top band's floor means the work
	// likely spanned more than one ticket. A true 13 is the band you're *least*
	// sure is a single unit, so route it to a scope/split check rather than
	// asserting it — keep the score, cap confidence at "medium", flag for insight.
	if cfg.SplitThreshold > 0 && points == scale[len(scale)-1] && raw >= cfg.SplitThreshold {
		res.NeedsInsight = true
		res.InsightReason = "Effort exceeds a single-ticket scale — likely should have been split; confirm scope"
		if res.Confidence == "high" {
			res.Confidence = "medium"
		}
	}
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
//
// Confidence then distinguishes "high" from "medium": a high band is only
// "high" when the thinking signal explains at least HighBandThinkingShare of
// the raw effort. A band that cleared the inflation floor but is still mostly
// quadrant base (calendar/LOC) is "medium" — shown, but not asserted with
// confidence. This restores a real low/medium/high spread at the top, where the
// engine previously had only binary low-or-high.
func judge(ev *TicketEvidence, cfg config.StoryPointsConfig, points int, straddle bool, thinking, raw float64, noDrivers bool) (confidence string, needsInsight bool, reason string) {
	switch {
	case len(ev.PRs) == 0:
		return "low", true, "No merged PR — typing effort can't be assessed from the diff"
	case straddle:
		return "low", true, "Raw effort sits between two Fibonacci steps — magnitude is ambiguous"
	case points >= 5 && thinking < cfg.MinThinkingForHighBand:
		// The band is high but nothing corroborates the difficulty.
		return "low", true, "High band driven by cycle time with no corroborating rework/review — likely queue latency, not complexity"
	case noDrivers && points >= 3:
		// Sized into a 3+ purely on volume with zero thinking signal.
		return "low", true, "Sized on volume alone — no complexity signal corroborates the band"
	}

	// Below the inflation floor on a low band → no strong claim either way.
	if thinking < cfg.MinThinkingForHighBand || straddle {
		return "medium", false, ""
	}
	// High band that's mostly quadrant base, not corroborated thinking → medium.
	if points >= 5 && cfg.HighBandThinkingShare > 0 && raw > 0 && thinking < cfg.HighBandThinkingShare*raw {
		return "medium", false, ""
	}
	return "high", false, ""
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

// EffectiveScale is the Fibonacci ladder the engine snaps to: the configured
// Scale, or the default 1,2,3,5,8,13 when unset. Single source of truth for the
// ladder — band/spike snapping, the override-value guard, and the scoring UI's
// allowed steps all read it.
func EffectiveScale(cfg config.StoryPointsConfig) []int {
	if len(cfg.Scale) == 0 {
		return []int{1, 2, 3, 5, 8, 13}
	}
	return cfg.Scale
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

// saturate caps count at cap (Phase 4a), so a runaway process-signal count
// can't max the band. cap ≤ 0 disables (legacy linear count).
func saturate(count, cap int) int {
	if cap > 0 && count > cap {
		return cap
	}
	return count
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
