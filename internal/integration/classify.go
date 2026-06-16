package integration

import (
	"sort"
	"strconv"
	"strings"

	"github.com/mathewepstein/velocity/internal/cache"
)

// Signals holds the per-PR signal values that feed the integration score. Each
// field is in [0,1] except KeyShape, which is in [-1,1] (a clean single-key PR
// scores against integration).
type Signals struct {
	ReshipFraction       float64 // S4: commits already merged in a strictly-earlier PR (same repo)
	AuthorDiversity      float64 // S6: known-author commits not authored by the PR author
	MergeCommitFraction  float64 // commits with parent_count >= 2
	NoReview             float64 // 1 if no inline comments and no deep threads
	KeyShape             float64 // +1 (0 or many keys), -1 (exactly 1 key), 0 otherwise
	BigDiffNoReview      float64 // 1 if large LOC and unreviewed
	BaseHeadLongLived    float64 // 1 if base and head are both long-lived integration branches
	KeylessIntoLongLived float64 // 1 if base is long-lived and the PR carries no issue key (head-agnostic)
	TitleHint            float64 // 1 if an optional title pattern matched
}

// Result is the classifier verdict for one PR.
type Result struct {
	Score         float64
	IsIntegration bool
	Signals       Signals
}

// Classifier scores PRs against the commit-overlap-primary model. Build it once
// over the full merged-PR corpus with NewClassifier; it precomputes the
// intra-repo first-seen commit index that the re-ship signal needs.
type Classifier struct {
	cfg Config
	// reship[prKey] = fraction of the PR's distinct commit SHAs that were first
	// seen in a strictly-earlier merged PR within the same repo.
	reship map[string]float64
}

// PRKey is the stable per-PR identity (repo + number) used to look up cached
// re-ship fractions. Exported so the audit can correlate results back to PRs.
func PRKey(pr cache.GitHubPR) string {
	return pr.Repo + "#" + strconv.Itoa(pr.Number)
}

// NewClassifier builds the re-ship index from every merged PR that has commit
// data. PRs are walked in merge-time order so "already seen" means "shipped in
// an earlier PR". SHAs are scoped per repo: an integration PR moves commits
// within a repo, and scoping avoids any cross-repo coincidence.
func NewClassifier(prs []cache.GitHubPR, cfg Config) *Classifier {
	merged := make([]cache.GitHubPR, 0, len(prs))
	for _, pr := range prs {
		if pr.Merged == nil || len(pr.Commits) == 0 {
			continue
		}
		merged = append(merged, pr)
	}
	// Earliest merge first; ties broken by PR number for determinism.
	sort.SliceStable(merged, func(i, j int) bool {
		if !merged[i].Merged.Equal(*merged[j].Merged) {
			return merged[i].Merged.Before(*merged[j].Merged)
		}
		return merged[i].Number < merged[j].Number
	})

	reship := make(map[string]float64, len(merged))
	seen := map[string]struct{}{} // key: repo + "\x00" + sha
	for _, pr := range merged {
		// Distinct SHAs in this PR (a PR can list a SHA more than once in
		// pathological cases; dedupe so the fraction stays a true ratio).
		shas := make(map[string]struct{}, len(pr.Commits))
		for _, c := range pr.Commits {
			if c.SHA == "" {
				continue
			}
			shas[c.SHA] = struct{}{}
		}
		if len(shas) == 0 {
			continue
		}
		var already int
		for sha := range shas {
			if _, ok := seen[pr.Repo+"\x00"+sha]; ok {
				already++
			}
		}
		reship[PRKey(pr)] = float64(already) / float64(len(shas))
		// Record first-sighting AFTER scoring this PR, so a PR never counts its
		// own commits as already-seen.
		for sha := range shas {
			seen[pr.Repo+"\x00"+sha] = struct{}{}
		}
	}
	return &Classifier{cfg: cfg, reship: reship}
}

// Classify scores one PR. Non-merged PRs are never integration PRs (Score 0). The
// re-ship fraction comes from the precomputed index; every other signal is
// derived from the PR record directly.
func (c *Classifier) Classify(pr cache.GitHubPR) Result {
	if pr.Merged == nil {
		return Result{}
	}
	s := c.signals(pr)
	score := c.score(s)
	return Result{
		Score:         score,
		IsIntegration: score >= c.cfg.Threshold,
		Signals:       s,
	}
}

