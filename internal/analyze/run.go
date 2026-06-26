package analyze

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

// selfFilter returns the Loaded scoped to the configured user. When the
// profile lacks a github_login or jira_account_id (or both), the corresponding
// side stays unfiltered so we don't accidentally drop everything before the
// user has run `velocity devs discover`.
func selfFilter(data *Loaded, p config.Profile) *Loaded {
	self := config.DevIdentity{
		JiraAccountID: p.Jira.AccountID,
	}
	if p.GitHub.Username != "" {
		self.GitHubLogins = []string{p.GitHub.Username}
	}
	if len(self.AllGitHubLogins()) == 0 && self.JiraAccountID == "" {
		return data
	}
	return filterForDev(data, self)
}

// Options controls one Run. Now is injected so tests can pin the current
// month; production callers pass time.Now(). Rebuild drops the persisted
// Elo state and walks every completed period from the earliest cached
// month — used after a cache-extending backfill so Elo history sees the
// new data, since advanceRatings is forward-only past rt.LastPeriod.
type Options struct {
	Profile config.Profile
	Now     time.Time
	Rebuild bool
	// NoPersist skips the ratings.json write at the end of Run, leaving Run a
	// pure read-only computation. Used by read-only audit tooling that wants
	// the full assembled Result (composite + Elo + Spearman) without mutating
	// live rating history. Combine with Rebuild to walk every period in-memory
	// from a clean slate.
	NoPersist bool
	// Store is the cache backend to read records + manifest from. Nil defaults
	// to the JSON store, so existing callers and tests are unaffected.
	Store cache.Store
}

