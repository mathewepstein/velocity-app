package main

import (
	"os/signal"
	"syscall"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
	"github.com/mathewepstein/velocity/internal/pull"
	"github.com/mathewepstein/velocity/internal/scoring"
	"github.com/mathewepstein/velocity/internal/secrets"
	"github.com/mathewepstein/velocity/internal/server"
	"github.com/spf13/cobra"
)

func serveCmd() *cobra.Command {
	var port int
	var open bool
	var incognito bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve the embedded web UI from the local cache",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()
			// Config powers the "Me" nav link's destination; a missing or
			// unconfigured profile is fine — the nav falls back to /dev/ and
			// the page still renders.
			selfLogin := ""
			var profile config.Profile
			if cfg, err := config.Load(); err == nil {
				profile = cfg.ActiveProfile()
				selfLogin = profile.GitHub.Username
			}
			store, err := cache.OpenStore()
			if err != nil {
				return err
			}
			defer store.Close()
			scoreStore, err := scoring.OpenScoreStore("")
			if err != nil {
				return err
			}
			defer scoreStore.Close()
			warnIfNoRoster(cmd.OutOrStdout(), profile)
			// Build the Jira write-back poster only when a token is in the
			// keychain. Absent token → nil poster: the dashboard still serves and
			// dry-run previews work; only live posting (/api/scoring/post with
			// dry_run:false) is unavailable, and jira-status reports why.
			var poster scoring.JiraPoster
			if tok, err := secrets.Get(config.DefaultProfile, "jira"); err == nil && tok != "" {
				poster = pull.NewJiraWriter(profile.Jira, tok)
			}
			return server.Serve(ctx, server.Options{
				Port:       port,
				Open:       open,
				Out:        cmd.OutOrStdout(),
				SelfLogin:  selfLogin,
				Incognito:  incognito,
				Profile:    profile,
				Store:      store,
				ScoreStore: scoreStore,
				Poster:     poster,
			})
		},
	}
	cmd.Flags().IntVar(&port, "port", 8000, "Port to serve on (0 → OS-assigned)")
	cmd.Flags().BoolVar(&open, "open", false, "Open the dashboard in the default browser")
	cmd.Flags().BoolVar(&incognito, "incognito", false, "Anonymize dev/epic identities in responses (persisted in incognito-names.json)")
	return cmd
}
