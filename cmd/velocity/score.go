package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/mathewepstein/velocity/internal/analyze"
	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
	"github.com/mathewepstein/velocity/internal/pull"
	"github.com/mathewepstein/velocity/internal/scoring"
	"github.com/mathewepstein/velocity/internal/secrets"
	"github.com/spf13/cobra"
)

// scoreDBPath optionally overrides the scores database path (default: the
// shared velocity.db). Persistent flag on `score`, honored by the subcommands
// that touch the scores store.
var scoreDBPath string

func scoreCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "score",
		Short: "Story-points engine: build evidence + deterministic bands from the cache",
	}
	cmd.PersistentFlags().StringVar(&scoreDBPath, "db", "", "scores database path (default: <data dir>/velocity.db)")
	cmd.AddCommand(scoreEvidenceCmd())
	cmd.AddCommand(scoreBandCmd())
	cmd.AddCommand(scoreGenerateCmd())
	cmd.AddCommand(scoreListCmd())
	cmd.AddCommand(scoreExportCmd())
	cmd.AddCommand(scoreCalibrateCmd())
	cmd.AddCommand(scoreRiskDiscoverCmd())
	cmd.AddCommand(scorePostCmd())
	return cmd
}

func scoreRiskDiscoverCmd() *cobra.Command {
	var (
		minTickets   int
		depth        int
		top          int
		seedKeywords bool
		jsonOut      bool
	)
	cmd := &cobra.Command{
		Use:   "risk-discover",
		Short: "Propose a [storypoints.risk] domain-risk config from the corpus (read-only)",
		Long: `Sweep the cached corpus and propose directories worth flagging as
high/medium domain risk, ranked by outcome-correlation — areas whose tickets
historically cost more (active cycle + rework) than the corpus baseline —
blended with a structural migration-directory detector (db/changelog,
migrations, liquibase). Empirical and org-agnostic: no opinions are baked in.

Prints a paste-ready [storypoints.risk] TOML block plus the evidence behind each
proposal. It does NOT write config (mirrors 'devs discover') — review, curate,
paste into config.toml, then re-run 'score generate'. Run after 'refresh'.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, store, ext, err := loadCorpus()
			if err != nil {
				return err
			}
			defer store.Close()

			res := scoring.RiskDiscover(ext, scoring.RiskDiscoverOpts{
				MinTickets:   minTickets,
				Depth:        depth,
				Top:          top,
				SeedKeywords: seedKeywords,
			})

			w := cmd.OutOrStdout()
			if jsonOut {
				enc := json.NewEncoder(w)
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}

			fmt.Fprintf(w, "Scanned %d post-hoc tickets · corpus baseline: cycle %.1fd, rework %.0f%%\n\n",
				res.TicketsScanned, res.BaselineCycle, res.BaselineRework*100)
			if len(res.Candidates) == 0 {
				fmt.Fprintln(w, "No directories cleared the risk thresholds. Lower --min-tickets or try --seed-keywords.")
				return nil
			}

			var high, medium []string
			for _, c := range res.Candidates {
				if c.Tier == "high" {
					high = append(high, c.Glob)
				} else {
					medium = append(medium, c.Glob)
				}
			}
			fmt.Fprintln(w, "Paste-ready (curate before using):")
			fmt.Fprintln(w, "[profiles.default.storypoints.risk]")
			fmt.Fprintf(w, "high   = [%s]\n", quoteList(high))
			fmt.Fprintf(w, "medium = [%s]\n\n", quoteList(medium))

			fmt.Fprintln(w, "Evidence:")
			for _, c := range res.Candidates {
				note := ""
				if c.Migration {
					note = "  (migration dir)"
				} else if c.Seed {
					note = "  (seed-keyword suggestion)"
				}
				fmt.Fprintf(w, "  %-44s %3d tickets · cycle %.1fd (%.1f×) · rework %.0f%% (%.1f×)  → %s%s\n",
					c.Glob, c.Tickets, c.CycleDays, c.CycleRatio, c.ReworkRate*100, c.ReworkRatio, c.Tier, note)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&minTickets, "min-tickets", 5, "minimum tickets touching a directory for it to be ranked")
	cmd.Flags().IntVar(&depth, "depth", 3, "in-repo path depth at which directories are grouped")
	cmd.Flags().IntVar(&top, "top", 0, "keep only the top-N ranked directories (0 = all)")
	cmd.Flags().BoolVar(&seedKeywords, "seed-keywords", false, "also surface directories whose path contains a generic sensitive term (auth/billing/credit/…)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit the full proposal as JSON")
	return cmd
}

// quoteList renders a slice of globs as a TOML inline-array body (quoted,
// comma-separated). Empty slice → "" so the array reads `[]`.
func quoteList(items []string) string {
	if len(items) == 0 {
		return ""
	}
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(q, ", ")
}

func scoreCalibrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "calibrate",
		Short: "Measure the active-cycle distribution + band/flag rates under candidate cycle thresholds",
		Long: `Read-only calibration aid. Reports the active-cycle-days distribution over
post-hoc tickets, and the band distribution + needs-insight flag rate the engine
would produce at several candidate cycle_days_threshold values — so the
[storypoints] default can be chosen from the data, not guessed.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			profile, store, ext, err := loadCorpus()
			if err != nil {
				return err
			}
			defer store.Close()

			// Collect post-hoc evidence (resolved or has PRs).
			var evs []*scoring.TicketEvidence
			var activeDays, rawDays []float64
			for _, key := range ext.Keys() {
				ev, ok := ext.Extract(key)
				if !ok || len(ev.PRs) == 0 {
					continue
				}
				evs = append(evs, ev)
				if ev.ActiveCycleHours > 0 {
					activeDays = append(activeDays, ev.ActiveCycleHours/24)
				}
				if ev.CycleHours > 0 {
					rawDays = append(rawDays, ev.CycleHours/24)
				}
			}

			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "Post-hoc tickets: %d (with cycle data: raw %d / active %d)\n\n", len(evs), len(rawDays), len(activeDays))
			fmt.Fprintln(w, "Cycle-days distribution (days):")
			fmt.Fprintf(w, "  %-8s p25=%.1f  p50=%.1f  p75=%.1f  p90=%.1f  p95=%.1f\n", "raw", pct(rawDays, 25), pct(rawDays, 50), pct(rawDays, 75), pct(rawDays, 90), pct(rawDays, 95))
			fmt.Fprintf(w, "  %-8s p25=%.1f  p50=%.1f  p75=%.1f  p90=%.1f  p95=%.1f\n\n", "active", pct(activeDays, 25), pct(activeDays, 50), pct(activeDays, 75), pct(activeDays, 90), pct(activeDays, 95))

			fmt.Fprintln(w, "Band + flag rate under candidate cycle_days_threshold:")
			fmt.Fprintf(w, "  %-7s %-26s %-9s %-9s\n", "thresh", "band distribution", "flagged", "confident")
			for _, th := range []float64{2, 5, 7, 10, 14, 21} {
				cfg := profile.StoryPoints
				cfg.CycleDaysThreshold = th
				dist := map[string]int{}
				flagged, confident := 0, 0
				for _, ev := range evs {
					b := scoring.Band(ev, cfg)
					dist[b.Band]++
					if b.NeedsInsight {
						flagged++
					}
					if b.Confidence == "high" {
						confident++
					}
				}
				fmt.Fprintf(w, "  %-7.0f %-26s %-9s %-9s\n", th, distString(dist), pctStr(flagged, len(evs)), pctStr(confident, len(evs)))
			}
			return nil
		},
	}
	return cmd
}

