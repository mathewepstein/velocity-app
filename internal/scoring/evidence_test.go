package scoring

import (
	"testing"
	"time"

	"github.com/mathewepstein/velocity/internal/analyze"
	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

func ptr(t time.Time) *time.Time { return &t }

// fixture builds a small corpus: one ticket CD-100 with one merged PR that
// touches a source file, a test file, and a generated lockfile; plus background
// PRs so the hot-file frequency index has signal.
func fixture() *analyze.Loaded {
	created := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	merged := created.Add(72 * time.Hour)
	resolved := merged.Add(time.Hour)

	loaded := &analyze.Loaded{
		Issues: []cache.JiraIssue{
			{
				Key:             "CD-100",
				Summary:         "Fix session storage compliance bug",
				Description:     "Auth middleware writes the session to the wrong store.",
				IssueType:       "Bug",
				Status:          "Done",
				Resolution:      "Done",
				Created:         created,
				Resolved:        ptr(resolved),
				StoryPoints:     5,
				Labels:          []string{"compliance"},
				CycleHours:      71.5,
				StatusFlips:     2,
				PreCodeComments: 3,
				Comments: []cache.IssueComment{
					{Author: "a", Created: created},
					{Author: "b", Created: created.Add(time.Hour)},
				},
				FirstInProgress: ptr(created.Add(time.Hour)),
				DoneAt:          ptr(resolved),
				DetailFetched:   true,
			},
		},
		PRs: []cache.GitHubPR{
			{
				Number:    11,
				Repo:      "org/app",
				Title:     "CD-100 fix session store",
				Author:    "dev1",
				State:     "merged",
				Created:   created.Add(time.Hour),
				Merged:    ptr(merged),
				IssueKeys: []string{"CD-100"},
				FileChanges: []cache.FileChange{
					{Path: "src/auth/middleware.go", Status: "modified", Additions: 30, Deletions: 10},
					{Path: "src/auth/middleware_test.go", Status: "modified", Additions: 40, Deletions: 5},
					{Path: "yarn.lock", Status: "modified", Additions: 500, Deletions: 200},
				},
				Additions:      570,
				Deletions:      215,
				InlineComments: 4,
				DeepThreads:    1,
			},
			// Background PRs touching the auth middleware so it reads as hot.
			bgPR(20, "org/app", "src/auth/middleware.go"),
			bgPR(21, "org/app", "src/auth/middleware.go"),
			bgPR(22, "org/app", "src/auth/middleware.go"),
			bgPR(23, "org/app", "src/auth/middleware.go"),
			bgPR(24, "org/app", "src/auth/middleware.go"),
			bgPR(25, "org/app", "src/other/util.go"),
		},
		Reviews: []cache.GitHubReview{
			{PRNumber: 11, Repo: "org/app", Reviewer: "rev1", State: "CHANGES_REQUESTED", Submitted: created.Add(24 * time.Hour)},
			{PRNumber: 11, Repo: "org/app", Reviewer: "rev1", State: "APPROVED", Submitted: created.Add(48 * time.Hour)},
			{PRNumber: 11, Repo: "org/app", Reviewer: "rev2", State: "APPROVED", Submitted: created.Add(49 * time.Hour)},
		},
	}
	return loaded
}

func bgPR(n int, repo, file string) cache.GitHubPR {
	return cache.GitHubPR{
		Number:      n,
		Repo:        repo,
		State:       "merged",
		Files:       []string{file},
		FileChanges: []cache.FileChange{{Path: file, Status: "modified", Additions: 1, Deletions: 1}},
	}
}

func defaultNorm() config.NormalizeConfig {
	return config.DefaultScoringConfig().Normalize
}

func TestExtract_NotFound(t *testing.T) {
	ext := NewExtractor(fixture(), defaultNorm(), 5*time.Minute)
	if _, ok := ext.Extract("CD-999"); ok {
		t.Fatal("expected CD-999 to be absent")
	}
}

func TestExtract_JiraAndDerivedSignals(t *testing.T) {
	ext := NewExtractor(fixture(), defaultNorm(), 5*time.Minute)
	ev, ok := ext.Extract("cd-100") // case-insensitive
	if !ok {
		t.Fatal("CD-100 not found")
	}
	if ev.Key != "CD-100" || ev.Summary == "" {
		t.Fatalf("jira fields not copied: %+v", ev.Key)
	}
	if ev.ExistingStoryPoints != 5 {
		t.Errorf("ExistingStoryPoints = %v, want 5", ev.ExistingStoryPoints)
	}
	if ev.CycleHours != 71.5 || ev.StatusFlips != 2 || ev.PreCodeComments != 3 {
		t.Errorf("derived signals wrong: cycle=%v flips=%v pre=%v", ev.CycleHours, ev.StatusFlips, ev.PreCodeComments)
	}
	if ev.TotalComments != 2 {
		t.Errorf("TotalComments = %d, want 2", ev.TotalComments)
	}
}

func TestExtract_PRRollups(t *testing.T) {
	ext := NewExtractor(fixture(), defaultNorm(), 5*time.Minute)
	ev, _ := ext.Extract("CD-100")

	if len(ev.PRs) != 1 {
		t.Fatalf("matched %d PRs, want 1", len(ev.PRs))
	}
	// RawLOC includes the lockfile; NetLOC excludes it.
	if ev.RawLOC != 785 {
		t.Errorf("RawLOC = %d, want 785", ev.RawLOC)
	}
	if ev.NetLOC != 85 { // 30+10 + 40+5, yarn.lock excluded
		t.Errorf("NetLOC = %d, want 85 (lockfile excluded)", ev.NetLOC)
	}
	// FileCount = non-generated distinct paths (2: middleware.go + _test.go).
	if ev.FileCount != 2 {
		t.Errorf("FileCount = %d, want 2", ev.FileCount)
	}
	if ev.TestFilesTouched != 1 {
		t.Errorf("TestFilesTouched = %d, want 1", ev.TestFilesTouched)
	}
	if ev.ReviewRounds != 1 {
		t.Errorf("ReviewRounds = %d, want 1 (one CHANGES_REQUESTED)", ev.ReviewRounds)
	}
	if ev.DistinctReviewers != 2 {
		t.Errorf("DistinctReviewers = %d, want 2", ev.DistinctReviewers)
	}
	if ev.InlineComments != 4 || ev.DeepThreads != 1 {
		t.Errorf("inline=%d deep=%d, want 4/1", ev.InlineComments, ev.DeepThreads)
	}
	if ev.TimeToMergeHours != 71 { // PR opened at created+1h, merged at created+72h
		t.Errorf("TimeToMergeHours = %v, want 71", ev.TimeToMergeHours)
	}
}

func TestExtract_TouchedAreaRisk(t *testing.T) {
	ext := NewExtractor(fixture(), defaultNorm(), 5*time.Minute)
	ev, _ := ext.Extract("CD-100")
	// middleware.go is touched by 6 PRs total (>= floor 5) → hot.
	found := false
	for _, f := range ev.HotFiles {
		if f == "src/auth/middleware.go" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected middleware.go in HotFiles, got %v", ev.HotFiles)
	}
	if ev.TouchedAreaRisk != "medium" { // exactly 1 hot file
		t.Errorf("TouchedAreaRisk = %q, want medium", ev.TouchedAreaRisk)
	}
}

func TestExtract_NoPRsStillReturns(t *testing.T) {
	data := fixture()
	data.Issues = append(data.Issues, cache.JiraIssue{
		Key:     "CD-200",
		Summary: "Doc-only ticket",
		Created: time.Now(),
	})
	ext := NewExtractor(data, defaultNorm(), 5*time.Minute)
	ev, ok := ext.Extract("CD-200")
	if !ok {
		t.Fatal("CD-200 should be returned (Jira-only evidence)")
	}
	if len(ev.PRs) != 0 || ev.NetLOC != 0 || ev.TouchedAreaRisk != "low" {
		t.Errorf("expected empty PR rollups for Jira-only ticket: %+v", ev)
	}
}
