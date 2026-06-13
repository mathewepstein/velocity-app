package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/AlecAivazis/survey/v2"
	"github.com/AlecAivazis/survey/v2/terminal"
	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
	"github.com/mathewepstein/velocity/internal/devs"
	"github.com/mathewepstein/velocity/internal/secrets"
	"github.com/spf13/cobra"
)

// devsCmd groups the developer-identity management commands. v1 ships
// `discover`; future revisions will likely add `list`, `add`, and `remove`.
func devsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "devs",
		Short: "Manage the developer identity table ([[devs]] in config.toml)",
	}
	cmd.AddCommand(devsDiscoverCmd())
	return cmd
}

// warnIfNoRoster nudges toward `velocity devs discover` when the contributor
// identity table is empty. Without it the per-dev rollups have nobody to
// attribute work to, so the leaderboard / contributors / velocity pages render
// empty — the single most common "fresh setup looks broken" trap.
func warnIfNoRoster(w io.Writer, profile config.Profile) {
	if len(profile.Devs) > 0 {
		return
	}
	fmt.Fprintln(w, "Note: no [[devs]] mapped yet — the leaderboard and contributor views will be empty.")
	fmt.Fprintln(w, "      Run `velocity devs discover` (after `velocity refresh`) to build the roster.")
}

// Sentinel selections used in the interactive prompt list. Survey returns the
// raw option string; these constants keep the branch logic readable.
const (
	choiceSkip           = "[skip — leave unmapped]"
	choiceJiraOnly       = "[map as Jira-only entry (no GitHub login)]"
	choiceGitHubOnly     = "[keep GitHub login only (no Jira accountId)]"
	choiceBrowseAll      = "[browse all Jira users — type to filter]"
	choiceAttachExisting = "[attach to already-mapped person...]"
)

// roleChoices lists the selectable DevIdentity roles in the discover wizard,
// dev first (the default). qa/exec/excluded come off the leaderboard via
// Scoring.ExcludedRoles; lead/devops stay on the board but carry the tag for
// UI highlighting. Capturing the role at discover time means no hand-editing
// config.toml afterwards.
var roleChoices = []string{"dev", "lead", "devops", "qa", "exec", "excluded"}

