package detail

import (
	"testing"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
)

// The candidate gates are the resume contract: resolved Jira issues hydrate
// once, open issues every run; GitHub phases only touch merged PRs whose
// sentinel slice is still nil.

func TestJiraDetailGate(t *testing.T) {
	ph := JiraDetailPhase(nil, nil)
	now := time.Now()

	cases := []struct {
		name string
		iss  cache.JiraIssue
		want bool
	}{
		{"unfetched", cache.JiraIssue{}, true},
		{"resolved fetched once", cache.JiraIssue{DetailFetched: true, Resolved: &now}, false},
		{"open re-hydrates every run", cache.JiraIssue{DetailFetched: true}, true},
	}
	for _, c := range cases {
		if got := ph.NeedsWork(&c.iss); got != c.want {
			t.Errorf("%s: NeedsWork = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestGitHubPhaseGates(t *testing.T) {
	now := time.Now()
	merged := cache.GitHubPR{Merged: &now}
	unmerged := cache.GitHubPR{}

	pc := PRCommentsPhase(nil, nil)
	if !pc.NeedsWork(&merged) {
		t.Error("pr-comments: merged PR with nil ReviewComments should need work")
	}
	if pc.NeedsWork(&unmerged) {
		t.Error("pr-comments: unmerged PR should not need work")
	}
	fetched := merged
	fetched.ReviewComments = []cache.ReviewComment{}
	if pc.NeedsWork(&fetched) {
		t.Error("pr-comments: empty non-nil sentinel should not need work")
	}

	fc := FileChangesPhase(nil, nil)
	if !fc.NeedsWork(&merged) {
		t.Error("file-changes: merged PR with nil FileChanges should need work")
	}
	done := merged
	done.FileChanges = []cache.FileChange{}
	if fc.NeedsWork(&done) {
		t.Error("file-changes: empty non-nil sentinel should not need work")
	}
}
