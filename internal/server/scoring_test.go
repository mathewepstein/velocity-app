package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/mathewepstein/velocity/internal/config"
	"github.com/mathewepstein/velocity/internal/scoring"
)

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
	h, err := buildHandler("", false, config.Profile{}, nil, scores)
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}
	return h
}

func TestScoringList_503WithoutStore(t *testing.T) {
	h := handlerWith(t, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/scoring/list", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestScoringList_BadFilter400(t *testing.T) {
	h := handlerWith(t, seededScoreStore(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/scoring/list?filter=bogus", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestScoringList_ReturnsRows(t *testing.T) {
	h := handlerWith(t, seededScoreStore(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/scoring/list", nil))
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
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/scoring/list?filter=needs_insight", nil))
	var rows []scoring.ScoreRecord
	json.Unmarshal(rec.Body.Bytes(), &rows)
	if len(rows) != 1 || rows[0].Ticket != "CD-2" {
		t.Errorf("needs_insight filter = %+v, want only CD-2", rows)
	}
}

func TestScoringTicket_503WithoutCacheStore(t *testing.T) {
	h := handlerWith(t, seededScoreStore(t)) // scores present, but no corpus store
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/scoring/ticket/CD-1", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (no cache store)", rec.Code)
	}
}

func TestScoringGenerate_503WithoutStores(t *testing.T) {
	h := handlerWith(t, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/scoring/generate", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}
