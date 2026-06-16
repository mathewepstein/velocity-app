// velocity-integration-audit measures the integration / merge-up PR classifier
// against the live cache, read-only. It classifies every merged PR with the
// commit-overlap-primary model and reports:
//
//   - corpus totals and the share of PR count + LOC flagged as integration
//     (sanity vs the scope-doc's 26%/36% finding);
//   - each signal's discriminating power (mean value for flagged vs non-flagged
//     PRs) — so we can see whether commit-overlap actually separates here or
//     whether squash-merge suppresses it and the corroborators carry the load;
//   - the score histogram, per-repo flagged counts, and flagged-by-head-branch;
//   - a down-weight impact preview per affected dev (current vs down-weighted
//     prs_merged + LOC at the configured factor);
//   - the top-N flagged PRs for hand precision-labeling.
//
// Read-only: no scores DB / velocity.db / config writes, no scoring change.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mathewepstein/velocity/internal/analyze"
	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
	"github.com/mathewepstein/velocity/internal/integration"
)

func main() {
	top := flag.Int("top", 30, "number of flagged PRs to list for hand-labeling")
	threshold := flag.Float64("threshold", -1, "override classifier threshold (default: config/built-in)")
	band := flag.String("band", "", "sample every PR with score in [lo,hi) for hand-labeling, e.g. 0.50,0.60 (with full signal breakdown)")
	bandMax := flag.Int("band-max", 60, "max PRs to print for --band (0 = all)")
	flag.Parse()
	lo, hi, err := parseBand(*band)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if err := run(*top, *threshold, lo, hi, *bandMax); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// parseBand parses a "lo,hi" score-band spec. Empty string disables the
// sampler (returns lo=hi=-1). Both bounds must be in [0,1] with lo < hi.
func parseBand(s string) (lo, hi float64, err error) {
	if strings.TrimSpace(s) == "" {
		return -1, -1, nil
	}
	parts := strings.SplitN(s, ",", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("--band must be lo,hi (e.g. 0.50,0.60)")
	}
	lo, err = strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	if err != nil {
		return 0, 0, fmt.Errorf("--band lo: %w", err)
	}
	hi, err = strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err != nil {
		return 0, 0, fmt.Errorf("--band hi: %w", err)
	}
	if lo < 0 || hi > 1 || lo >= hi {
		return 0, 0, fmt.Errorf("--band bounds must satisfy 0 <= lo < hi <= 1")
	}
	return lo, hi, nil
}

func run(top int, threshold, bandLo, bandHi float64, bandMax int) error {
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

	pcfg := integration.DefaultConfig()
	if threshold >= 0 {
		pcfg.Threshold = threshold
	}
	clf := integration.NewClassifier(data.PRs, pcfg)

	// Classify every merged PR once.
	type scored struct {
		pr  cache.GitHubPR
		res integration.Result
	}
	var all []scored
	for _, pr := range data.PRs {
		if pr.Merged == nil {
			continue
		}
		all = append(all, scored{pr, clf.Classify(pr)})
	}

	var (
		mergedTotal   int
		mergedLOC     int
		flaggedTotal  int
		flaggedLOC    int
		histogram     = map[int]int{} // floor(score*10) → count
		flaggedByRepo = map[string]int{}
		flaggedByHead = map[string]int{}
	)
	// Signal means, split by flagged/non-flagged.
	var sigFlag, sigOther sigAccum
	for _, s := range all {
		loc := s.pr.Additions + s.pr.Deletions
		mergedTotal++
		mergedLOC += loc
		bucket := int(s.res.Score * 10)
		if bucket > 9 {
			bucket = 9
		}
		histogram[bucket]++
		if s.res.IsIntegration {
			flaggedTotal++
			flaggedLOC += loc
			flaggedByRepo[s.pr.Repo]++
			flaggedByHead[headLabel(s.pr.Branch)]++
			sigFlag.add(s.res.Signals)
		} else {
			sigOther.add(s.res.Signals)
		}
	}

	// Down-weight impact preview per dev: how much prs_merged / LOC each
	// configured dev loses when integration PRs are scaled by the factor. Matches
	// on GitHub login (the authorship side that the inflated metrics use).
	type devImpact struct {
		name        string
		merged      int
		integMerged int
		loc         int
		integLOC    int
		// mergeShare/locShare are the factor-INDEPENDENT promotion shares; the
		// loss at any factor f is (1-f) * share, so the table derives every
		// candidate factor's impact from these two numbers.
		mergeShare float64
		locShare   float64
	}
	byLogin := map[string]*devImpact{}
	loginToDev := map[string]string{}
	for _, d := range profile.Devs {
		for _, l := range d.AllGitHubLogins() {
			loginToDev[l] = d.DisplayName
		}
	}
	for _, s := range all {
		name, ok := loginToDev[s.pr.Author]
		if !ok {
			continue
		}
		di := byLogin[name]
		if di == nil {
			di = &devImpact{name: name}
			byLogin[name] = di
		}
		loc := s.pr.Additions + s.pr.Deletions
		di.merged++
		di.loc += loc
		if s.res.IsIntegration {
			di.integMerged++
			di.integLOC += loc
		}
	}
	impacts := make([]*devImpact, 0, len(byLogin))
	for _, di := range byLogin {
		if di.integMerged == 0 {
			continue
		}
		di.mergeShare = float64(di.integMerged) / float64(max1(di.merged))
		di.locShare = float64(di.integLOC) / float64(max1(di.loc))
		impacts = append(impacts, di)
	}
	// Sort by LOC share descending — LOC is the bigger inflation (the scope
	// doc's 36%-of-LOC finding), so it's the more decision-relevant axis.
	sort.SliceStable(impacts, func(i, j int) bool {
		if impacts[i].locShare != impacts[j].locShare {
			return impacts[i].locShare > impacts[j].locShare
		}
		return impacts[i].name < impacts[j].name
	})

	// --- Report ---
	w := os.Stdout
	fmt.Fprintf(w, "# Integration-PR classifier audit (read-only)\n")
	fmt.Fprintf(w, "# corpus: %d merged PRs · threshold %.2f · downweight factor %.2f\n",
		mergedTotal, pcfg.Threshold, pcfg.DownweightFactor)
	fmt.Fprintf(w, "# weights: reship %.1f · authordiv %.1f · merge %.1f · noreview %.1f · keyshape %.1f · bigdiff %.1f · basehead %.1f · keyless %.1f · title %.1f\n#\n",
		pcfg.WeightReship, pcfg.WeightAuthorDiversity, pcfg.WeightMergeCommits, pcfg.WeightNoReview,
		pcfg.WeightKeyShape, pcfg.WeightBigDiffNoReview, pcfg.WeightBaseHeadLong, pcfg.WeightKeylessIntoLong, pcfg.WeightTitleHint)

	fmt.Fprintf(w, "## Flagged share\n")
	fmt.Fprintf(w, "  PRs flagged : %d / %d (%.1f%%)\n", flaggedTotal, mergedTotal, pct(flaggedTotal, mergedTotal))
	fmt.Fprintf(w, "  LOC flagged : %d / %d (%.1f%%)\n", flaggedLOC, mergedLOC, pct(flaggedLOC, mergedLOC))

	fmt.Fprintf(w, "## Signal means (flagged vs non-flagged)\n")
	fmt.Fprintf(w, "  %-20s %8s %8s\n", "signal", "integ", "other")
	printSig(w, "reship", sigFlag.reship, flaggedTotal, sigOther.reship, mergedTotal-flaggedTotal)
	printSig(w, "author_diversity", sigFlag.authorDiv, flaggedTotal, sigOther.authorDiv, mergedTotal-flaggedTotal)
	printSig(w, "merge_commit_frac", sigFlag.merge, flaggedTotal, sigOther.merge, mergedTotal-flaggedTotal)
	printSig(w, "no_review", sigFlag.noReview, flaggedTotal, sigOther.noReview, mergedTotal-flaggedTotal)
	printSig(w, "key_shape", sigFlag.keyShape, flaggedTotal, sigOther.keyShape, mergedTotal-flaggedTotal)
	printSig(w, "big_diff_no_review", sigFlag.bigDiff, flaggedTotal, sigOther.bigDiff, mergedTotal-flaggedTotal)
	printSig(w, "base_head_longlived", sigFlag.baseHead, flaggedTotal, sigOther.baseHead, mergedTotal-flaggedTotal)
	printSig(w, "keyless_into_long", sigFlag.keyless, flaggedTotal, sigOther.keyless, mergedTotal-flaggedTotal)
	printSig(w, "title_hint", sigFlag.title, flaggedTotal, sigOther.title, mergedTotal-flaggedTotal)

	fmt.Fprintf(w, "## Score histogram (0.0–1.0 by 0.1)\n")
	for b := 0; b <= 9; b++ {
		fmt.Fprintf(w, "  %.1f–%.1f : %5d  %s\n", float64(b)/10, float64(b+1)/10, histogram[b], bar(histogram[b], mergedTotal))
	}

	fmt.Fprintf(w, "## Flagged by repo (top 20)\n")
	for i, rc := range sortByCount(flaggedByRepo) {
		if i >= 20 {
			break
		}
		fmt.Fprintf(w, "  %5d  %s\n", rc.n, rc.k)
	}

	fmt.Fprintf(w, "## Flagged by head branch (top 20)\n")
	for i, rc := range sortByCount(flaggedByHead) {
		if i >= 20 {
			break
		}
		fmt.Fprintf(w, "  %5d  %s\n", rc.n, rc.k)
	}

	// Loss at a factor f = (1-f) * share. Show the share (factor-independent
	// truth) plus the LOC-loss at three candidate factors so the factor choice
	// is a direct read. exclude=0.0, the recommended 0.25, and a softer 0.50.
	factors := []float64{0.0, 0.25, 0.50}
	fmt.Fprintf(w, "## Down-weight impact per dev (sorted by LOC share; devs with any integration PR)\n")
	fmt.Fprintf(w, "# 'share' = factor-independent promotion fraction; loss at factor f = (1-f)*share\n")
	fmt.Fprintf(w, "  %-22s %9s %8s %9s | %8s %8s %8s | %8s %8s %8s\n",
		"dev", "integ/all", "iLOC", "locShare",
		"mrg@0.0", "mrg@.25", "mrg@.50", "loc@0.0", "loc@.25", "loc@.50")
	for _, di := range impacts {
		row := fmt.Sprintf("  %-22s %4d/%-4d %8d %8.1f%% |", trunc(di.name, 22), di.integMerged, di.merged, di.integLOC, 100*di.locShare)
		for _, f := range factors {
			row += fmt.Sprintf(" %7.1f%%", 100*(1-f)*di.mergeShare)
		}
		row += " |"
		for _, f := range factors {
			row += fmt.Sprintf(" %7.1f%%", 100*(1-f)*di.locShare)
		}
		fmt.Fprintln(w, row)
	}

	// Top flagged PRs by score for hand-labeling precision.
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].res.Score != all[j].res.Score {
			return all[i].res.Score > all[j].res.Score
		}
		return all[i].pr.Repo < all[j].pr.Repo
	})
	fmt.Fprintf(w, "## Top %d flagged PRs (for precision labeling)\n", top)
	fmt.Fprintf(w, "score\trepo#num\thead→base\tkeys\tloc\ttitle\n")
	shown := 0
	for _, s := range all {
		if !s.res.IsIntegration {
			continue
		}
		if shown >= top {
			break
		}
		shown++
		fmt.Fprintf(w, "%.2f\t%s\t%s→%s\t%d\t%d\t%s\n",
			s.res.Score, integration.PRKey(s.pr), headLabel(s.pr.Branch), baseLabel(s.pr.BaseBranch),
			len(s.pr.IssueKeys), s.pr.Additions+s.pr.Deletions, trunc(s.pr.Title, 60))
	}

	// Band sampler: every PR with score in [bandLo, bandHi), full signal
	// breakdown, for hand-labeling precision at the threshold boundary. The
	// columns are the raw signals so a labeler can see WHY each PR scored where
	// it did. Ordered by score ascending so the riskiest (lowest, nearest the
	// flag cut) are first.
	if bandLo >= 0 {
		var inBand []scored
		for _, s := range all {
			if s.res.Score >= bandLo && s.res.Score < bandHi {
				inBand = append(inBand, s)
			}
		}
		sort.SliceStable(inBand, func(i, j int) bool {
			if inBand[i].res.Score != inBand[j].res.Score {
				return inBand[i].res.Score < inBand[j].res.Score
			}
			return inBand[i].pr.Repo < inBand[j].pr.Repo
		})
		fmt.Fprintf(w, "## Band sample [%.2f, %.2f) — %d PRs (signal breakdown for hand-labeling)\n", bandLo, bandHi, len(inBand))
		fmt.Fprintf(w, "score\tflag\treship\tadiv\tmerge\tnorev\tkey\tbigd\tbh\tkil\trepo#num\thead→base\tkeys\tloc\ttitle\n")
		for i, s := range inBand {
			if bandMax > 0 && i >= bandMax {
				fmt.Fprintf(w, "... (%d more in band; raise --band-max to see all)\n", len(inBand)-bandMax)
				break
			}
			sg := s.res.Signals
			flag := "-"
			if s.res.IsIntegration {
				flag = "Y"
			}
			fmt.Fprintf(w, "%.2f\t%s\t%.2f\t%.2f\t%.2f\t%.0f\t%+.0f\t%.0f\t%.0f\t%.0f\t%s\t%s→%s\t%d\t%d\t%s\n",
				s.res.Score, flag, sg.ReshipFraction, sg.AuthorDiversity, sg.MergeCommitFraction,
				sg.NoReview, sg.KeyShape, sg.BigDiffNoReview, sg.BaseHeadLongLived, sg.KeylessIntoLongLived,
				integration.PRKey(s.pr), headLabel(s.pr.Branch), baseLabel(s.pr.BaseBranch),
				len(s.pr.IssueKeys), s.pr.Additions+s.pr.Deletions, trunc(s.pr.Title, 50))
		}
	}
	return nil
}

