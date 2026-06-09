// velocity-spike-audit reports the spike (investigation-ticket) band
// distribution and isolates the contribution of the jira-field-capture Phase D
// link signals — spawned follow-up work and link breadth — to the spike score.
// It recomputes everything in-process from the local cache (read-only: no scores
// DB, no velocity.db writes), so it reflects the latest pull without depending on
// a prior `velocity score generate`.
//
// Use this when calibrating the spike weights (SpawnedWeight / BreadthWeight /
// BreadthThreshold) and thresholds (CycleDaysThreshold / ArtifactThreshold): the
// sweep shows the band histogram, how many spikes the link nudges moved up a
// band, how many dormancy flags the spawned-work signal suppressed, and the
// Phase C description-coverage lift on the spike path. Tune weights only if the
// distribution comes out under-banded.
//
// Output is markdown to stdout; redirect to a file for archiving.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mathewepstein/velocity/internal/analyze"
	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
	"github.com/mathewepstein/velocity/internal/scoring"
)

func main() {
	top := flag.Int("top", 15, "Number of highest-banded spikes to list for anchor review.")
	flag.Parse()
	if err := run(*top); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// spikeRow is one investigation ticket scored on the spike path, with the
// Phase D link-signal breakdown attributed.
type spikeRow struct {
	Key                 string
	Summary             string
	CycleDays           float64
	ArtifactLinks       int
	SubstantiveComments int
	SpawnedCount        int
	LinkBreadth         int
	HasDescription      bool
	StatusFlips         int

	Points        int // band with Phase D link weights live (the "after" world)
	PointsNoNudge int // band with spawned/breadth weights zeroed
	RawEffort     float64
	NeedsInsight  bool
	Cell          string
}

func run(top int) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	profile := cfg.ActiveProfile()

	store, err := cache.OpenStore()
	if err != nil {
		return fmt.Errorf("open cache: %w", err)
	}
	defer store.Close()

	data, err := analyze.LoadAll(profile, cache.CurrentMonth(time.Now()), store)
	if err != nil {
		return fmt.Errorf("load corpus: %w", err)
	}
	ext := scoring.NewExtractor(data, profile.Scoring.Normalize, profile.StoryPoints.ReworkMinDwell(), profile.StoryPoints.Risk)

	sc := profile.StoryPoints.Spike
	// Band variant with the Phase D link nudges disabled, to isolate how many
	// bands the spawned/breadth signals moved. (Flag suppression is measured
	// separately — it keys on SpawnedCount, not on the weights.)
	cfgNoNudge := profile.StoryPoints
	cfgNoNudge.Spike.SpawnedWeight = 0
	cfgNoNudge.Spike.BreadthWeight = 0

	var rows []spikeRow
	for _, key := range ext.Keys() {
		ev, ok := ext.Extract(key)
		if !ok {
			continue
		}
		// Route exactly as the band engine does: spike heuristic + no matched PR.
		if !scoring.IsSpike(ev) || len(ev.PRs) > 0 {
			continue
		}
		band := scoring.Band(ev, profile.StoryPoints)
		noNudge := scoring.Band(ev, cfgNoNudge)

		rows = append(rows, spikeRow{
			Key:                 ev.Key,
			Summary:             ev.Summary,
			CycleDays:           cycleDays(ev),
			ArtifactLinks:       ev.ArtifactLinks,
			SubstantiveComments: ev.SubstantiveComments,
			SpawnedCount:        ev.SpawnedCount,
			LinkBreadth:         ev.LinkBreadth,
			HasDescription:      strings.TrimSpace(ev.Description) != "",
			StatusFlips:         ev.StatusFlips,
			Points:              band.Points,
			PointsNoNudge:       noNudge.Points,
			RawEffort:           band.RawEffort,
			NeedsInsight:        band.NeedsInsight,
			Cell:                band.QuadrantCell,
		})
	}

	report(os.Stdout, rows, sc, len(data.Issues))
	printTop(os.Stdout, rows, top)
	return nil
}

