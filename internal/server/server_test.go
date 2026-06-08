package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mathewepstein/velocity/internal/analyze"
	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

func TestHandler_HealthzIsOK(t *testing.T) {
	h, err := buildHandler("", false, config.Profile{}, nil, nil)
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status: %d", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("body: %q", rec.Body.String())
	}
}

func TestHandler_RootRendersLeaderboard(t *testing.T) {
	h, err := buildHandler("", false, config.Profile{}, nil, nil)
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<title>Velocity — Leaderboard</title>") {
		t.Errorf("/ did not render the leaderboard template; body starts with: %q", body[:min(200, len(body))])
	}
	if !strings.Contains(body, "/leaderboard.js") {
		t.Errorf("/ missing leaderboard.js reference")
	}
	if !strings.Contains(body, `aria-current="page"`) {
		t.Errorf("/ missing aria-current on nav (Leaderboard should be active)")
	}
}

func TestHandler_DevPageRendered(t *testing.T) {
	h, err := buildHandler("", false, config.Profile{}, nil, nil)
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/dev/octocat", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/dev.js") {
		t.Errorf("/dev/{login} missing dev.js reference")
	}
}

func TestHandler_ContributorsRendered(t *testing.T) {
	h, err := buildHandler("", false, config.Profile{}, nil, nil)
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/contributors", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "/contributors.js") {
		t.Errorf("/contributors missing contributors.js reference")
	}
}

func TestHandler_MeLinkUsesSelfLogin(t *testing.T) {
	h, err := buildHandler("alice", false, config.Profile{}, nil, nil)
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, `href="/dev/alice"`) {
		t.Errorf(`expected nav "Me" link href="/dev/alice"; body did not contain it`)
	}
}

func TestHandler_ServesEmbeddedJS(t *testing.T) {
	h, err := buildHandler("", false, config.Profile{}, nil, nil)
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/dev.js", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "javascript") && !strings.Contains(ct, "text/") {
		t.Errorf("Content-Type: %s", ct)
	}
}

func TestHandler_MetricsHeadersNoCache(t *testing.T) {
	h, err := buildHandler("", false, config.Profile{}, nil, nil)
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/metrics.json", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control: %q", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type: %q", got)
	}
}

func TestHandler_APIContributorsUnavailableWithoutStore(t *testing.T) {
	h, err := buildHandler("", false, config.Profile{}, nil, nil) // nil store
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/contributors?from=2026-01&to=2026-03", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 without a store, got %d", rec.Code)
	}
}

func TestHandler_APIContributorsRequiresWindow(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	profile := config.Profile{
		GitHub: config.GitHubConfig{Orgs: []string{"consumerdirect"}},
		Window: config.WindowConfig{BackfillStart: "2026-01", DefaultLengthMonths: 4},
	}
	h, err := buildHandler("", false, profile, cache.JSONStore{}, nil)
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}
	for _, q := range []string{"", "?from=2026-01", "?to=2026-03", "?from=bad&to=2026-03"} {
		req := httptest.NewRequest(http.MethodGet, "/api/contributors"+q, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("query %q: want 400, got %d", q, rec.Code)
		}
	}
}

func TestHandler_APIContributorsRejectsBadSort(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	profile := config.Profile{
		GitHub: config.GitHubConfig{Orgs: []string{"consumerdirect"}},
		Window: config.WindowConfig{BackfillStart: "2026-01", DefaultLengthMonths: 4},
	}
	h, err := buildHandler("", false, profile, cache.JSONStore{}, nil)
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/contributors?from=2026-01&to=2026-03&sort=bogus", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for unknown sort, got %d", rec.Code)
	}
}

func TestSortContributors(t *testing.T) {
	mk := func(login string, score, elo *float64) analyze.DevWindowMetrics {
		d := analyze.DevWindowMetrics{Dev: config.DevIdentity{GitHubLogins: []string{login}}}
		if score != nil {
			d.Score = &analyze.ContributorScore{Total: *score}
		}
		if elo != nil {
			d.Rating = &analyze.EloRating{Current: *elo}
		}
		return d
	}
	f := func(v float64) *float64 { return &v }

	// a: high score / low elo; b: low score / high elo; c: no score, mid elo.
	a := mk("a", f(10), f(900))
	b := mk("b", f(2), f(1200))
	c := mk("c", nil, f(1000))

	byScore := []analyze.DevWindowMetrics{b, c, a}
	sortContributors(byScore, "score")
	if got := []string{login0(byScore[0]), login0(byScore[1]), login0(byScore[2])}; got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("sort=score order = %v, want [a b c] (unscored last)", got)
	}

	byElo := []analyze.DevWindowMetrics{a, c, b}
	sortContributors(byElo, "elo")
	if got := []string{login0(byElo[0]), login0(byElo[1]), login0(byElo[2])}; got[0] != "b" || got[1] != "c" || got[2] != "a" {
		t.Errorf("sort=elo order = %v, want [b c a]", got)
	}
}

func login0(d analyze.DevWindowMetrics) string {
	if ls := d.Dev.AllGitHubLogins(); len(ls) > 0 {
		return ls[0]
	}
	return ""
}

func TestHandler_APIDevUnavailableWithoutStore(t *testing.T) {
	h, err := buildHandler("", false, config.Profile{}, nil, nil) // nil store
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/dev/octocat?from=2026-01&to=2026-03", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 without a store, got %d", rec.Code)
	}
}

func TestHandler_APIDevRequiresWindow(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	profile := config.Profile{
		GitHub: config.GitHubConfig{Orgs: []string{"consumerdirect"}},
		Window: config.WindowConfig{BackfillStart: "2026-01", DefaultLengthMonths: 4},
	}
	h, err := buildHandler("", false, profile, cache.JSONStore{}, nil)
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}
	for _, q := range []string{"", "?from=2026-01", "?to=2026-03", "?from=bad&to=2026-03"} {
		req := httptest.NewRequest(http.MethodGet, "/api/dev/octocat"+q, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("query %q: want 400, got %d", q, rec.Code)
		}
	}
}

func TestHandler_APIDevNotFoundForUnknownLogin(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	profile := config.Profile{
		GitHub: config.GitHubConfig{Orgs: []string{"consumerdirect"}},
		Window: config.WindowConfig{BackfillStart: "2026-01", DefaultLengthMonths: 4},
	}
	h, err := buildHandler("", false, profile, cache.JSONStore{}, nil)
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}
	// Empty cache → empty cohort → no dev matches → 404 (not 500).
	req := httptest.NewRequest(http.MethodGet, "/api/dev/nobody?from=2026-01&to=2026-03", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 for unknown login, got %d", rec.Code)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
