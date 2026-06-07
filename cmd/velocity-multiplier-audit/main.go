// velocity-multiplier-audit reports the Phase 6.2 silent dampening factor
// per dev for the current-window mapped cohort. It recomputes the cohort
// in-process from the local cache (via analyze.CohortForCurrentWindow),
// derives the spam/stuff parts and the combined multiplier, and emits a TSV
// ranked by dampening severity.
//
// Use this when calibrating SpamThreshold / SpamPenalty / StuffPenalty /
// MultiplierFloor / GeneratedFile* — surface devs whose multiplier dropped
// substantially and audit the underlying signals against their real work
// pattern (legitimate refactor vs commit-spam, framework upgrade vs
// dependency dump).
//
// Output goes to stdout; redirect to a file for archiving. The cohort is
// recomputed straight from the cache, so it reflects the latest pull without
// depending on a prior `velocity analyze` having written metrics.json.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/mathewepstein/velocity/internal/analyze"
	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

func main() {
	threshold := flag.Float64("highlight-below", 0.95, "Mark devs whose multiplier dropped below this in the output.")
	flag.Parse()

	if err := run(*threshold); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(highlight float64) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	profile := cfg.ActiveProfile()
	norm := profile.Scoring.Normalize

	// Open the configured cache (sqlite by default) explicitly so the audit
	// reads the live substrate rather than falling through to the JSON corpus.
	store, err := cache.OpenStore()
	if err != nil {
		return fmt.Errorf("open cache: %w", err)
	}
	defer store.Close()

	// Recompute the current-window cohort straight from the cache (read-only,
	// no Elo side effects); the dashboard no longer serializes `devs` into
	// metrics.json.
	devs, curWin, err := analyze.CohortForCurrentWindow(analyze.Options{Profile: profile, Store: store})
	if err != nil {
		return fmt.Errorf("compute cohort: %w", err)
	}

	teamLOCPerFile := analyze.AuditTeamLOCPerFile(devs)
	p90 := percentile(teamLOCPerFile, 90)

	type row struct {
		Name        string
		Commits     int
		Files       int
		LOC         int
		SpamRatio   float64
		StuffRatio  float64
		SpamPart    float64
		StuffPart   float64
		Multiplier  float64
		CodeImpact  float64
		Flagged     bool
	}

	rows := make([]row, 0, len(devs))
	for _, d := range devs {
		if d.Dev.DisplayName == "unknown" {
			continue
		}
		t := d.Totals
		m, sp, st := analyze.AuditEffortMultiplier(t, teamLOCPerFile, norm)
		var spamRatio, stuffRatio float64
		if t.UniqueFilesTouched > 0 {
			spamRatio = float64(t.Commits) / float64(t.UniqueFilesTouched)
			stuffRatio = float64(t.LOCAdded+t.LOCDeleted) / float64(t.UniqueFilesTouched)
		}
		rows = append(rows, row{
			Name:       d.Dev.DisplayName,
			Commits:    t.Commits,
			Files:      t.UniqueFilesTouched,
			LOC:        t.LOCAdded + t.LOCDeleted,
			SpamRatio:  spamRatio,
			StuffRatio: stuffRatio,
			SpamPart:   sp,
			StuffPart:  st,
			Multiplier: m,
			CodeImpact: t.CodeImpact,
			Flagged:    m < highlight,
		})
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].Multiplier < rows[j].Multiplier })

	fmt.Printf("# Phase 6.2 multiplier audit\n")
	fmt.Printf("# window: %s → %s · cohort size: %d · team p90 LOC/file: %.1f · highlight below: %.2f\n",
		curWin.Window.Start, curWin.Window.End, len(rows), p90, highlight)
	fmt.Printf("# normalize: spam_threshold=%.2f spam_penalty=%.2f stuff_penalty=%.2f floor=%.2f gen_weight=%.2f\n",
		norm.SpamThreshold, norm.SpamPenalty, norm.StuffPenalty, norm.MultiplierFloor, norm.GeneratedFileWeight)
	fmt.Printf("#\n")
	fmt.Printf("flag\tname\tcommits\tfiles\tloc\tspam_ratio\tstuff_ratio\tspam_part\tstuff_part\tmultiplier\tcode_impact\n")
	for _, r := range rows {
		marker := " "
		if r.Flagged {
			marker = "!"
		}
		fmt.Printf("%s\t%s\t%d\t%d\t%d\t%.2f\t%.1f\t%.3f\t%.3f\t%.3f\t%.2f\n",
			marker, r.Name, r.Commits, r.Files, r.LOC,
			r.SpamRatio, r.StuffRatio, r.SpamPart, r.StuffPart, r.Multiplier, r.CodeImpact)
	}
	return nil
}

// percentile mirrors analyze.percentile (linear interpolation, R type-7).
// Kept local so this tool doesn't expand the analyze public API beyond what
// the audit really needs.
func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := make([]float64, len(xs))
	copy(sorted, xs)
	sort.Float64s(sorted)
	if len(sorted) == 1 {
		return sorted[0]
	}
	rank := (p / 100.0) * float64(len(sorted)-1)
	lo := int(rank)
	hi := lo + 1
	if hi >= len(sorted) {
		return sorted[len(sorted)-1]
	}
	frac := rank - float64(lo)
	return sorted[lo] + frac*(sorted[hi]-sorted[lo])
}
