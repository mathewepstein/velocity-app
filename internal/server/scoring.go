package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
	"github.com/mathewepstein/velocity/internal/scoring"
)

// ticketDetail is the /api/scoring/ticket/{key} response: the evidence bundle,
// the deterministic band, the persisted score row (if any), and a copy-ready
// starting point for the human/LLM insight pass (plan S5).
type ticketDetail struct {
	Evidence           *scoring.TicketEvidence `json:"evidence"`
	Band               scoring.BandResult      `json:"band"`
	Persisted          *scoring.ScoreRecord    `json:"persisted,omitempty"`
	ScoreTicketCommand string                  `json:"score_ticket_command"`
	// JiraURL is the deep link to the ticket, built from the configured Jira
	// base URL (omitted when no base URL is set, so the frontend renders no
	// broken link). Instance-agnostic — never hardcoded.
	JiraURL string `json:"jira_url,omitempty"`
}

// jiraBrowseURL builds the standard Jira deep link ({base}/browse/{KEY}) from
// the instance's configured base URL. Returns "" when either part is missing so
// callers can omit the link rather than emit a broken one.
func jiraBrowseURL(base, key string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" || key == "" {
		return ""
	}
	return base + "/browse/" + key
}

// generateResult is the /api/scoring/generate response. Row is set for a
// single-ticket generate so the frontend can update that row in place; for a
// full sweep only the tally is returned and the frontend re-fetches the list.
type generateResult struct {
	Scored    int                  `json:"scored"`
	Inserted  int                  `json:"inserted"`
	Updated   int                  `json:"updated"`
	Skipped   int                  `json:"skipped"`
	Preserved int                  `json:"preserved"`
	Flagged   int                  `json:"flagged"`
	Row       *scoring.ScoreRecord `json:"row,omitempty"`
}

