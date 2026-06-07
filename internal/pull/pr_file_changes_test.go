package pull

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mathewepstein/velocity/internal/cache"
)

func prFile(name, status string, adds, dels int) map[string]interface{} {
	return map[string]interface{}{
		"filename":  name,
		"status":    status,
		"additions": adds,
		"deletions": dels,
	}
}

func TestFetchPRFileChanges_KeepsStatusAndLOC(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/pulls/3/files", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, []interface{}{
			prFile("new.go", "added", 40, 0),
			prFile("edit.go", "modified", 5, 3),
			prFile("gone.go", "removed", 0, 12),
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	changes, err := newTestGithubPuller(t, srv.URL).FetchPRFileChanges(context.Background(), "org/repo", 3)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(changes) != 3 {
		t.Fatalf("changes = %d, want 3", len(changes))
	}
	if changes[0] != (cache.FileChange{Path: "new.go", Status: "added", Additions: 40, Deletions: 0}) {
		t.Fatalf("change[0] = %+v", changes[0])
	}
	if changes[2].Status != "removed" || changes[2].Deletions != 12 {
		t.Fatalf("change[2] = %+v", changes[2])
	}
}

func TestHydratePRFileChanges_SetsSentinelOnPerm(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/gone/pulls/4/files", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	pr := &cache.GitHubPR{Repo: "org/gone", Number: 4}
	err := newTestGithubPuller(t, srv.URL).HydratePRFileChanges(context.Background(), pr)
	if !errors.Is(err, ErrPRUnreachable) {
		t.Fatalf("err = %v, want ErrPRUnreachable", err)
	}
	if pr.FileChanges == nil {
		t.Fatal("perm-skip must persist an empty non-nil slice so it isn't retried forever")
	}
}

func TestHydratePRFileChanges_HappyPath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/pulls/5/files", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, []interface{}{prFile("a.go", "modified", 1, 1)})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	pr := &cache.GitHubPR{Repo: "org/repo", Number: 5}
	if err := newTestGithubPuller(t, srv.URL).HydratePRFileChanges(context.Background(), pr); err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	if len(pr.FileChanges) != 1 || pr.FileChanges[0].Path != "a.go" {
		t.Fatalf("FileChanges = %+v", pr.FileChanges)
	}
}
