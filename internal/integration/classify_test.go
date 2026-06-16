package integration

import (
	"testing"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
)

func tp(s string) *time.Time { t, _ := time.Parse("2006-01-02", s); return &t }

// mkPR is a terse merged-PR builder for fixtures.
func mkPR(repo string, num int, merged string, head, base string, author string, keys []string, inline int, commits []cache.PRCommit) cache.GitHubPR {
	add, del := 0, 0
	return cache.GitHubPR{
		Number:         num,
		Repo:           repo,
		Author:         author,
		Branch:         head,
		BaseBranch:     base,
		Merged:         tp(merged),
		IssueKeys:      keys,
		InlineComments: inline,
		Additions:      add,
		Deletions:      del,
		Commits:        commits,
	}
}

func c(sha, author string, parents int) cache.PRCommit {
	return cache.PRCommit{SHA: sha, Author: author, ParentCount: parents}
}

// A clean feature PR (single key, reviewed, single author, feature head) should
// score low and not be flagged; a development→master merge-up that re-ships the
// feature's commits should score high and flag.
func TestClassify_FeatureVsIntegration(t *testing.T) {
	cfg := DefaultConfig()

	feature := mkPR("org/app", 100, "2026-01-01", "feature/login", "development", "alice",
		[]string{"CD-1"}, 3,
		[]cache.PRCommit{c("aaa", "alice", 1), c("bbb", "alice", 1)})

	// Integration merges later, re-shipping aaa/bbb plus a merge commit, no review,
	// no key, both ends long-lived, commits by a different author too.
	integ := mkPR("org/app", 101, "2026-01-02", "development", "master", "bob",
		nil, 0,
		[]cache.PRCommit{c("aaa", "alice", 1), c("bbb", "alice", 1), c("mmm", "bob", 2)})

	clf := NewClassifier([]cache.GitHubPR{feature, integ}, cfg)

	fr := clf.Classify(feature)
	if fr.IsIntegration {
		t.Errorf("feature PR flagged as integration (score %.3f, signals %+v)", fr.Score, fr.Signals)
	}
	pr := clf.Classify(integ)
	if !pr.IsIntegration {
		t.Errorf("integration PR not flagged (score %.3f, signals %+v)", pr.Score, pr.Signals)
	}
	// The earlier feature commits must register as re-shipped in the later integ.
	if pr.Signals.ReshipFraction == 0 {
		t.Errorf("expected non-zero reship for integration, got 0 (signals %+v)", pr.Signals)
	}
	// Author-diversity: 2 of 3 known-author commits are not by bob.
	if got := pr.Signals.AuthorDiversity; got < 0.66 || got > 0.67 {
		t.Errorf("author diversity = %.3f, want ~0.667", got)
	}
}

// Re-ship must respect merge order: the FIRST PR to ship a SHA is never charged
// for it, only strictly-later PRs.
func TestReship_OrderAndFirstSeen(t *testing.T) {
	cfg := DefaultConfig()
	first := mkPR("org/app", 1, "2026-01-01", "feature/a", "development", "alice", []string{"CD-1"}, 1,
		[]cache.PRCommit{c("x1", "alice", 1), c("x2", "alice", 1)})
	second := mkPR("org/app", 2, "2026-02-01", "development", "master", "alice", nil, 0,
		[]cache.PRCommit{c("x1", "alice", 1), c("x2", "alice", 1)})
	clf := NewClassifier([]cache.GitHubPR{second, first}, cfg) // unsorted input

	if got := clf.Classify(first).Signals.ReshipFraction; got != 0 {
		t.Errorf("first PR reship = %.3f, want 0 (it shipped the SHAs first)", got)
	}
	if got := clf.Classify(second).Signals.ReshipFraction; got != 1 {
		t.Errorf("second PR reship = %.3f, want 1 (both SHAs already shipped)", got)
	}
}