type sigAccum struct {
	reship, authorDiv, merge, noReview, keyShape, bigDiff, baseHead, keyless, title float64
}

func (a *sigAccum) add(s integration.Signals) {
	a.reship += s.ReshipFraction
	a.authorDiv += s.AuthorDiversity
	a.merge += s.MergeCommitFraction
	a.noReview += s.NoReview
	a.keyShape += s.KeyShape
	a.bigDiff += s.BigDiffNoReview
	a.baseHead += s.BaseHeadLongLived
	a.keyless += s.KeylessIntoLongLived
	a.title += s.TitleHint
}

func printSig(w *os.File, name string, sum float64, n int, otherSum float64, otherN int) {
	fmt.Fprintf(w, "  %-20s %8.3f %8.3f\n", name, mean(sum, n), mean(otherSum, otherN))
}

func mean(sum float64, n int) float64 {
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

func headLabel(b string) string {
	if b == "" {
		return "(none)"
	}
	return b
}

func baseLabel(b string) string {
	if b == "" {
		return "(none)"
	}
	return b
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
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

func pct(part, total int) float64 {
	if total == 0 {
		return 0
	}
	return 100 * float64(part) / float64(total)
}

func bar(n, total int) string {
	if total == 0 {
		return ""
	}
	width := n * 40 / total
	b := make([]byte, width)
	for i := range b {
		b[i] = '#'
	}
	return string(b)
}