// signals computes every per-PR signal value.
func (c *Classifier) signals(pr cache.GitHubPR) Signals {
	var s Signals
	s.ReshipFraction = c.reship[PRKey(pr)]

	// Author-diversity (S6): of commits with a linked author login, the fraction
	// not authored by the PR author. Commits without a linked login are excluded
	// from the denominator rather than guessed.
	var known, other, merges int
	for _, cm := range pr.Commits {
		if cm.ParentCount >= 2 {
			merges++
		}
		if cm.Author == "" {
			continue
		}
		known++
		if cm.Author != pr.Author {
			other++
		}
	}
	if known > 0 {
		s.AuthorDiversity = float64(other) / float64(known)
	}
	if n := len(pr.Commits); n > 0 {
		s.MergeCommitFraction = float64(merges) / float64(n)
	}

	noReview := pr.InlineComments == 0 && pr.DeepThreads == 0
	if noReview {
		s.NoReview = 1
	}

	switch n := len(pr.IssueKeys); {
	case n == 0:
		s.KeyShape = 1
	case n >= c.cfg.ManyKeysCutoff:
		s.KeyShape = 1
	case n == 1:
		s.KeyShape = -1
	}

	if noReview && pr.Additions+pr.Deletions >= c.cfg.BigDiffLOC {
		s.BigDiffNoReview = 1
	}

	baseLong := c.isLongLived(pr.BaseBranch)
	bothEndsLong := baseLong && c.isLongLived(pr.Branch)
	if bothEndsLong {
		s.BaseHeadLongLived = 1
	}

	// Head-agnostic complement: a keyless merge INTO a long-lived branch that
	// ALSO shows structural integration evidence — it re-ships already-merged
	// commits (reship > 0) or both ends are long-lived. The no-key condition
	// alone is too weak (a feature PR whose key wasn't extracted would trip it);
	// gating on reship/topology keeps it off feature work without assuming any
	// branch-naming convention. Catches temp-branch back-merges (head is a
	// throwaway merge branch, so BaseHeadLongLived misses them, but they re-ship
	// commits) and zero-reship first promotions (both ends long-lived).
	if baseLong && len(pr.IssueKeys) == 0 && (s.ReshipFraction > 0 || bothEndsLong) {
		s.KeylessIntoLongLived = 1
	}

	if c.matchesTitle(pr.Title) {
		s.TitleHint = 1
	}
	return s
}

// score is the weight-normalized sum, clamped to [0,1]. The key-shape penalty
// can push the raw numerator negative for a clean single-key PR; that clamps to
// 0 (definitively not an integration PR).
func (c *Classifier) score(s Signals) float64 {
	num := c.cfg.WeightReship*s.ReshipFraction +
		c.cfg.WeightAuthorDiversity*s.AuthorDiversity +
		c.cfg.WeightMergeCommits*s.MergeCommitFraction +
		c.cfg.WeightNoReview*s.NoReview +
		c.cfg.WeightKeyShape*s.KeyShape +
		c.cfg.WeightBigDiffNoReview*s.BigDiffNoReview +
		c.cfg.WeightBaseHeadLong*s.BaseHeadLongLived +
		c.cfg.WeightKeylessIntoLong*s.KeylessIntoLongLived +
		c.cfg.WeightTitleHint*s.TitleHint
	tw := c.cfg.TotalWeight()
	if tw <= 0 {
		return 0
	}
	score := num / tw
	if score < 0 {
		return 0
	}
	if score > 1 {
		return 1
	}
	return score
}

// isLongLived reports whether branch is one of the configured integration
// branches. A configured entry ending in "/" is a case-insensitive prefix match
// (e.g. "release/" matches "release/1.2.3"); others match the full name.
func (c *Classifier) isLongLived(branch string) bool {
	if branch == "" {
		return false
	}
	b := strings.ToLower(branch)
	for _, raw := range c.cfg.LongLivedBranches {
		p := strings.ToLower(strings.TrimSpace(raw))
		if p == "" {
			continue
		}
		if strings.HasSuffix(p, "/") {
			if strings.HasPrefix(b, p) {
				return true
			}
			continue
		}
		if b == p {
			return true
		}
	}
	return false
}

// matchesTitle reports whether any configured title pattern is a
// case-insensitive substring of the PR title. Empty pattern list never matches.
func (c *Classifier) matchesTitle(title string) bool {
	if len(c.cfg.TitlePatterns) == 0 || title == "" {
		return false
	}
	t := strings.ToLower(title)
	for _, p := range c.cfg.TitlePatterns {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" && strings.Contains(t, p) {
			return true
		}
	}
	return false
}