// registerScoringRoutes wires the story-points engine endpoints onto mux. These
// are identity-free — score rows carry ticket keys, bands, and file names, no
// people — so unlike the contributor endpoints they need no incognito scrub.
// The one identity in the evidence bundle (PR author) is blanked from the
// response, since scoring is explicitly not a per-person metric (the rubric).
//
// store backs on-demand evidence extraction (per-request LoadAll, same as the
// other /api/* handlers); scores is the persistent ScoreStore. Either being nil
// makes the dependent endpoints return 503.
func registerScoringRoutes(mux *http.ServeMux, profile config.Profile, store cache.Store, scores *scoring.ScoreStore, poster scoring.JiraPoster) {
	// GET /api/scoring/list?filter=all|needs_insight&limit=N — persisted scores.
	mux.HandleFunc("GET /api/scoring/list", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if scores == nil {
			http.Error(w, "scoring store unavailable (server started without one)", http.StatusServiceUnavailable)
			return
		}
		var f scoring.ScoreFilter
		switch r.URL.Query().Get("filter") {
		case "", "all":
		case "needs_insight":
			f.NeedsInsightOnly = true
		default:
			http.Error(w, "unknown filter (want all or needs_insight)", http.StatusBadRequest)
			return
		}
		if ls := r.URL.Query().Get("limit"); ls != "" {
			n, err := strconv.Atoi(ls)
			if err != nil || n < 0 {
				http.Error(w, "limit must be a non-negative integer", http.StatusBadRequest)
				return
			}
			f.Limit = n
		}
		recs, err := scores.List(f)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if recs == nil {
			recs = []scoring.ScoreRecord{}
		}
		writeJSON(w, recs)
	})

	// GET /api/scoring/ticket/{key} — evidence + band + persisted row + the
	// copy-ready /score-ticket starting point for the human/LLM pass.
	mux.HandleFunc("GET /api/scoring/ticket/{key}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if store == nil {
			http.Error(w, "query API unavailable (server started without a cache store)", http.StatusServiceUnavailable)
			return
		}
		key := r.PathValue("key")
		if key == "" {
			http.Error(w, "ticket key is required", http.StatusBadRequest)
			return
		}
		ext, err := scoring.BuildExtractor(profile, store, time.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		ev, ok := ext.Extract(key)
		if !ok {
			http.Error(w, fmt.Sprintf("ticket %s not found in cache", key), http.StatusNotFound)
			return
		}
		band := scoring.Band(ev, profile.StoryPoints)
		for i := range ev.PRs {
			ev.PRs[i].Author = "" // scoring is not a per-person metric
		}
		var persisted *scoring.ScoreRecord
		if scores != nil {
			if rec, found, _ := scores.Get(key, ""); found {
				persisted = rec
			}
		}
		writeJSON(w, ticketDetail{
			Evidence:           ev,
			Band:               band,
			Persisted:          persisted,
			ScoreTicketCommand: "/score-ticket " + ev.Key,
			JiraURL:            jiraBrowseURL(profile.Jira.BaseURL, ev.Key),
		})
	})

	// POST /api/scoring/generate — run the band engine and persist. Body:
	//   {"ticket":"CD-123"}  → score that one ticket, return its row
	//   {} or {"all":true}   → full post-hoc sweep (tickets with ≥1 merged PR)
	mux.HandleFunc("POST /api/scoring/generate", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if store == nil || scores == nil {
			http.Error(w, "generate unavailable (server started without a cache or scoring store)", http.StatusServiceUnavailable)
			return
		}
		var body struct {
			Ticket string `json:"ticket"`
			All    bool   `json:"all"`
		}
		if r.Body != nil {
			// Tolerate an empty body (= full sweep); only a malformed non-empty
			// body is an error.
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err.Error() != "EOF" {
				http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
				return
			}
		}
		ext, err := scoring.BuildExtractor(profile, store, time.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		now := time.Now()

		if body.Ticket != "" {
			ev, ok := ext.Extract(body.Ticket)
			if !ok {
				http.Error(w, fmt.Sprintf("ticket %s not found in cache", body.Ticket), http.StatusNotFound)
				return
			}
			band := scoring.Band(ev, profile.StoryPoints)
			if _, err := scores.SaveAuto(scoring.NewAutoRecord(ev, band, now)); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			rec, _, _ := scores.Get(body.Ticket, "")
			res := generateResult{Scored: 1, Row: rec}
			if band.NeedsInsight {
				res.Flagged = 1
			}
			writeJSON(w, res)
			return
		}

		var res generateResult
		for _, key := range ext.Keys() {
			ev, ok := ext.Extract(key)
			if !ok || len(ev.PRs) == 0 { // post-hoc scope: needs a merged PR
				continue
			}
			band := scoring.Band(ev, profile.StoryPoints)
			outcome, err := scores.SaveAuto(scoring.NewAutoRecord(ev, band, now))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			res.Scored++
			if band.NeedsInsight {
				res.Flagged++
			}
			switch outcome {
			case scoring.OutcomeInserted:
				res.Inserted++
			case scoring.OutcomeUpdated:
				res.Updated++
			case scoring.OutcomeSkipped:
				res.Skipped++
			case scoring.OutcomePreserved:
				res.Preserved++
			}
		}
		writeJSON(w, res)
	})

	// GET /api/scoring/jira-status — whether live posting is available, so the
	// frontend can enable/disable the post button (Phase 5 build-order step 2:
	// verify token write scope before anything live). Never writes.
	mux.HandleFunc("GET /api/scoring/jira-status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		type status struct {
			Configured bool   `json:"configured"`
			CanWrite   bool   `json:"can_write"`
			Detail     string `json:"detail,omitempty"`
		}
		if poster == nil {
			writeJSON(w, status{Detail: "no jira token configured on the server"})
			return
		}
		if err := poster.VerifyWriteScope(r.Context()); err != nil {
			writeJSON(w, status{Configured: true, Detail: err.Error()})
			return
		}
		writeJSON(w, status{Configured: true, CanWrite: true})
	})

	// POST /api/scoring/post — write persisted scores back to Jira. Body:
	//   {"tickets":["CD-1","CD-2"], "dry_run":true}
	// dry_run defaults to true (decoded via a pointer), so an omitted or garbled
	// flag previews rather than writes — a live post requires explicit
	// "dry_run":false. Per-ticket outcomes (incl. idempotent skips and failures)
	// come back in the report; the call only errors on a bad request or a store
	// fault, not on an individual ticket's write failure.
	mux.HandleFunc("POST /api/scoring/post", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if scores == nil {
			http.Error(w, "scoring store unavailable (server started without one)", http.StatusServiceUnavailable)
			return
		}
		var body struct {
			Tickets []string `json:"tickets"`
			DryRun  *bool    `json:"dry_run"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
			return
		}
		if len(body.Tickets) == 0 {
			http.Error(w, "tickets is required", http.StatusBadRequest)
			return
		}
		dryRun := true
		if body.DryRun != nil {
			dryRun = *body.DryRun
		}
		if !dryRun && poster == nil {
			http.Error(w, "live posting unavailable (server has no jira token)", http.StatusServiceUnavailable)
			return
		}
		rep, err := scoring.PostScores(r.Context(), scores, poster, scoring.PostOptions{
			Tickets: body.Tickets,
			DryRun:  dryRun,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, rep)
	})

	// POST /api/scoring/override — record a human's final points for a ticket
	// (the per-ticket "Approve & post" path's first step). Treats the value as
	// ground truth (Source=human) while preserving the deterministic band
	// columns for calibration; does NOT post to Jira (the caller posts after).
	mux.HandleFunc("POST /api/scoring/override", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if scores == nil {
			http.Error(w, "scoring store unavailable (server started without one)", http.StatusServiceUnavailable)
			return
		}
		var body struct {
			Ticket string `json:"ticket"`
			Points *int   `json:"points"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
			return
		}
		if body.Ticket == "" || body.Points == nil {
			http.Error(w, "ticket and points are required", http.StatusBadRequest)
			return
		}
		if *body.Points < 0 {
			http.Error(w, "points must be a non-negative integer", http.StatusBadRequest)
			return
		}
		existing, ok, err := scores.Get(body.Ticket, "")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, fmt.Sprintf("no score row for %s — run the generator first", body.Ticket), http.StatusNotFound)
			return
		}
		if err := scores.SetHumanOverride(body.Ticket, "", *body.Points, existing.AutoPoints, time.Now()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rec, _, _ := scores.Get(body.Ticket, "")
		writeJSON(w, rec)
	})
}

// writeJSON encodes v as the response body, setting the content type. A trailing
// encode error can't be surfaced to the client (headers are already sent), so it
// is intentionally dropped — same as the other /api handlers.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
