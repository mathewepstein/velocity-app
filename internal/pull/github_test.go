package pull

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

// TestPullPRsHydratesFilesInline locks in the contract that `velocity refresh`
// now populates GitHubPR.Files for every merged PR it pulls, so the backfill
// script never has to re-touch newly-pulled months. Three PRs cover the three
// branches in pullPRs:
//
//   - merged, files endpoint returns paths → Files = those paths
//   - open / unmerged → Files stays nil (not wasted budget)
//   - merged, files endpoint returns 404 → Files = []string{} so the
//     backfill script's "Files == nil means unfetched" invariant marks this
//     PR done, not retry-forever.
func TestPullPRsHydratesFilesInline(t *testing.T) {
	mergedAt := time.Date(2024, 3, 5, 10, 0, 0, 0, time.UTC)
	closedAt := time.Date(2024, 3, 6, 9, 0, 0, 0, time.UTC)

	mux := http.NewServeMux()

	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total_count": 3,
			"items": []map[string]any{
				{
					"number":         100,
					"title":          "Merged PR with files",
					"body":           "",
					"repository_url": "https://api.github.com/repos/test-org/repo-a",
					"state":          "closed",
					"created_at":     "2024-03-04T12:00:00Z",
					"closed_at":      closedAt,
					"user":           map[string]string{"login": "alice"},
				},
				{
					"number":         200,
					"title":          "Open PR",
					"body":           "",
					"repository_url": "https://api.github.com/repos/test-org/repo-b",
					"state":          "open",
					"created_at":     "2024-03-10T12:00:00Z",
					"user":           map[string]string{"login": "bob"},
				},
				{
					"number":         300,
					"title":          "Merged PR whose files endpoint 404s",
					"body":           "",
					"repository_url": "https://api.github.com/repos/test-org/repo-c",
					"state":          "closed",
					"created_at":     "2024-03-15T12:00:00Z",
					"user":           map[string]string{"login": "carol"},
				},
			},
		})
	})

	mux.HandleFunc("/repos/test-org/repo-a/pulls/100", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"state":     "closed",
			"merged":    true,
			"merged_at": mergedAt,
			"additions": 50,
			"deletions": 10,
			"head":      map[string]string{"ref": "feature/a"},
		})
	})
	mux.HandleFunc("/repos/test-org/repo-a/pulls/100/files", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"filename": "src/foo.go"},
			{"filename": "src/bar.go"},
		})
	})

	mux.HandleFunc("/repos/test-org/repo-b/pulls/200", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"state":     "open",
			"merged":    false,
			"additions": 5,
			"deletions": 0,
			"head":      map[string]string{"ref": "feature/b"},
		})
	})
	mux.HandleFunc("/repos/test-org/repo-b/pulls/200/files", func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("FetchPRFiles must not be called for open/unmerged PRs (saw request for #200)")
	})

	mux.HandleFunc("/repos/test-org/repo-c/pulls/300", func(w http.ResponseWriter, r *http.Request) {
		merged := mergedAt.Add(48 * time.Hour)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"state":     "closed",
			"merged":    true,
			"merged_at": merged,
			"additions": 1,
			"deletions": 1,
			"head":      map[string]string{"ref": "feature/c"},
		})
	})
	mux.HandleFunc("/repos/test-org/repo-c/pulls/300/files", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Not Found", http.StatusNotFound)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	orig := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = orig })

	p := NewGithubPuller(config.GitHubConfig{}, "test-token", 0)
	prs, err := p.pullPRs(context.Background(), "test-org", cache.MustParseMonth("2024-03"))
	if err != nil {
		t.Fatalf("pullPRs returned error: %v", err)
	}
	if len(prs) != 3 {
		t.Fatalf("expected 3 PRs, got %d", len(prs))
	}

	byNumber := map[int]cache.GitHubPR{}
	for _, pr := range prs {
		byNumber[pr.Number] = pr
	}

	a, ok := byNumber[100]
	if !ok {
		t.Fatalf("PR #100 missing from results")
	}
	if a.Files == nil {
		t.Errorf("PR #100: Files = nil, want populated slice")
	}
	if got := a.Files; len(got) != 2 || got[0] != "src/foo.go" || got[1] != "src/bar.go" {
		t.Errorf("PR #100: Files = %v, want [src/foo.go src/bar.go]", got)
	}

	b, ok := byNumber[200]
	if !ok {
		t.Fatalf("PR #200 missing from results")
	}
	if b.Files != nil {
		t.Errorf("PR #200 (unmerged): Files = %v, want nil — unmerged PRs must not be fetched", b.Files)
	}

	c, ok := byNumber[300]
	if !ok {
		t.Fatalf("PR #300 missing from results")
	}
	if c.Files == nil {
		t.Errorf("PR #300 (404 on files): Files = nil, want empty non-nil slice so backfill skips it")
	}
	if len(c.Files) != 0 {
		t.Errorf("PR #300 (404 on files): Files = %v, want empty slice", c.Files)
	}
}
