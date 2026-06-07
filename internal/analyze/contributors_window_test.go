package analyze

import (
	"reflect"
	"testing"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

// TestContributorsForWindowMatchesRun is the Step 2 parity gate: the on-demand
// /api/contributors backend must reproduce metrics.json's `devs` slice exactly
// when handed the same window Run used. If this drifts, the query layer and the
// precomputed blob disagree and the range picker would silently mis-rank.
func TestContributorsForWindowMatchesRun(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	month := cache.MustParseMonth("2026-04")
	prior := cache.MustParseMonth("2026-03")
	if err := cache.WriteMonth(cache.SourceGitHubPRs, "consumerdirect", month, []cache.GitHubPR{
		{Number: 1, Repo: "consumerdirect/x", Author: "alice", Created: mustTime("2026-04-05T00:00:00Z"),
			Merged: ptrTime(mustTime("2026-04-06T00:00:00Z")), Additions: 40, Deletions: 5},
		{Number: 2, Repo: "consumerdirect/x", Author: "bob", Created: mustTime("2026-04-10T00:00:00Z"),
			Merged: ptrTime(mustTime("2026-04-12T00:00:00Z")), Additions: 12, Deletions: 1},
	}); err != nil {
		t.Fatalf("write prs: %v", err)
	}
	if err := cache.WriteMonth(cache.SourceGitHubPRs, "consumerdirect", prior, []cache.GitHubPR{
		{Number: 3, Repo: "consumerdirect/x", Author: "alice", Created: mustTime("2026-03-05T00:00:00Z"),
			Merged: ptrTime(mustTime("2026-03-06T00:00:00Z")), Additions: 8, Deletions: 0},
	}); err != nil {
		t.Fatalf("write prs prior: %v", err)
	}
	if err := cache.WriteMonth(cache.SourceGitHubReviews, "consumerdirect", month, []cache.GitHubReview{
		{PRNumber: 2, Repo: "consumerdirect/x", Reviewer: "alice", State: "APPROVED", Submitted: mustTime("2026-04-11T00:00:00Z")},
	}); err != nil {
		t.Fatalf("write reviews: %v", err)
	}

	profile := config.Profile{
		Jira:   config.JiraConfig{Projects: []string{"CD"}, AccountID: "acct-alice"},
		GitHub: config.GitHubConfig{Username: "alice", Orgs: []string{"consumerdirect"}},
		Window: config.WindowConfig{BackfillStart: "2026-01", DefaultLengthMonths: 4},
		Devs: []config.DevIdentity{
			{GitHubLogin: "alice", JiraAccountID: "acct-alice", DisplayName: "Alice"},
			{GitHubLogin: "bob", DisplayName: "Bob"},
		},
	}
	nowT := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)

	// Run writes ratings.json + bakes the current-window devs into the Result.
	res, err := Run(Options{Profile: profile, Now: nowT})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Recompute the same window on demand (Elo read back from the ratings.json
	// Run just wrote — the precompute boundary).
	from := cache.MustParseMonth(res.Current.Window.Start)
	to := cache.MustParseMonth(res.Current.Window.End)
	devs, err := ContributorsForWindow(Options{Profile: profile, Now: nowT}, from, to)
	if err != nil {
		t.Fatalf("ContributorsForWindow: %v", err)
	}

	if !reflect.DeepEqual(devs, res.Devs) {
		t.Fatalf("contributors window != Run devs for the same window:\n got=%#v\nwant=%#v", devs, res.Devs)
	}
}

// TestTeamFlowForWindowMatchesRun is the /api/team/flow parity gate: handed the
// window Run used, the on-demand team flow must equal metrics.json's team_flow.
func TestTeamFlowForWindowMatchesRun(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	month := cache.MustParseMonth("2026-04")
	if err := cache.WriteMonth(cache.SourceJira, "CD", month, []cache.JiraIssue{
		{Key: "CD-1", Status: "Done", Created: mustTime("2026-04-01T00:00:00Z"), Updated: mustTime("2026-04-08T00:00:00Z"),
			Resolved: ptrTime(mustTime("2026-04-08T00:00:00Z")), CycleHours: 30, Labels: []string{"CLAUDE_GEN"}},
		{Key: "CD-2", Status: "Done", Created: mustTime("2026-04-02T00:00:00Z"), Updated: mustTime("2026-04-12T00:00:00Z"),
			Resolved: ptrTime(mustTime("2026-04-12T00:00:00Z")), CycleHours: 90},
	}); err != nil {
		t.Fatalf("write issues: %v", err)
	}
	if err := cache.WriteMonth(cache.SourceGitHubPRs, "consumerdirect", month, []cache.GitHubPR{
		{Number: 1, Repo: "consumerdirect/x", Author: "alice", Created: mustTime("2026-04-05T00:00:00Z"),
			Merged: ptrTime(mustTime("2026-04-06T00:00:00Z")), Additions: 10},
	}); err != nil {
		t.Fatalf("write prs: %v", err)
	}

	profile := config.Profile{
		Jira:   config.JiraConfig{Projects: []string{"CD"}, AccountID: "acct-alice"},
		GitHub: config.GitHubConfig{Username: "alice", Orgs: []string{"consumerdirect"}},
		Window: config.WindowConfig{BackfillStart: "2026-01", DefaultLengthMonths: 4},
		Devs:   []config.DevIdentity{{GitHubLogin: "alice", JiraAccountID: "acct-alice", DisplayName: "Alice"}},
	}
	nowT := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)

	res, err := Run(Options{Profile: profile, Now: nowT})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	from := cache.MustParseMonth(res.Current.Window.Start)
	to := cache.MustParseMonth(res.Current.Window.End)
	flow, err := TeamFlowForWindow(Options{Profile: profile, Now: nowT}, from, to)
	if err != nil {
		t.Fatalf("TeamFlowForWindow: %v", err)
	}
	if !reflect.DeepEqual(flow, res.TeamFlow) {
		t.Fatalf("team flow != Run team_flow for the same window:\n got=%#v\nwant=%#v", flow, res.TeamFlow)
	}
}

func TestContributorsForWindowRejectsInvertedWindow(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	profile := config.Profile{
		GitHub: config.GitHubConfig{Orgs: []string{"consumerdirect"}},
		Window: config.WindowConfig{BackfillStart: "2026-01", DefaultLengthMonths: 4},
	}
	_, err := ContributorsForWindow(Options{Profile: profile, Now: time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)},
		cache.MustParseMonth("2026-04"), cache.MustParseMonth("2026-02"))
	if err == nil {
		t.Fatal("expected error for inverted window (to before from)")
	}
}