func pct(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	idx := int(p / 100 * float64(len(s)-1))
	return s[idx]
}

func distString(d map[string]int) string {
	order := []string{"1", "1–2", "2", "2–3", "3", "3–5", "5", "5–8", "8", "8–13", "13"}
	parts := []string{}
	for _, b := range order {
		if d[b] > 0 {
			parts = append(parts, fmt.Sprintf("%s:%d", b, d[b]))
		}
	}
	return strings.Join(parts, " ")
}

func pctStr(n, total int) string {
	if total == 0 {
		return "0%"
	}
	return fmt.Sprintf("%d%%", n*100/total)
}

// loadCorpus opens the store and loads the full corpus + builds an extractor.
// Shared by the score subcommands. Caller must Close the returned store.
func loadCorpus() (config.Profile, cache.Store, *scoring.Extractor, error) {
	cfg, err := config.Load()
	if err != nil {
		return config.Profile{}, nil, nil, err
	}
	profile := cfg.ActiveProfile()
	store, err := cache.OpenStore()
	if err != nil {
		return profile, nil, nil, err
	}
	data, err := analyze.LoadAll(profile, cache.CurrentMonth(time.Now()), store)
	if err != nil {
		store.Close()
		return profile, nil, nil, err
	}
	return profile, store, scoring.NewExtractor(data, profile.Scoring.Normalize, profile.StoryPoints.ReworkMinDwell(), profile.StoryPoints.Risk), nil
}