func devsDiscoverCmd() *cobra.Command {
	var (
		dryRun              bool
		applyAll            bool
		confidenceThreshold int
		topN                int
		minScore            int
	)
	cmd := &cobra.Command{
		Use:   "discover",
		Short: "Scan the cache and propose [[devs]] entries you haven't mapped yet",
		Long: `Walks every cached month, collects every distinct GitHub login and Jira
accountId, then proposes [[devs]] entries you can confirm interactively.

The flow is two passes:
  1. Self-pair bootstrap (uses profile.github.username + profile.jira.account_id).
  2. Org-wide pairing — for every other unmapped GitHub login, score the
     unmapped Jira accountIds for likely matches and prompt for confirmation.

Use --apply-all to skip prompts for high-confidence matches (score >=
--confidence-threshold, default 90). Lower-confidence candidates still
require per-pair confirmation.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("load config (try `velocity init`): %w", err)
			}
			profile := cfg.ActiveProfile()

			out := cmd.OutOrStdout()
			store, err := cache.OpenStore()
			if err != nil {
				return err
			}
			defer store.Close()

			fmt.Fprintln(out, "Scanning cache...")
			id, err := devs.Scan(profile, store)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "  GitHub logins:    %d\n", len(id.GitHubLogins))
			fmt.Fprintf(out, "  Jira accountIds:  %d\n", len(id.JiraAccountIDs))

			unmapped := id.Unmapped(profile)
			alreadyMapped := len(id.GitHubLogins) + len(id.JiraAccountIDs) -
				len(unmapped.GitHubLogins) - len(unmapped.JiraAccountIDs)
			fmt.Fprintf(out, "  Already mapped:   %d\n", alreadyMapped)
			fmt.Fprintf(out, "  Unmapped:         %d GitHub + %d Jira\n",
				len(unmapped.GitHubLogins), len(unmapped.JiraAccountIDs))

			if len(unmapped.GitHubLogins) == 0 && len(unmapped.JiraAccountIDs) == 0 {
				fmt.Fprintln(out, "Nothing to propose — every cached identity is already in [[devs]].")
				return nil
			}

			var pending []config.DevIdentity

			// Pass 0 — self-pair bootstrap. Only proposed if both sides of the
			// self pair are still unmapped.
			selfGH := profile.GitHub.Username
			selfJira := profile.Jira.AccountID
			if selfGH != "" && selfJira != "" &&
				contains(unmapped.GitHubLogins, selfGH) &&
				contains(unmapped.JiraAccountIDs, selfJira) {
				entry, err := proposeSelfPair(cmd.Context(), out, profile, selfGH, selfJira)
				if err != nil {
					return err
				}
				if entry != nil {
					pending = append(pending, *entry)
				}
			}

			// Pass 1 + 2 — org-wide pairing. Skip if there's nothing left to do
			// after the self-pair flow.
			remaining := remainingAfterPending(unmapped, pending)
			if len(remaining.GitHubLogins) > 0 || len(remaining.JiraAccountIDs) > 0 {
				added, err := proposeOrgWide(cmd.Context(), out, profile, remaining,
					applyAll, confidenceThreshold, topN, minScore)
				if err != nil {
					return err
				}
				pending = append(pending, added...)
			}

			if len(pending) == 0 {
				fmt.Fprintln(out, "No new mappings confirmed.")
				return nil
			}

			// Classify each newly-discovered contributor's role at capture
			// time (programmatic, not hand-edited config.toml). Skipped under
			// --apply-all so a non-interactive run stays prompt-free; those
			// entries default to "dev".
			if !applyAll {
				if err := assignNewDevRoles(out, profile.Devs, pending); err != nil {
					return err
				}
			}

			p := cfg.ActiveProfile()
			merged, extended, appended := mergePending(p.Devs, pending)

			if dryRun {
				fmt.Fprintf(out, "\n[dry-run] Would apply %d change%s (%d new, %d extension%s):\n",
					extended+appended, pluralS(extended+appended), appended, extended, pluralS(extended))
				for _, e := range pending {
					logins := strings.Join(e.AllGitHubLogins(), ", ")
					if logins == "" {
						logins = "(none)"
					}
					action := "append"
					for _, existing := range p.Devs {
						if existing.JiraAccountID != "" && existing.JiraAccountID == e.JiraAccountID {
							action = "extend " + existing.DisplayName
							break
						}
					}
					fmt.Fprintf(out, "  [%s]  github_logins=%-40s  jira_account_id=%-30q  display_name=%-24q  role=%s\n",
						action, logins, e.JiraAccountID, e.DisplayName, e.EffectiveRole())
				}
				return nil
			}

			p.Devs = merged
			cfg.Profiles[config.DefaultProfile] = p
			if err := cfg.Save(); err != nil {
				return fmt.Errorf("save config: %w", err)
			}
			fmt.Fprintf(out, "\nApplied %d change%s to [[devs]] (%d new, %d extension%s) and wrote config.\n",
				extended+appended, pluralS(extended+appended), appended, extended, pluralS(extended))
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print the proposed changes without writing the config")
	cmd.Flags().BoolVar(&applyAll, "apply-all", false, "Auto-confirm high-confidence proposals (score >= --confidence-threshold) without prompting")
	cmd.Flags().IntVar(&confidenceThreshold, "confidence-threshold", 90, "Minimum score for --apply-all to auto-confirm a pair without prompting")
	cmd.Flags().IntVar(&topN, "top-n", 3, "Max Jira candidates surfaced per unmapped GitHub login")
	cmd.Flags().IntVar(&minScore, "min-score", 60, "Minimum match score for a Jira candidate to appear in the interactive list")
	return cmd
}

// proposeSelfPair handles the self-pair bootstrap. Returns the entry to add
// (nil if the user declined), or an error if Jira /myself fails fatally.
func proposeSelfPair(ctx context.Context, out io.Writer, profile config.Profile, selfGH, selfJira string) (*config.DevIdentity, error) {
	displayName, err := fetchJiraDisplayName(ctx, profile.Jira)
	if err != nil {
		fmt.Fprintf(out, "Note: could not fetch Jira display name (%v); using accountId as placeholder.\n", err)
		displayName = selfJira
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Proposed identity pairing (self):")
	fmt.Fprintf(out, "  GitHub:  %s\n", selfGH)
	fmt.Fprintf(out, "  Jira:    %s (%s)\n", displayName, selfJira)
	fmt.Fprintln(out, "  Source:  matches profile.github.username + profile.jira.account_id")

	confirm := true
	if err := survey.AskOne(&survey.Confirm{
		Message: "Add this self-pair to [[devs]]?",
		Default: true,
	}, &confirm); err != nil {
		return nil, err
	}
	if !confirm {
		return nil, nil
	}
	return &config.DevIdentity{
		GitHubLogins:  []string{selfGH},
		JiraAccountID: selfJira,
		DisplayName:   displayName,
	}, nil
}

// assignNewDevRoles prompts for a role on each genuinely-new pending entry and
// records it on the entry in place. Entries that extend an already-mapped
// person (their jira_account_id is already in [[devs]]) are skipped — that
// person's role is already set, and attaching another GitHub identifier
// shouldn't re-ask. Anything with no jira_account_id (GitHub-only) is always
// treated as new. Leaving role unset is fine: EffectiveRole defaults to "dev".
func assignNewDevRoles(out io.Writer, existing, pending []config.DevIdentity) error {
	existingJira := map[string]struct{}{}
	for _, d := range existing {
		if d.JiraAccountID != "" {
			existingJira[d.JiraAccountID] = struct{}{}
		}
	}
	isExtension := func(e config.DevIdentity) bool {
		if e.JiraAccountID == "" {
			return false
		}
		_, ok := existingJira[e.JiraAccountID]
		return ok
	}

	newCount := 0
	for _, e := range pending {
		if !isExtension(e) {
			newCount++
		}
	}
	if newCount == 0 {
		return nil
	}

	fmt.Fprintf(out, "\nClassify %d new contributor%s (qa/exec/excluded come off the leaderboard; lead/devops stay, tagged):\n",
		newCount, pluralS(newCount))
	for i := range pending {
		if isExtension(pending[i]) {
			continue
		}
		name := pending[i].DisplayName
		if name == "" {
			if logins := pending[i].AllGitHubLogins(); len(logins) > 0 {
				name = logins[0]
			} else {
				name = pending[i].JiraAccountID
			}
		}
		var role string
		if err := survey.AskOne(&survey.Select{
			Message:  fmt.Sprintf("Role for %s:", name),
			Options:  roleChoices,
			Default:  "dev",
			PageSize: len(roleChoices),
		}, &role); err != nil {
			return err
		}
		// Store anything other than the default so config.toml stays terse
		// (omitempty drops "" and EffectiveRole fills "dev" back in).
		if role != "" && role != "dev" {
			pending[i].Role = role
		}
	}
	return nil
}

// proposeOrgWide runs the two-pass org-wide pairing flow. Pass 1 walks every
// unmapped GitHub login and proposes Jira candidates; Pass 2 sweeps the
// still-unmapped Jira accountIds and offers them as Jira-only entries.
func proposeOrgWide(ctx context.Context, out io.Writer, profile config.Profile, remaining devs.Identities,
	applyAll bool, threshold, topN, minScore int) ([]config.DevIdentity, error) {

	if len(remaining.GitHubLogins) == 0 && len(remaining.JiraAccountIDs) == 0 {
		return nil, nil
	}

	// Resolve Jira display names so we can score and prompt with real names.
	var names map[string]string
	if len(remaining.JiraAccountIDs) > 0 {
		token, err := secrets.Get(config.DefaultProfile, "jira")
		if err != nil {
			return nil, fmt.Errorf("jira token: %w", err)
		}
		fmt.Fprintf(out, "\nResolving %d Jira display names...\n", len(remaining.JiraAccountIDs))
		names, err = devs.ResolveJiraNames(ctx, profile.Jira, token, remaining.JiraAccountIDs)
		if err != nil {
			return nil, fmt.Errorf("resolve jira names: %w", err)
		}
	}

	// Pool of unclaimed Jira accountIds — gets drained as the user maps them.
	unclaimedJira := map[string]string{}
	for _, id := range remaining.JiraAccountIDs {
		if name, ok := names[id]; ok {
			unclaimedJira[id] = name
		} else {
			// Unknown to Jira (deleted/inactive account). Surface it anyway so
			// the user has the option to map by accountId.
			unclaimedJira[id] = id
		}
	}

	var pending []config.DevIdentity

	// Pass 1: per-GitHub-login proposals. `attachable` is the union of
	// already-saved devs and anything mapped earlier in this session — so
	// attach-to-existing can target a Jira person the user just paired with a
	// different GitHub identifier two prompts ago.
	if len(remaining.GitHubLogins) > 0 {
		fmt.Fprintf(out, "\nPass 1 — %d unmapped GitHub identifier%s\n",
			len(remaining.GitHubLogins), pluralS(len(remaining.GitHubLogins)))
		for _, gh := range remaining.GitHubLogins {
			attachable := mergedAttachable(profile.Devs, pending)
			entry, err := proposeForGitHubLogin(out, gh, unclaimedJira, attachable,
				applyAll, threshold, topN, minScore)
			if err != nil {
				return nil, err
			}
			if entry != nil {
				pending = append(pending, *entry)
				if entry.JiraAccountID != "" {
					// Drain the pool only when this is a NEW jira mapping. An
					// attach-to-existing reuses an already-mapped accountId
					// that isn't in unclaimedJira anyway, so delete is a no-op.
					delete(unclaimedJira, entry.JiraAccountID)
				}
			}
		}
	}

	// Pass 2: Jira-only sweep. Useful for PMs/QA who file tickets but don't
	// commit. Skipped under --apply-all (no high-confidence signal here).
	if len(unclaimedJira) > 0 && !applyAll {
		fmt.Fprintf(out, "\nPass 2 — %d Jira account%s with no GitHub match\n",
			len(unclaimedJira), pluralS(len(unclaimedJira)))
		entries, err := proposeJiraOnly(out, unclaimedJira)
		if err != nil {
			return nil, err
		}
		pending = append(pending, entries...)
	}

	return pending, nil
}

// proposeForGitHubLogin scores every candidate in unclaimedJira against the
// given GitHub login, surfaces the top N above minScore, and returns the
// confirmed pairing (or nil if the user skipped). When applyAll is set and
// the top candidate scores >= threshold, the pairing is taken without a
// prompt. Even when no candidate clears minScore, the user gets a chance to
// browse the full Jira pool or attach the login to an already-mapped person —
// the matcher's recall is imperfect, so silently dropping a GitHub login
// risks losing a real pairing.
func proposeForGitHubLogin(out io.Writer, gh string, unclaimedJira map[string]string,
	existingDevs []config.DevIdentity,
	applyAll bool, threshold, topN, minScore int) (*config.DevIdentity, error) {

	candidates := devs.Propose(gh, unclaimedJira, topN, minScore)

	if applyAll && len(candidates) > 0 && candidates[0].Score >= threshold {
		c := candidates[0]
		fmt.Fprintf(out, "  %s → %s (%s)  [auto-applied @ score=%d]\n", gh, c.DisplayName, c.JiraAccountID, c.Score)
		return &config.DevIdentity{
			GitHubLogins:  []string{gh},
			JiraAccountID: c.JiraAccountID,
			DisplayName:   c.DisplayName,
		}, nil
	}

	options := make([]string, 0, len(candidates)+4)
	labelToCandidate := map[string]devs.MatchCandidate{}
	for _, c := range candidates {
		label := fmt.Sprintf("%s  (%s)  score=%d", c.DisplayName, c.JiraAccountID, c.Score)
		options = append(options, label)
		labelToCandidate[label] = c
	}
	// Browse-all is always offered so a no-candidate or wrong-candidate GH
	// login can still be mapped manually without dropping out of the flow.
	if len(unclaimedJira) > 0 {
		options = append(options, choiceBrowseAll)
	}
	// Attach-to-existing handles the multi-GH-identifier-per-person case (a
	// real GH login + git-author-name fallbacks for unlinked commits). Only
	// shown when there's actually an existing entry to attach to.
	if hasMappedDev(existingDevs) {
		options = append(options, choiceAttachExisting)
	}
	options = append(options, choiceGitHubOnly, choiceSkip)

	prompt := fmt.Sprintf("Map GitHub %q to:", gh)
	if len(candidates) == 0 {
		prompt = fmt.Sprintf("Map GitHub %q (no auto-candidate above min-score=%d):", gh, minScore)
	}

	// Loop so that backing out of a second-level picker (browse-all or
	// attach-to-existing) returns to this parent menu for the SAME GH login,
	// rather than skipping the login entirely.
	for {
		var picked string
		if err := survey.AskOne(&survey.Select{
			Message:  prompt,
			Options:  options,
			Default:  options[0],
			PageSize: 10,
		}, &picked); err != nil {
			return nil, err
		}

		switch picked {
		case choiceSkip:
			return nil, nil
		case choiceGitHubOnly:
			return &config.DevIdentity{GitHubLogins: []string{gh}, DisplayName: gh}, nil
		case choiceBrowseAll:
			acctID, name, err := browseAllJiraUsers(gh, unclaimedJira)
			if err != nil {
				return nil, err
			}
			if acctID == "" {
				continue // back to parent menu, same GH login
			}
			return &config.DevIdentity{
				GitHubLogins:  []string{gh},
				JiraAccountID: acctID,
				DisplayName:   name,
			}, nil
		case choiceAttachExisting:
			acctID, name, err := pickExistingDev(gh, existingDevs)
			if err != nil {
				return nil, err
			}
			if acctID == "" {
				continue // back to parent menu, same GH login
			}
			// Same jira_account_id signals to the save layer that this should
			// merge into the existing entry rather than create a new one.
			return &config.DevIdentity{
				GitHubLogins:  []string{gh},
				JiraAccountID: acctID,
				DisplayName:   name,
			}, nil
		}

		c := labelToCandidate[picked]
		return &config.DevIdentity{
			GitHubLogins:  []string{gh},
			JiraAccountID: c.JiraAccountID,
			DisplayName:   c.DisplayName,
		}, nil
	}
}

// hasMappedDev reports whether any [[devs]] entry currently has a Jira
// accountId — i.e. whether there's anything for attach-to-existing to
// attach to.
func hasMappedDev(existing []config.DevIdentity) bool {
	for _, d := range existing {
		if d.JiraAccountID != "" {
			return true
		}
	}
	return false
}

// mergedAttachable returns a snapshot suitable for the attach-to-existing
// picker: every saved DevIdentity plus the in-session pending mappings,
// folded by jira_account_id so a person paired this session shows up with
// their current set of GitHub identifiers. Folding mirrors mergePending so
// the picker matches what would actually land on disk.
func mergedAttachable(saved, pending []config.DevIdentity) []config.DevIdentity {
	merged, _, _ := mergePending(saved, pending)
	return merged
}

// pickExistingDev shows a filterable list of every existing [[devs]] entry
// with a Jira accountId so the user can extend it with another GitHub
// identifier. Returns ("", "", nil) on cancel.
func pickExistingDev(gh string, existing []config.DevIdentity) (string, string, error) {
	type row struct{ acctID, name, label string }
	rows := make([]row, 0, len(existing))
	for _, d := range existing {
		if d.JiraAccountID == "" {
			continue
		}
		name := d.DisplayName
		if name == "" {
			name = d.JiraAccountID
		}
		existingLogins := strings.Join(d.AllGitHubLogins(), ", ")
		if existingLogins == "" {
			existingLogins = "(no GH logins yet)"
		}
		label := fmt.Sprintf("%s  ← %s", name, existingLogins)
		rows = append(rows, row{acctID: d.JiraAccountID, name: name, label: label})
	}
	if len(rows) == 0 {
		return "", "", nil
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })

	options := make([]string, 0, len(rows)+1)
	labelToRow := map[string]row{}
	for _, r := range rows {
		options = append(options, r.label)
		labelToRow[r.label] = r
	}
	options = append(options, choiceSkip)

	var picked string
	err := survey.AskOne(&survey.Select{
		Message:  fmt.Sprintf("Attach GitHub %q to which already-mapped person? (type to filter, Ctrl-C to go back)", gh),
		Options:  options,
		Default:  options[0],
		PageSize: 15,
	}, &picked)
	if errors.Is(err, terminal.InterruptErr) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	if picked == choiceSkip {
		return "", "", nil
	}
	r := labelToRow[picked]
	return r.acctID, r.name, nil
}

// browseAllJiraUsers shows a filterable list of every unclaimed Jira user so
// the user can manually pick the right one when the matcher missed it.
// survey.Select supports type-to-filter natively; PageSize caps the visible
// window. Returns ("", "", nil) if the user picks [skip] or hits Ctrl-C —
// Ctrl-C here is treated as "back out to the parent picker" rather than
// aborting the whole discover run, so in-progress mappings aren't lost.
func browseAllJiraUsers(gh string, unclaimedJira map[string]string) (string, string, error) {
	if len(unclaimedJira) == 0 {
		return "", "", nil
	}
	type row struct{ id, name, label string }
	rows := make([]row, 0, len(unclaimedJira))
	for id, name := range unclaimedJira {
		label := fmt.Sprintf("%s  (%s)", name, id)
		rows = append(rows, row{id: id, name: name, label: label})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].name != rows[j].name {
			return rows[i].name < rows[j].name
		}
		return rows[i].id < rows[j].id
	})

	options := make([]string, 0, len(rows)+1)
	labelToRow := map[string]row{}
	for _, r := range rows {
		options = append(options, r.label)
		labelToRow[r.label] = r
	}
	options = append(options, choiceSkip)

	var picked string
	err := survey.AskOne(&survey.Select{
		Message:  fmt.Sprintf("Pick a Jira user for GitHub %q (type to filter, Ctrl-C to go back):", gh),
		Options:  options,
		Default:  options[0],
		PageSize: 15,
	}, &picked)
	if errors.Is(err, terminal.InterruptErr) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	if picked == choiceSkip {
		return "", "", nil
	}
	r := labelToRow[picked]
	return r.id, r.name, nil
}

// proposeJiraOnly walks any leftover Jira accountIds (no GitHub match found)
// and offers each one as a Jira-only entry. Confirmed entries land in
// [[devs]] with an empty github_login.
func proposeJiraOnly(out io.Writer, unclaimedJira map[string]string) ([]config.DevIdentity, error) {
	ids := make([]string, 0, len(unclaimedJira))
	for id := range unclaimedJira {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var pending []config.DevIdentity
	for _, id := range ids {
		name := unclaimedJira[id]
		label := name
		if name == id {
			label = id + " (display name unknown — may be inactive)"
		}
		confirm := false
		if err := survey.AskOne(&survey.Confirm{
			Message: fmt.Sprintf("Map Jira-only entry for %s?", label),
			Default: false,
		}, &confirm); err != nil {
			return nil, err
		}
		if confirm {
			pending = append(pending, config.DevIdentity{
				JiraAccountID: id,
				DisplayName:   name,
			})
		}
	}
	return pending, nil
}

// remainingAfterPending returns the subset of `unmapped` not covered by
// already-pending entries — used between the self-pair and org-wide passes
// so the same identifier isn't proposed twice in one run. A pending entry
// that targets an already-mapped jira_account_id (attach-to-existing) still
// claims that accountId here so pass 2's Jira-only sweep doesn't propose it
// again.
func remainingAfterPending(unmapped devs.Identities, pending []config.DevIdentity) devs.Identities {
	ghPending := map[string]struct{}{}
	jrPending := map[string]struct{}{}
	for _, e := range pending {
		for _, login := range e.AllGitHubLogins() {
			ghPending[login] = struct{}{}
		}
		if e.JiraAccountID != "" {
			jrPending[e.JiraAccountID] = struct{}{}
		}
	}
	out := devs.Identities{}
	for _, gh := range unmapped.GitHubLogins {
		if _, ok := ghPending[gh]; !ok {
			out.GitHubLogins = append(out.GitHubLogins, gh)
		}
	}
	for _, jr := range unmapped.JiraAccountIDs {
		if _, ok := jrPending[jr]; !ok {
			out.JiraAccountIDs = append(out.JiraAccountIDs, jr)
		}
	}
	return out
}

// mergePending applies pending DevIdentity entries to existing. A pending
// entry whose jira_account_id matches an existing entry is treated as an
// extension (union of GitHubLogins, existing DisplayName preserved). Anything
// else is appended as a new entry. Returns the merged slice plus a tally of
// (extended, appended) for the user-facing summary.
func mergePending(existing, pending []config.DevIdentity) (merged []config.DevIdentity, extended, appended int) {
	merged = make([]config.DevIdentity, len(existing))
	copy(merged, existing)

	// Build a jira_account_id → merged[] index once.
	byJira := map[string]int{}
	for i, d := range merged {
		if d.JiraAccountID != "" {
			byJira[d.JiraAccountID] = i
		}
	}

	for _, e := range pending {
		if e.JiraAccountID != "" {
			if idx, ok := byJira[e.JiraAccountID]; ok {
				// Extension: union GitHubLogins (dedupe), keep existing display.
				cur := merged[idx]
				existingLogins := map[string]struct{}{}
				for _, l := range cur.AllGitHubLogins() {
					existingLogins[l] = struct{}{}
				}
				for _, l := range e.AllGitHubLogins() {
					if _, ok := existingLogins[l]; ok {
						continue
					}
					existingLogins[l] = struct{}{}
					if len(cur.GitHubLogins) == 0 && cur.GitHubLogin != "" {
						// Convert legacy singular to plural before appending.
						cur.GitHubLogins = []string{cur.GitHubLogin}
						cur.GitHubLogin = ""
					}
					cur.GitHubLogins = append(cur.GitHubLogins, l)
				}
				merged[idx] = cur
				extended++
				continue
			}
		}
		merged = append(merged, e)
		if e.JiraAccountID != "" {
			byJira[e.JiraAccountID] = len(merged) - 1
		}
		appended++
	}
	return merged, extended, appended
}

// fetchJiraDisplayName resolves the configured user's displayName by calling
// /rest/api/3/myself. Kept local to the devs command — the existing initflow
// helper isn't exported and discover doesn't need the rest of its surface.
func fetchJiraDisplayName(ctx context.Context, j config.JiraConfig) (string, error) {
	if j.BaseURL == "" || j.Email == "" {
		return "", errors.New("jira base_url or email missing from config")
	}
	token, err := secrets.Get(config.DefaultProfile, "jira")
	if err != nil {
		return "", fmt.Errorf("jira token: %w", err)
	}

	if ctx == nil {
		ctx = context.Background()
	}
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	url := strings.TrimRight(j.BaseURL, "/") + "/rest/api/3/myself"
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(j.Email, token)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("jira /myself returned %d: %s", resp.StatusCode, truncate(body, 200))
	}
	var m struct {
		AccountID   string `json:"accountId"`
		DisplayName string `json:"displayName"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return "", fmt.Errorf("parse /myself: %w", err)
	}
	if m.DisplayName == "" {
		return m.AccountID, nil
	}
	return m.DisplayName, nil
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func pluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
