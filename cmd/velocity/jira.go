package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mathewepstein/velocity/internal/config"
	"github.com/mathewepstein/velocity/internal/jirafields"
	"github.com/mathewepstein/velocity/internal/secrets"
	"github.com/spf13/cobra"
)

func jiraCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "jira",
		Short: "Jira instance introspection helpers",
	}
	cmd.AddCommand(jiraFieldsCmd())
	return cmd
}

func jiraFieldsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fields",
		Short: "Inspect this instance's Jira fields",
	}
	cmd.AddCommand(jiraFieldsDiscoverCmd())
	return cmd
}

func jiraFieldsDiscoverCmd() *cobra.Command {
	var (
		tickets int
		sleepMS int
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:   "discover",
		Short: "Propose the [jira.fields] custom-field mapping from recent tickets (read-only)",
		Long: `Sample recently-updated issues from the configured project(s), tally which
fields are actually populated, and propose the custom-field mapping that ingest
needs — which custom field holds the story points, the epic link, and the real
description (often a custom "Description" field, not the sparse standard one).

Prints a paste-ready [profiles.default.jira.fields] block plus the populated
fields it could not map (capture-worthy suggestions vs. service-desk/HR noise)
and the full population evidence. It does NOT write config (mirrors 'devs
discover' / 'score risk-discover') — review, curate, paste into config.toml.

Only story_points, epic_link, and description are proposed as named mappings —
the only signals with a consumer in the engine today. Everything else is shown
as evidence so you can decide what the upcoming field-capture backfill should
grab.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			profile := cfg.ActiveProfile()
			jc := profile.Jira
			if jc.BaseURL == "" || jc.Email == "" {
				return fmt.Errorf("jira base_url/email not configured (run `velocity init`)")
			}
			if len(jc.Projects) == 0 {
				return fmt.Errorf("no jira projects configured (run `velocity init`)")
			}

			token, err := secrets.Get(config.DefaultProfile, "jira")
			if err != nil {
				return fmt.Errorf("fetch jira token (try `velocity auth set jira`): %w", err)
			}

			client := jirafields.NewClient(jc.BaseURL, jc.Email, token, 0)
			rep, err := client.Discover(cmd.Context(), jc.Projects, tickets, time.Duration(sleepMS)*time.Millisecond)
			if err != nil {
				return err
			}

			w := cmd.OutOrStdout()
			if jsonOut {
				enc := json.NewEncoder(w)
				enc.SetIndent("", "  ")
				return enc.Encode(rep)
			}
			printFieldReport(cmd, rep)
			return nil
		},
	}
	cmd.Flags().IntVar(&tickets, "tickets", 20, "number of recently-updated issues to sample")
	cmd.Flags().IntVar(&sleepMS, "sleep-ms", 200, "milliseconds to sleep between per-issue fetches (0 disables)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit the full report as JSON")
	return cmd
}

func printFieldReport(cmd *cobra.Command, rep *jirafields.Report) {
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "Sampled %d recently-updated issue(s).\n\n", rep.TicketsScanned)

	fmt.Fprintln(w, "Paste-ready (curate before using):")
	fmt.Fprintln(w, "[profiles.default.jira.fields]")
	for _, p := range rep.Proposed {
		fmt.Fprintf(w, "%-13s = %-20q # %s\n", p.Canonical, p.FieldID, p.Reason)
	}

	if len(rep.Extra) > 0 {
		fmt.Fprintln(w, "\nOther populated custom fields (no engine consumer yet — capture candidates):")
		fmt.Fprintln(w, "# [profiles.default.jira.fields.extra]")
		for _, s := range rep.Extra {
			fmt.Fprintf(w, "# %-26s # %q · %s · %d/%d populated\n", sanitizeKey(s.Name)+" = "+quote(s.ID), s.Name, s.Shape, s.Populated, s.Sampled)
		}
	}

	if len(rep.Denylisted) > 0 {
		fmt.Fprintf(w, "\nExcluded as service-desk/HR/finance noise (%d): ", len(rep.Denylisted))
		names := make([]string, 0, len(rep.Denylisted))
		for _, s := range rep.Denylisted {
			names = append(names, s.Name)
		}
		fmt.Fprintln(w, joinTrunc(names, 8))
	}

	fmt.Fprintln(w, "\nPopulation evidence (all fields seen, most-used first):")
	for _, s := range rep.Stats {
		tag := "standard"
		if s.Custom {
			tag = s.ID
		}
		fmt.Fprintf(w, "  %-28s %-10s %3d/%-3d  %s\n", trunc(s.Name, 28), tag, s.Populated, s.Sampled, s.Shape)
	}
}

func quote(s string) string { return fmt.Sprintf("%q", s) }

// sanitizeKey turns a field name into a plausible TOML key for the extra block
// (lowercase, spaces→underscores). It's only a comment suggestion; the operator
// names the canonical key.
func sanitizeKey(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z':
			out = append(out, r+('a'-'A'))
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, r)
		case r == ' ', r == '-', r == '_':
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "field"
	}
	return string(out)
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func joinTrunc(items []string, max int) string {
	if len(items) <= max {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:max], ", ") + fmt.Sprintf(", … (+%d more)", len(items)-max)
}
