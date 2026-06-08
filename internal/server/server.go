package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mathewepstein/velocity/internal/analyze"
	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
	"github.com/mathewepstein/velocity/internal/incognito"
	"github.com/mathewepstein/velocity/internal/scoring"
	"github.com/mathewepstein/velocity/web"
)

// Options controls one Serve run. Port 0 lets the OS pick an ephemeral port
// (mostly useful in tests). SelfLogin is the GitHub username from the loaded
// profile; it powers the "Me" nav link's href and is empty in tests that
// don't load a config.
type Options struct {
	Port      int
	Open      bool
	Out       io.Writer
	SelfLogin string
	Incognito bool         // when true, anonymize identities on every /metrics.json + /dev/{slug} + /api/* response
	Handler   http.Handler // overrides the default handler (tests)

	// Profile + Store back the on-demand /api/* query layer (architecture
	// Step 2). Store nil → the /api/* endpoints return 503 (the legacy
	// metrics.json routes still work); the serve command always supplies one.
	Profile config.Profile
	Store   cache.Store

	// ScoreStore backs the /api/scoring/* story-points endpoints. Nil → those
	// endpoints return 503; the serve command always supplies one.
	ScoreStore *scoring.ScoreStore
}

// Serve starts the HTTP server and blocks until ctx is canceled.
//
// Routes:
//   - GET /              → leaderboard home (template-rendered index.html)
//   - GET /contributors  → contributors table (template-rendered)
//   - GET /dev/{login}   → individual dev dashboard (template-rendered)
//   - GET /<static>      → embedded asset (CSS/JS)
//   - GET /metrics.json  → served from cache root (fresh on every request)
//   - GET /healthz       → "ok"
//
// We don't cache metrics.json in memory because `velocity refresh` may have
// rewritten it while the server is running — the sub-second stat cost is
// negligible, and this keeps the UX correct without cache-invalidation logic.
func Serve(ctx context.Context, opts Options) error {
	if opts.Out == nil {
		return errors.New("server: Options.Out is required")
	}
	handler := opts.Handler
	if handler == nil {
		h, err := buildHandler(opts.SelfLogin, opts.Incognito, opts.Profile, opts.Store, opts.ScoreStore)
		if err != nil {
			return err
		}
		handler = h
	}

	addr := fmt.Sprintf("127.0.0.1:%d", opts.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	boundPort := ln.Addr().(*net.TCPAddr).Port
	url := fmt.Sprintf("http://127.0.0.1:%d", boundPort)

	srv := &http.Server{
		Handler:      handler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	fmt.Fprintf(opts.Out, "Velocity dashboard serving at %s\n", url)
	if opts.Open {
		if err := openBrowser(url); err != nil {
			fmt.Fprintf(opts.Out, "(could not open browser: %v)\n", err)
		}
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	}
}

// pageData is what the nav partial and each page template receive at render
// time. ActiveNav is one of "leaderboard" | "contributors" | "me" and drives
// the aria-current highlight; SelfDevURL is the resolved href for the "Me"
// link, or a generic /dev/ fallback when no profile username is configured.
// Incognito surfaces the `--incognito` flag so the nav can render a chip
// and the layout can pick up the alternate accent theme via body class.
type pageData struct {
	ActiveNav  string
	SelfDevURL string
	Incognito  bool
}

// buildHandler wires the routes. Extracted so tests can drive the mux
// without a listening socket. selfLogin powers the "Me" link in the shared
// nav; empty string falls back to /dev/. When incog is true, the "Me" link
// resolves through the persistent anonymization mapping so it points at the
// alias slug rather than the real GitHub username.
func buildHandler(selfLogin string, incog bool, profile config.Profile, store cache.Store, scores *scoring.ScoreStore) (http.Handler, error) {
	mux := http.NewServeMux()

	webFS, err := fs.Sub(web.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("subfs: %w", err)
	}

	tmpl, err := template.ParseFS(web.FS, "*.html", "partials/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}

	// Incognito state shared across handlers. The mapping is mutated when
	// /metrics.json mints new entries; subsequent /dev/{slug} lookups read
	// from the same instance. A single sync.Mutex guards mutation since
	// the http.Server multiplexes requests across goroutines.
	var (
		mapping    *incognito.Mapping
		mappingMu  sync.Mutex
		mappingErr error
	)
	if incog {
		mapping, mappingErr = incognito.Load()
		if mappingErr != nil {
			return nil, fmt.Errorf("load incognito mapping: %w", mappingErr)
		}
		// Pre-warm the mapping against the current metrics.json. Two reasons:
		//   1. The "Me" nav link is server-rendered at template-execute time,
		//      so the login → slug resolution needs the mapping populated
		//      before the first page request — otherwise the link points to
		//      /dev/ (no slug) and lands on the wrong dev.
		//   2. New dev names get persisted up front rather than at first
		//      /metrics.json fetch, so a screenshot of the persisted file
		//      after startup shows the full cohort.
		// Best-effort: a missing or unreadable metrics.json doesn't block
		// the server — `velocity analyze` is a prerequisite anyway, and the
		// /metrics.json handler will surface that error to the user.
		if root, rootErr := cache.Root(); rootErr == nil {
			path := filepath.Join(root, cache.MetricsFile)
			if raw, readErr := os.ReadFile(path); readErr == nil {
				var result analyze.Result
				if json.Unmarshal(raw, &result) == nil {
					_, mutated := incognito.ScrubResult(&result, mapping)
					if mutated {
						_ = mapping.Save()
					}
				}
			}
		}
	}

	// selfURL is the "Me" link destination. In incognito mode we resolve
	// the configured username to an alias slug — but only after at least
	// one /metrics.json hit has populated the mapping. Until then, the
	// link falls back to /dev/ (which lands on a no-dev-found page that's
	// already part of the legacy fallback).
	selfURL := func() string {
		if !incog {
			if selfLogin == "" {
				return "/dev/"
			}
			return "/dev/" + selfLogin
		}
		mappingMu.Lock()
		defer mappingMu.Unlock()
		if selfLogin == "" {
			return "/dev/"
		}
		// The login → display-name index is populated by ScrubResult; if
		// the user hasn't loaded /metrics.json yet, the index is empty.
		// In that case, fall back to /dev/ — clicking "Me" before loading
		// the dashboard isn't a real flow.
		display := ""
		for login, name := range mapping.LoginIndex() {
			if login == selfLogin {
				display = name
				break
			}
		}
		if display == "" {
			return "/dev/"
		}
		if alias, ok := mapping.Devs[display]; ok && alias.Slug != "" {
			return "/dev/" + alias.Slug
		}
		return "/dev/"
	}

	renderPage := func(pageFile, activeNav string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			data := pageData{ActiveNav: activeNav, SelfDevURL: selfURL(), Incognito: incog}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			if err := tmpl.ExecuteTemplate(w, pageFile, data); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
		}
	}

	mux.HandleFunc("GET /{$}", renderPage("index.html", "leaderboard"))
	mux.HandleFunc("GET /velocity", renderPage("velocity.html", "velocity"))
	mux.HandleFunc("GET /velocity/{$}", renderPage("velocity.html", "velocity"))
	mux.HandleFunc("GET /contributors", renderPage("contributors.html", "contributors"))
	mux.HandleFunc("GET /contributors/{$}", renderPage("contributors.html", "contributors"))
	mux.HandleFunc("GET /scoring", renderPage("scoring.html", "scoring"))
	mux.HandleFunc("GET /scoring/{$}", renderPage("scoring.html", "scoring"))
	mux.HandleFunc("GET /dev/", renderPage("dev.html", "me"))

	mux.HandleFunc("GET /metrics.json", func(w http.ResponseWriter, r *http.Request) {
		root, err := cache.Root()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		path := filepath.Join(root, cache.MetricsFile)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")

		if !incog {
			http.ServeFile(w, r, path)
			return
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var result analyze.Result
		if err := json.Unmarshal(raw, &result); err != nil {
			http.Error(w, fmt.Sprintf("decode metrics.json: %v", err), http.StatusInternalServerError)
			return
		}

		mappingMu.Lock()
		scrubbed, mutated := incognito.ScrubResult(&result, mapping)
		// Persist new mappings BEFORE writing the response so a crash
		// mid-write doesn't leave the client with names that don't survive
		// a restart.
		var saveErr error
		if mutated {
			saveErr = mapping.Save()
		}
		mappingMu.Unlock()

		if saveErr != nil {
			http.Error(w, fmt.Sprintf("persist incognito mapping: %v", saveErr), http.StatusInternalServerError)
			return
		}

		out, err := json.Marshal(scrubbed)
		if err != nil {
			http.Error(w, fmt.Sprintf("encode scrubbed metrics: %v", err), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(out)
	})

	// GET /api/contributors?from=YYYY-MM&to=YYYY-MM[&sort=score|elo] — the
	// on-demand contributor table for an arbitrary scoring window (architecture
	// Step 2). Backs both the contributors range picker (P2.3, default sort by
	// composite score) and the leaderboard (sort=elo), which is why there is no
	// separate /api/leaderboard endpoint: every row already carries its Elo
	// rating (attachRatings, read-only from precomputed ratings.json — E6), so
	// the leaderboard is the same cohort ordered by a different field.
	//
	// from/to are required: the frontend always supplies them, and requiring
	// them keeps the endpoint a pure function of the window rather than
	// re-deriving the default-window clamp. Output is the same []DevWindowMetrics
	// shape as metrics.json's `devs`, so a page can swap its source with no
	// reshaping.
	mux.HandleFunc("GET /api/contributors", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if store == nil {
			http.Error(w, "query API unavailable (server started without a cache store)", http.StatusServiceUnavailable)
			return
		}
		from, to, err := parseWindow(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		sortKey := r.URL.Query().Get("sort")
		switch sortKey {
		case "", "score", "elo":
		default:
			http.Error(w, fmt.Sprintf("unknown sort %q (want score or elo)", sortKey), http.StatusBadRequest)
			return
		}
		devs, err := analyze.ContributorsForWindow(analyze.Options{Profile: profile, Store: store}, from, to)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if incog {
			mappingMu.Lock()
			scrubbed, mutated := incognito.ScrubResult(&analyze.Result{Devs: devs}, mapping)
			var saveErr error
			if mutated {
				saveErr = mapping.Save()
			}
			mappingMu.Unlock()
			if saveErr != nil {
				http.Error(w, fmt.Sprintf("persist incognito mapping: %v", saveErr), http.StatusInternalServerError)
				return
			}
			devs = scrubbed.Devs
		}

		sortContributors(devs, sortKey)

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(devs); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	// GET /api/team/flow?from=YYYY-MM&to=YYYY-MM — the macro team-flow view
	// (architecture Step 2). Monthly is always the full history (the frontend
	// windows the chart client-side); from/to scope the Claude-attribution cut.
	// No incognito scrub: TeamFlow is counts/months/cycle-hours, no identities.
	mux.HandleFunc("GET /api/team/flow", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if store == nil {
			http.Error(w, "query API unavailable (server started without a cache store)", http.StatusServiceUnavailable)
			return
		}
		from, to, err := parseWindow(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		flow, err := analyze.TeamFlowForWindow(analyze.Options{Profile: profile, Store: store}, from, to)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(flow); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	// GET /api/dev/{login}?from=YYYY-MM&to=YYYY-MM — one developer's window
	// metrics on demand (architecture Step 2; the backend for the dev page).
	// The composite score and rank are team-relative, so this computes the full
	// cohort for the window via ContributorsForWindow and selects the requested
	// dev — there is no cheaper correct path (a lone-dev z-score is meaningless).
	// {login} matches a github login case-insensitively; in incognito mode the
	// cohort is scrubbed first, so {login} matches the alias slug. 404 if no dev
	// in the window matches.
	mux.HandleFunc("GET /api/dev/{login}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if store == nil {
			http.Error(w, "query API unavailable (server started without a cache store)", http.StatusServiceUnavailable)
			return
		}
		login := r.PathValue("login")
		if login == "" {
			http.Error(w, "dev login is required", http.StatusBadRequest)
			return
		}
		from, to, err := parseWindow(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		devs, err := analyze.ContributorsForWindow(analyze.Options{Profile: profile, Store: store}, from, to)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if incog {
			mappingMu.Lock()
			scrubbed, mutated := incognito.ScrubResult(&analyze.Result{Devs: devs}, mapping)
			var saveErr error
			if mutated {
				saveErr = mapping.Save()
			}
			mappingMu.Unlock()
			if saveErr != nil {
				http.Error(w, fmt.Sprintf("persist incognito mapping: %v", saveErr), http.StatusInternalServerError)
				return
			}
			devs = scrubbed.Devs
		}

		dev := findDevByLogin(devs, login)
		if dev == nil {
			http.Error(w, fmt.Sprintf("no dev matching %q in window %s..%s", login, from, to), http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(dev); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	// Story-points engine endpoints (/api/scoring/*).
	registerScoringRoutes(mux, profile, store, scores)

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok"))
	})

	// Static assets (CSS/JS). The HTML files are intercepted above via the
	// explicit page routes, so this catch-all only ends up serving raw
	// .css / .js / partial files.
	//
	// no-store is deliberate: the embedded FS has no usable modtime or ETag, so
	// without it browsers heuristically cache stale JS/CSS and a rebuilt UI
	// won't reach the user until a hard refresh. This is a local dashboard, so
	// always refetching assets costs nothing.
	mux.Handle("GET /", noStore(http.FileServer(http.FS(webFS))))

	return guardLocalhost(mux), nil
}

// guardLocalhost rejects any request whose Host header is not a loopback
// address. The server only ever listens on 127.0.0.1, so a legitimate browser
// request always carries a loopback Host (127.0.0.1, localhost, or ::1). The
// guard exists to defeat DNS rebinding: an attacker page that rebinds its
// domain to 127.0.0.1 can reach the socket, but its requests still carry the
// attacker's domain in the Host header, so they are refused here — closing the
// only same-origin path to the unauthenticated /api/* data and the
// state-mutating POST /api/scoring/generate endpoint.
func guardLocalhost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			host = h
		}
		switch host {
		case "127.0.0.1", "localhost", "::1", "[::1]":
			next.ServeHTTP(w, r)
		default:
			http.Error(w, "forbidden: non-loopback Host header", http.StatusForbidden)
		}
	})
}

// parseWindow reads the required from/to month params (YYYY-MM) off an /api
// request and validates ordering. Both are mandatory — the query layer is a
// pure function of an explicit window.
func parseWindow(r *http.Request) (from, to cache.Month, err error) {
	fromStr := r.URL.Query().Get("from")
	toStr := r.URL.Query().Get("to")
	if fromStr == "" || toStr == "" {
		return from, to, fmt.Errorf("from and to are required (YYYY-MM)")
	}
	if from, err = cache.ParseMonth(fromStr); err != nil {
		return from, to, fmt.Errorf("bad from: %w", err)
	}
	if to, err = cache.ParseMonth(toStr); err != nil {
		return from, to, fmt.Errorf("bad to: %w", err)
	}
	if to.Before(from) {
		return from, to, fmt.Errorf("to %s precedes from %s", to, from)
	}
	return from, to, nil
}

// sortContributors orders the cohort in place for presentation:
//   - "elo": Elo rating descending (the leaderboard ordering).
//   - "score" / "" (default): composite score descending (the contributors
//     ordering, matching Score.Rank).
//
// A dev missing the relevant value sorts last; ties keep input order (stable),
// so the underlying pipeline order is the final tiebreak. The sort key is
// validated at the handler before this is called, so an unrecognized key falls
// through to the score default rather than erroring here.
func sortContributors(devs []analyze.DevWindowMetrics, key string) {
	keyFor := scoreOf
	if key == "elo" {
		keyFor = eloOf
	}
	sort.SliceStable(devs, func(i, j int) bool {
		return keyFor(devs[i]) > keyFor(devs[j])
	})
}

// scoreOf is a dev's composite score total, or -Inf when unscored (so unscored
// devs sort last under a descending sort).
func scoreOf(d analyze.DevWindowMetrics) float64 {
	if d.Score == nil {
		return math.Inf(-1)
	}
	return d.Score.Total
}

// eloOf is a dev's current Elo rating, or -Inf when unrated.
func eloOf(d analyze.DevWindowMetrics) float64 {
	if d.Rating == nil {
		return math.Inf(-1)
	}
	return d.Rating.Current
}

// findDevByLogin selects the dev whose github logins contain login
// (case-insensitive), mirroring the frontend's findDev. Returns nil when none
// match. In incognito mode the logins are alias slugs, so the same match holds
// against an already-scrubbed cohort.
func findDevByLogin(devs []analyze.DevWindowMetrics, login string) *analyze.DevWindowMetrics {
	lower := strings.ToLower(login)
	for i := range devs {
		for _, l := range devs[i].Dev.AllGitHubLogins() {
			if strings.ToLower(l) == lower {
				return &devs[i]
			}
		}
	}
	return nil
}

// noStore wraps a handler to disable browser caching of its responses.
func noStore(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		h.ServeHTTP(w, r)
	})
}

// openBrowser launches the system browser at url. Best-effort — failures
// don't kill the server since the user can always open the URL themselves.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return fmt.Errorf("unsupported platform %s", runtime.GOOS)
	}
	return cmd.Start()
}
