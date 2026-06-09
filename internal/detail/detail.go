// Package detail wires the lazy per-record hydration phases (Jira changelog +
// comments + description, PR inline review comments, PR file changes) into
// runnable units shared by the standalone velocity-backfill command and the
// default post-pull detail stage of `velocity refresh`.
//
// The phase definitions here are the single source of truth for each phase's
// candidate gate and fetch behavior; cmd/velocity-backfill and the refresh
// integration both run them through the internal/backfill runner.
package detail

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/mathewepstein/velocity/internal/backfill"
	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
	"github.com/mathewepstein/velocity/internal/progress"
	"github.com/mathewepstein/velocity/internal/pull"
)

// PrintfFunc writes a permanent output line (clearing any transient status
// line first — progress.Bar.Printf does this atomically).
type PrintfFunc func(format string, args ...interface{})

// JiraPhase is the single comprehensive per-issue hydration: one fields=*all
// pull capturing description, changelog, comments, relationships (subtasks +
// links), attachments, fix versions, and the raw field catch-all, then the
// derived cycle/rework/pre-code signals. NeedsWork re-hydrates any issue not yet
// fully captured (never detail-fetched, or no raw fields). It does NOT blanket-
// re-hydrate open issues: the base pull's mergeHydration carries forward the
// cached record for any issue whose `updated` hasn't advanced, so a stale issue
// stays fully captured and is skipped here, while a genuinely-changed one (its
// `updated` advanced → not carried forward → RawFields nil) is re-hydrated.
func JiraPhase(p *pull.JiraPuller, printf PrintfFunc) backfill.Phase[cache.JiraIssue] {
	return backfill.Phase[cache.JiraIssue]{
		Name:   "jira",
		Source: cache.SourceJira,
		NeedsWork: func(iss *cache.JiraIssue) bool {
			return !iss.DetailFetched || iss.RawFields == nil
		},
		Fetch: func(ctx context.Context, iss *cache.JiraIssue) (backfill.Outcome, error) {
			if err := p.HydrateIssue(ctx, iss, time.Now()); err != nil {
				if errors.Is(err, pull.ErrIssueUnreachable) {
					printf("[perm-skip] %s: %v\n", iss.Key, err)
					return backfill.PermSkip, nil
				}
				return backfill.AlreadyDone, err
			}
			return backfill.Hydrated, nil
		},
		Read: func(s cache.Store, scope string, m cache.Month) ([]cache.JiraIssue, error) {
			return s.ReadJiraIssues(scope, m)
		},
		Write: func(s cache.Store, scope string, m cache.Month, recs []cache.JiraIssue) error {
			return s.WriteJiraIssues(scope, m, recs)
		},
	}
}

// PRCommentsPhase hydrates per-merged-PR inline review comments (+ derived
// inline-comment count and deep-thread count).
func PRCommentsPhase(p *pull.GithubPuller, printf PrintfFunc) backfill.Phase[cache.GitHubPR] {
	return backfill.Phase[cache.GitHubPR]{
		Name:   "pr-comments",
		Source: cache.SourceGitHubPRs,
		NeedsWork: func(pr *cache.GitHubPR) bool {
			return pr.Merged != nil && pr.ReviewComments == nil
		},
		Fetch: func(ctx context.Context, pr *cache.GitHubPR) (backfill.Outcome, error) {
			return ghPermAwareOutcome(printf, pr, p.HydrateReviewComments(ctx, pr))
		},
		Read:  readGitHubPRs,
		Write: writeGitHubPRs,
	}
}

// FileChangesPhase hydrates per-merged-PR per-file status + LOC (FileChanges,
// the richer successor to Files).
func FileChangesPhase(p *pull.GithubPuller, printf PrintfFunc) backfill.Phase[cache.GitHubPR] {
	return backfill.Phase[cache.GitHubPR]{
		Name:   "file-changes",
		Source: cache.SourceGitHubPRs,
		NeedsWork: func(pr *cache.GitHubPR) bool {
			return pr.Merged != nil && pr.FileChanges == nil
		},
		Fetch: func(ctx context.Context, pr *cache.GitHubPR) (backfill.Outcome, error) {
			return ghPermAwareOutcome(printf, pr, p.HydratePRFileChanges(ctx, pr))
		},
		Read:  readGitHubPRs,
		Write: writeGitHubPRs,
	}
}

