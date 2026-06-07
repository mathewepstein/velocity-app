package analyze

import (
	"fmt"
	"testing"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

// momentumSurge is the momentum-detection config the tests run against.
// RecentWeeks=2 / BaselineWeeks=8 means the recent window is the latest two
// ISO weeks in the data and the baseline is the eight weeks before that.
func momentumSurge() config.SurgeConfig {
	return config.SurgeConfig{
		RecentWeeks:       2,
		BaselineWeeks:     8,
		MinRecentActivity: 3,
		HotRatio:          2.0,
		RisingRatio:       1.2,
		CoolingRatio:      0.8,
	}
}

// Weeks for the fixtures below (Mondays): W01=2025-12-29, W02=2026-01-05,
// W03=2026-01-12, ... W08=2026-02-16, W09=2026-02-23, W10=2026-03-02. The
// latest week present is the anchor; recent = {W09, W10}, baseline = W01..W08.

func TestDetectProjects_HotMomentumAndDedupesPRsAcrossSubissues(t *testing.T) {
	data := &Loaded{
		Issues: []cache.JiraIssue{
			{Key: "CD-100", IssueType: "Epic", Summary: "Widget rewrite", Updated: mustTime("2026-01-01T00:00:00Z")}, // W01
			{Key: "CD-101", EpicKey: "CD-100", Updated: mustTime("2026-01-06T00:00:00Z")},                            // W02
			{Key: "CD-102", EpicKey: "CD-100", Updated: mustTime("2026-01-07T00:00:00Z")},                            // W02
			{Key: "CD-900", Updated: mustTime("2026-01-06T00:00:00Z")},                                               // unrelated
		},
		PRs: []cache.GitHubPR{
			{Number: 1, Created: mustTime("2026-01-06T00:00:00Z"), IssueKeys: []string{"CD-101"}, Additions: 100}, // baseline W02
			// Recent burst. PR 2 tags TWO sub-issues of the same epic → counts once.
			{Number: 2, Created: mustTime("2026-02-24T00:00:00Z"), IssueKeys: []string{"CD-101", "CD-102"}, Additions: 300}, // W09
			{Number: 3, Created: mustTime("2026-03-03T00:00:00Z"), IssueKeys: []string{"CD-102"}, Additions: 200},          // W10
			{Number: 4, Created: mustTime("2026-03-04T00:00:00Z"), IssueKeys: []string{"CD-101"}, Additions: 150},          // W10
		},
	}
	projects := detectProjects(data, momentumSurge())
	if len(projects) != 1 {
		t.Fatalf("want 1 project, got %d", len(projects))
	}
	p := projects[0]
	if p.EpicKey != "CD-100" || p.Summary != "Widget rewrite" {
		t.Errorf("epic identity: %s / %q", p.EpicKey, p.Summary)
	}
	// PR 2 tags two sub-issues but must count once for the epic → 4 PRs, not 5.
	if p.Totals.PRs != 4 {
		t.Errorf("PRs counted: want 4 (PR2 deduped), got %d", p.Totals.PRs)
	}
	if p.Totals.LOCAdded != 750 {
		t.Errorf("LOC added: want 750, got %d", p.Totals.LOCAdded)
	}
	// The epic issue CD-100 has no EpicKey (it IS the epic), so it adds no
	// weekly signal — baseline is W02 only: 2 issue touches + 1 PR = 3, rate
	// 3/8=0.375. Recent 3/2=1.5 → momentum 4.0 → hot.
	if p.Direction != "hot" {
		t.Errorf("direction: want hot, got %q (momentum %.2f)", p.Direction, p.Momentum)
	}
	if p.Momentum < 3.9 || p.Momentum > 4.1 {
		t.Errorf("momentum: want ~4.0, got %.2f", p.Momentum)
	}
	if p.RecentPRs != 3 {
		t.Errorf("recent PRs: want 3, got %d", p.RecentPRs)
	}
	if !containsTrigger(p.Triggers, "hot") {
		t.Errorf("want 'hot' trigger, got %v", p.Triggers)
	}
}

func TestDetectProjects_DropsEpicsBelowRecentActivityFloor(t *testing.T) {
	data := &Loaded{
		Issues: []cache.JiraIssue{
			{Key: "CD-200", IssueType: "Epic", Summary: "Tiny"},
			{Key: "CD-201", EpicKey: "CD-200", Updated: mustTime("2026-03-03T00:00:00Z")}, // W10
		},
		PRs: []cache.GitHubPR{
			// Only one recent PR — below MinRecentActivity (3).
			{Number: 10, Created: mustTime("2026-03-03T00:00:00Z"), IssueKeys: []string{"CD-201"}, Additions: 10},
		},
	}
	projects := detectProjects(data, momentumSurge())
	if len(projects) != 0 {
		t.Errorf("epic below the recent-activity floor should be dropped, got %d", len(projects))
	}
}

func TestDetectProjects_UnlinkedPRsIgnored(t *testing.T) {
	data := &Loaded{
		PRs: []cache.GitHubPR{
			{Number: 1, Created: mustTime("2026-03-03T00:00:00Z"), Additions: 2000}, // no issue keys
		},
	}
	projects := detectProjects(data, momentumSurge())
	if len(projects) != 0 {
		t.Errorf("PR without issue keys should not produce a project, got %d", len(projects))
	}
}

func TestDetectProjects_CoolingWhenRecentRateBelowBaseline(t *testing.T) {
	data := &Loaded{
		Issues: []cache.JiraIssue{
			{Key: "CD-300", IssueType: "Epic", Summary: "Winding down"},
		},
		PRs: []cache.GitHubPR{
			// Recent: exactly the floor (3 PRs) in W10.
			{Number: 1, Created: mustTime("2026-03-02T00:00:00Z"), IssueKeys: []string{"CD-301"}, Additions: 10},
			{Number: 2, Created: mustTime("2026-03-03T00:00:00Z"), IssueKeys: []string{"CD-301"}, Additions: 10},
			{Number: 3, Created: mustTime("2026-03-04T00:00:00Z"), IssueKeys: []string{"CD-301"}, Additions: 10},
		},
	}
	// Heavy baseline: 16 issue touches in W03. Recent is the 3 PRs only
	// (rate 1.5); baseline rate ≈ 17/8 → momentum ≈ 0.71 → cooling.
	for i := 0; i < 16; i++ {
		data.Issues = append(data.Issues, cache.JiraIssue{
			Key:     fmt.Sprintf("CD-3%02d", 10+i),
			EpicKey: "CD-300",
			Updated: mustTime("2026-01-12T00:00:00Z"), // W03
		})
	}
	// CD-301 carries the recent PRs; link it to the epic but keep its own touch
	// in the baseline (W03) so it doesn't inflate the recent signal.
	data.Issues = append(data.Issues, cache.JiraIssue{Key: "CD-301", EpicKey: "CD-300", Updated: mustTime("2026-01-12T00:00:00Z")})

	projects := detectProjects(data, momentumSurge())
	if len(projects) != 1 {
		t.Fatalf("want 1 project, got %d", len(projects))
	}
	p := projects[0]
	if p.Direction != "cooling" {
		t.Errorf("direction: want cooling, got %q (momentum %.2f)", p.Direction, p.Momentum)
	}
	if len(p.Triggers) != 0 {
		t.Errorf("cooling epic should carry no triggers, got %v", p.Triggers)
	}
}

func TestDetectProjects_NewEpicWithNoBaseline(t *testing.T) {
	data := &Loaded{
		Issues: []cache.JiraIssue{
			{Key: "CD-400", IssueType: "Epic", Summary: "Fresh start"},
			{Key: "CD-401", EpicKey: "CD-400", Updated: mustTime("2026-03-02T00:00:00Z")}, // W10
		},
		PRs: []cache.GitHubPR{
			// All activity in the recent window; nothing in the baseline.
			{Number: 1, Created: mustTime("2026-02-24T00:00:00Z"), IssueKeys: []string{"CD-401"}, Additions: 100}, // W09
			{Number: 2, Created: mustTime("2026-03-03T00:00:00Z"), IssueKeys: []string{"CD-401"}, Additions: 100}, // W10
			{Number: 3, Created: mustTime("2026-03-04T00:00:00Z"), IssueKeys: []string{"CD-401"}, Additions: 100}, // W10
		},
	}
	projects := detectProjects(data, momentumSurge())
	if len(projects) != 1 {
		t.Fatalf("want 1 project, got %d", len(projects))
	}
	if projects[0].Direction != "new" {
		t.Errorf("direction: want new (no baseline), got %q", projects[0].Direction)
	}
	if !containsTrigger(projects[0].Triggers, "new") {
		t.Errorf("want 'new' trigger, got %v", projects[0].Triggers)
	}
}

func TestDetectProjects_PeakWeekUsesCombinedDiscreteSignals(t *testing.T) {
	data := &Loaded{
		Issues: []cache.JiraIssue{
			{Key: "CD-500", IssueType: "Epic", Summary: "Foo"},
			{Key: "CD-501", EpicKey: "CD-500", Updated: mustTime("2026-02-02T00:00:00Z")}, // W06
			{Key: "CD-502", EpicKey: "CD-500", Updated: mustTime("2026-02-09T00:00:00Z")}, // W07
			{Key: "CD-503", EpicKey: "CD-500", Updated: mustTime("2026-02-16T00:00:00Z")}, // W08
		},
		PRs: []cache.GitHubPR{
			// W07 has 2 PRs — should win peak despite a W08 PR with huge LoC.
			{Number: 1, Created: mustTime("2026-02-10T00:00:00Z"), IssueKeys: []string{"CD-502"}, Additions: 50},
			{Number: 2, Created: mustTime("2026-02-11T00:00:00Z"), IssueKeys: []string{"CD-502"}, Additions: 50},
			{Number: 3, Created: mustTime("2026-02-17T00:00:00Z"), IssueKeys: []string{"CD-503"}, Additions: 5000},
		},
	}
	projects := detectProjects(data, momentumSurge())
	if len(projects) != 1 {
		t.Fatalf("want 1 project, got %d", len(projects))
	}
	if got := projects[0].PeakWeek; got != "2026-W07" {
		t.Errorf("peak week: want 2026-W07 (3 signals: 1 issue + 2 PRs), got %s", got)
	}
}

func containsTrigger(triggers []string, want string) bool {
	for _, t := range triggers {
		if t == want {
			return true
		}
	}
	return false
}
