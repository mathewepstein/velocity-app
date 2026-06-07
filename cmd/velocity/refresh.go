package main

import (
	"fmt"
	"time"

	"github.com/AlecAivazis/survey/v2"
	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
	"github.com/mathewepstein/velocity/internal/detail"
	"github.com/mathewepstein/velocity/internal/progress"
	"github.com/mathewepstein/velocity/internal/pull"
	"github.com/mathewepstein/velocity/internal/secrets"
	"github.com/spf13/cobra"
)

func refreshCmd() *cobra.Command {
	var since string
	var force bool
	var dryRun bool
	var reset bool
	var yes bool
	var sleepMS int
	var noDetail bool
	var fast bool
	cmd := &cobra.Command{
		Use:   "refresh",
		Short: "Pull missing months from Jira + GitHub into the local cache",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			profile := cfg.ActiveProfile()
			out := cmd.OutOrStdout()

			store, err := cache.OpenStore()
			if err != nil {
				return err
			}
			defer store.Close()

			if reset {
				if since == "" {
					fmt.Fprintln(out, "WARN: --reset without --since will backfill from config's backfill_start.")
					fmt.Fprintln(out, "      Org-wide pulls over many years can run for hours and exhaust API quota.")
					fmt.Fprintln(out, "      Pair with `--since YYYY-MM` (e.g. last 24 months) for a bounded run.")
				}
				if dryRun {
					fmt.Fprintln(out, "[dry-run] Would wipe cache (manifest, metrics, every source dir) before pulling.")
				} else {
					if !yes {
						confirm := false
						if err := survey.AskOne(&survey.Confirm{
							Message: "Reset will delete the entire on-disk cache and re-pull from the chosen window. Continue?",
							Default: false,
						}, &confirm); err != nil {
							return err
						}
						if !confirm {
							fmt.Fprintln(out, "Aborted.")
							return nil
						}
					}
					fmt.Fprintln(out, "Resetting cache...")
					if _, err := store.Reset(out); err != nil {
						return err
					}
				}
			}

			jiraToken, err := secrets.Get(config.DefaultProfile, "jira")
			if err != nil {
				return fmt.Errorf("fetch jira token (try `velocity auth set jira`): %w", err)
			}
			ghToken, err := secrets.Get(config.DefaultProfile, "github")
			if err != nil {
				return fmt.Errorf("fetch github token (try `velocity auth set github`): %w", err)
			}

			bar := progress.New(out)
			opts := pull.RefreshOptions{
				Force:             force,
				DryRun:            dryRun,
				SleepBetweenPages: time.Duration(sleepMS) * time.Millisecond,
				Out:               out,
				Reporter:          bar,
				Store:             store,
			}
			if since != "" {
				m, err := cache.ParseMonth(since)
				if err != nil {
					return err
				}
				opts.Since = &m
			}

			tokens := pull.Tokens{Jira: jiraToken, GitHub: ghToken}
			res, err := pull.Refresh(cmd.Context(), profile, tokens, opts)
			if err != nil {
				return err
			}

			// Detail hydration is a default phase of refresh (plan decision
			// B2): changelog/comments/description per Jira issue, review
			// comments + file changes per merged PR, scoped to the months the
			// base pull just visited. Cheap on routine runs — only new and
			// still-open records hydrate.
			if noDetail || fast {
				return nil
			}
			fmt.Fprintln(out, "\nDetail hydration (skip with --no-detail):")
			return detail.Hydrate(cmd.Context(), profile, tokens, detail.HydrateOptions{
				Since:  res.WindowStart.String(),
				DryRun: dryRun,
				Out:    out,
				Bar:    bar,
				Store:  store,
			})
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "Earliest month to pull (YYYY-MM). Defaults to config's backfill_start on first run. Recommended with --reset for bounded backfills.")
	cmd.Flags().BoolVar(&force, "force", false, "Re-pull months already cached")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show planned work without calling APIs")
	cmd.Flags().BoolVar(&reset, "reset", false, "Wipe the cache before pulling — required once after the pull query surface changes. Pair with --since to bound the post-reset backfill.")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt (only meaningful with --reset)")
	cmd.Flags().IntVar(&sleepMS, "sleep-between-pages", 1500, "Milliseconds to sleep between paginated GitHub/Jira requests (0 disables). Default keeps us under the 30/min Search API secondary limit.")
	cmd.Flags().BoolVar(&noDetail, "no-detail", false, "Skip the per-record detail hydration phase (Jira changelog/comments/description, PR review comments + file changes).")
	cmd.Flags().BoolVar(&fast, "fast", false, "Alias for --no-detail.")
	return cmd
}
