// velocity-backfill hydrates per-record detail the base `velocity refresh`
// pull doesn't fetch, walking the month-partitioned cache via the shared
// internal/backfill runner. Phases:
//
//	--phase jira          one comprehensive per-issue pull (fields=*all):
//	                      description, changelog, comments, links/subtasks,
//	                      attachments, fix versions, the raw field catch-all, and
//	                      derived cycle-time / rework / pre-code signals
//	--phase pr-comments   per-merged-PR inline review comments (+ derived
//	                      inline-comment / deep-thread counts)
//	--phase file-changes  per-merged-PR per-file status + LOC (FileChanges)
//
// The phase definitions live in internal/detail, shared with the default
// post-pull detail stage of `velocity refresh`; this command exists for the
// one-time historical corpus fill and targeted re-runs.
//
// The job is idempotent and resumable: an interrupted run persists a checkpoint
// and the next run picks up the records still missing detail. Resolved Jira
// issues are hydrated once (their history is frozen); open issues are
// re-hydrated every run so their changelog/comments stay current.
//
// Usage:
//
//	velocity-backfill --phase jira-detail --since 2026-01      # recent months
//	velocity-backfill --phase jira-detail --scope CD --limit 50  # smoke test
//	velocity-backfill --phase jira-detail --dry-run            # count only
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
	"github.com/mathewepstein/velocity/internal/detail"
	"github.com/mathewepstein/velocity/internal/progress"
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
	phase           string
	qps             float64
	dryRun          bool
	limit           int
	checkpointEvery int
	rateCheckEvery  int
	minRemaining    int
	scope           string
	since           string
}

func parseFlags() opts {
	var o opts
	flag.StringVar(&o.phase, "phase", "", "Backfill phase to run: jira | pr-comments | file-changes")
	flag.Float64Var(&o.qps, "qps", 5, "Request-rate ceiling (req/s). The adaptive governor paces under it; Jira has no fixed limit so a high baseline is fine — a 429's Retry-After is the hard backstop.")
	flag.BoolVar(&o.dryRun, "dry-run", false, "Count records needing hydration; don't call the API")
	flag.IntVar(&o.limit, "limit", 0, "Stop after hydrating this many records (0 = no limit). Useful for smoke tests.")
	flag.IntVar(&o.checkpointEvery, "checkpoint", 25, "Persist the current month partition after every N hydrated records.")
	flag.IntVar(&o.rateCheckEvery, "rate-check", 50, "GitHub phases: poll /rate_limit every N records and sleep until reset if remaining < min-remaining. Ignored for jira-detail.")
	flag.IntVar(&o.minRemaining, "min-remaining", 500, "GitHub phases: governor floor + the /rate_limit pause threshold. Ignored for jira-detail.")
	flag.StringVar(&o.scope, "scope", "", "Restrict to one cache scope (Jira project key or repo name). Empty = all.")
	flag.StringVar(&o.since, "since", "", "Only process months >= this YYYY-MM. Empty = full corpus.")
	flag.Parse()
	return o
}

func run() error {
	o := parseFlags()
	if o.phase == "" {
		return errors.New("--phase is required (jira | pr-comments | file-changes)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

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

	switch o.phase {
	case "jira", "jira-detail", "jira-fields":
		// One comprehensive Jira hydration. jira-detail/jira-fields are kept as
		// aliases for the now-unified phase so existing invocations don't break.
		return runJiraPhase(ctx, cfg, manifest, store, o, minInterval, detail.JiraPhase)
	case "pr-comments":
		return runGithubPRPhase(ctx, cfg, manifest, store, o, minInterval, detail.PRCommentsPhase)
	case "file-changes":
		return runGithubPRPhase(ctx, cfg, manifest, store, o, minInterval, detail.FileChangesPhase)
	default:
		return fmt.Errorf("unknown --phase %q (supported: jira, pr-comments, file-changes)", o.phase)
	}
}

func runJiraPhase(ctx context.Context, cfg *config.Config, manifest *cache.Manifest, store cache.Store, o opts, minInterval time.Duration, buildPhase func(*pull.JiraPuller, detail.PrintfFunc) backfill.Phase[cache.JiraIssue]) error {
	profile := cfg.ActiveProfile()

	token := ""
	if !o.dryRun {
		var err error
		token, err = secrets.Get(config.DefaultProfile, "jira")
		if err != nil {
			return fmt.Errorf("fetch jira token (try `velocity auth set jira`): %w", err)
		}
	}

	bar := progress.New(os.Stdout)
	puller := pull.NewJiraPuller(profile.Jira, token, 0, 0)
	gov := pull.NewRateGovernor(pull.GovernorConfig{MinInterval: minInterval, Reporter: bar})
	puller.UseGovernor(gov)
	puller.SetReporter(bar)

	runOpts := backfill.Options{
		DryRun:          o.dryRun,
		Limit:           o.limit,
		CheckpointEvery: o.checkpointEvery,
		Pace:            gov.Wait,
		Scope:           o.scope,
		Since:           o.since,
		Reporter:        bar,
	}

	st, err := backfill.Run(ctx, buildPhase(puller, bar.Printf), manifest, store, runOpts)
	bar.Printf("\nfinal: %d hydrated, %d unreachable (marked fetched), %d already-done, %d candidates seen\n",
		st.Hydrated, st.SkippedPerm, st.AlreadyDone, st.Candidates)
	return err
}

// runGithubPRPhase wires the shared GitHub scaffolding (token, puller, adaptive
// governor, progress reporter, /rate_limit RateGuard, run options) and runs the
// phase that buildPhase produces.
func runGithubPRPhase(ctx context.Context, cfg *config.Config, manifest *cache.Manifest, store cache.Store, o opts, minInterval time.Duration, buildPhase func(*pull.GithubPuller, detail.PrintfFunc) backfill.Phase[cache.GitHubPR]) error {
	profile := cfg.ActiveProfile()

	token := ""
	if !o.dryRun {
		var err error
		token, err = secrets.Get(config.DefaultProfile, "github")
		if err != nil {
			return fmt.Errorf("fetch github token (try `velocity auth set github`): %w", err)
		}
	}

	bar := progress.New(os.Stdout)
	puller := pull.NewGithubPuller(profile.GitHub, token, 0)
	gov := pull.NewRateGovernor(pull.GovernorConfig{Floor: o.minRemaining, MinInterval: minInterval, Reporter: bar})
	puller.UseGovernor(gov)
	puller.SetReporter(bar)

	runOpts := backfill.Options{
		DryRun:          o.dryRun,
		Limit:           o.limit,
		CheckpointEvery: o.checkpointEvery,
		Pace:            gov.Wait,
		RateCheckEvery:  o.rateCheckEvery,
		RateGuard:       detail.GitHubRateGuard(puller, o.minRemaining, bar.Printf),
		Scope:           o.scope,
		Since:           o.since,
		Reporter:        bar,
	}

	st, err := backfill.Run(ctx, buildPhase(puller, bar.Printf), manifest, store, runOpts)
	bar.Printf("\nfinal: %d hydrated, %d unreachable (marked permanent), %d already-done, %d candidates seen\n",
		st.Hydrated, st.SkippedPerm, st.AlreadyDone, st.Candidates)
	return err
}
