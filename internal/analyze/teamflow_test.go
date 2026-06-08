package analyze

import (
	"testing"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
)

// ipCL builds a changelog with `hours` of In Progress active-dev time
// (In Progress → Done, `hours` apart) so a fixture issue exercises
// ActiveDevHours — the dormancy-free metric the flow medians now use.
func ipCL(start string, hours float64) []cache.StatusTransition {
	t0 := mustTime(start)
	return []cache.StatusTransition{
		{At: t0, From: "Selected for Development", To: "In Progress", Field: "status"},
		{At: t0.Add(time.Duration(hours * float64(time.Hour))), From: "In Progress", To: "Done", Field: "status"},
	}
}

func TestHasClaudeLabel(t *testing.T) {
	cases := []struct {
		labels []string
		want   bool
	}{
		{[]string{"CLAUDE_GEN"}, true},
		{[]string{"Claude-Gen"}, true},
		{[]string{"backend", "claude_gen"}, true},
		{[]string{"claude-ready"}, false}, // a request, not authorship
		{[]string{"CODEX_GEN"}, false},    // different tool
		{[]string{"frontend"}, false},
		{nil, false},
	}
	for _, tc := range cases {
		if got := hasClaudeLabel(tc.labels); got != tc.want {
			t.Errorf("hasClaudeLabel(%v) = %v, want %v", tc.labels, got, tc.want)
		}
	}
}

func TestDeriveTeamFlowMonthlyBucketing(t *testing.T) {
	data := &Loaded{
		Issues: []cache.JiraIssue{
			// created Jan, resolved Feb, Claude-labeled, 5 SP, 48h active dev
			{Created: mustTime("2026-01-05T00:00:00Z"), Resolved: ptrTime(mustTime("2026-02-10T00:00:00Z")),
				Labels: []string{"CLAUDE_GEN"}, StoryPoints: 5, Changelog: ipCL("2026-02-08T00:00:00Z", 48)},
			// created Feb, resolved Feb, not Claude, 96h active dev
			{Created: mustTime("2026-02-01T00:00:00Z"), Resolved: ptrTime(mustTime("2026-02-20T00:00:00Z")),
				Changelog: ipCL("2026-02-14T00:00:00Z", 96)},
			// created Feb, still open
			{Created: mustTime("2026-02-15T00:00:00Z")},
		},
		PRs: []cache.GitHubPR{
			{Created: mustTime("2026-01-10T00:00:00Z")},
			{Created: mustTime("2026-02-02T00:00:00Z"), Merged: ptrTime(mustTime("2026-02-12T00:00:00Z"))},
		},
	}

	tf := deriveTeamFlow(data,
		cache.MustParseMonth("2026-01"), cache.MustParseMonth("2026-03"),
		cache.MustParseMonth("2026-01"), cache.MustParseMonth("2026-03"))

	if len(tf.Monthly) != 3 {
		t.Fatalf("want 3 months (Jan–Mar), got %d", len(tf.Monthly))
	}
	jan, feb := tf.Monthly[0], tf.Monthly[1]
	if jan.Month != "2026-01" || feb.Month != "2026-02" {
		t.Fatalf("month order wrong: %s, %s", jan.Month, feb.Month)
	}
	if jan.IssuesCreated != 1 || jan.PRsCreated != 1 {
		t.Errorf("Jan created: issues=%d prs=%d, want 1/1", jan.IssuesCreated, jan.PRsCreated)
	}
	if feb.IssuesCreated != 2 {
		t.Errorf("Feb issues created = %d, want 2", feb.IssuesCreated)
	}
	if feb.IssuesResolved != 2 {
		t.Errorf("Feb issues resolved = %d, want 2", feb.IssuesResolved)
	}
	if feb.PRsMerged != 1 {
		t.Errorf("Feb PRs merged = %d, want 1", feb.PRsMerged)
	}
	if feb.ClaudeIssuesResolved != 1 {
		t.Errorf("Feb Claude issues resolved = %d, want 1", feb.ClaudeIssuesResolved)
	}
	if feb.StoryPoints != 5 {
		t.Errorf("Feb story points = %v, want 5", feb.StoryPoints)
	}
	if feb.MedianCycleHours != 72 { // median(48,96)
		t.Errorf("Feb median cycle = %v, want 72", feb.MedianCycleHours)
	}
}

func TestDeriveClaudeCut(t *testing.T) {
	data := &Loaded{Issues: []cache.JiraIssue{
		{Resolved: ptrTime(mustTime("2026-02-10T00:00:00Z")), Labels: []string{"CLAUDE_GEN"}, Changelog: ipCL("2026-02-05T00:00:00Z", 50)},
		{Resolved: ptrTime(mustTime("2026-02-12T00:00:00Z")), Labels: []string{"CLAUDE_GEN"}, Changelog: ipCL("2026-02-05T00:00:00Z", 70)},
		{Resolved: ptrTime(mustTime("2026-02-15T00:00:00Z")), Changelog: ipCL("2026-02-05T00:00:00Z", 200)},
		{Resolved: ptrTime(mustTime("2026-09-01T00:00:00Z")), Labels: []string{"CLAUDE_GEN"}, Changelog: ipCL("2026-08-25T00:00:00Z", 999)}, // out of window
	}}
	cut := deriveClaudeCut(data, cache.MustParseMonth("2026-01"), cache.MustParseMonth("2026-04"))
	if cut.IssuesResolved != 3 {
		t.Errorf("IssuesResolved = %d, want 3 (out-of-window excluded)", cut.IssuesResolved)
	}
	if cut.ClaudeIssuesResolved != 2 {
		t.Errorf("ClaudeIssuesResolved = %d, want 2", cut.ClaudeIssuesResolved)
	}
	if cut.MedianCycleHoursClaude != 60 { // median(50,70)
		t.Errorf("Claude median cycle = %v, want 60", cut.MedianCycleHoursClaude)
	}
	if cut.MedianCycleHoursOther != 200 {
		t.Errorf("Other median cycle = %v, want 200", cut.MedianCycleHoursOther)
	}
}
