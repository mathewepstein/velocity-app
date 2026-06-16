// velocity-backfill-prmeta backfills the PR-metadata fields added by the
// github-corpus-capture-plan onto every already-cached merged GitHub PR,
// WITHOUT a full re-crawl. It makes exactly two API calls per PR —
// /pulls/{n} (base/head ref+sha+repo, merged_by, commit/file counts,
// merge_commit_sha, draft, auto_merge, updated, author_association, labels,
// assignees, requested_reviewers) and /pulls/{n}/commits (the PR→commit
// membership that drives promotion-PR detection's S4/S6 signals) — and skips
// Jira, the /search re-enumeration, and re-fetching the files/comments payloads
// the original crawl already stored.
//
// Gate: NeedsWork = merged && Commits == nil. PRs in months a prior full crawl
// already covered carry a non-nil Commits sentinel and are skipped, so this
// resumes/complements that work cleanly.
//
// NOT captured here (the "extras later" scope decision, 2026-06-15): per-file
// previous_filename/blob_sha, review-comment ids/lines, and commit-search
// parents. Those need /files + /comments + /search/commits re-fetches of data
// we already largely hold; add dedicated phases if a future signal needs them.
//
// Usage:
//
//	velocity-backfill-prmeta --qps 0.67           # ~1.5s between calls
//	velocity-backfill-prmeta --dry-run            # count, don't fetch
//	velocity-backfill-prmeta --limit 100          # exit after N PRs
//	velocity-backfill-prmeta --checkpoint 25      # persist every N PRs
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mathewepstein/velocity/internal/backfill"
	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
	"github.com/mathewepstein/velocity/internal/pull"
	"github.com/mathewepstein/velocity/internal/secrets"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type opts struct {
	qps             float64
	dryRun          bool
	limit           int
	checkpointEvery int
	rateCheckEvery  int
	minRemaining    int
	since           string
}

func parseFlags() opts {
	var o opts
	flag.Float64Var(&o.qps, "qps", 0.67, "Max requests per second to GitHub (default 0.67 ≈ 1.5s between calls; well under the 5000/hr authenticated limit)")
	flag.BoolVar(&o.dryRun, "dry-run", false, "Count merged PRs that need backfill; don't call GitHub")
	flag.IntVar(&o.limit, "limit", 0, "Stop after backfilling this many PRs (0 = no limit). Useful for smoke tests.")
	flag.IntVar(&o.checkpointEvery, "checkpoint", 25, "Persist the current month after every N backfilled PRs. Lower = less work lost on crash, more disk churn.")
	flag.IntVar(&o.rateCheckEvery, "rate-check", 50, "Poll GitHub's /rate_limit endpoint every N PRs and sleep until reset if remaining < min-remaining.")
	flag.IntVar(&o.minRemaining, "min-remaining", 500, "Pause until reset when remaining authenticated requests drops below this.")
	flag.StringVar(&o.since, "since", "", "Only process cache cells at or after this month (YYYY-MM). Empty = whole corpus (already-done PRs are skipped by the sentinel regardless).")
	flag.Parse()
	return o
}

func run() error {
	o := parseFlags()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	profile := cfg.ActiveProfile()

	ghToken := ""
	if !o.dryRun {
		ghToken, err = secrets.Get(config.DefaultProfile, "github")
		if err != nil {
			return fmt.Errorf("fetch github token (try `velocity auth set github`): %w", err)
		}
	}

	// /pulls/{n}/commits paginates for large PRs, so give the puller a small
	// per-page sleep; the adaptive governor still paces the bulk of calls.
	puller := pull.NewGithubPuller(profile.GitHub, ghToken, 500*time.Millisecond)

	store, err := cache.OpenStore()
	if err != nil {
		return err
	}
	defer store.Close()

	manifest, err := store.LoadManifest()
	if err != nil {
		return fmt.Errorf("load manifest: %w", err)
	}

	minInterval := time.Duration(0)
	if o.qps > 0 {
		minInterval = time.Duration(float64(time.Second) / o.qps)
	}
	gov := pull.NewRateGovernor(pull.GovernorConfig{
		Floor:       o.minRemaining,
		MinInterval: minInterval,
	})
	puller.UseGovernor(gov)

	phase := backfill.Phase[cache.GitHubPR]{
		Name:   "pr-meta",
		Source: cache.SourceGitHubPRs,
		NeedsWork: func(pr *cache.GitHubPR) bool {
			return pr.Merged != nil && pr.Commits == nil
		},
		Fetch: func(ctx context.Context, pr *cache.GitHubPR) (backfill.Outcome, error) {
			err := puller.HydratePRMeta(ctx, pr)
			switch {
			case err == nil:
				return backfill.Hydrated, nil
			case errors.Is(err, pull.ErrPRUnreachable):
				fmt.Printf("[perm-skip] %s#%d: %v\n", pr.Repo, pr.Number, err)
				return backfill.PermSkip, nil
			default:
				return backfill.AlreadyDone, err
			}
		},
		Read: func(s cache.Store, scope string, m cache.Month) ([]cache.GitHubPR, error) {
			return s.ReadGitHubPRs(scope, m)
		},
		Write: func(s cache.Store, scope string, m cache.Month, recs []cache.GitHubPR) error {
			return s.WriteGitHubPRs(scope, m, recs)
		},
	}

	runOpts := backfill.Options{
		DryRun:          o.dryRun,
		Limit:           o.limit,
		CheckpointEvery: o.checkpointEvery,
		Since:           o.since,
		Pace:            gov.Wait,
		RateCheckEvery:  o.rateCheckEvery,
		RateGuard: func(ctx context.Context) error {
			return waitForRateLimit(ctx, puller, o.minRemaining)
		},
	}

	st, err := backfill.Run(ctx, phase, manifest, store, runOpts)
	fmt.Printf("\nfinal: %d backfilled, %d unreachable (marked permanent), %d already-done, %d candidates seen\n",
		st.Hydrated, st.SkippedPerm, st.AlreadyDone, st.Candidates)
	return err
}

// waitForRateLimit pings /rate_limit and sleeps until reset if the authenticated
// remaining count has dipped below threshold — the proactive complement to the
// governor's header-based pacing and the client's reactive 429/5xx backoff.
func waitForRateLimit(ctx context.Context, p *pull.GithubPuller, minRemaining int) error {
	remaining, reset, err := p.RateLimitCore(ctx)
	if err != nil {
		fmt.Printf("[rate-check] ignored error: %v\n", err)
		return nil
	}
	if remaining >= minRemaining {
		return nil
	}
	wait := time.Until(reset) + 5*time.Second
	if wait < 0 {
		wait = 0
	}
	fmt.Printf("[rate-check] %d remaining < %d threshold; sleeping %s until reset\n", remaining, minRemaining, wait.Round(time.Second))
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
		return nil
	}
}
