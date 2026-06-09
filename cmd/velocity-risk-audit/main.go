// velocity-risk-audit measures what the config-driven domain-risk dimension
// does to the story-points band distribution, read-only. It bands every
// standard-path ticket twice — once with an EMPTY RiskConfig (the churn-only
// baseline, i.e. domain risk off) and once with the live `[storypoints.risk]`
// list (domain risk on) — and reports the band distribution, flag rate,
// touched-area-risk tier distribution, how many tickets the domain dimension
// elevated, and the tickets whose band moved (with the glob that drove it).
//
// Domain risk only ever elevates (TouchedAreaRisk = max(churn, domain)), so
// every mover goes up. Read-only: no scores DB / velocity.db / config writes.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/mathewepstein/velocity/internal/analyze"
	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
	"github.com/mathewepstein/velocity/internal/scoring"
)

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

	norm := profile.Scoring.Normalize
	dwell := profile.StoryPoints.ReworkMinDwell()
	offExt := scoring.NewExtractor(data, norm, dwell, config.RiskConfig{})        // domain risk off
	onExt := scoring.NewExtractor(data, norm, dwell, profile.StoryPoints.Risk)    // domain risk on (live list)

	offDist := map[int]int{}
	onDist := map[int]int{}
	offTier := map[string]int{}
	onTier := map[string]int{}
	var total, offFlag, onFlag, elevated int

	type mover struct {
		key              string
		from, to         int
		tierFrom, tierTo string
		reason           string
	}
	var movers []mover

	for _, key := range offExt.Keys() {
		oev, ok := offExt.Extract(key)
		if !ok {
			continue
		}
		if scoring.IsSpike(oev) || len(oev.PRs) == 0 {
			continue // standard path only
		}
		nev, _ := onExt.Extract(key)
		total++

		ob := scoring.Band(oev, profile.StoryPoints)
		nb := scoring.Band(nev, profile.StoryPoints)
		offDist[ob.Points]++
		onDist[nb.Points]++
		offTier[oev.TouchedAreaRisk]++
		onTier[nev.TouchedAreaRisk]++
		if ob.NeedsInsight {
			offFlag++
		}
		if nb.NeedsInsight {
			onFlag++
		}
		if oev.TouchedAreaRisk != nev.TouchedAreaRisk {
			elevated++
		}
		if nb.Points != ob.Points {
			movers = append(movers, mover{key, ob.Points, nb.Points, oev.TouchedAreaRisk, nev.TouchedAreaRisk, nev.RiskReason})
		}
	}

	w := os.Stdout
	fmt.Fprintf(w, "# Risk variant sweep — domain risk OFF (churn-only) → ON (live [storypoints.risk])\n")
	fmt.Fprintf(w, "# corpus: %d cached issues · %d scored on the standard path\n", len(data.Issues), total)
	fmt.Fprintf(w, "# live high globs: %d · medium globs: %d\n#\n", len(profile.StoryPoints.Risk.High), len(profile.StoryPoints.Risk.Medium))

	fmt.Fprintf(w, "## Band distribution (off → on)\n")
	for _, pts := range unionKeys(offDist, onDist) {
		fmt.Fprintf(w, "  %2d pts : %4d (%.1f%%)  →  %4d (%.1f%%)\n", pts, offDist[pts], pct(offDist[pts], total), onDist[pts], pct(onDist[pts], total))
	}
	fmt.Fprintf(w, "## Touched-area risk tier (off → on)\n")
	for _, t := range []string{"low", "medium", "high"} {
		fmt.Fprintf(w, "  %-6s : %4d (%.1f%%)  →  %4d (%.1f%%)\n", t, offTier[t], pct(offTier[t], total), onTier[t], pct(onTier[t], total))
	}
	fmt.Fprintf(w, "## Flag rate\n")
	fmt.Fprintf(w, "  needs-insight: %d (%.1f%%)  →  %d (%.1f%%)\n", offFlag, pct(offFlag, total), onFlag, pct(onFlag, total))
	fmt.Fprintf(w, "## Domain-risk impact\n")
	fmt.Fprintf(w, "  tickets whose risk tier was elevated by domain: %d (%.1f%%)\n", elevated, pct(elevated, total))
	fmt.Fprintf(w, "  tickets that changed band                     : %d (%.1f%%)\n", len(movers), pct(len(movers), total))

	// Which globs drove the most movements.
	reasonCount := map[string]int{}
	for _, m := range movers {
		r := m.reason
		if r == "" {
			r = "(churn/other)"
		}
		reasonCount[r]++
	}
	fmt.Fprintf(w, "## Movers by driving glob\n")
	for _, rc := range sortByCount(reasonCount) {
		fmt.Fprintf(w, "  %4d  %s\n", rc.n, rc.k)
	}

	sort.SliceStable(movers, func(i, j int) bool {
		di, dj := movers[i].to-movers[i].from, movers[j].to-movers[j].from
		if di != dj {
			return di > dj
		}
		return movers[i].key < movers[j].key
	})
	fmt.Fprintf(w, "## Movers (top %d by band gain)\n", top)
	fmt.Fprintf(w, "key\tband\ttier\tdriving_glob\n")
	for i, m := range movers {
		if i >= top {
			break
		}
		fmt.Fprintf(w, "%s\t%d→%d\t%s→%s\t%s\n", m.key, m.from, m.to, m.tierFrom, m.tierTo, m.reason)
	}
	return nil
}

type kv struct {
	k string
	n int
}

func sortByCount(m map[string]int) []kv {
	out := make([]kv, 0, len(m))
	for k, n := range m {
		out = append(out, kv{k, n})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].n != out[j].n {
			return out[i].n > out[j].n
		}
		return out[i].k < out[j].k
	})
	return out
}

func unionKeys(a, b map[int]int) []int {
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
