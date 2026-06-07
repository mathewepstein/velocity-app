package analyze

import (
	"testing"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

func twoDevDataset() *Loaded {
	return &Loaded{
		Months: cache.MonthsInRange(cache.MustParseMonth("2026-01"), cache.MustParseMonth("2026-04")),
		Issues: []cache.JiraIssue{
			{Key: "CD-1", Assignee: "acct-alice", Reporter: "acct-bob",
				Created: mustTime("2026-01-05T00:00:00Z"), Updated: mustTime("2026-01-10T00:00:00Z")},
			{Key: "CD-2", Assignee: "acct-bob", Reporter: "acct-bob",
				Created: mustTime("2026-02-01T00:00:00Z"), Updated: mustTime("2026-02-20T00:00:00Z"),
				Resolved: ptrTime(mustTime("2026-03-01T00:00:00Z"))},
			{Key: "CD-3", Assignee: "acct-stranger", Reporter: "acct-stranger",
				Created: mustTime("2026-02-15T00:00:00Z"), Updated: mustTime("2026-02-15T00:00:00Z")},
		},
		PRs: []cache.GitHubPR{
			{Number: 1, Author: "alice", Created: mustTime("2026-01-15T00:00:00Z"),
				Merged: ptrTime(mustTime("2026-01-20T00:00:00Z")), Additions: 50, Deletions: 10},
			{Number: 2, Author: "bob", Created: mustTime("2026-02-15T00:00:00Z")},
			{Number: 3, Author: "stranger", Created: mustTime("2026-03-01T00:00:00Z")},
			{Number: 4, Author: "dependabot[bot]", Created: mustTime("2026-03-10T00:00:00Z")},
		},
		Commits: []cache.GitHubCommit{
			{SHA: "a", Author: "alice", Committed: mustTime("2026-01-16T00:00:00Z")},
			{SHA: "b", Author: "alice", Committed: mustTime("2026-01-17T00:00:00Z")},
			{SHA: "c", Author: "bob", Committed: mustTime("2026-02-16T00:00:00Z")},
		},
		Reviews: []cache.GitHubReview{
			{PRNumber: 2, Reviewer: "alice", State: "APPROVED", Submitted: mustTime("2026-02-16T00:00:00Z")},
			{PRNumber: 1, Reviewer: "bob", State: "CHANGES_REQUESTED", Submitted: mustTime("2026-01-18T00:00:00Z")},
			{PRNumber: 3, Reviewer: "stranger", State: "APPROVED", Submitted: mustTime("2026-03-02T00:00:00Z")},
		},
	}
}

func TestFilterForDevAttributesByIdentity(t *testing.T) {
	data := twoDevDataset()
	alice := config.DevIdentity{GitHubLogin: "alice", JiraAccountID: "acct-alice"}

	got := filterForDev(data, alice)

	if len(got.PRs) != 1 || got.PRs[0].Author != "alice" {
		t.Errorf("alice's PRs wrong: %+v", got.PRs)
	}
	if len(got.Commits) != 2 {
		t.Errorf("alice's commit count = %d, want 2", len(got.Commits))
	}
	if len(got.Reviews) != 1 || got.Reviews[0].Reviewer != "alice" {
		t.Errorf("alice's reviews wrong: %+v", got.Reviews)
	}
	// CD-1 (alice is assignee) is the only Jira issue she touches.
	if len(got.Issues) != 1 || got.Issues[0].Key != "CD-1" {
		t.Errorf("alice's issues wrong: %+v", got.Issues)
	}
}

func TestBuildDevWindowsSplitsAcrossDevsPlusUnknown(t *testing.T) {
	data := twoDevDataset()
	devs := []config.DevIdentity{
		{GitHubLogin: "alice", JiraAccountID: "acct-alice", DisplayName: "Alice"},
		{GitHubLogin: "bob", JiraAccountID: "acct-bob", DisplayName: "Bob"},
	}
	start := cache.MustParseMonth("2026-01")
	end := cache.MustParseMonth("2026-04")

	got := buildDevWindows(data, devs, nil, start, end, start, end, testCI(), testNorm())
	if len(got) != 3 {
		t.Fatalf("expected 3 buckets (alice, bob, unknown), got %d", len(got))
	}

	byName := map[string]DevWindowMetrics{}
	for _, d := range got {
		byName[d.Dev.DisplayName] = d
	}

	alice := byName["Alice"]
	if alice.Totals.PRsCreated != 1 || alice.Totals.PRsMerged != 1 {
		t.Errorf("alice PR totals wrong: %+v", alice.Totals)
	}
	if alice.Totals.Commits != 2 {
		t.Errorf("alice commits = %d, want 2", alice.Totals.Commits)
	}
	if alice.Totals.PRsReviewed != 1 {
		t.Errorf("alice reviews = %d, want 1", alice.Totals.PRsReviewed)
	}
	// CD-1 assigned to Alice
	if alice.Totals.JiraIssuesTouched != 1 {
		t.Errorf("alice touched = %d, want 1", alice.Totals.JiraIssuesTouched)
	}

	bob := byName["Bob"]
	if bob.Totals.PRsCreated != 1 {
		t.Errorf("bob PR created = %d, want 1", bob.Totals.PRsCreated)
	}
	if bob.Totals.PRsReviewed != 1 {
		t.Errorf("bob reviews = %d, want 1", bob.Totals.PRsReviewed)
	}
	// CD-1 (reporter), CD-2 (assignee + reporter)
	if bob.Totals.JiraIssuesTouched != 2 {
		t.Errorf("bob touched = %d, want 2 (CD-1 as reporter + CD-2 as both)", bob.Totals.JiraIssuesTouched)
	}
	if bob.Totals.JiraIssuesResolved != 1 {
		t.Errorf("bob resolved = %d, want 1 (CD-2)", bob.Totals.JiraIssuesResolved)
	}

	unknown := byName["unknown"]
	if unknown.Totals.PRsCreated < 2 {
		t.Errorf("unknown should hold stranger + dependabot PRs, got %d", unknown.Totals.PRsCreated)
	}
	if unknown.Totals.PRsReviewed != 1 {
		t.Errorf("unknown reviews = %d, want 1 (stranger)", unknown.Totals.PRsReviewed)
	}
	if unknown.Totals.JiraIssuesTouched != 1 {
		t.Errorf("unknown touched = %d, want 1 (CD-3)", unknown.Totals.JiraIssuesTouched)
	}
}

func TestBuildDevWindowsOmitsUnknownWhenEmpty(t *testing.T) {
	// Synthetic dataset where every record maps to a configured dev.
	data := &Loaded{
		Months: cache.MonthsInRange(cache.MustParseMonth("2026-01"), cache.MustParseMonth("2026-01")),
		PRs: []cache.GitHubPR{
			{Number: 1, Author: "alice", Created: mustTime("2026-01-15T00:00:00Z")},
		},
	}
	devs := []config.DevIdentity{
		{GitHubLogin: "alice", DisplayName: "Alice"},
	}
	got := buildDevWindows(data, devs, nil, cache.MustParseMonth("2026-01"), cache.MustParseMonth("2026-01"), cache.MustParseMonth("2026-01"), cache.MustParseMonth("2026-01"), testCI(), testNorm())
	if len(got) != 1 {
		t.Errorf("expected 1 bucket (no unknown), got %d", len(got))
	}
	if got[0].Dev.DisplayName != "Alice" {
		t.Errorf("expected alice's bucket, got %+v", got[0].Dev)
	}
}

func TestBuildDevWindowsDropsExcludedAuthorsFromUnknown(t *testing.T) {
	data := twoDevDataset()
	devs := []config.DevIdentity{
		{GitHubLogin: "alice", JiraAccountID: "acct-alice", DisplayName: "Alice"},
		{GitHubLogin: "bob", JiraAccountID: "acct-bob", DisplayName: "Bob"},
	}
	excludes := []string{"stranger", "*[bot]"}
	start := cache.MustParseMonth("2026-01")
	end := cache.MustParseMonth("2026-04")

	got := buildDevWindows(data, devs, excludes, start, end, start, end, testCI(), testNorm())

	var unknown *DevWindowMetrics
	for i := range got {
		if got[i].Dev.DisplayName == "unknown" {
			unknown = &got[i]
		}
	}
	if unknown == nil {
		// CD-3 is assigned to acct-stranger (Jira-only); exclude list is
		// GitHub-login-only, so CD-3 should still surface under unknown.
		t.Fatalf("expected unknown bucket to remain (for CD-3 / Jira-only stranger), got buckets: %+v", got)
	}
	if unknown.Totals.PRsCreated != 0 {
		t.Errorf("unknown PRsCreated = %d, want 0 (stranger + dependabot both excluded)", unknown.Totals.PRsCreated)
	}
	if unknown.Totals.PRsReviewed != 0 {
		t.Errorf("unknown PRsReviewed = %d, want 0 (stranger review excluded)", unknown.Totals.PRsReviewed)
	}
	if unknown.Totals.JiraIssuesTouched != 1 {
		t.Errorf("unknown JiraIssuesTouched = %d, want 1 (CD-3 still unmapped Jira-side)", unknown.Totals.JiraIssuesTouched)
	}
}

func TestBuildDevWindowsSkipsExcludedDev(t *testing.T) {
	data := twoDevDataset()
	devs := []config.DevIdentity{
		{GitHubLogin: "alice", JiraAccountID: "acct-alice", DisplayName: "Alice"},
		{GitHubLogin: "bob", JiraAccountID: "acct-bob", DisplayName: "Bob"},
	}
	excludes := []string{"bob"}
	start := cache.MustParseMonth("2026-01")
	end := cache.MustParseMonth("2026-04")

	got := buildDevWindows(data, devs, excludes, start, end, start, end, testCI(), testNorm())

	for _, d := range got {
		if d.Dev.DisplayName == "Bob" {
			t.Errorf("Bob should be excluded but appears: %+v", d)
		}
	}
}

func TestDevExcludedRequiresAllLoginsToMatch(t *testing.T) {
	// Multi-login dev: only excluded if EVERY login matches a pattern.
	multi := config.DevIdentity{GitHubLogins: []string{"alice", "alice-bot"}}
	if devExcluded(multi, []string{"*[bot]"}) {
		t.Errorf("dev with one excluded + one real login should NOT be excluded")
	}
	if !devExcluded(multi, []string{"alice", "alice-bot"}) {
		t.Errorf("dev with all logins matching exclude should be excluded")
	}
	// Jira-only dev never excluded by a GitHub-login pattern.
	jiraOnly := config.DevIdentity{JiraAccountID: "acct-x"}
	if devExcluded(jiraOnly, []string{"x"}) {
		t.Errorf("Jira-only dev should not be excluded by GitHub-login pattern")
	}
}

func TestSelfFilterScopesToConfiguredUser(t *testing.T) {
	data := twoDevDataset()
	profile := config.Profile{
		GitHub: config.GitHubConfig{Username: "alice"},
		Jira:   config.JiraConfig{AccountID: "acct-alice"},
	}
	scoped := selfFilter(data, profile)

	if len(scoped.PRs) != 1 || scoped.PRs[0].Author != "alice" {
		t.Errorf("self-filtered PRs wrong: %+v", scoped.PRs)
	}
	if len(scoped.Reviews) != 1 || scoped.Reviews[0].Reviewer != "alice" {
		t.Errorf("self-filtered reviews wrong: %+v", scoped.Reviews)
	}
	if len(scoped.Issues) != 1 || scoped.Issues[0].Key != "CD-1" {
		t.Errorf("self-filtered issues wrong: %+v", scoped.Issues)
	}
}

func TestSelfFilterPassesThroughWhenIdentityMissing(t *testing.T) {
	data := twoDevDataset()
	scoped := selfFilter(data, config.Profile{})
	if len(scoped.PRs) != len(data.PRs) {
		t.Errorf("empty profile should not filter: got %d PRs, want %d", len(scoped.PRs), len(data.PRs))
	}
}

func TestRunPopulatesDevsAndFiltersSelfView(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	month := cache.MustParseMonth("2026-04")
	prior := cache.MustParseMonth("2026-03")
	// Use the cache layer so LoadAll can read it back.
	if err := cache.WriteMonth(cache.SourceGitHubPRs, "consumerdirect", month, []cache.GitHubPR{
		{Number: 1, Repo: "consumerdirect/x", Author: "alice", Created: mustTime("2026-04-05T00:00:00Z"),
			Merged: ptrTime(mustTime("2026-04-06T00:00:00Z")), Additions: 10},
		{Number: 2, Repo: "consumerdirect/x", Author: "bob", Created: mustTime("2026-04-10T00:00:00Z")},
	}); err != nil {
		t.Fatalf("write prs: %v", err)
	}
	if err := cache.WriteMonth(cache.SourceGitHubPRs, "consumerdirect", prior, []cache.GitHubPR{}); err != nil {
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

	res, err := Run(Options{Profile: profile, Now: time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Single-user view should only count alice's PR.
	if res.Current.Totals.PRsCreated != 1 {
		t.Errorf("self view PRsCreated = %d, want 1 (only alice's)", res.Current.Totals.PRsCreated)
	}
	if res.Current.Totals.PRsReviewed != 1 {
		t.Errorf("self view PRsReviewed = %d, want 1", res.Current.Totals.PRsReviewed)
	}

	// Devs view should split the PRs between alice and bob.
	if len(res.Devs) != 2 {
		t.Fatalf("expected 2 devs, got %d", len(res.Devs))
	}
	byLogin := map[string]DevWindowMetrics{}
	for _, d := range res.Devs {
		byLogin[d.Dev.GitHubLogin] = d
	}
	if byLogin["alice"].Totals.PRsCreated != 1 {
		t.Errorf("alice in devs view: PRs = %d, want 1", byLogin["alice"].Totals.PRsCreated)
	}
	if byLogin["bob"].Totals.PRsCreated != 1 {
		t.Errorf("bob in devs view: PRs = %d, want 1", byLogin["bob"].Totals.PRsCreated)
	}
	if res.Meta.ReviewsLoaded != 1 {
		t.Errorf("meta reviews = %d, want 1", res.Meta.ReviewsLoaded)
	}
	if res.Meta.DevsMapped != 2 {
		t.Errorf("meta devs mapped = %d, want 2", res.Meta.DevsMapped)
	}
}
