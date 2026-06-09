package pull

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

func newTestJiraPuller(t *testing.T, baseURL string) *JiraPuller {
	t.Helper()
	return NewJiraPuller(config.JiraConfig{BaseURL: baseURL, Email: "tester@example.com"}, "token", 0, 0)
}

// adfDoc builds a minimal ADF document with one paragraph of the given text.
func adfDoc(text string) map[string]interface{} {
	return map[string]interface{}{
		"type":    "doc",
		"version": 1,
		"content": []interface{}{
			map[string]interface{}{
				"type":    "paragraph",
				"content": []interface{}{map[string]interface{}{"type": "text", "text": text}},
			},
		},
	}
}

func TestHydrateIssue_BasicNoPagination(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/3/issue/CD-1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{
			"key":   "CD-1",
			"names": map[string]interface{}{"description": "Description", "comment": "Comment"},
			"fields": map[string]interface{}{
				"description": adfDoc("See https://confluence.example.com/x for scope."),
				"comment": map[string]interface{}{
					"comments": []interface{}{
						map[string]interface{}{
							"author":  map[string]interface{}{"accountId": "acct-a"},
							"created": "2026-01-02T10:00:00.000+0000",
							"body":    adfDoc("first comment"),
						},
					},
					"startAt": 0, "maxResults": 100, "total": 1,
				},
				"issuelinks": []interface{}{
					map[string]interface{}{
						"type":         map[string]interface{}{"name": "Blocks", "inward": "is blocked by", "outward": "blocks"},
						"outwardIssue": map[string]interface{}{"key": "CD-9"},
					},
				},
			},
			"changelog": map[string]interface{}{
				"startAt": 0, "maxResults": 100, "total": 1,
				"histories": []interface{}{
					map[string]interface{}{
						"created": "2026-01-03T09:00:00.000+0000",
						"author":  map[string]interface{}{"accountId": "acct-a"},
						"items": []interface{}{
							map[string]interface{}{"field": "status", "fromString": "To Do", "toString": "In Progress"},
							map[string]interface{}{"field": "assignee", "fromString": "", "toString": "x"},
						},
					},
				},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	iss := &cache.JiraIssue{Key: "CD-1"}
	if err := newTestJiraPuller(t, srv.URL).HydrateIssue(context.Background(), iss, time.Unix(1_700_000_000, 0)); err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	if iss.Description != "See https://confluence.example.com/x for scope." {
		t.Fatalf("description = %q", iss.Description)
	}
	if len(iss.Comments) != 1 || iss.Comments[0].Body != "first comment" || iss.Comments[0].Author != "acct-a" {
		t.Fatalf("comments = %+v", iss.Comments)
	}
	// Only the status item is kept; the assignee change is dropped.
	if len(iss.Changelog) != 1 || iss.Changelog[0].To != "In Progress" || iss.Changelog[0].Field != "status" {
		t.Fatalf("changelog = %+v", iss.Changelog)
	}
	// Relationships captured from the same comprehensive pull.
	if len(iss.Links) != 1 || iss.Links[0].Key != "CD-9" || iss.Links[0].LinkType != "Blocks" {
		t.Fatalf("links = %+v", iss.Links)
	}
	// Both resume sentinels set, and the raw catch-all populated.
	if !iss.DetailFetched || iss.RawFields == nil {
		t.Fatalf("expected DetailFetched + non-nil RawFields, got fetched=%v raw=%v", iss.DetailFetched, iss.RawFields)
	}
}

func TestHydrateIssue_PagesChangelogAndComments(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/3/issue/CD-2", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{
			"key": "CD-2",
			"fields": map[string]interface{}{
				"comment": map[string]interface{}{
					"comments":   []interface{}{comment("acct-a", "2026-01-01T00:00:00.000+0000", "c1")},
					"startAt":    0,
					"maxResults": 1,
					"total":      2,
				},
			},
			"changelog": map[string]interface{}{
				"startAt": 0, "maxResults": 1, "total": 2,
				"histories": []interface{}{statusHistory("2026-01-02T00:00:00.000+0000", "To Do", "In Progress")},
			},
		})
	})
	mux.HandleFunc("/rest/api/3/issue/CD-2/comment", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("startAt") != "1" {
			t.Errorf("comment page startAt = %q, want 1", r.URL.Query().Get("startAt"))
		}
		writeJSON(t, w, map[string]interface{}{
			"comments":   []interface{}{comment("acct-b", "2026-01-03T00:00:00.000+0000", "c2")},
			"startAt":    1,
			"maxResults": 1,
			"total":      2,
		})
	})
	mux.HandleFunc("/rest/api/3/issue/CD-2/changelog", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("startAt") != "1" {
			t.Errorf("changelog page startAt = %q, want 1", r.URL.Query().Get("startAt"))
		}
		writeJSON(t, w, map[string]interface{}{
			"values":     []interface{}{statusHistory("2026-01-05T00:00:00.000+0000", "In Progress", "Done")},
			"startAt":    1,
			"maxResults": 1,
			"total":      2,
			"isLast":     true,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	iss := &cache.JiraIssue{Key: "CD-2"}
	if err := newTestJiraPuller(t, srv.URL).HydrateIssue(context.Background(), iss, time.Unix(1_700_000_000, 0)); err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	if len(iss.Comments) != 2 {
		t.Fatalf("want 2 comments after paging, got %d", len(iss.Comments))
	}
	if len(iss.Changelog) != 2 {
		t.Fatalf("want 2 status transitions after paging, got %d", len(iss.Changelog))
	}
}

func TestHydrateIssue_PermSkipFreezesSentinels(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/3/issue/CD-404", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	iss := &cache.JiraIssue{Key: "CD-404"}
	err := newTestJiraPuller(t, srv.URL).HydrateIssue(context.Background(), iss, time.Unix(1_700_000_000, 0))
	if !errors.Is(err, ErrIssueUnreachable) {
		t.Fatalf("err = %v, want ErrIssueUnreachable", err)
	}
	if !iss.DetailFetched || iss.DetailFetchedAt == nil {
		t.Fatal("perm-skip should still mark DetailFetched so it isn't retried forever")
	}
	if iss.Changelog == nil || iss.Comments == nil || iss.Links == nil || iss.Attachments == nil || iss.RawFields == nil {
		t.Fatal("perm-skip should freeze all sentinels non-nil so the issue isn't re-selected")
	}
}

func TestDeriveIssueSignals(t *testing.T) {
	tp := func(s string) time.Time { return parseJiraTime(s) }
	resolved := tp("2026-01-10T00:00:00.000+0000")
	iss := &cache.JiraIssue{
		Resolved: &resolved,
		Changelog: []cache.StatusTransition{
			// Deliberately out of order to exercise the sort.
			{At: tp("2026-01-05T00:00:00.000+0000"), From: "In Progress", To: "To Do"}, // backward = flip
			{At: tp("2026-01-02T00:00:00.000+0000"), From: "To Do", To: "In Progress"}, // first move
			{At: tp("2026-01-06T00:00:00.000+0000"), From: "To Do", To: "In Progress"}, // re-enter = flip
		},
		Comments: []cache.IssueComment{
			{Created: tp("2026-01-01T00:00:00.000+0000")}, // before first move → pre-code
			{Created: tp("2026-01-02T00:00:00.000+0000")}, // exactly at first move → pre-code (inclusive)
			{Created: tp("2026-01-04T00:00:00.000+0000")}, // after → not pre-code
		},
	}
	DeriveIssueSignals(iss)

	if iss.FirstInProgress == nil || !iss.FirstInProgress.Equal(tp("2026-01-02T00:00:00.000+0000")) {
		t.Fatalf("FirstInProgress = %v, want 2026-01-02", iss.FirstInProgress)
	}
	if iss.DoneAt == nil || !iss.DoneAt.Equal(resolved) {
		t.Fatalf("DoneAt = %v, want resolutiondate", iss.DoneAt)
	}
	wantCycle := resolved.Sub(tp("2026-01-02T00:00:00.000+0000")).Hours()
	if iss.CycleHours != wantCycle {
		t.Fatalf("CycleHours = %v, want %v", iss.CycleHours, wantCycle)
	}
	// Visited seeds with "To Do" (first From). Transitions: To Do(flip),
	// In Progress(seed? no), In Progress(flip). Re-enters: "To Do" at 01-05
	// (seeded) and "In Progress" at 01-06 (visited at 01-02) → 2 flips.
	if iss.StatusFlips != 2 {
		t.Fatalf("StatusFlips = %d, want 2", iss.StatusFlips)
	}
	if iss.PreCodeComments != 2 {
		t.Fatalf("PreCodeComments = %d, want 2", iss.PreCodeComments)
	}
}

func TestDeriveIssueSignals_OpenIssueUsesLastTransition(t *testing.T) {
	tp := func(s string) time.Time { return parseJiraTime(s) }
	iss := &cache.JiraIssue{
		// no Resolved
		Changelog: []cache.StatusTransition{
			{At: tp("2026-02-01T00:00:00.000+0000"), From: "To Do", To: "In Progress"},
			{At: tp("2026-02-09T00:00:00.000+0000"), From: "In Progress", To: "In Review"},
		},
	}
	DeriveIssueSignals(iss)
	if iss.DoneAt == nil || !iss.DoneAt.Equal(tp("2026-02-09T00:00:00.000+0000")) {
		t.Fatalf("DoneAt = %v, want last transition for an open issue", iss.DoneAt)
	}
	if iss.StatusFlips != 0 {
		t.Fatalf("StatusFlips = %d, want 0 (no re-entries)", iss.StatusFlips)
	}
}

// ---- helpers ----

func comment(accountID, created, body string) map[string]interface{} {
	return map[string]interface{}{
		"author":  map[string]interface{}{"accountId": accountID},
		"created": created,
		"body":    adfDoc(body),
	}
}

func statusHistory(created, from, to string) map[string]interface{} {
	return map[string]interface{}{
		"created": created,
		"author":  map[string]interface{}{"accountId": "acct"},
		"items": []interface{}{
			map[string]interface{}{"field": "status", "fromString": from, "toString": to},
		},
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, v interface{}) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}