// SHAs are scoped per repo: an identical SHA in a different repo is not a
// re-ship (defensive — git SHAs don't realistically collide cross-repo, but the
// semantic is intra-repo integration).
func TestReship_RepoScoped(t *testing.T) {
	cfg := DefaultConfig()
	a := mkPR("org/app", 1, "2026-01-01", "feature/a", "development", "alice", []string{"CD-1"}, 1,
		[]cache.PRCommit{c("shared", "alice", 1)})
	b := mkPR("org/other", 1, "2026-02-01", "feature/b", "development", "alice", []string{"CD-2"}, 1,
		[]cache.PRCommit{c("shared", "alice", 1)})
	clf := NewClassifier([]cache.GitHubPR{a, b}, cfg)
	if got := clf.Classify(b).Signals.ReshipFraction; got != 0 {
		t.Errorf("cross-repo reship = %.3f, want 0", got)
	}
}

// Squash-merge case: the feature PR lists its pre-squash branch SHAs; the
// integration lists the squash-result SHAs (never seen before) → reship 0. This
// is the documented limitation of the commit-overlap signal under squash-merge.
// The corroborators (author-diversity, no-review, no-key, both-ends-long-lived)
// must still ELEVATE the score well above a clean feature PR — but whether it
// crosses the flag threshold is deliberately a corpus-calibration question
// (Phase A audit), not asserted here against synthetic weights. The point of
// this test is to lock the two structural facts: reship dies under squash, and
// the corroborators carry real signal so the model is not reship-only.
func TestClassify_SquashMergeReshipDiesCorroboratorsCarry(t *testing.T) {
	cfg := DefaultConfig()
	feature := mkPR("org/app", 10, "2026-01-01", "feature/x", "development", "alice", []string{"CD-9"}, 2,
		[]cache.PRCommit{c("branch1", "alice", 1), c("branch2", "alice", 1)})
	// Squash result on development authored by several devs, promoted with no
	// review / no key / both ends long-lived.
	integ := mkPR("org/app", 11, "2026-02-01", "development", "master", "alice", nil, 0,
		[]cache.PRCommit{c("squashA", "alice", 1), c("squashB", "carol", 1), c("squashC", "dave", 1)})
	clf := NewClassifier([]cache.GitHubPR{feature, integ}, cfg)

	pr := clf.Classify(integ)
	if pr.Signals.ReshipFraction != 0 {
		t.Fatalf("expected reship 0 under squash, got %.3f", pr.Signals.ReshipFraction)
	}
	// Corroborators alone must lift it well clear of a clean feature PR.
	fr := clf.Classify(feature)
	if pr.Score <= fr.Score {
		t.Errorf("squash integration score %.3f not above feature score %.3f", pr.Score, fr.Score)
	}
	if pr.Score < 0.4 {
		t.Errorf("squash integration score %.3f too low — corroborators should elevate it near threshold", pr.Score)
	}
}

// Temp-branch back-merge: a real "merge master to dev" integration whose head is
// a throwaway merge branch (not literally a long-lived branch). The both-ends
// signal misses it (head not long-lived), but keyless-into-long-lived + the
// commit signals must still flag it. Regression test for the Phase-A recall leak.
func TestClassify_TempBranchBackMerge(t *testing.T) {
	cfg := DefaultConfig()
	// Earlier feature establishes commits on master.
	feature := mkPR("org/app", 1, "2026-01-01", "feature/x", "master", "alice", []string{"CD-1"}, 2,
		[]cache.PRCommit{c("m1", "alice", 1), c("m2", "alice", 1)})
	// Back-merge master→development via a temp branch, no key, no review.
	backMerge := mkPR("org/app", 2, "2026-02-01", "mergeMasterToDevCO", "development", "bob", nil, 0,
		[]cache.PRCommit{c("m1", "alice", 1), c("m2", "alice", 1), c("mc", "bob", 2)})
	clf := NewClassifier([]cache.GitHubPR{feature, backMerge}, cfg)

	r := clf.Classify(backMerge)
	if r.Signals.BaseHeadLongLived != 0 {
		t.Errorf("temp-branch head should NOT be long-lived, got bh=%.0f", r.Signals.BaseHeadLongLived)
	}
	if r.Signals.KeylessIntoLongLived != 1 {
		t.Errorf("keyless-into-long-lived should fire (base=development, 0 keys), got %.0f", r.Signals.KeylessIntoLongLived)
	}
	if !r.IsIntegration {
		t.Errorf("temp-branch back-merge not flagged (score %.3f, signals %+v)", r.Score, r.Signals)
	}
}