func scoreGenerateCmd() *cobra.Command {
	var (
		tickets []string
		all     bool
		limit   int
	)
	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Compute deterministic bands and persist them to the scores store",
		Long: `Run the band engine over cached tickets and upsert the results into the
scores table (in velocity.db). Idempotent: a ticket whose evidence is unchanged
since the last run is skipped, and any human override is preserved (only its
auto-derived columns are refreshed).

By default only resolved (post-hoc) tickets are scored, matching the rubric's
"after the PR has merged" stance. Use --all to include open tickets, or
--ticket KEY (repeatable) to score specific tickets.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			profile, store, ext, err := loadCorpus()
			if err != nil {
				return err
			}
			defer store.Close()

			ss, err := scoring.OpenScoreStore(scoreDBPath)
			if err != nil {
				return err
			}
			defer ss.Close()

			keys := tickets
			if len(keys) == 0 {
				keys = ext.Keys()
			}

			now := time.Now()
			var tally struct{ inserted, updated, skipped, preserved, flagged, scored int }
			for _, key := range keys {
				ev, ok := ext.Extract(key)
				if !ok {
					if len(tickets) > 0 {
						fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s not found in cache\n", key)
					}
					continue
				}
				// Default scope is post-hoc code work: tickets with ≥1 matched
				// merged PR (the rubric is "after the PR merged"). --all also
				// scores resolved tickets that have no linked PR.
				if !all && len(tickets) == 0 && len(ev.PRs) == 0 {
					continue
				}
				band := scoring.Band(ev, profile.StoryPoints)
				outcome, err := ss.SaveAuto(scoring.NewAutoRecord(ev, band, now))
				if err != nil {
					return fmt.Errorf("save %s: %w", key, err)
				}
				tally.scored++
				if band.NeedsInsight {
					tally.flagged++
				}
				switch outcome {
				case scoring.OutcomeInserted:
					tally.inserted++
				case scoring.OutcomeUpdated:
					tally.updated++
				case scoring.OutcomeSkipped:
					tally.skipped++
				case scoring.OutcomePreserved:
					tally.preserved++
				}
				if limit > 0 && tally.scored >= limit {
					break
				}
			}

			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "Scored %d tickets: %d new, %d updated, %d unchanged, %d human-preserved\n",
				tally.scored, tally.inserted, tally.updated, tally.skipped, tally.preserved)
			fmt.Fprintf(w, "  %d flagged needs-insight\n", tally.flagged)
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&tickets, "ticket", nil, "score only these ticket keys (repeatable)")
	cmd.Flags().BoolVar(&all, "all", false, "include open (unresolved) tickets")
	cmd.Flags().IntVar(&limit, "limit", 0, "stop after scoring N tickets (for testing)")
	return cmd
}

func scorePostCmd() *cobra.Command {
	var (
		tickets   []string
		confident bool
		confirm   bool
		limit     int
	)
	cmd := &cobra.Command{
		Use:   "post",
		Short: "Write persisted scores back to Jira (story points field + calibration comment)",
		Long: `Post the deterministic/overridden story points and one calibration comment to
Jira for the selected tickets. Safe by default: without --confirm this is a
dry-run that prints the exact comment for each ticket and writes nothing.

Select tickets with --ticket KEY (repeatable) or --confident (every confident,
unflagged, not-yet-posted ticket that has no existing Jira story points). Posting
is idempotent — a ticket already marked posted_to_jira is skipped. A live run
first verifies the token can edit issues + add comments, and aborts if not.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(tickets) == 0 && !confident {
				return fmt.Errorf("specify --ticket KEY (repeatable) or --confident")
			}
			ss, err := scoring.OpenScoreStore(scoreDBPath)
			if err != nil {
				return err
			}
			defer ss.Close()

			keys := tickets
			if confident {
				recs, err := ss.List(scoring.ScoreFilter{})
				if err != nil {
					return err
				}
				for _, r := range recs {
					if r.NeedsInsight || r.PostedToJira || r.ExistingStoryPoints != 0 {
						continue
					}
					keys = append(keys, r.Ticket)
					if limit > 0 && len(keys) >= limit {
						break
					}
				}
				if len(keys) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "no confident, unflagged, unposted tickets without existing story points")
					return nil
				}
			}

			dryRun := !confirm
			var poster scoring.JiraPoster
			if !dryRun {
				cfg, err := config.Load()
				if err != nil {
					return err
				}
				tok, err := secrets.Get(config.DefaultProfile, "jira")
				if err != nil {
					return fmt.Errorf("fetch jira token (try `velocity auth set jira`): %w", err)
				}
				writer := pull.NewJiraWriter(cfg.ActiveProfile().Jira, tok)
				if err := writer.VerifyWriteScope(cmd.Context()); err != nil {
					return fmt.Errorf("jira write-scope check failed: %w", err)
				}
				poster = writer
			}

			rep, err := scoring.PostScores(cmd.Context(), ss, poster, scoring.PostOptions{
				Tickets: keys,
				DryRun:  dryRun,
			})
			if err != nil {
				return err
			}
			printPostReport(cmd.OutOrStdout(), rep)
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&tickets, "ticket", nil, "post only these ticket keys (repeatable)")
	cmd.Flags().BoolVar(&confident, "confident", false, "post every confident, unflagged, unposted ticket with no existing Jira story points")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "perform the live write (without this it is a dry-run preview)")
	cmd.Flags().IntVar(&limit, "limit", 0, "with --confident, cap the number of tickets selected")
	return cmd
}

