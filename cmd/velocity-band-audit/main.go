// velocity-band-audit measures how a candidate change to the generated/noise
// exclude lists moves the story-points band distribution, read-only. It bands
// every standard-path (non-spike) ticket twice — once under the live config,
// once under the live config plus a candidate set of `generated_file_patterns`
// — and reports the band distribution, flag rate, and the tickets that moved,
// with their net-LOC before/after.
//
// The candidate exclude set encodes the FE/asset exclude rules (fonts, video,
// lottie, logs, mock JSON, compiled-style sourcemaps, agent/git-hook tooling
// dirs). Because a file matching `generated_file_patterns` is dropped from net
// LOC and dampened in code_impact, the band only moves for tickets whose PRs
// carried real LOC in a now-excluded file (mostly mock-JSON / compiled output;
// binaries like fonts/video are ~0 LOC and move code_impact, not the band).
//
// Read-only: no scores DB, no velocity.db, no config writes.
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

// candidateExcludes are the patterns under evaluation — appended to the live
// `generated_file_patterns`. Matched by analyze.matchesGeneratedPattern
// (basename glob, full-path glob, and `*/seg/*` segment-anywhere), so `*.woff`
// matches by basename and `*/lotties/*` matches the segment anywhere.
var candidateExcludes = []string{
	"*.map",                                       // JS/CSS sourcemaps (compiled-style output; authored .css/.scss kept)
	"*.woff", "*.woff2", "*.ttf", "*.otf", "*.eot", // fonts — handed off as-is
	"*.mp4", "*.webm", "*.mov", "*.m4v", // video — handed off as-is
	"*.lottie", "*/lotties/*", // lottie animations — handed off as-is
	"*.log",                    // logs
	"*/__mocks__/*", "*.mock.json", // mock/fixture JSON — not real LOC
	"*/.gemini/*", "*/.claude/*", "*/.husky/*", // agent / git-hook tooling — outright ignore
}

type tallies struct {
	dist     map[int]int
	flagged  int
	total    int
}

func newTallies() tallies { return tallies{dist: map[int]int{}} }

func main() {
	top := flag.Int("top", 25, "number of movers to list")
	flag.Parse()
	if err := run(*top); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
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

	baseNorm := profile.Scoring.Normalize
	candNorm := baseNorm
	// Copy + append so we don't alias the baseline's slice.
	candNorm.GeneratedFilePatterns = append(append([]string(nil), baseNorm.GeneratedFilePatterns...), candidateExcludes...)

	baseExt := scoring.NewExtractor(data, baseNorm, profile.StoryPoints.ReworkMinDwell(), profile.StoryPoints.Risk)
	candExt := scoring.NewExtractor(data, candNorm, profile.StoryPoints.ReworkMinDwell(), profile.StoryPoints.Risk)

	base := newTallies()
	cand := newTallies()

	type mover struct {
		key                string
		fromBand, toBand   int
		fromLOC, toLOC     int
	}
	var movers []mover
	var touched int // tickets whose net LOC changed at all (exclude blast radius)

	for _, key := range baseExt.Keys() {
		bev, ok := baseExt.Extract(key)
		if !ok {
			continue
		}
		// Standard path only — spikes are scored on a PR-less axis the exclude
		// list doesn't touch, and are covered by velocity-spike-audit.
		if scoring.IsSpike(bev) || len(bev.PRs) == 0 {
			continue
		}
		cev, _ := candExt.Extract(key)

		bBand := scoring.Band(bev, profile.StoryPoints)
		cBand := scoring.Band(cev, profile.StoryPoints)

		tally(&base, bBand)
		tally(&cand, cBand)

		if bev.NetLOC != cev.NetLOC {
			touched++
		}
		if bBand.Points != cBand.Points {
			movers = append(movers, mover{key, bBand.Points, cBand.Points, bev.NetLOC, cev.NetLOC})
		}
	}

	w := os.Stdout
	fmt.Fprintf(w, "# Band audit — candidate generated/noise exclude additions\n")
	fmt.Fprintf(w, "# corpus: %d cached issues · %d scored on the standard path\n", len(data.Issues), base.total)
	fmt.Fprintf(w, "# candidate patterns appended to generated_file_patterns:\n#   %s\n#\n", strings.Join(candidateExcludes, " "))

	fmt.Fprintf(w, "## Band distribution (baseline → candidate)\n")
	for _, pts := range unionBands(base.dist, cand.dist) {
		fmt.Fprintf(w, "  %2d pts : %4d (%.1f%%)  →  %4d (%.1f%%)\n",
			pts, base.dist[pts], pct(base.dist[pts], base.total), cand.dist[pts], pct(cand.dist[pts], cand.total))
	}
	fmt.Fprintf(w, "## Flag rate\n")
	fmt.Fprintf(w, "  needs-insight: %d (%.1f%%)  →  %d (%.1f%%)\n", base.flagged, pct(base.flagged, base.total), cand.flagged, pct(cand.flagged, cand.total))

	fmt.Fprintf(w, "## Exclude blast radius\n")
	fmt.Fprintf(w, "  tickets whose net LOC dropped under the candidate: %d (%.1f%%)\n", touched, pct(touched, base.total))
	fmt.Fprintf(w, "  tickets that changed band                        : %d (%.1f%%)\n", len(movers), pct(len(movers), base.total))

	sort.SliceStable(movers, func(i, j int) bool {
		di := movers[i].fromBand - movers[i].toBand
		dj := movers[j].fromBand - movers[j].toBand
		if di != dj {
			return di > dj
		}
		return movers[i].key < movers[j].key
	})
	fmt.Fprintf(w, "## Movers (top %d by band drop)\n", top)
	fmt.Fprintf(w, "key\tband\tnet_loc\n")
	for i, m := range movers {
		if i >= top {
			break
		}
		fmt.Fprintf(w, "%s\t%d→%d\t%d→%d\n", m.key, m.fromBand, m.toBand, m.fromLOC, m.toLOC)
	}
	return nil
}

func tally(t *tallies, b scoring.BandResult) {
	t.total++
	t.dist[b.Points]++
	if b.NeedsInsight {
		t.flagged++
	}
}

func unionBands(a, b map[int]int) []int {
	seen := map[int]struct{}{}
	for k := range a {
		seen[k] = struct{}{}
	}
	for k := range b {
		seen[k] = struct{}{}
	}
	out := make([]int, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

func pct(part, total int) float64 {
	if total == 0 {
		return 0
	}
	return 100 * float64(part) / float64(total)
}
