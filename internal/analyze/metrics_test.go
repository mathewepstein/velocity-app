package analyze

import (
	"testing"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
)

func ptrTime(t time.Time) *time.Time { return &t }

// mustTime parses RFC3339, panics on failure (test-only).
func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestRollupMonthly_AttributionAndZeroFill(t *testing.T) {
	data := &Loaded{
		Issues: []cache.JiraIssue{
			{
				Key: "CD-1", Created: mustTime("2026-01-05T00:00:00Z"),
				Updated: mustTime("2026-02-10T00:00:00Z"),
			},
			{
				Key: "CD-2", Created: mustTime("2026-02-01T00:00:00Z"),
				Updated:  mustTime("2026-02-20T00:00:00Z"),
				Resolved: ptrTime(mustTime("2026-03-01T00:00:00Z")),
			},
		},
		PRs: []cache.GitHubPR{
			{Number: 1, Created: mustTime("2026-01-10T00:00:00Z"), Additions: 100, Deletions: 20},
			{
				Number: 2, Created: mustTime("2026-02-15T00:00:00Z"),
				Merged:    ptrTime(mustTime("2026-03-05T00:00:00Z")),
				Additions: 500, Deletions: 200,
			},
		},
		Commits: []cache.GitHubCommit{
			{SHA: "a", Committed: mustTime("2026-01-12T00:00:00Z")},
			{SHA: "b", Committed: mustTime("2026-03-20T00:00:00Z")},
		},
	}

	start := cache.MustParseMonth("2026-01")
	end := cache.MustParseMonth("2026-04")
	rows := rollupMonthly(data, start, end, testCI(), testNorm())

	if got, want := len(rows), 4; got != want {
		t.Fatalf("want %d months, got %d", want, got)
	}
	if rows[0].Month != "2026-01" || rows[3].Month != "2026-04" {
		t.Fatalf("chronological month order broken: %v", rows)
	}

	// 2026-01: 1 issue created (CD-1), 1 PR created, 1 commit, no touches, no merges.
	jan := rows[0]
	if jan.JiraIssuesCreated != 1 || jan.JiraIssuesTouched != 0 {
		t.Errorf("Jan: expected 1 created / 0 touched, got %+v", jan)
	}
	if jan.PRsCreated != 1 || jan.PRsMerged != 0 || jan.LOCAdded != 0 {
		t.Errorf("Jan PRs wrong: %+v", jan)
	}
	if jan.Commits != 1 {
		t.Errorf("Jan commits wrong: %d", jan.Commits)
	}

	// 2026-02: CD-1 touched (Updated in Feb), CD-2 created + touched, PR #2 created, no merges.
	feb := rows[1]
	if feb.JiraIssuesTouched != 2 {
		t.Errorf("Feb touched: expected 2, got %d", feb.JiraIssuesTouched)
	}
	if feb.JiraIssuesCreated != 1 {
		t.Errorf("Feb created: expected 1, got %d", feb.JiraIssuesCreated)
	}
	if feb.PRsCreated != 1 || feb.PRsMerged != 0 {
		t.Errorf("Feb PR counts wrong: %+v", feb)
	}

	// 2026-03: CD-2 resolved, PR #2 merged (500 add / 200 del), 1 commit.
	mar := rows[2]
	if mar.JiraIssuesResolved != 1 {
		t.Errorf("Mar resolved: expected 1, got %d", mar.JiraIssuesResolved)
	}
	if mar.PRsMerged != 1 || mar.LOCAdded != 500 || mar.LOCDeleted != 200 {
		t.Errorf("Mar merged LoC wrong: %+v", mar)
	}
	if mar.Commits != 1 {
		t.Errorf("Mar commits: expected 1, got %d", mar.Commits)
	}

	// 2026-04: zero-filled.
	apr := rows[3]
	if apr.JiraIssuesTouched+apr.PRsCreated+apr.Commits != 0 {
		t.Errorf("Apr should be zero-filled: %+v", apr)
	}
}

func TestRollupWeekly_ActiveWeeksCount(t *testing.T) {
	data := &Loaded{
		PRs: []cache.GitHubPR{
			{Created: mustTime("2026-01-05T00:00:00Z")}, // ISO week 2026-W02
			{Created: mustTime("2026-01-12T00:00:00Z")}, // 2026-W03
			{Created: mustTime("2026-01-13T00:00:00Z")}, // 2026-W03 (same week)
		},
	}
	start := cache.MustParseMonth("2026-01")
	end := cache.MustParseMonth("2026-01")
	weeks := rollupWeekly(data, start, end, testCI(), testNorm())
	active := activeWeeksCount(weeks)
	if active != 2 {
		t.Errorf("expected 2 active ISO weeks (W02, W03), got %d", active)
	}
}

func TestIsoWeeksInRange_CrossYearBoundary(t *testing.T) {
	// 2025-12 and 2026-01 — ISO weeks span the boundary.
	start := cache.MustParseMonth("2025-12")
	end := cache.MustParseMonth("2026-01")
	weeks := isoWeeksInRange(start, end)
	if len(weeks) == 0 {
		t.Fatal("expected weeks, got none")
	}
	// Must be unique + sorted.
	seen := map[string]bool{}
	for i, w := range weeks {
		if seen[w] {
			t.Errorf("duplicate week %s", w)
		}
		seen[w] = true
		if i > 0 && weeks[i-1] >= w {
			t.Errorf("not sorted at %d: %s >= %s", i, weeks[i-1], w)
		}
	}
}