// Run loads the cache, computes every view, and returns the assembled Result.
// No disk writes happen here — WriteMetrics handles persistence so callers
// can choose to inspect or serve the result directly.
func Run(opts Options) (*Result, error) {
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	if opts.Store == nil {
		opts.Store = cache.JSONStore{}
	}
	current := cache.CurrentMonth(opts.Now)

	data, err := LoadAll(opts.Profile, current, opts.Store)
	if err != nil {
		return nil, err
	}

	backfillStart, err := cache.ParseMonth(opts.Profile.Window.BackfillStart)
	if err != nil {
		return nil, fmt.Errorf("invalid backfill_start: %w", err)
	}

	// Single-user view rolls up only the configured user's activity so the
	// dashboard keeps making sense against an org-wide cache.
	selfData := selfFilter(data, opts.Profile)

	length := opts.Profile.Window.DefaultLengthMonths
	if length < 1 {
		length = 4
	}
	ci := opts.Profile.Scoring.CodeImpact
	norm := opts.Profile.Scoring.Normalize
	curWin := currentWindow(selfData, current, length, ci, norm)
	curWin.Label = fmt.Sprintf("Current (%s → %s)", curWin.Window.Start, curWin.Window.End)

	priorWin := priorWindow(selfData, curWin, ci, norm)
	priorWin.Label = fmt.Sprintf("Prior (%s → %s)", priorWin.Window.Start, priorWin.Window.End)

	yoyWin := yoyWindow(selfData, curWin, ci, norm)
	yoyWin.Label = fmt.Sprintf("YoY (%s → %s)", yoyWin.Window.Start, yoyWin.Window.End)

	// Project detection stays on the full dataset — surges should pick up
	// large initiatives regardless of which devs contributed.
	projects := detectProjects(data, opts.Profile.Surge)

	curStart, _ := cache.ParseMonth(curWin.Window.Start)
	curEnd, _ := cache.ParseMonth(curWin.Window.End)
	// Integration-PR down-weighter, built once over the full corpus (nil when
	// the feature is disabled — then every scoring path is byte-identical to the
	// pre-feature behavior). Shared by the composite path here and the Elo path
	// in advanceRatings so the two attribution sites can't drift.
	integWeight := newIntegrationWeighter(data, opts.Profile.Scoring.Integration)
	devs := buildDevWindows(data, opts.Profile.Devs, opts.Profile.Scoring.EffectiveExcludes(), opts.Profile.Scoring.ExcludedRoles, curStart, curEnd, backfillStart, current, ci, norm, integWeight)
	applyCodeImpactCap(devs, ci)
	devs = attachProjectShares(devs, buildProjectShares(data, projects, ci, norm))
	devs = computeContributorScores(devs, opts.Profile.Scoring.Weights, norm)

	// Advance Elo ratings through every completed bi-weekly period since the
	// last persisted period. State lives at $DATA/ratings.json — outside the
	// per-source cache so `velocity refresh --reset` doesn't wipe it.
	//
	// Clamp the walk start to the earliest cached month: backfill_start can
	// long predate when the cache was actually populated (the user may have
	// reset with `--since` to bound the pull), and walking pre-data periods
	// produces spurious history entries from issues whose Created/Resolved
	// fell before the cache window even though their Updated dragged them in.
	var ratings *cache.Ratings
	if opts.Rebuild {
		ratings = &cache.Ratings{Version: cache.CurrentRatingsVersion, Devs: map[string]cache.DevRatingState{}}
	} else {
		ratings, err = cache.LoadRatings()
		if err != nil {
			return nil, fmt.Errorf("load ratings: %w", err)
		}
	}
	excludes := opts.Profile.Scoring.EffectiveExcludes()
	scoringDevs := make([]config.DevIdentity, 0, len(opts.Profile.Devs))
	for _, d := range opts.Profile.Devs {
		if devExcluded(d, excludes) || config.RoleExcluded(d.EffectiveRole(), opts.Profile.Scoring.ExcludedRoles) {
			continue
		}
		scoringDevs = append(scoringDevs, d)
	}
	eloStart := backfillStart
	manifest, mErr := opts.Store.LoadManifest()
	if mErr == nil {
		if first, ok := manifest.FirstCachedMonth(); ok && backfillStart.Before(first) {
			eloStart = first
		}
	}
	if _, err := advanceRatings(ratings, scoringDevs, data, eloStart, current, opts.Profile.Scoring, opts.Now, integWeight); err != nil {
		return nil, fmt.Errorf("advance ratings: %w", err)
	}
	if !opts.NoPersist {
		if err := cache.SaveRatings(ratings); err != nil {
			return nil, fmt.Errorf("save ratings: %w", err)
		}
	}
	devs = attachRatings(devs, ratings.Devs, opts.Profile.Scoring)

	return &Result{
		GeneratedAt:   opts.Now.UTC(),
		BackfillStart: backfillStart.String(),
		CurrentMonth:  current.String(),
		Current:       curWin,
		Prior:         priorWin,
		YoY:           yoyWin,
		Quarters:      lastQuarters(selfData, current, quartersToShow, ci, norm),
		FullHistory:   fullHistory(selfData, backfillStart, current, ci, norm),
		Projects:      projects,
		Devs:          devs,
		QAFlow:        deriveQAFlow(data, curStart, curEnd),
		TeamFlow:      deriveTeamFlow(data, backfillStart, current, curStart, curEnd),
		Meta: Meta{
			JiraIssuesLoaded:     len(data.Issues),
			PRsLoaded:            len(data.PRs),
			CommitsLoaded:        len(data.Commits),
			ReviewsLoaded:        len(data.Reviews),
			MonthsLoaded:         len(data.Months),
			ProjectsDetected:     len(projects),
			DevsMapped:           len(opts.Profile.Devs),
			EloCompositeSpearman: spearmanRho(devs),
		},
	}, nil
}

// WriteMetrics serializes a Result to cache root's metrics.json, atomically
// (tmp + rename). 0o600 because the file mirrors cache contents, which are
// already 0o600.
func WriteMetrics(r *Result) (string, error) {
	root, err := cache.Root()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create cache root: %w", err)
	}
	path := filepath.Join(root, cache.MetricsFile)

	// Strip the per-dev cohort from the persisted blob: every live page reads
	// it from /api/contributors|dev now, so serializing Devs (the largest slice
	// in the payload) would just duplicate what the query API computes on
	// demand. Devs keeps a normal JSON tag for the in-memory clone/scrub paths,
	// so we drop it on a shallow copy here rather than via a `json:"-"` tag —
	// omitempty then omits the nil slice. The caller's Result is untouched.
	toWrite := *r
	toWrite.Devs = nil

	data, err := json.MarshalIndent(&toWrite, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode metrics: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("commit metrics: %w", err)
	}
	return path, nil
}
