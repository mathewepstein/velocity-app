package pull

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

func newTestGithubPuller(t *testing.T, baseURL string) *GithubPuller {
	t.Helper()
	orig := githubAPIBase
	githubAPIBase = baseURL
	t.Cleanup(func() { githubAPIBase = orig })
	return NewGithubPuller(config.GitHubConfig{}, "token", 0)
}

func reviewComment(id, inReplyTo int, login, path, body, created string) map[string]interface{} {
	return map[string]interface{}{
		"id":             id,
		"in_reply_to_id": inReplyTo,
		"user":           map[string]interface{}{"login": login},
		"path":           path,
		"body":           body,
		"created_at":     created,
	}
}

func TestFetchReviewComments_MapsAndCountsThreads(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/pulls/7/comments", func(w http.ResponseWriter, r *http.Request) {
		// Thread A (root 1): 1 → 2 → 3  = 3 comments → deep.
		// Thread B (root 10): 10 → 11    = 2 comments → not deep.
		// Standalone 20: 1 comment → not deep.
		writeJSON(t, w, []interface{}{
			reviewComment(1, 0, "alice", "a.go", "root A", "2026-01-01T00:00:00Z"),
			reviewComment(2, 1, "bob", "a.go", "reply A1", "2026-01-01T01:00:00Z"),
			reviewComment(3, 2, "alice", "a.go", "reply A2", "2026-01-01T02:00:00Z"),
			reviewComment(10, 0, "carol", "b.go", "root B", "2026-01-02T00:00:00Z"),
			reviewComment(11, 10, "alice", "b.go", "reply B1", "2026-01-02T01:00:00Z"),
			reviewComment(20, 0, "dave", "c.go", "standalone", "2026-01-03T00:00:00Z"),
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	data, err := newTestGithubPuller(t, srv.URL).FetchReviewComments(context.Background(), "org/repo", 7)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(data.Comments) != 6 {
		t.Fatalf("comments = %d, want 6", len(data.Comments))
	}
	if data.Comments[1].InReplyTo != 1 || data.Comments[1].Author != "bob" {
		t.Fatalf("comment mapping wrong: %+v", data.Comments[1])
	}
	if data.DeepThreads != 1 {
		t.Fatalf("DeepThreads = %d, want 1 (only thread A has 3+ comments)", data.DeepThreads)
	}
}

func TestFetchReviewComments_Paginates(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/pulls/8/comments", func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		switch page {
		case "1":
			// Exactly 100 → triggers a second page fetch.
			items := make([]interface{}, 0, 100)
			for i := 0; i < 100; i++ {
				items = append(items, reviewComment(i+1, 0, "u", "f.go", fmt.Sprintf("c%d", i), "2026-01-01T00:00:00Z"))
			}
			writeJSON(t, w, items)
		case "2":
			writeJSON(t, w, []interface{}{reviewComment(200, 0, "u", "f.go", "last", "2026-01-01T00:00:00Z")})
		default:
			t.Errorf("unexpected page %q", page)
			writeJSON(t, w, []interface{}{})
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	data, err := newTestGithubPuller(t, srv.URL).FetchReviewComments(context.Background(), "org/repo", 8)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(data.Comments) != 101 {
		t.Fatalf("comments = %d, want 101 across two pages", len(data.Comments))
	}
}

func TestFetchReviewComments_NotFoundUnreachable(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/gone/pulls/9/comments", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, err := newTestGithubPuller(t, srv.URL).FetchReviewComments(context.Background(), "org/gone", 9)
	if !errors.Is(err, ErrPRUnreachable) {
		t.Fatalf("err = %v, want ErrPRUnreachable", err)
	}
}

func TestHydrateReviewComments_SetsCountsAndSentinel(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/pulls/11/comments", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, []interface{}{
			reviewComment(1, 0, "a", "x.go", "r", "2026-01-01T00:00:00Z"),
			reviewComment(2, 1, "b", "x.go", "r1", "2026-01-01T01:00:00Z"),
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	pr := &cache.GitHubPR{Repo: "org/repo", Number: 11}
	if err := newTestGithubPuller(t, srv.URL).HydrateReviewComments(context.Background(), pr); err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	if pr.InlineComments != 2 {
		t.Fatalf("InlineComments = %d, want 2", pr.InlineComments)
	}
	if pr.DeepThreads != 0 {
		t.Fatalf("DeepThreads = %d, want 0", pr.DeepThreads)
	}
	if pr.ReviewComments == nil {
		t.Fatal("ReviewComments should be non-nil after hydration")
	}
}

func TestHydrateReviewComments_PermSkipSetsEmptySentinel(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/gone/pulls/12/comments", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	pr := &cache.GitHubPR{Repo: "org/gone", Number: 12}
	err := newTestGithubPuller(t, srv.URL).HydrateReviewComments(context.Background(), pr)
	if !errors.Is(err, ErrPRUnreachable) {
		t.Fatalf("err = %v, want ErrPRUnreachable", err)
	}
	if pr.ReviewComments == nil {
		t.Fatal("perm-skip must persist an empty non-nil slice so it isn't retried forever")
	}
}
