package scoring

import (
	"testing"
	"time"

	"github.com/mathewepstein/velocity/internal/analyze"
	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

// discoverFixture builds a corpus with:
//   - a "src/auth" dir whose tickets have long cycles + high rework (should rank high),
//   - a "src/util" dir of baseline tickets (should not surface),
//   - a "src/main/resources/db/changelog" migration dir touched by a single
//     trivial ticket (should surface high via the migration detector despite
//     low ticket count / trivial stats).
func discoverFixture() *analyze.Loaded {
	base := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	var issues []cache.JiraIssue
	var prs []cache.GitHubPR
	n := 0
	mk := func(dir string, cycleH float64, rework bool, flips int) {
		n++
		key := "CD-" + itos(n)
		created := base.Add(time.Duration(n) * time.Hour)
		merged := created.Add(time.Duration(cycleH) * time.Hour)
		resolved := merged.Add(time.Hour)
		iss := cache.JiraIssue{
			Key: key, Summary: "work " + key, IssueType: "Task", Status: "Done",
			Resolution: "Done", Created: created, Resolved: ptr(resolved),
			CycleHours: cycleH, DetailFetched: true,
		}
		if rework {
			// A backward bounce with a commit so the de-noiser keeps it as real rework.
			iss.Changelog = []cache.StatusTransition{
				{At: created.Add(time.Hour), From: "In Progress", To: "Code Review", Field: "status"},
				{At: created.Add(2 * time.Hour), From: "Code Review", To: "In Progress", Field: "status"},
			}
			iss.StatusFlips = flips
		}
		issues = append(issues, iss)
		prs = append(prs, cache.GitHubPR{
			Number: 100 + n, Repo: "org/app", State: "merged", Created: created, Merged: ptr(merged),
			IssueKeys:   []string{key},
			FileChanges: []cache.FileChange{{Path: dir + "/F" + itos(n) + ".java", Status: "modified", Additions: 10, Deletions: 2}},
			Additions:   10, Deletions: 2,
		})
	}

	// 6 high-cost auth tickets (long cycle + rework).
	for i := 0; i < 6; i++ {
		mk("src/auth", 200, true, 3)
	}
	// 6 baseline util tickets (short cycle, no rework).
	for i := 0; i < 6; i++ {
		mk("src/util", 8, false, 0)
	}
	// 1 trivial migration ticket.
	mk("src/main/resources/db/changelog", 5, false, 0)

	return &analyze.Loaded{Issues: issues, PRs: prs}
}

func TestRiskDiscover_RanksHighCostDirTop(t *testing.T) {
	ext := NewExtractor(discoverFixture(), defaultNorm(), 5*time.Minute, config.RiskConfig{})
	res := RiskDiscover(ext, RiskDiscoverOpts{MinTickets: 5})

	if len(res.Candidates) == 0 {
		t.Fatal("expected at least one candidate")
	}
	// The auth dir should be present and tier high.
	var auth *RiskCandidate
	for i := range res.Candidates {
		if res.Candidates[i].Glob == "**/src/auth/**" {
			auth = &res.Candidates[i]
		}
	}
	if auth == nil {
		t.Fatalf("auth dir not proposed; got %+v", res.Candidates)
	}
	if auth.Tier != "high" {
		t.Errorf("auth tier = %q, want high (outcome=%.2f)", auth.Tier, auth.Outcome)
	}

	// The baseline util dir should NOT clear the threshold.
	for _, c := range res.Candidates {
		if c.Glob == "**/src/util/**" {
			t.Errorf("baseline util dir should not be proposed, got tier %q", c.Tier)
		}
	}
}

func TestRiskDiscover_MigrationDetectorSurfacesHigh(t *testing.T) {
	ext := NewExtractor(discoverFixture(), defaultNorm(), 5*time.Minute, config.RiskConfig{})
	res := RiskDiscover(ext, RiskDiscoverOpts{MinTickets: 5})

	found := false
	for _, c := range res.Candidates {
		if c.Migration && c.Tier == "high" {
			found = true
		}
	}
	if !found {
		t.Errorf("migration dir should surface as high despite 1 trivial ticket; got %+v", res.Candidates)
	}
}

func TestContainsSeedTerm_TokenBoundary(t *testing.T) {
	cases := []struct {
		dir  string
		want bool
	}{
		{"src/auth-microservice", true},     // delimited token "auth"
		{"src/privacy-microservice", true},  // "privacy"
		{"src/components/credit-u", true},   // "credit"
		{"frontend/signup-funnel", true},    // "signup"
		{"fusionauth-python/bin", false},    // "fusionauth" is one token, not "auth"
		{"CorporateWebSites/smartcreditbiz", false}, // "smartcreditbiz" ≠ "credit"
		{"src/creditorhighway", false},      // partner name, not "credit"
		{"src/components/layout", false},    // no sensitive token
	}
	for _, c := range cases {
		if got := containsSeedTerm(c.dir); got != c.want {
			t.Errorf("containsSeedTerm(%q) = %v, want %v (tokens=%v)", c.dir, got, c.want, tokenizePath(c.dir))
		}
	}
}

func TestRiskDiscover_MinTicketFloor(t *testing.T) {
	ext := NewExtractor(discoverFixture(), defaultNorm(), 5*time.Minute, config.RiskConfig{})
	// With an impossibly high floor, only migration/seed dirs (which bypass the
	// floor) may surface — the outcome-ranked auth dir must drop out.
	res := RiskDiscover(ext, RiskDiscoverOpts{MinTickets: 100})
	for _, c := range res.Candidates {
		if c.Glob == "**/src/auth/**" {
			t.Errorf("auth dir (6 tickets) should be filtered by min-tickets=100")
		}
	}
}
