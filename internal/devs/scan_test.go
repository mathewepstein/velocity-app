package devs

import (
	"reflect"
	"testing"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

func TestScanCollectsUniqueIdentitiesAndFiltersBots(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)

	month := cache.MustParseMonth("2026-01")

	if err := cache.WriteMonth(cache.SourceGitHubPRs, "consumerdirect", month, []cache.GitHubPR{
		{Number: 1, Repo: "consumerdirect/x", Author: "alice"},
		{Number: 2, Repo: "consumerdirect/x", Author: "dependabot[bot]"}, // bot, drop
		{Number: 3, Repo: "consumerdirect/x", Author: "alice"},           // dedupe
		{Number: 4, Repo: "consumerdirect/x", Author: "bob"},
	}); err != nil {
		t.Fatalf("write prs: %v", err)
	}
	if err := cache.WriteMonth(cache.SourceGitHubCommits, "consumerdirect", month, []cache.GitHubCommit{
		{SHA: "aa", Repo: "consumerdirect/x", Author: "carol", Committed: time.Now()},
		{SHA: "bb", Repo: "consumerdirect/x", Author: "renovate", Committed: time.Now()}, // bot, drop
	}); err != nil {
		t.Fatalf("write commits: %v", err)
	}
	if err := cache.WriteMonth(cache.SourceJira, "CD", month, []cache.JiraIssue{
		{Key: "CD-1", Assignee: "acct-1", Reporter: "acct-2"},
		{Key: "CD-2", Assignee: "acct-1", Reporter: "acct-3"}, // dedupe acct-1
	}); err != nil {
		t.Fatalf("write jira: %v", err)
	}

	// Manifest must record every cell so Scan walks them.
	man := cache.NewManifest()
	now := time.Now()
	man.Update(cache.SourceGitHubPRs, "consumerdirect", month, 4, now)
	man.Update(cache.SourceGitHubCommits, "consumerdirect", month, 2, now)
	man.Update(cache.SourceJira, "CD", month, 2, now)
	if err := man.Save(); err != nil {
		t.Fatalf("save manifest: %v", err)
	}

	profile := config.DefaultProfileConfig()
	got, err := Scan(profile, cache.JSONStore{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	wantGH := []string{"alice", "bob", "carol"}
	wantJR := []string{"acct-1", "acct-2", "acct-3"}
	if !reflect.DeepEqual(got.GitHubLogins, wantGH) {
		t.Errorf("GitHubLogins = %v, want %v", got.GitHubLogins, wantGH)
	}
	if !reflect.DeepEqual(got.JiraAccountIDs, wantJR) {
		t.Errorf("JiraAccountIDs = %v, want %v", got.JiraAccountIDs, wantJR)
	}
}

func TestUnmappedSkipsConfiguredDevs(t *testing.T) {
	id := Identities{
		GitHubLogins:   []string{"alice", "bob", "carol"},
		JiraAccountIDs: []string{"acct-1", "acct-2", "acct-3"},
	}
	profile := config.Profile{
		Devs: []config.DevIdentity{
			{GitHubLogin: "alice", JiraAccountID: "acct-1"},
			{GitHubLogin: "carol", JiraAccountID: ""},
		},
	}

	got := id.Unmapped(profile)
	wantGH := []string{"bob"}
	wantJR := []string{"acct-2", "acct-3"}
	if !reflect.DeepEqual(got.GitHubLogins, wantGH) {
		t.Errorf("Unmapped.GitHubLogins = %v, want %v", got.GitHubLogins, wantGH)
	}
	if !reflect.DeepEqual(got.JiraAccountIDs, wantJR) {
		t.Errorf("Unmapped.JiraAccountIDs = %v, want %v", got.JiraAccountIDs, wantJR)
	}
}
