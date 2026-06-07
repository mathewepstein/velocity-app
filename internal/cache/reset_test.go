package cache

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResetWipesEverySourceAndManifest(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)

	month := MustParseMonth("2026-01")

	if err := WriteMonth(SourceJira, "CD", month, []JiraIssue{{Key: "CD-1"}}); err != nil {
		t.Fatalf("write jira: %v", err)
	}
	if err := WriteMonth(SourceGitHubPRs, "consumerdirect", month, []GitHubPR{{Number: 1, Repo: "consumerdirect/x"}}); err != nil {
		t.Fatalf("write prs: %v", err)
	}
	if err := WriteMonth(SourceGitHubReviews, "consumerdirect", month, []GitHubReview{{PRNumber: 1, Repo: "consumerdirect/x", Reviewer: "alice"}}); err != nil {
		t.Fatalf("write reviews: %v", err)
	}

	man := NewManifest()
	man.Update(SourceJira, "CD", month, 1, time.Now())
	if err := man.Save(); err != nil {
		t.Fatalf("save manifest: %v", err)
	}

	// A neighbor file that must survive — simulates a future ratings.json.
	root, _ := Root()
	keeper := filepath.Join(root, "ratings.json")
	if err := os.WriteFile(keeper, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write keeper: %v", err)
	}

	removed, err := Reset(io.Discard)
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if len(removed) == 0 {
		t.Fatalf("Reset reported no removals")
	}

	for _, s := range []Source{SourceJira, SourceGitHubPRs, SourceGitHubReviews} {
		dir, _ := SourceDir(s)
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s dir still present after reset: %v", s, err)
		}
	}
	mp, _ := ManifestPath()
	if _, err := os.Stat(mp); !os.IsNotExist(err) {
		t.Errorf("manifest still present after reset")
	}
	if _, err := os.Stat(keeper); err != nil {
		t.Errorf("ratings.json was wiped (must survive reset): %v", err)
	}
}

func TestResetIdempotentOnEmptyCache(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)

	if _, err := Reset(io.Discard); err != nil {
		t.Errorf("Reset on empty cache should not error: %v", err)
	}
}
