// velocity-backfill-files hydrates the per-PR Files list on every merged
// GitHub PR in the local cache. Phase 6.0 needs that data for the
// `code_impact` composite metric; the regular `velocity refresh` flow only
// hydrates Files going forward, so historical PRs (~22k across the 5-year
// cache) get filled by this dedicated long-running job.
//
// The resume/checkpoint/signal/rate scaffolding lives in internal/backfill;
// this command is now just the GitHub-files phase wired into that shared
// runner. Design priorities (resume gracefully, distinguish transient from
// permanent failures, stay polite to GitHub) are documented there.
//
// Usage:
//
//	velocity-backfill-files --qps 0.67           # ~1.5s between calls
//	velocity-backfill-files --dry-run            # count, don't fetch
//	velocity-backfill-files --limit 100          # exit after N PRs
//	velocity-backfill-files --checkpoint 25      # persist every N PRs
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
}

func parseFlags() opts {
	var o opts
	flag.Float64Var(&o.qps, "qps", 0.67, "Max requests per second to GitHub (default 0.67 ≈ 1.5s between calls; well under the 5000/hr authenticated limit)")
	flag.BoolVar(&o.dryRun, "dry-run", false, "Count merged PRs that need hydration; don't call GitHub")
	flag.IntVar(&o.limit, "limit", 0, "Stop after hydrating this many PRs (0 = no limit). Useful for smoke tests.")
	flag.IntVar(&o.checkpointEvery, "checkpoint", 25, "Persist the current month's prs.json after every N hydrated PRs. Lower = less work lost on crash, more disk churn.")
	flag.IntVar(&o.rateCheckEvery, "rate-check", 50, "Poll GitHub's /rate_limit endpoint every N PRs and sleep until reset if remaining < min-remaining.")
	flag.IntVar(&o.minRemaining, "min-remaining", 500, "Pause until reset when remaining authenticated requests drops below this.")
	flag.Parse()
	return o
}

func run() error {
	o := parseFlags()

	// Wire SIGINT/SIGTERM to a context so an in-flight HTTP call can cancel
	// cleanly and the next checkpoint persists before we exit.
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

	puller := pull.NewGithubPuller(profile.GitHub, ghToken, 0 /* no per-page sleep needed; /files rarely paginates */)

	store, err := cache.OpenStore()
	if err != nil {
		return err
	}
	defer store.Close()

	manifest, err := store.LoadManifest()
	if err != nil {
		return fmt.Errorf("load manifest: %w", err)
	}

	// Adaptive governor: --qps is now the ceiling (minInterval), and the
	// governor paces under it by reading X-RateLimit headers off each response,
	// keeping --min-remaining in reserve. The fixed pre-call sleep is gone.
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
		Name:   "files",
		Source: cache.SourceGitHubPRs,
		NeedsWork: func(pr *cache.GitHubPR) bool {
			return pr.Merged != nil && pr.Files == nil
		},
		Fetch: func(ctx context.Context, pr *cache.GitHubPR) (backfill.Outcome, error) {
			files, ferr := puller.FetchPRFiles(ctx, pr.Repo, pr.Number)
			switch {
			case ferr == nil:
				if files == nil {
					files = []string{} // genuinely zero-file PR; persist non-nil so we don't retry
				}
				pr.Files = files
				return backfill.Hydrated, nil
			case errors.Is(ferr, pull.ErrPRUnreachable):
				pr.Files = []string{} // permanent: mark so it's never retried
				fmt.Printf("[perm-skip] %s#%d: %v\n", pr.Repo, pr.Number, ferr)
				return backfill.PermSkip, nil
			default:
				return backfill.AlreadyDone, ferr
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
		Pace:            gov.Wait,
		RateCheckEvery:  o.rateCheckEvery,
		RateGuard: func(ctx context.Context) error {
			return waitForRateLimit(ctx, puller, o.minRemaining)
		},
	}

	st, err := backfill.Run(ctx, phase, manifest, store, runOpts)
	fmt.Printf("\nfinal: %d hydrated, %d unreachable (marked permanent), %d already-done, %d candidates seen\n",
		st.Hydrated, st.SkippedPerm, st.AlreadyDone, st.Candidates)
	return err
}

// waitForRateLimit pings /rate_limit and sleeps until the reset time if the
// authenticated remaining count has dipped below threshold. The standard
// backoffClient catches 429/5xx after the fact; this is the proactive
// belt-and-suspenders check that prevents triggering a secondary limit in
// the first place.
func waitForRateLimit(ctx context.Context, p *pull.GithubPuller, minRemaining int) error {
	remaining, reset, err := p.RateLimitCore(ctx)
	if err != nil {
		// Don't fail the whole run on a rate-limit-check failure; it's an
		// optimization, not a correctness requirement.
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
