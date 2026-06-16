package analyze

import (
	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
	"github.com/mathewepstein/velocity/internal/integration"
)

// prIntegrationWeight returns the scored-contribution weight for one PR: the
// configured down-weight factor for a detected integration / merge-up PR, else
// 1.0. A nil prIntegrationWeight means the feature is disabled — every call
// site treats nil as "weight 1.0 for all PRs", so scoring is byte-identical to
// today. This is the single shared predicate used at BOTH the composite path
// (buildOneDev) and the Elo path (periodTotals) so the two cannot drift.
type prIntegrationWeight func(cache.GitHubPR) float64

// weightFor is a nil-safe accessor: a nil weighter weights every PR at 1.0.
func (w prIntegrationWeight) weightFor(pr cache.GitHubPR) float64 {
	if w == nil {
		return 1.0
	}
	return w(pr)
}

// integrationClassifierConfig converts the plain config block into the
// classifier's own Config, starting from the Phase-A-locked defaults and
// overlaying any tunables the operator set. Lives here (not in internal/config)
// so internal/config never imports internal/integration — no import cycle.
func integrationClassifierConfig(c config.IntegrationConfig) integration.Config {
	cfg := integration.DefaultConfig()
	if c.Factor > 0 {
		cfg.DownweightFactor = c.Factor
	}
	if c.Threshold > 0 {
		cfg.Threshold = c.Threshold
	}
	if c.BigDiffLOC > 0 {
		cfg.BigDiffLOC = c.BigDiffLOC
	}
	if c.ManyKeysCutoff > 0 {
		cfg.ManyKeysCutoff = c.ManyKeysCutoff
	}
	if len(c.LongLivedBranches) > 0 {
		cfg.LongLivedBranches = c.LongLivedBranches
	}
	if len(c.TitlePatterns) > 0 {
		cfg.TitlePatterns = c.TitlePatterns
	}
	for name, w := range c.Weights {
		switch name {
		case "reship":
			cfg.WeightReship = w
		case "author_diversity":
			cfg.WeightAuthorDiversity = w
		case "merge_commits":
			cfg.WeightMergeCommits = w
		case "no_review":
			cfg.WeightNoReview = w
		case "key_shape":
			cfg.WeightKeyShape = w
		case "big_diff_no_review":
			cfg.WeightBigDiffNoReview = w
		case "base_head_long":
			cfg.WeightBaseHeadLong = w
		case "keyless_into_long":
			cfg.WeightKeylessIntoLong = w
		case "title_hint":
			cfg.WeightTitleHint = w
		}
	}
	return cfg
}

// scopedIntegrationScoring walks one dev's window PRs and returns the
// integration-down-weighted prs_created / prs_merged / loc_changed scoring
// inputs, plus the count of flagged merged PRs (display-only IntegrationPRs).
// The windowing predicates mirror rollupMonthly exactly, so with an all-1.0
// weighter the floats equal the raw Totals — but this is only ever called when
// the weighter is non-nil (integration scoring enabled). A PR is flagged iff its
// weight is below 1.0 (the configured factor is always < 1 when enabled).
func scopedIntegrationScoring(scoped *Loaded, start, end cache.Month, w prIntegrationWeight) (scoredMetrics, int) {
	var s scoredMetrics
	var flagged int
	for _, p := range scoped.PRs {
		wt := w.weightFor(p)
		if monthInRange(monthKey(p.Created), start, end) {
			s.prsCreated += wt
		}
		if p.Merged != nil && monthInRange(monthKey(*p.Merged), start, end) {
			s.prsMerged += wt
			s.locChanged += wt * float64(p.Additions+p.Deletions)
			if wt < 1.0 {
				flagged++
			}
		}
	}
	return s, flagged
}

// newIntegrationWeighter builds the shared weighter from the full PR corpus.
// Returns nil when the feature is disabled, so callers stay on the exact
// pre-feature scoring path. The classifier precomputes its commit-overlap index
// once over all PRs, so this is built a single time in Run and threaded down.
func newIntegrationWeighter(data *Loaded, c config.IntegrationConfig) prIntegrationWeight {
	if !c.Enabled || data == nil {
		return nil
	}
	cfg := integrationClassifierConfig(c)
	factor := cfg.DownweightFactor
	clf := integration.NewClassifier(data.PRs, cfg)
	return func(pr cache.GitHubPR) float64 {
		if clf.Classify(pr).IsIntegration {
			return factor
		}
		return 1.0
	}
}
