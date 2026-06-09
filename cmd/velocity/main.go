// Command velocity is a personal engineering velocity dashboard that pulls
// Jira + GitHub activity into a local cache and serves a web UI showing
// trends, comparisons, and project surges.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// version is stamped in at build time via -ldflags.
var version = "dev"

func main() {
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:     "velocity",
		Short:   "Personal engineering velocity dashboard",
		Long:    "Velocity pulls your Jira + GitHub activity into a local cache and serves a dashboard showing trends, comparisons, and project surges.",
		Version: version,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Default: refresh current month + serve + open browser.
			// Wired in a later phase.
			return fmt.Errorf("default command not yet implemented; try `velocity --help`")
		},
	}

	root.AddCommand(
		initCmd(),
		refreshCmd(),
		serveCmd(),
		analyzeCmd(),
		authCmd(),
		doctorCmd(),
		devsCmd(),
		cacheCmd(),
		scoreCmd(),
		jiraCmd(),
	)
	return root
}
