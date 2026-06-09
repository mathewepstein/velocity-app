package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mathewepstein/velocity/internal/config"
	"github.com/mathewepstein/velocity/internal/scoring"
)

// stubPoster is a no-op scoring.JiraPoster for endpoint tests. scopeErr drives
// the jira-status response; SetStoryPoints/AddComment record nothing because the
// post-orchestration logic itself is covered by scoring's own tests.
type stubPoster struct{ scopeErr error }

func (s stubPoster) SetStoryPoints(context.Context, string, float64) error { return nil }
func (s stubPoster) AddComment(context.Context, string, []string) error    { return nil }
func (s stubPoster) VerifyWriteScope(context.Context) error                { return s.scopeErr }

func handlerWithPoster(t *testing.T, scores *scoring.ScoreStore, poster scoring.JiraPoster) http.Handler {
	t.Helper()
	h, err := buildHandler("", false, config.Profile{}, nil, scores, poster)
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}
	return h
}

func seededScoreStore(t *testing.T) *scoring.ScoreStore {
	t.Helper()
	ss, err := scoring.OpenScoreStore(filepath.Join(t.TempDir(), "velocity.db"))
	if err != nil {
		t.Fatalf("open score store: %v", err)
	}
	t.Cleanup(func() { ss.Close() })
	clean := scoring.ScoreRecord{Ticket: "CD-1", Points: 2, Source: scoring.SourceAuto, AutoPoints: 2, Band: "2", Confidence: "high", EvidenceHash: "h1", ScoredAt: time.Unix(1, 0)}
	flagged := scoring.ScoreRecord{Ticket: "CD-2", Points: 5, Source: scoring.SourceAuto, AutoPoints: 5, Band: "3–5", Confidence: "low", NeedsInsight: true, EvidenceHash: "h2", ScoredAt: time.Unix(2, 0)}
	if _, err := ss.SaveAuto(clean); err != nil {
		t.Fatal(err)
	}
	if _, err := ss.SaveAuto(flagged); err != nil {
		t.Fatal(err)
	}
	return ss
}

func handlerWith(t *testing.T, scores *scoring.ScoreStore) http.Handler {
	t.Helper()
	h, err := buildHandler("", false, config.Profile{}, nil, scores, nil)
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}
	return h
}

func TestScoringList_503WithoutStore(t *testing.T) {
	h := handlerWith(t, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest(http.MethodGet, "/api/scoring/list", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestScoringList_BadFilter400(t *testing.T) {
	h := handlerWith(t, seededScoreStore(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest(http.MethodGet, "/api/scoring/list?filter=bogus", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestScoringList_ReturnsRows(t *testing.T) {
	h := handlerWith(t, seededScoreStore(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest(http.MethodGet, "/api/scoring/list", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var rows []scoring.ScoreRecord
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// needs-insight sorts first.
	if rows[0].Ticket != "CD-2" {
		t.Errorf("first row = %s, want CD-2 (needs-insight first)", rows[0].Ticket)
	}
}

func TestScoringList_NeedsInsightFilter(t *testing.T) {
	h := handlerWith(t, seededScoreStore(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest(http.MethodGet, "/api/scoring/list?filter=needs_insight", nil))
	var rows []scoring.ScoreRecord
	json.Unmarshal(rec.Body.Bytes(), &rows)
	if len(rows) != 1 || rows[0].Ticket != "CD-2" {
		t.Errorf("needs_insight filter = %+v, want only CD-2", rows)
	}
}

func TestScoringTicket_503WithoutCacheStore(t *testing.T) {
	h := handlerWith(t, seededScoreStore(t)) // scores present, but no corpus store
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest(http.MethodGet, "/api/scoring/ticket/CD-1", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (no cache store)", rec.Code)
	}
}

func TestScoringGenerate_503WithoutStores(t *testing.T) {
	h := handlerWith(t, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest(http.MethodPost, "/api/scoring/generate", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestScoringPost_503WithoutStore(t *testing.T) {
	h := handlerWith(t, nil)
	rec := httptest.NewRecorder()
	body := strings.NewReader(`{"tickets":["CD-1"]}`)
	h.ServeHTTP(rec, loopbackRequest(http.MethodPost, "/api/scoring/post", body))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestScoringPost_400WithoutTickets(t *testing.T) {
	h := handlerWith(t, seededScoreStore(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest(http.MethodPost, "/api/scoring/post", strings.NewReader(`{}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestScoringPost_DryRunByDefault(t *testing.T) {
	// No poster supplied: an omitted dry_run must default to preview (a safe
	// no-write), so the request succeeds even with no Jira token configured.
	h := handlerWith(t, seededScoreStore(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest(http.MethodPost, "/api/scoring/post", strings.NewReader(`{"tickets":["CD-1"]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var rep scoring.PostReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !rep.DryRun || rep.Previewed != 1 || rep.Posted != 0 {
		t.Errorf("want a dry-run preview, got %+v", rep)
	}
}

func TestScoringPost_LiveWithoutPoster503(t *testing.T) {
	h := handlerWith(t, seededScoreStore(t)) // no poster
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest(http.MethodPost, "/api/scoring/post", strings.NewReader(`{"tickets":["CD-1"],"dry_run":false}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (live post, no token)", rec.Code)
	}
}

func TestScoringJiraStatus_NotConfigured(t *testing.T) {
	h := handlerWith(t, seededScoreStore(t)) // no poster
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest(http.MethodGet, "/api/scoring/jira-status", nil))
	var st struct {
		Configured bool `json:"configured"`
		CanWrite   bool `json:"can_write"`
	}
	json.Unmarshal(rec.Body.Bytes(), &st)
	if st.Configured || st.CanWrite {
		t.Errorf("no poster should report not-configured, got %+v", st)
	}
}

func TestScoringJiraStatus_CanWrite(t *testing.T) {
	h := handlerWithPoster(t, seededScoreStore(t), stubPoster{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest(http.MethodGet, "/api/scoring/jira-status", nil))
	var st struct {
		Configured bool `json:"configured"`
		CanWrite   bool `json:"can_write"`
	}
	json.Unmarshal(rec.Body.Bytes(), &st)
	if !st.Configured || !st.CanWrite {
		t.Errorf("scope-ok poster should report can_write, got %+v", st)
	}
}

func TestScoringJiraStatus_ScopeError(t *testing.T) {
	h := handlerWithPoster(t, seededScoreStore(t), stubPoster{scopeErr: errors.New("missing EDIT_ISSUES")})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest(http.MethodGet, "/api/scoring/jira-status", nil))
	var st struct {
		Configured bool   `json:"configured"`
		CanWrite   bool   `json:"can_write"`
		Detail     string `json:"detail"`
	}
	json.Unmarshal(rec.Body.Bytes(), &st)
	if !st.Configured || st.CanWrite || !strings.Contains(st.Detail, "EDIT_ISSUES") {
		t.Errorf("scope error should report configured-but-can't-write with detail, got %+v", st)
	}
}