// printPostReport renders a PostScores report: each dry-run preview shows the
// full comment; live/skip/error lines are one apiece; a final tally closes it.
func printPostReport(w io.Writer, rep scoring.PostReport) {
	for _, r := range rep.Results {
		switch r.Action {
		case scoring.ActionPreview:
			fmt.Fprintf(w, "\n[dry-run] %s → %d points\n", r.Ticket, r.Points)
			for _, ln := range r.Comment {
				fmt.Fprintf(w, "    %s\n", ln)
			}
		case scoring.ActionPosted:
			fmt.Fprintf(w, "posted   %s → %d points\n", r.Ticket, r.Points)
		case scoring.ActionAlreadyPosted:
			fmt.Fprintf(w, "skipped  %s (already posted)\n", r.Ticket)
		case scoring.ActionNoRow:
			fmt.Fprintf(w, "skipped  %s (no persisted score — run `velocity score generate`)\n", r.Ticket)
		case scoring.ActionError:
			fmt.Fprintf(w, "ERROR    %s: %s\n", r.Ticket, r.Error)
		}
	}
	mode := "live"
	if rep.DryRun {
		mode = "dry-run"
	}
	fmt.Fprintf(w, "\n%s: %d posted, %d previewed, %d already-posted, %d no-score, %d errors\n",
		mode, rep.Posted, rep.Previewed, rep.AlreadyPosted, rep.NoRow, rep.Errors)
}

