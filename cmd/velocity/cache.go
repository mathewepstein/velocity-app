package main

import (
	"fmt"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/spf13/cobra"
)

func cacheCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Inspect and migrate the local cache substrate",
	}
	cmd.AddCommand(cacheMigrateCmd())
	return cmd
}

func cacheMigrateCmd() *cobra.Command {
	var db string
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Import the JSON-file corpus into a SQLite database (one-time, architecture Step 1b)",
		Long: `Import the month-partitioned JSON cache into a SQLite database.

Only needed if you ran an older build on the JSON backend and want to switch:
sqlite is the default now, so once the import is done just remove any
[cache] backend = "json" from your config (or leave it unset). The JSON corpus
is left untouched as a cold backup.

The destination is cleared first, so re-running is safe and idempotent.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			path, err := cache.SQLitePath(db)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "Migrating JSON cache → %s\n", path)
			st, err := cache.MigrateToSQLite(db, out)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "\nDone. %d cells (%d skipped — missing partitions).\n", st.Cells, st.Skipped)
			fmt.Fprintf(out, "  %d Jira issues, %d PRs, %d commits, %d reviews\n", st.Issues, st.PRs, st.Commits, st.Reviews)
			fmt.Fprintln(out, "\nNext: remove any `[cache] backend = \"json\"` from your config (sqlite is the default) and re-run `velocity analyze`.")
			return nil
		},
	}
	cmd.Flags().StringVar(&db, "db", "", "Destination database path (default: <data dir>/velocity.db)")
	return cmd
}
