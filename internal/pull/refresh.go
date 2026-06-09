package pull

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
	"github.com/mathewepstein/velocity/internal/progress"
)

// Tokens bundles the API tokens needed for a refresh. The caller fetches them
// from the keychain once; this struct carries them through the orchestrator.
type Tokens struct {
	Jira   string
	GitHub string
}

// RefreshOptions controls one refresh run.
type RefreshOptions struct {
	// Since, if non-nil, is the earliest month to consider. Otherwise the
	// manifest-determined default applies (see effectiveStart).
	Since *cache.Month
	// Force re-pulls every month in range (overrides freshness rules).
	Force bool
	// DryRun prints planned work but doesn't call APIs or write cache.
	DryRun bool
	// SleepBetweenPages throttles paginated requests; 0 disables.
	SleepBetweenPages time.Duration
	// Now is injected for testability. Zero → time.Now().
	Now time.Time
	// Out receives progress messages.
	Out io.Writer
	// Reporter renders the transient in-cell status line (pagination, per-PR
	// detail, bisection, backoff waits). Nil → no-op. When set it must write
	// to the same stream as Out.
	Reporter progress.Reporter
	// Store is the cache backend records + manifest are written to. Nil
	// defaults to the JSON store.
	Store cache.Store
}

// RefreshResult summarizes the window a refresh visited, so follow-on stages
// (detail hydration) can scope to exactly those months.
type RefreshResult struct {
	// WindowStart is the earliest month visited.
	WindowStart cache.Month
	// Months is how many months the run iterated.
	Months int
}

