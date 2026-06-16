// Package integration detects integration / merge-up pull requests — PRs that move
// already-merged commits between long-lived branches (development → staging →
// master, or GitFlow release/* → main) rather than introducing new authored
// work. An integration PR double-counts LOC already credited to the original
// feature PRs and inflates prs_created/prs_merged/code_impact, with no ticket
// and no review.
//
// The classifier is repo- and org-agnostic by construction: the primary signals
// are structural (commit re-ship overlap and author-diversity of the diff),
// which are independent of branch names, titles, and GitFlow-vs-trunk
// conventions. Title-pattern matching exists only as a low-weight optional
// supplement. See implementation/velocity/integration-pr-detection-{scope,impl-plan}.md.
package integration

// Config parameterises the integration classifier. All weights are relative; the
// score is their weighted sum normalized by the total active weight, so the
// threshold stays in [0,1] regardless of how the weights are tuned. Zero-value
// is NOT valid — callers use DefaultConfig() and override fields.
type Config struct {
	// --- Signal weights (relative; primary signals carry the most). ---
	WeightReship          float64 // S4: fraction of commits already merged in an earlier PR
	WeightAuthorDiversity float64 // S6: fraction of commits not by the PR author
	WeightMergeCommits    float64 // fraction of commits that are merge commits (parent_count >= 2)
	WeightNoReview        float64 // no inline review comments and no deep threads
	WeightKeyShape        float64 // 0 keys or "many" keys (aggregation); exactly 1 key counts against
	WeightBigDiffNoReview float64 // large LOC AND unreviewed
	WeightBaseHeadLong    float64 // base and head are both long-lived integration branches
	WeightKeylessIntoLong float64 // base is long-lived AND the PR carries no issue key (head-agnostic)
	WeightTitleHint       float64 // optional org-specific title match (low weight)

	// Threshold is the score (after weight normalization) at or above which a PR
	// is classified as an integration PR. Conservative by default to protect precision.
	Threshold float64

	// BigDiffLOC is the additions+deletions floor for the big-diff-no-review
	// signal. ManyKeysCutoff is the distinct-issue-key count at or above which
	// key-shape reads as a release bundle (positive signal).
	BigDiffLOC     int
	ManyKeysCutoff int

	// DownweightFactor scales an integration PR's contribution to the inflated
	// metrics (prs_*/loc/code_impact). Used by the scoring wire-in (Phase B) and
	// by the audit's impact preview; the classifier itself does not apply it.
	// Range (0,1): 0.25 keeps a quarter of the credit (release ownership is real
	// coordination, just not authorship).
	DownweightFactor float64

	// TitlePatterns are optional, case-insensitive substrings that nudge the
	// score when matched against the PR title. Org-specific and never primary —
	// empty by default. A team that titles its GitFlow release merges predictably
	// can add e.g. "merge to", "promote", "release" here for extra recall.
	TitlePatterns []string

	// LongLivedBranches are branch names treated as long-lived integration
	// branches for the base/head topology signal. Matched case-insensitively;
	// an entry ending in "/" is a prefix match (e.g. "release/" matches
	// "release/1.2.3"). Org-tunable; the defaults cover the common conventions.
	LongLivedBranches []string
}

// The keyless-into-long-lived signal (WeightKeylessIntoLong) fires when a PR
// merges INTO a long-lived branch, carries no issue key, AND shows structural
// integration evidence — it re-ships already-merged commits (reship > 0) or
// both ends are long-lived. It is the head-agnostic complement to
// BaseHeadLongLived and exists to catch two integration patterns that the
// both-ends signal misses, found in Phase-A band validation:
//   - temp-branch back-merges (e.g. head "mergeMasterToDevCO" → development):
//     a real merge-up whose head is a throwaway merge branch, not literally a
//     long-lived branch — caught via reship > 0; and
//   - zero-reship first promotions (e.g. the first staging→master in a repo):
//     no earlier PR to overlap with, so commit-overlap is silent — caught via
//     both-ends-long-lived.
// The no-issue-key condition alone is too weak: a feature PR whose key was never
// extracted into IssueKeys would trip it. Gating on reship/topology is what
// keeps it off feature work WITHOUT assuming any branch-naming convention (a
// keyless feature PR introduces new commits — reship 0 — from a non-long-lived
// head, so it does not fire). It is a corroborator, not decisive.

// DefaultConfig returns the locked Phase-A defaults. Weights put the bulk on the
// two convention-free signals (re-ship overlap + author-diversity); structural
// corroborators are mid-weight; title is a small nudge. Threshold 0.5 is
// deliberately conservative — tuned against the labeled sample in Phase A
// validation before any scoring wire-in.
func DefaultConfig() Config {
	return Config{
		WeightReship:          3.0,
		WeightAuthorDiversity: 2.0,
		WeightMergeCommits:    1.0,
		WeightNoReview:        1.0,
		WeightKeyShape:        1.0,
		WeightBigDiffNoReview: 1.0,
		WeightBaseHeadLong:    1.5,
		WeightKeylessIntoLong: 2.0,
		WeightTitleHint:       0.5,
		Threshold:             0.5,
		BigDiffLOC:            1000,
		ManyKeysCutoff:        5,
		DownweightFactor:      0.25,
		TitlePatterns:         nil,
		LongLivedBranches: []string{
			"master",
			"main",
			"development",
			"develop",
			"dev",
			"staging",
			"stage",
			"release/",
			"hotfix/",
			"production",
			"prod",
			"qa",
			"uat",
		},
	}
}

// TotalWeight is the sum of the signal weights, used to normalize the score into
// [0,1]. Negative-direction signals (the "exactly 1 key" penalty) are folded
// into the key-shape term rather than the denominator, so the denominator is the
// sum of the positive-direction weights only.
func (c Config) TotalWeight() float64 {
	return c.WeightReship + c.WeightAuthorDiversity + c.WeightMergeCommits +
		c.WeightNoReview + c.WeightKeyShape + c.WeightBigDiffNoReview +
		c.WeightBaseHeadLong + c.WeightKeylessIntoLong + c.WeightTitleHint
}
