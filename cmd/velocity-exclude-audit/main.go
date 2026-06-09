// velocity-exclude-audit measures how the candidate generated/noise exclude
// additions (fonts, video, lottie, logs, mock JSON, sourcemaps, agent/git-hook
// tooling dirs) move the CONTRIBUTOR model — current-window code_impact,
// composite, and rank — for the mapped cohort. These files match
// generated_file_patterns, so they're dampened to GeneratedFileWeight in the
// code_impact file-count input; this tool shows the resulting per-dev shift,
// the Elo input. Read-only: recomputes the cohort twice via
// analyze.CohortCodeImpact (no ratings.json / metrics.json / velocity.db write).
//
// It compares the live profile WITH the candidate patterns removed (baseline)
// vs WITH them present (candidate), so it reports the delta regardless of
// whether the patterns have already been merged into the default list.
//
// Note: generated_file_patterns dampens code_impact FILE COUNT, not its LOC
// input — so "mock JSON shouldn't count as LOC" in the contributor model is the
// separately-deferred per-file-LOC exclusion, not measured here.
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

// candidateExcludes mirrors the set added to the default generated_file_patterns.
var candidateExcludes = map[string]bool{
	"*.map": true,
	"*.woff": true, "*.woff2": true, "*.ttf": true, "*.otf": true, "*.eot": true,
	"*.mp4": true, "*.webm": true, "*.mov": true, "*.m4v": true,
	"*.lottie": true, "*/lotties/*": true,
	"*.log":          true,
	"*/__mocks__/*":  true, "*.mock.json": true,
	"*/.gemini/*": true, "*/.claude/*": true, "*/.husky/*": true,
}

func main() {
	onlyChanged := flag.Bool("only-changed", true, "only print devs whose code_impact changed")
	flag.Parse()
	if err := run(*onlyChanged); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(onlyChanged bool) error {
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

	live := profile.Scoring.Normalize.GeneratedFilePatterns
	var basePatterns, candPatterns []string
	for _, p := range live {
		if !candidateExcludes[p] {
			basePatterns = append(basePatterns, p)
		}
	}
	candPatterns = append([]string(nil), basePatterns...)
	for p := range candidateExcludes {
		candPatterns = append(candPatterns, p)
	}

	baseProfile := profile
	baseProfile.Scoring.Normalize.GeneratedFilePatterns = basePatterns
	candProfile := profile
	candProfile.Scoring.Normalize.GeneratedFilePatterns = candPatterns

	baseRows, err := analyze.CohortCodeImpact(analyze.Options{Profile: baseProfile, Store: store})
	if err != nil {
		return fmt.Errorf("baseline cohort: %w", err)
	}
	candRows, err := analyze.CohortCodeImpact(analyze.Options{Profile: candProfile, Store: store})
	if err != nil {
		return fmt.Errorf("candidate cohort: %w", err)
	}

	type row struct {
		dev                              string
		baseCI, candCI                   float64
		baseComp, candComp               float64
		baseRank, candRank               int
	}
	byDev := map[string]*row{}
	for _, r := range baseRows {
		byDev[r.Dev] = &row{dev: r.Dev, baseCI: r.CodeImpact, baseComp: r.Composite, baseRank: r.Rank}
	}
	for _, r := range candRows {
		if x, ok := byDev[r.Dev]; ok {
			x.candCI, x.candComp, x.candRank = r.CodeImpact, r.Composite, r.Rank
		}
	}

	rows := make([]*row, 0, len(byDev))
	var changedCI, rankShifts, maxRankShift int
	for _, x := range byDev {
		rows = append(rows, x)
		if math.Abs(x.candCI-x.baseCI) > 1e-9 {
			changedCI++
		}
		if x.baseRank != 0 && x.candRank != 0 && x.baseRank != x.candRank {
			rankShifts++
			if d := abs(x.baseRank - x.candRank); d > maxRankShift {
				maxRankShift = d
			}
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return math.Abs(rows[i].candCI-rows[i].baseCI) > math.Abs(rows[j].candCI-rows[j].baseCI)
	})

	fmt.Printf("# Exclude audit — contributor-model effect of candidate generated_file_patterns\n")
	fmt.Printf("# cohort: %d mapped devs (current window) · code_impact file-count dampening only (LOC unaffected)\n", len(rows))
	fmt.Printf("# devs with changed code_impact: %d · rank shifts: %d · max rank shift: %d\n#\n", changedCI, rankShifts, maxRankShift)

	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "dev\tcode_impact\tcomposite\trank")
	for _, x := range rows {
		if onlyChanged && math.Abs(x.candCI-x.baseCI) < 1e-9 {
			continue
		}
		fmt.Fprintf(w, "%s\t%.3f→%.3f (%+.3f)\t%.3f→%.3f\t%d→%d\n",
			x.dev, x.baseCI, x.candCI, x.candCI-x.baseCI, x.baseComp, x.candComp, x.baseRank, x.candRank)
	}
	w.Flush()
	return nil
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