// Refresh iterates every (source, scope, month) in range, pulls where needed,
// writes to the cache, and updates the manifest. Returns the first failure
// encountered — partial progress still lands on disk because each cell is
// committed immediately.
func Refresh(ctx context.Context, p config.Profile, t Tokens, opts RefreshOptions) (RefreshResult, error) {
	if opts.Out == nil {
		return RefreshResult{}, fmt.Errorf("pull: RefreshOptions.Out is required")
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	if opts.Reporter == nil {
		opts.Reporter = progress.Nop()
	}
	if opts.Store == nil {
		opts.Store = cache.JSONStore{}
	}
	rep := opts.Reporter
	defer rep.Clear()
	// printf writes a permanent (scrolling) line, clearing the transient
	// status line first so the two never collide on the shared stream.
	printf := func(format string, args ...interface{}) {
		progress.Printf(rep, opts.Out, format, args...)
	}

	manifest, err := opts.Store.LoadManifest()
	if err != nil {
		return RefreshResult{}, err
	}

	months := refreshPlan(manifest, p, opts)
	if len(months) == 0 {
		return RefreshResult{}, fmt.Errorf("no months in range (start=%s, end=%s)",
			effectiveStart(manifest, p, opts), cache.CurrentMonth(opts.Now))
	}
	start, end := months[0], months[len(months)-1]
	res := RefreshResult{WindowStart: start, Months: len(months)}

	printf("Refresh window: %s … %s (%d months)\n", start, end, len(months))
	if opts.DryRun {
		printf("(dry-run — no API calls, no writes)\n")
	}

	var jp *JiraPuller
	if t.Jira != "" && len(p.Jira.Projects) > 0 {
		jp = NewJiraPuller(p.Jira, t.Jira, 0, opts.SleepBetweenPages)
		jp.SetReporter(rep)
	}
	var gp *GithubPuller
	if t.GitHub != "" && len(p.GitHub.Orgs) > 0 {
		gp = NewGithubPuller(p.GitHub, t.GitHub, opts.SleepBetweenPages)
		gp.SetReporter(rep)
	}

	for _, m := range months {
		printf("\n[%s]\n", m)

		// Jira per project.
		for _, project := range p.Jira.Projects {
			if err := refreshJira(ctx, printf, rep, jp, manifest, project, m, opts); err != nil {
				return res, err
			}
		}

		// GitHub per org (PRs + commits pulled together; manifested separately).
		for _, org := range p.GitHub.Orgs {
			if err := refreshGithub(ctx, printf, rep, gp, manifest, org, m, opts); err != nil {
				return res, err
			}
		}

		// Persist manifest after each month so a mid-run failure preserves
		// the months already on disk. Per-cell records are written atomically
		// by WriteMonth; the manifest is the index that lets a resumed run
		// skip them.
		if !opts.DryRun {
			if err := opts.Store.SaveManifest(manifest); err != nil {
				return res, fmt.Errorf("save manifest after %s: %w", m, err)
			}
		}
	}

	rep.EnterCell("") // retire the base pull from the status line
	printf("\nRefresh complete.\n")
	return res, nil
}

// refreshPlan computes the months a refresh will visit, earliest first. It is
// pure (no I/O) so the coverage guarantee can be asserted in tests without
// calling any API: the per-month pull decision is then NeedsPull, which
// re-pulls any month captured while still partial (Hole 2) — so the catch-up
// for a lapse needs only the right range here, not a separate force.
func refreshPlan(mf *cache.Manifest, p config.Profile, opts RefreshOptions) []cache.Month {
	return cache.MonthsInRange(effectiveStart(mf, p, opts), cache.CurrentMonth(opts.Now))
}

// normalWindowStart is the routine lookback start: default_length_months back
// from the current month. This is the window a healthy, regularly-run refresh
// visits. effectiveStart may reach further back to cover a lapse, but anything
// from this month forward is the "normal" window.
func normalWindowStart(p config.Profile, now time.Time) cache.Month {
	length := p.Window.DefaultLengthMonths
	if length <= 0 {
		length = 4
	}
	return cache.CurrentMonth(now).Add(-(length - 1))
}

// effectiveStart picks the earliest month to visit.
//
// Priority:
//  1. --since (explicit user override)
//  2. If the manifest is empty → backfill_start from config
//  3. Otherwise → the earlier of the normal window start and the last pulled
//     month. Anchoring to the last actual pull (not a fixed offset from now)
//     ensures that if the gap since the last refresh exceeds the window
//     length, the intervening months are still in range rather than silently
//     skipped (coverage Hole 1).
func effectiveStart(mf *cache.Manifest, p config.Profile, opts RefreshOptions) cache.Month {
	if opts.Since != nil {
		return *opts.Since
	}
	if mf == nil || len(mf.Entries) == 0 {
		if start, err := cache.ParseMonth(p.Window.BackfillStart); err == nil {
			return start
		}
		// Fallback if config is malformed.
		return cache.CurrentMonth(opts.Now).Add(-3)
	}
	start := normalWindowStart(p, opts.Now)
	if last, ok := mf.LatestCachedMonth(); ok && last.Before(start) {
		return last
	}
	return start
}

func refreshJira(ctx context.Context, printf func(string, ...interface{}), rep progress.Reporter, jp *JiraPuller, mf *cache.Manifest, project string, m cache.Month, opts RefreshOptions) error {
	if jp == nil {
		return nil
	}
	source := cache.SourceJira
	if !cache.NeedsPull(mf, source, project, m, opts.Now, opts.Force) {
		printf("  %s/%s — cached\n", source, project)
		return nil
	}
	if opts.DryRun {
		printf("  %s/%s — would pull\n", source, project)
		return nil
	}
	rep.EnterCell(fmt.Sprintf("jira/%s %s", project, m))
	issues, err := jp.PullMonth(ctx, project, m)
	if err != nil {
		return fmt.Errorf("jira %s %s: %w", project, m, err)
	}
	// Carry forward hydration for issues whose `updated` hasn't advanced since
	// the last pull, so the detail stage only re-hydrates new/changed issues
	// instead of the whole (always-re-pulled) current month.
	if existing, rerr := opts.Store.ReadJiraIssues(project, m); rerr == nil {
		issues = mergeHydration(existing, issues)
	} else if !errors.Is(rerr, fs.ErrNotExist) {
		return fmt.Errorf("read existing jira %s %s: %w", project, m, rerr)
	}
	if err := opts.Store.WriteJiraIssues(project, m, issues); err != nil {
		return err
	}
	mf.Update(source, project, m, len(issues), opts.Now)
	printf("  %s/%s — %d issues\n", source, project, len(issues))
	return nil
}

// mergeHydration carries forward the already-hydrated cache record for any
// freshly-pulled issue whose `updated` timestamp hasn't advanced — so the
// wholesale month-cell rewrite no longer nulls hydration the detail stage would
// just have to redo. Jira's `updated` advances on every change we capture
// (comments, status, fields, links), so an unchanged `updated` means the cached
// record is still complete and the cheap base fields are unchanged too; a new
// or `updated`-advanced issue passes through as the fresh base record for the
// detail stage to hydrate. The output set is exactly `fresh` (issues that aged
// out of the window are dropped as before), in `fresh` order for stable ord.
func mergeHydration(existing, fresh []cache.JiraIssue) []cache.JiraIssue {
	prior := make(map[string]cache.JiraIssue, len(existing))
	for _, e := range existing {
		prior[e.Key] = e
	}
	out := make([]cache.JiraIssue, 0, len(fresh))
	for _, f := range fresh {
		if c, ok := prior[f.Key]; ok && c.DetailFetched && c.RawFields != nil && c.Updated.Equal(f.Updated) {
			out = append(out, c) // unchanged → keep the fully-hydrated cached record
			continue
		}
		out = append(out, f) // new or changed → fresh base record; detail stage hydrates it
	}
	return out
}

func refreshGithub(ctx context.Context, printf func(string, ...interface{}), rep progress.Reporter, gp *GithubPuller, mf *cache.Manifest, org string, m cache.Month, opts RefreshOptions) error {
	if gp == nil {
		return nil
	}
	prsStale := cache.NeedsPull(mf, cache.SourceGitHubPRs, org, m, opts.Now, opts.Force)
	commitsStale := cache.NeedsPull(mf, cache.SourceGitHubCommits, org, m, opts.Now, opts.Force)
	reviewsStale := cache.NeedsPull(mf, cache.SourceGitHubReviews, org, m, opts.Now, opts.Force)

	if !prsStale && !commitsStale && !reviewsStale {
		printf("  github/%s — cached\n", org)
		return nil
	}
	if opts.DryRun {
		if prsStale {
			printf("  github-prs/%s — would pull\n", org)
		}
		if commitsStale {
			printf("  github-commits/%s — would pull\n", org)
		}
		if reviewsStale {
			printf("  github-reviews/%s — would pull\n", org)
		}
		return nil
	}

	rep.EnterCell(fmt.Sprintf("github/%s %s", org, m))
	pull, err := gp.PullMonth(ctx, org, m)
	if err != nil {
		return err
	}

	if prsStale {
		if err := opts.Store.WriteGitHubPRs(org, m, pull.PRs); err != nil {
			return err
		}
		mf.Update(cache.SourceGitHubPRs, org, m, len(pull.PRs), opts.Now)
		printf("  github-prs/%s — %d PRs\n", org, len(pull.PRs))
	}
	if commitsStale {
		if err := opts.Store.WriteGitHubCommits(org, m, pull.Commits); err != nil {
			return err
		}
		mf.Update(cache.SourceGitHubCommits, org, m, len(pull.Commits), opts.Now)
		printf("  github-commits/%s — %d commits\n", org, len(pull.Commits))
	}
	if reviewsStale {
		if err := opts.Store.WriteGitHubReviews(org, m, pull.Reviews); err != nil {
			return err
		}
		mf.Update(cache.SourceGitHubReviews, org, m, len(pull.Reviews), opts.Now)
		printf("  github-reviews/%s — %d reviews\n", org, len(pull.Reviews))
	}
	return nil
}
