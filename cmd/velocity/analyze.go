package main

import (
	"fmt"
	"time"

	"github.com/mathewepstein/velocity/internal/analyze"
	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
	"github.com/spf13/cobra"
)

func analyzeCmd() *cobra.Command {
	var rebuild bool
	cmd := &cobra.Command{
		Use:   "analyze",
		Short: "Recompute metrics.json from the local cache (no network)",
		Long: `Recompute metrics.json from the local cache.

--rebuild drops the persisted Elo ratings.json and walks every completed
period from the earliest cached month forward. Use after a cache-extending
backfill (advanceRatings is forward-only past its last_period, so historical
periods added by a backfill would otherwise never enter the Elo computation).
Output may differ from incremental analyze on identical input — that's
intentional; the rebuild reflects the true state of the extended cache.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			profile := cfg.ActiveProfile()

			store, err := cache.OpenStore()
			if err != nil {
				return err
			}
			defer store.Close()

			result, err := analyze.Run(analyze.Options{
				Profile: profile,
				Now:     time.Now(),
				Rebuild: rebuild,
				Store:   store,
			})
			if err != nil {
				return err
			}
			path, err := analyze.WriteMetrics(result)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "Wrote %s\n", path)
			fmt.Fprintf(w, "  %d months of history, %d Jira issues, %d PRs, %d commits\n",
				result.Meta.MonthsLoaded, result.Meta.JiraIssuesLoaded, result.Meta.PRsLoaded, result.Meta.CommitsLoaded)
			fmt.Fprintf(w, "  %d projects detected\n", result.Meta.ProjectsDetected)
			fmt.Fprintf(w, "  current: %s (%d issues, %d PRs, %d commits in %d months)\n",
				result.Current.Label,
				result.Current.Totals.JiraIssuesTouched,
				result.Current.Totals.PRsCreated,
				result.Current.Totals.Commits,
				result.Current.Window.LengthMonths,
			)
			fmt.Fprintf(w, "  Elo-composite Spearman ρ: %.3f\n", result.Meta.EloCompositeSpearman)
			warnIfNoRoster(w, profile)
			return nil
		},
	}
	cmd.Flags().BoolVar(&rebuild, "rebuild", false, "drop ratings.json and walk every cached period from scratch")
	return cmd
}