// Zero-reship first promotion: the first staging→master in a repo (nothing
// shipped earlier to overlap), so reship is silent. Both ends are long-lived and
// it carries no key — keyless-into-long-lived + topology must carry it.
func TestClassify_ZeroReshipFirstPromotion(t *testing.T) {
	cfg := DefaultConfig()
	first := mkPR("org/app", 1, "2026-01-01", "staging", "master", "alice", nil, 0,
		[]cache.PRCommit{c("a", "alice", 1), c("b", "bob", 1)})
	first.Additions = 6000 // big diff, no review
	clf := NewClassifier([]cache.GitHubPR{first}, cfg)
	r := clf.Classify(first)
	if r.Signals.ReshipFraction != 0 {
		t.Fatalf("expected reship 0 for first promotion, got %.3f", r.Signals.ReshipFraction)
	}
	if !r.IsIntegration {
		t.Errorf("zero-reship first promotion not flagged (score %.3f, signals %+v)", r.Score, r.Signals)
	}
}

// Guard: a KEYLESS feature PR merged into a long-lived branch (key never
// extracted into IssueKeys) introduces new commits (reship 0) from a
// non-long-lived head, so the keyless signal must NOT fire — that's the
// structural guard that protects precision without any branch-name assumption.
func TestClassify_KeylessFeatureIntoLongLivedDoesNotFire(t *testing.T) {
	cfg := DefaultConfig()
	feat := mkPR("org/app", 1, "2026-01-01", "feature-no-extracted-key", "development", "alice", nil, 0,
		[]cache.PRCommit{c("z1", "alice", 1), c("z2", "alice", 1)})
	feat.Additions = 600
	clf := NewClassifier([]cache.GitHubPR{feat}, cfg)
	r := clf.Classify(feat)
	if r.Signals.ReshipFraction != 0 {
		t.Fatalf("fixture should have reship 0, got %.3f", r.Signals.ReshipFraction)
	}
	if r.Signals.KeylessIntoLongLived != 0 {
		t.Errorf("keyless signal must NOT fire for a new-commit feature PR (reship 0, head not long-lived), got %.0f", r.Signals.KeylessIntoLongLived)
	}
	if r.IsIntegration {
		t.Errorf("keyless feature PR wrongly flagged (score %.3f, signals %+v)", r.Score, r.Signals)
	}
}

// A non-merged PR is never an integration PR.
func TestClassify_NonMerged(t *testing.T) {
	cfg := DefaultConfig()
	open := cache.GitHubPR{Number: 5, Repo: "org/app", Author: "alice", Branch: "development", BaseBranch: "master"}
	clf := NewClassifier(nil, cfg)
	if clf.Classify(open).IsIntegration {
		t.Error("open PR classified as integration")
	}
}

func TestIsLongLived(t *testing.T) {
	clf := NewClassifier(nil, DefaultConfig())
	cases := map[string]bool{
		"master": true, "main": true, "development": true, "staging": true,
		"release/1.2.3": true, "hotfix/x": true,
		"feature/login": false, "": false, "my-master-branch": false,
	}
	for branch, want := range cases {
		if got := clf.isLongLived(branch); got != want {
			t.Errorf("isLongLived(%q) = %v, want %v", branch, got, want)
		}
	}
}

func TestKeyShape(t *testing.T) {
	clf := NewClassifier(nil, DefaultConfig())
	merged := tp("2026-01-01")
	mk := func(keys []string) float64 {
		return clf.signals(cache.GitHubPR{Merged: merged, IssueKeys: keys}).KeyShape
	}
	if mk(nil) != 1 {
		t.Error("0 keys should score +1")
	}
	if mk([]string{"CD-1"}) != -1 {
		t.Error("exactly 1 key should score -1")
	}
	if mk([]string{"CD-1", "CD-2"}) != 0 {
		t.Error("2 keys (below cutoff) should score 0")
	}
	if mk([]string{"A", "B", "C", "D", "E"}) != 1 {
		t.Error("many keys (>= cutoff) should score +1")
	}
}