// readGitHubPRs / writeGitHubPRs are the shared cell I/O bound into every
// GitHub-PR-sourced backfill phase (pr-comments, file-changes), so the generic
// runner reaches the store's typed PR accessors.
func readGitHubPRs(s cache.Store, scope string, m cache.Month) ([]cache.GitHubPR, error) {
	return s.ReadGitHubPRs(scope, m)
}

func writeGitHubPRs(s cache.Store, scope string, m cache.Month, recs []cache.GitHubPR) error {
	return s.WriteGitHubPRs(scope, m, recs)
}

// ghPermAwareOutcome maps a Hydrate* call's error into a backfill.Outcome: a
// permanent ErrPRUnreachable (the hydrate has already persisted the empty
// sentinel) becomes a logged perm-skip; any other error stops the run.
func ghPermAwareOutcome(printf PrintfFunc, pr *cache.GitHubPR, err error) (backfill.Outcome, error) {
	switch {
	case err == nil:
		return backfill.Hydrated, nil
	case errors.Is(err, pull.ErrPRUnreachable):
		printf("[perm-skip] %s#%d: %v\n", pr.Repo, pr.Number, err)
		return backfill.PermSkip, nil
	default:
		return backfill.AlreadyDone, err
	}
}

// GitHubRateGuard returns a backfill RateGuard that polls /rate_limit and
// sleeps until reset when the authenticated remaining count dips below
// minRemaining — the proactive belt-and-suspenders alongside the governor and
// the reactive backoff.
func GitHubRateGuard(p *pull.GithubPuller, minRemaining int, printf PrintfFunc) func(context.Context) error {
	return func(ctx context.Context) error {
		remaining, reset, err := p.RateLimitCore(ctx)
		if err != nil {
			printf("[rate-check] ignored error: %v\n", err)
			return nil
		}
		if remaining >= minRemaining {
			return nil
		}
		wait := time.Until(reset) + 5*time.Second
		if wait < 0 {
			wait = 0
		}
		printf("[rate-check] %d remaining < %d threshold; sleeping %s until reset\n", remaining, minRemaining, wait.Round(time.Second))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
			return nil
		}
	}
}

// HydrateOptions tunes one Hydrate pass. Zero values get the same defaults the
// standalone velocity-backfill command uses.
type HydrateOptions struct {
	// Since restricts hydration to cache cells at or after this "YYYY-MM"
	// month — refresh passes its window start so detail covers exactly the
	// months the base pull just visited. Empty = full corpus.
	Since string
	// DryRun counts candidates without calling the APIs.
	DryRun bool
	// CheckpointEvery persists the current month partition after every N
	// hydrated records. 0 → 25.
	CheckpointEvery int
	// JiraQPS / GitHubQPS are the governor request-rate ceilings (req/s);
	// the adaptive governor paces under them. 0 → 5.
	JiraQPS, GitHubQPS float64
	// MinRemaining is the GitHub governor floor and the /rate_limit pause
	// threshold. 0 → 500.
	MinRemaining int
	// RateCheckEvery polls GitHub's /rate_limit every N records. 0 → 50.
	RateCheckEvery int
	// Out receives permanent output lines. Nil → os.Stdout. When Bar is set
	// it must write to the same stream.
	Out io.Writer
	// Bar, when set, renders the live status line; each lane (Jira, GitHub)
	// reports through its own Bar lane so the two compose on one line.
	Bar *progress.Bar
	// Store is the cache backend records + manifest are read/written through.
	// Nil defaults to the JSON store.
	Store cache.Store
}

func (o HydrateOptions) withDefaults() HydrateOptions {
	if o.CheckpointEvery == 0 {
		o.CheckpointEvery = 25
	}
	if o.JiraQPS == 0 {
		o.JiraQPS = 5
	}
	if o.GitHubQPS == 0 {
		o.GitHubQPS = 5
	}
	if o.MinRemaining == 0 {
		o.MinRemaining = 500
	}
	if o.RateCheckEvery == 0 {
		o.RateCheckEvery = 50
	}
	if o.Out == nil {
		o.Out = os.Stdout
	}
	if o.Store == nil {
		o.Store = cache.JSONStore{}
	}
	return o
}