// cycleDays mirrors the unexported band.go selector (active cycle → raw cycle →
// created-to-resolved), used here only to classify the dormancy-flag condition.
func cycleDays(ev *scoring.TicketEvidence) float64 {
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

func report(w *os.File, rows []spikeRow, sc config.SpikeConfig, totalIssues int) {
	n := len(rows)
	fmt.Fprintf(w, "# Spike calibration sweep (jira-field-capture Phase E)\n")
	fmt.Fprintf(w, "# corpus: %d cached issues · %d scored on the spike path\n", totalIssues, n)
	fmt.Fprintf(w, "# weights: spawned=%.2f breadth=%.2f breadth_threshold=%d · cycle_days_threshold=%.1f artifact_threshold=%d\n",
		sc.SpawnedWeight, sc.BreadthWeight, sc.BreadthThreshold, sc.CycleDaysThreshold, sc.ArtifactThreshold)
	fmt.Fprintf(w, "# bases: short_low=%.1f short_high=%.1f long_low=%.1f long_high=%.1f\n#\n",
		sc.BaseShortLow, sc.BaseShortHigh, sc.BaseLongLow, sc.BaseLongHigh)
	if n == 0 {
		fmt.Fprintln(w, "no spikes found in cache")
		return
	}

	// --- Band distribution ---
	dist := map[int]int{}
	for _, r := range rows {
		dist[r.Points]++
	}
	fmt.Fprintf(w, "## Band distribution\n")
	for _, pts := range sortedIntKeys(dist) {
		fmt.Fprintf(w, "  %2d pts : %4d (%.1f%%)\n", pts, dist[pts], pct(dist[pts], n))
	}

	// --- Phase D link-signal populations ---
	var withSpawned, withBreadth int
	for _, r := range rows {
		if r.SpawnedCount > 0 {
			withSpawned++
		}
		if r.LinkBreadth > sc.BreadthThreshold {
			withBreadth++
		}
	}
	fmt.Fprintf(w, "## Link-signal population\n")
	fmt.Fprintf(w, "  spawned follow-up work (>0)       : %4d (%.1f%%)\n", withSpawned, pct(withSpawned, n))
	fmt.Fprintf(w, "  link breadth past threshold (>%d) : %4d (%.1f%%)\n", sc.BreadthThreshold, withBreadth, pct(withBreadth, n))

	// --- Link-nudge band movement (after vs nudges-off) ---
	var moved, sumDelta int
	for _, r := range rows {
		if r.Points > r.PointsNoNudge {
			moved++
			sumDelta += r.Points - r.PointsNoNudge
		}
	}
	fmt.Fprintf(w, "## Link-nudge band movement (spawned + breadth)\n")
	fmt.Fprintf(w, "  spikes moved up a band by the nudges: %d (%.1f%%)\n", moved, pct(moved, n))
	fmt.Fprintf(w, "  total band-steps added             : %d\n", sumDelta)

	// --- Dormancy-flag suppression (the before/after flag delta) ---
	// A multi-day, zero-artifact, low-churn spike that spawned work is no longer
	// flagged as possible dormancy. These are exactly the flags Phase D removed.
	var suppressed, flaggedAfter int
	for _, r := range rows {
		if r.NeedsInsight {
			flaggedAfter++
		}
		artifacts := r.ArtifactLinks + r.SubstantiveComments
		if r.CycleDays >= sc.CycleDaysThreshold && artifacts == 0 && r.StatusFlips < 4 && r.SpawnedCount > 0 {
			suppressed++
		}
	}
	fmt.Fprintf(w, "## Needs-insight flag\n")
	fmt.Fprintf(w, "  flagged now (after Phase D)        : %4d (%.1f%%)\n", flaggedAfter, pct(flaggedAfter, n))
	fmt.Fprintf(w, "  dormancy flags suppressed by spawn : %4d (would be +%.1f%% flagged without it)\n", suppressed, pct(suppressed, n))

	// --- Description coverage (confirm Phase C lift reached the spike path) ---
	var withDesc, withDocLink int
	for _, r := range rows {
		if r.HasDescription {
			withDesc++
		}
		if r.ArtifactLinks > 0 {
			withDocLink++
		}
	}
	fmt.Fprintf(w, "## Description coverage among spikes (Phase C)\n")
	fmt.Fprintf(w, "  non-empty description : %4d (%.1f%%)\n", withDesc, pct(withDesc, n))
	fmt.Fprintf(w, "  ≥1 artifact link      : %4d (%.1f%%)\n", withDocLink, pct(withDocLink, n))
}

func printTop(w *os.File, rows []spikeRow, top int) {
	if top <= 0 || len(rows) == 0 {
		return
	}
	sorted := append([]spikeRow(nil), rows...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].RawEffort != sorted[j].RawEffort {
			return sorted[i].RawEffort > sorted[j].RawEffort
		}
		return sorted[i].Key < sorted[j].Key
	})
	fmt.Fprintf(w, "## Top %d spikes by raw effort (anchor review)\n", top)
	fmt.Fprintf(w, "flag\tkey\tpts\traw\tcycle_d\tdoc_links\tsubst_cmt\tspawned\tbreadth\tsummary\n")
	for i, r := range sorted {
		if i >= top {
			break
		}
		marker := " "
		if r.NeedsInsight {
			marker = "!"
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%.1f\t%.1f\t%d\t%d\t%d\t%d\t%s\n",
			marker, r.Key, r.Points, r.RawEffort, r.CycleDays,
			r.ArtifactLinks, r.SubstantiveComments, r.SpawnedCount, r.LinkBreadth, truncate(r.Summary, 60))
	}
}

func pct(part, total int) float64 {
	if total == 0 {
		return 0
	}
	return 100 * float64(part) / float64(total)
}

func sortedIntKeys(m map[int]int) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\t", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
