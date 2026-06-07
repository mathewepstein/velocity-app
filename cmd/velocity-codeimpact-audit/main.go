// velocity-codeimpact-audit compares the current-window code_impact metric and
// composite ranking for the mapped cohort under two CodeImpactConfigs: the
// knobs-off baseline and a candidate with churn-weighting and/or bulk-import
// dampening enabled (dashboard-overhaul P5.1). It reuses the live analyze
// pipeline via analyze.CohortCodeImpact, so the baseline column matches the
// dashboard's metrics.json, but writes nothing — ratings.json and metrics.json
// are untouched, so it's safe to run repeatedly while sweeping tunables.
//
// Flags toggle each knob and override every tunable, so calibration is a
// matter of re-running with different values and reading the re-ranking. Output
// is a TSV ranked by code_impact change (most-dampened first) plus a summary;
// redirect stdout to archive a run.
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/mathewepstein/velocity/internal/analyze"
	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

func main() {
	churn := flag.Bool("churn", true, "Enable churn-weighting in the candidate config.")
	bulk := flag.Bool("bulk", true, "Enable bulk-import dampening in the candidate config.")
	churnFloor := flag.Float64("churn-floor", 0, "Override churn_floor (0 = use config default).")
	churnFullAt := flag.Int("churn-full-at", 0, "Override churn_full_at (0 = use config default).")
	bulkMinLOC := flag.Int("bulk-min-loc", 0, "Override bulk_import_min_loc (0 = use config default).")
	bulkAddRatio := flag.Float64("bulk-add-ratio", 0, "Override bulk_import_add_ratio (0 = use config default).")
	bulkMinFiles := flag.Int("bulk-min-files", 0, "Override bulk_import_min_files (0 = use config default).")
	bulkWeight := flag.Float64("bulk-weight", 0, "Override bulk_import_weight (0 = use config default).")
	onlyChanged := flag.Bool("only-changed", false, "Only print devs whose code_impact changed.")
	flag.Parse()

	if err := run(*churn, *bulk, *churnFloor, *churnFullAt, *bulkMinLOC, *bulkAddRatio, *bulkMinFiles, *bulkWeight, *onlyChanged); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(churn, bulk bool, churnFloor float64, churnFullAt, bulkMinLOC int, bulkAddRatio float64, bulkMinFiles int, bulkWeight float64, onlyChanged bool) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	profile := cfg.ActiveProfile()

	// Baseline: both knobs forced off, regardless of what the live config says,
	// so the column is an unambiguous "pre-patch" reference.
	baseProfile := profile
	baseProfile.Scoring.CodeImpact.ChurnWeighting = false
	baseProfile.Scoring.CodeImpact.BulkImportDampening = false

	// Candidate: knobs per flags, tunables overridden where a flag was given.
	candProfile := profile
	cand := candProfile.Scoring.CodeImpact // starts from config (defaults already filled by Load)
	cand.ChurnWeighting = churn
	cand.BulkImportDampening = bulk
	if churnFloor > 0 {
		cand.ChurnFloor = churnFloor
	}
	if churnFullAt > 0 {
		cand.ChurnFullAt = churnFullAt
	}
	if bulkMinLOC > 0 {
		cand.BulkImportMinLOC = bulkMinLOC
	}
	if bulkAddRatio > 0 {
		cand.BulkImportAddRatio = bulkAddRatio
	}
	if bulkMinFiles > 0 {
		cand.BulkImportMinFiles = bulkMinFiles
	}
	if bulkWeight > 0 {
		cand.BulkImportWeight = bulkWeight
	}
	candProfile.Scoring.CodeImpact = cand

	// Open the configured cache (sqlite by default) explicitly so the audit
	// reads the live substrate rather than falling through to the JSON corpus.
	store, err := cache.OpenStore()
	if err != nil {
		return fmt.Errorf("open cache: %w", err)
	}
	defer store.Close()

	baseRows, err := analyze.CohortCodeImpact(analyze.Options{Profile: baseProfile, Store: store})
	if err != nil {
		return fmt.Errorf("baseline: %w", err)
	}
	candRows, err := analyze.CohortCodeImpact(analyze.Options{Profile: candProfile, Store: store})
	if err != nil {
		return fmt.Errorf("candidate: %w", err)
	}

	candByDev := make(map[string]analyze.CodeImpactRow, len(candRows))
	for _, r := range candRows {
		candByDev[r.Dev] = r
	}

	type joined struct {
		base, cand analyze.CodeImpactRow
	}
	rows := make([]joined, 0, len(baseRows))
	for _, b := range baseRows {
		if c, ok := candByDev[b.Dev]; ok {
			rows = append(rows, joined{base: b, cand: c})
		}
	}
	// Most-dampened first (largest code_impact drop at the top).
	sort.Slice(rows, func(i, j int) bool {
		di := rows[i].cand.CodeImpact - rows[i].base.CodeImpact
		dj := rows[j].cand.CodeImpact - rows[j].base.CodeImpact
		return di < dj
	})

	fmt.Printf("# code_impact audit — candidate: churn=%v bulk=%v", churn, bulk)
	fmt.Printf(" | churn_floor=%.2f churn_full_at=%d", cand.ChurnFloor, cand.ChurnFullAt)
	fmt.Printf(" | bulk_min_loc=%d add_ratio=%.2f min_files=%d weight=%.2f\n",
		cand.BulkImportMinLOC, cand.BulkImportAddRatio, cand.BulkImportMinFiles, cand.BulkImportWeight)
	fmt.Printf("# %d mapped devs; baseline = both knobs OFF (matches live metrics.json)\n", len(rows))

	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "rnk→\tdev\trawLOC\teffLOC\teffLOC%\tci_off\tci_on\tci_Δ%\tcomp_off\tcomp_on\tΔrank")
	var dropped, ranksShifted int
	var maxShift int
	for _, j := range rows {
		ciDelta := pctDelta(j.base.CodeImpact, j.cand.CodeImpact)
		locPct := 100.0
		if j.base.RawLOC > 0 {
			locPct = j.cand.EffectiveLOC / j.base.RawLOC * 100
		}
		dRank := 0
		if j.base.Scored && j.cand.Scored {
			dRank = j.cand.Rank - j.base.Rank // negative = moved up the board
		}
		if math.Abs(j.cand.CodeImpact-j.base.CodeImpact) > 1e-9 {
			dropped++
		}
		if dRank != 0 {
			ranksShifted++
			if abs(dRank) > maxShift {
				maxShift = abs(dRank)
			}
		}
		if onlyChanged && math.Abs(j.cand.CodeImpact-j.base.CodeImpact) <= 1e-9 {
			continue
		}
		fmt.Fprintf(tw, "%d→%d\t%s\t%.0f\t%.0f\t%.0f%%\t%.1f\t%.1f\t%+.0f%%\t%+.2f\t%+.2f\t%+d\n",
			j.base.Rank, j.cand.Rank, trunc(j.base.Dev, 22),
			j.base.RawLOC, j.cand.EffectiveLOC, locPct,
			j.base.CodeImpact, j.cand.CodeImpact, ciDelta,
			j.base.Composite, j.cand.Composite, dRank)
	}
	tw.Flush()

	fmt.Printf("\n# summary: %d/%d devs' code_impact changed; %d composite ranks shifted (max %d positions)\n",
		dropped, len(rows), ranksShifted, maxShift)
	return nil
}

func pctDelta(from, to float64) float64 {
	if from == 0 {
		return 0
	}
	return (to - from) / from * 100
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