func scoreListCmd() *cobra.Command {
	var (
		needsInsight bool
		limit        int
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List persisted scores from the store",
		RunE: func(cmd *cobra.Command, args []string) error {
			ss, err := scoring.OpenScoreStore(scoreDBPath)
			if err != nil {
				return err
			}
			defer ss.Close()

			recs, err := ss.List(scoring.ScoreFilter{NeedsInsightOnly: needsInsight, Limit: limit})
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if len(recs) == 0 {
				fmt.Fprintln(w, "no scores yet — run `velocity score generate`")
				return nil
			}
			for _, r := range recs {
				flag := ""
				if r.NeedsInsight {
					flag = " [needs-insight]"
				}
				posted := ""
				if r.PostedToJira {
					posted = " (posted)"
				}
				src := ""
				if r.Source == scoring.SourceHuman {
					src = fmt.Sprintf(" (human override; auto was %d)", r.AutoPoints)
				}
				fmt.Fprintf(w, "%-12s %-6s pts=%-2d %-8s%s%s%s\n",
					r.Ticket, r.Band, r.Points, r.Confidence, flag, posted, src)
			}
			fmt.Fprintf(w, "\n%d scores\n", len(recs))
			return nil
		},
	}
	cmd.Flags().BoolVar(&needsInsight, "needs-insight", false, "only show tickets flagged for a human/LLM pass")
	cmd.Flags().IntVar(&limit, "limit", 0, "limit rows")
	return cmd
}

func scoreExportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Dump all persisted scores as JSON (shareable bundle for scorer comparison)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ss, err := scoring.OpenScoreStore(scoreDBPath)
			if err != nil {
				return err
			}
			defer ss.Close()

			recs, err := ss.List(scoring.ScoreFilter{})
			if err != nil {
				return err
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(recs)
		},
	}
	return cmd
}

func scoreBandCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "band TICKET-KEY",
		Short: "Compute the deterministic story-points band for one ticket",
		Long: `Extract the evidence bundle for a ticket and run the deterministic band
engine over it. Prints the picked Fibonacci band, confidence, whether it needs
a human/LLM insight pass, the quadrant prior, and the top drivers. Reads the
cache only — no API calls, no LLM.`,
		Args: cobra.ExactArgs(1),
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

			data, err := analyze.LoadAll(profile, cache.CurrentMonth(time.Now()), store)
			if err != nil {
				return err
			}

			ext := scoring.NewExtractor(data, profile.Scoring.Normalize, profile.StoryPoints.ReworkMinDwell(), profile.StoryPoints.Risk)
			ev, ok := ext.Extract(args[0])
			if !ok {
				return fmt.Errorf("ticket %s not found in cache", args[0])
			}
			band := scoring.Band(ev, profile.StoryPoints)

			w := cmd.OutOrStdout()
			if jsonOut {
				enc := json.NewEncoder(w)
				enc.SetIndent("", "  ")
				return enc.Encode(band)
			}

			flag := ""
			if band.NeedsInsight {
				flag = "  [needs insight — run /score-ticket " + ev.Key + "]"
			}
			fmt.Fprintf(w, "%s  %s\n", ev.Key, ev.Summary)
			fmt.Fprintf(w, "Band: %s (points %d, confidence %s)%s\n", band.Band, band.Points, band.Confidence, flag)
			fmt.Fprintf(w, "Quadrant: %s → prior %s | raw effort %.1f\n", band.QuadrantCell, band.QuadrantBand, band.RawEffort)
			fmt.Fprintf(w, "Hardest aspect: %s\n", band.HardestAspectHint)
			if len(band.Drivers) > 0 {
				fmt.Fprintln(w, "Drivers:")
				for _, d := range band.Drivers {
					fmt.Fprintf(w, "  - %s\n", d)
				}
			}
			fmt.Fprintf(w, "Signals: %s\n", band.SignalSummary)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit the full BandResult as JSON")
	return cmd
}

func scoreEvidenceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "evidence TICKET-KEY",
		Short: "Print the evidence bundle for one ticket (no scoring, no network)",
		Long: `Assemble and print the TicketEvidence bundle for a Jira ticket from the
local cache: Jira fields + derived cycle/rework signals, matched PRs, review
rounds, net LOC, touched-area risk. Reads the cache only — no API calls.

This is the data-provider surface of the story-points engine; the band stage
and any external scorer consume the same bundle.`,
		Args: cobra.ExactArgs(1),
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

			data, err := analyze.LoadAll(profile, cache.CurrentMonth(time.Now()), store)
			if err != nil {
				return err
			}

			ext := scoring.NewExtractor(data, profile.Scoring.Normalize, profile.StoryPoints.ReworkMinDwell(), profile.StoryPoints.Risk)
			ev, ok := ext.Extract(args[0])
			if !ok {
				return fmt.Errorf("ticket %s not found in cache", args[0])
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(ev)
		},
	}
	return cmd
}