// qpsInterval converts a req/s ceiling into the governor's MinInterval.
func qpsInterval(qps float64) time.Duration {
	if qps <= 0 {
		return 0
	}
	return time.Duration(float64(time.Second) / qps)
}

// Hydrate runs the detail-hydration phases over the cached months >= o.Since:
// jira-detail in one lane and pr-comments + file-changes in another, the two
// lanes concurrent because they spend separate API budgets (B3). Each lane is
// skipped when its token or scope config is absent — same rule as the base
// refresh. An interrupted run resumes on the next call because the underlying
// runner re-selects only records still needing work.
func Hydrate(ctx context.Context, profile config.Profile, t pull.Tokens, o HydrateOptions) error {
	o = o.withDefaults()

	manifest, err := o.Store.LoadManifest()
	if err != nil {
		return err
	}

	laneReporter := func() progress.Reporter {
		if o.Bar != nil {
			return o.Bar.Lane()
		}
		return progress.Nop()
	}
	printf := PrintfFunc(func(format string, args ...interface{}) {
		var rep progress.Reporter = progress.Nop()
		if o.Bar != nil {
			rep = o.Bar
		}
		progress.Printf(rep, o.Out, format, args...)
	})

	runOpts := backfill.Options{
		DryRun:          o.DryRun,
		CheckpointEvery: o.CheckpointEvery,
		Since:           o.Since,
		Out:             o.Out,
	}

	var wg sync.WaitGroup
	var jiraErr, ghErr error

	if t.Jira != "" && len(profile.Jira.Projects) > 0 {
		rep := laneReporter()
		puller := pull.NewJiraPuller(profile.Jira, t.Jira, 0, 0)
		gov := pull.NewRateGovernor(pull.GovernorConfig{MinInterval: qpsInterval(o.JiraQPS), Reporter: rep})
		puller.UseGovernor(gov)
		puller.SetReporter(rep)

		opts := runOpts
		opts.Pace = gov.Wait
		opts.Reporter = rep

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer rep.EnterCell("") // retire the lane from the status line
			st, err := backfill.Run(ctx, JiraPhase(puller, printf), manifest, o.Store, opts)
			printf("[jira] %d hydrated, %d unreachable, %d already-done\n",
				st.Hydrated, st.SkippedPerm, st.AlreadyDone)
			jiraErr = err
		}()
	}

	if t.GitHub != "" && len(profile.GitHub.Orgs) > 0 {
		rep := laneReporter()
		puller := pull.NewGithubPuller(profile.GitHub, t.GitHub, 0)
		gov := pull.NewRateGovernor(pull.GovernorConfig{Floor: o.MinRemaining, MinInterval: qpsInterval(o.GitHubQPS), Reporter: rep})
		puller.UseGovernor(gov)
		puller.SetReporter(rep)

		opts := runOpts
		opts.Pace = gov.Wait
		opts.Reporter = rep
		opts.RateCheckEvery = o.RateCheckEvery
		opts.RateGuard = GitHubRateGuard(puller, o.MinRemaining, printf)

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer rep.EnterCell("")
			// Sequential within the lane: both phases spend the same GitHub
			// budget, so running them concurrently would gain nothing.
			for _, ph := range []backfill.Phase[cache.GitHubPR]{
				PRCommentsPhase(puller, printf),
				FileChangesPhase(puller, printf),
			} {
				st, err := backfill.Run(ctx, ph, manifest, o.Store, opts)
				printf("[%s] %d hydrated, %d unreachable, %d already-done\n",
					ph.Name, st.Hydrated, st.SkippedPerm, st.AlreadyDone)
				if err != nil {
					ghErr = fmt.Errorf("%s: %w", ph.Name, err)
					return
				}
			}
		}()
	}

	wg.Wait()
	if jiraErr != nil {
		jiraErr = fmt.Errorf("jira-detail: %w", jiraErr)
	}
	return errors.Join(jiraErr, ghErr)
}
