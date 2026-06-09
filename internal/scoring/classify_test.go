package scoring

import (
	"testing"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
)

func TestIsBugType(t *testing.T) {
	cases := []struct {
		issueType string
		labels    []string
		want      bool
	}{
		{"Bug", nil, true},
		{"Regression", nil, true},
		{"Hotfix", nil, true},
		{"Task", nil, false},
		{"Story", []string{"regression"}, true},
		{"Task", []string{"feature"}, false},
	}
	for _, c := range cases {
		ev := &TicketEvidence{IssueType: c.issueType, Labels: c.labels}
		if got := isBugType(ev); got != c.want {
			t.Errorf("isBugType(%q,%v) = %v, want %v", c.issueType, c.labels, got, c.want)
		}
	}
}

func TestIsSpike(t *testing.T) {
	cases := []struct {
		summary   string
		issueType string
		labels    []string
		want      bool
	}{
		{"Spike: investigate caching", "Task", nil, true},
		{"Investigate flaky pipeline", "Task", []string{"investigate"}, true},
		{"Refactor billing", "Spike", nil, true},
		{"The work spiked CPU usage", "Task", nil, false}, // "spiked" not a word match
		{"Normal feature work", "Story", nil, false},
	}
	for _, c := range cases {
		ev := &TicketEvidence{Summary: c.summary, IssueType: c.issueType, Labels: c.labels}
		if got := IsSpike(ev); got != c.want {
			t.Errorf("IsSpike(%q) = %v, want %v", c.summary, got, c.want)
		}
	}
}

func TestSpikeArtifactSignals(t *testing.T) {
	iss := &cache.JiraIssue{
		Description: "See https://consumerdirect.atlassian.net/wiki/spaces/CDS/page and implementation/velocity/foo-plan.md",
		Comments: []cache.IssueComment{
			{Body: "short note", Created: time.Now().UTC()},
			{Body: "```go\nfunc x(){}\n```", Created: time.Now().UTC()},                       // code fence → substantive
			{Body: "Follow-up: https://docs.google.com/document/d/abc", Created: time.Now().UTC()}, // url → substantive + doc link
		},
	}
	links, substantive := spikeArtifactSignals(iss)
	// Confluence wiki + md ref in description, google doc in comment = 3 distinct links.
	if links != 3 {
		t.Errorf("links = %d, want 3", links)
	}
	if substantive != 2 {
		t.Errorf("substantive = %d, want 2", substantive)
	}
}

func TestSpikeLinkSignals(t *testing.T) {
	iss := &cache.JiraIssue{
		Links: []cache.LinkedIssue{
			{Key: "CD-401", LinkType: "subtask", Direction: "outward", Phrase: "subtask"},      // spawned
			{Key: "CD-402", LinkType: "subtask", Direction: "outward", Phrase: "subtask"},      // spawned
			{Key: "CD-403", LinkType: "Defect", Direction: "outward", Phrase: "created"},       // spawned (creation, outward)
			{Key: "CD-404", LinkType: "Defect", Direction: "inward", Phrase: "created by"},     // NOT spawned (inward origin)
			{Key: "CD-405", LinkType: "A Relate", Direction: "outward", Phrase: "relates to"},  // plain link
			{Key: "CD-405", LinkType: "A Relate", Direction: "inward", Phrase: "is related to"}, // dup counterpart
		},
	}
	spawned, breadth := spikeLinkSignals(iss)
	if spawned != 3 {
		t.Errorf("spawned = %d, want 3", spawned)
	}
	// Distinct counterparts: CD-401..405 = 5 (CD-405 appears twice).
	if breadth != 5 {
		t.Errorf("breadth = %d, want 5", breadth)
	}
}

func TestIsSpawnLink(t *testing.T) {
	cases := []struct {
		l    cache.LinkedIssue
		want bool
	}{
		{cache.LinkedIssue{LinkType: "subtask", Direction: "outward"}, true},
		{cache.LinkedIssue{LinkType: "Defect", Direction: "outward", Phrase: "created"}, true},
		{cache.LinkedIssue{LinkType: "Cloners", Direction: "outward", Phrase: "clones"}, true},
		{cache.LinkedIssue{LinkType: "Defect", Direction: "inward", Phrase: "created by"}, false},
		{cache.LinkedIssue{LinkType: "Blocks", Direction: "outward", Phrase: "blocks"}, false},
		{cache.LinkedIssue{LinkType: "A Relate", Direction: "outward", Phrase: "relates to"}, false},
	}
	for _, c := range cases {
		if got := isSpawnLink(c.l); got != c.want {
			t.Errorf("isSpawnLink(%+v) = %v, want %v", c.l, got, c.want)
		}
	}
}

// --- Phase 3: bug-aware small-diff floor ---

// A tiny diff that bounced: as a Task the rework credit is downscaled (flaky
// churn); as a Bug it is NOT — a 2-line fix that bounced is real diagnosis.
func TestBand_BugSuppressesSmallDiffDownscale(t *testing.T) {
	mk := func(issueType string) *TicketEvidence {
		ev := onePR()
		ev.IssueType = issueType
		ev.NetLOC = 5 // below SmallDiffLOCFloor (20)
		ev.FileCount = 1
		ev.CycleHours = 12
		ev.ReworkCount = 2
		ev.Repos = []string{"org/app"}
		return ev
	}
	cfg := spCfg()

	task := Band(mk("Task"), cfg)
	bug := Band(mk("Bug"), cfg)

	// The bug keeps full rework credit, so its raw effort must exceed the task's
	// (downscaled) raw effort for the same diff.
	if !(bug.RawEffort > task.RawEffort) {
		t.Errorf("bug raw effort (%v) should exceed task raw effort (%v) — small-diff downscale should be suppressed for bugs",
			bug.RawEffort, task.RawEffort)
	}
}
