package analyze

import (
	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
	"github.com/mathewepstein/velocity/internal/devs"
)

// filterForDev returns a Loaded containing only records attributed to id.
//
// Attribution rules:
//   - PRs:     Author claimed by id.AllGitHubLogins()
//   - Commits: Author claimed by id.AllGitHubLogins()
//   - Reviews: Reviewer claimed by id.AllGitHubLogins()
//   - Issues:  Assignee == id.JiraAccountID OR Reporter == id.JiraAccountID
//
// An empty id field on either side disables that side's filter — a Jira-only
// stub dev (no GitHub identifiers) still attributes its issues correctly.
// Months is copied through so downstream zero-fill keeps working.
func filterForDev(data *Loaded, id config.DevIdentity) *Loaded {
	out := &Loaded{Months: data.Months}

	if id.JiraAccountID != "" {
		for _, iss := range data.Issues {
			if iss.Assignee == id.JiraAccountID || iss.Reporter == id.JiraAccountID {
				out.Issues = append(out.Issues, iss)
			}
		}
	}
	if len(id.AllGitHubLogins()) > 0 {
		for _, pr := range data.PRs {
			if id.MatchesGitHubLogin(pr.Author) {
				out.PRs = append(out.PRs, pr)
			}
		}
		for _, c := range data.Commits {
			if id.MatchesGitHubLogin(c.Author) {
				out.Commits = append(out.Commits, c)
			}
		}
		for _, r := range data.Reviews {
			if id.MatchesGitHubLogin(r.Reviewer) {
				out.Reviews = append(out.Reviews, r)
			}
		}
	}
	return out
}

// unknownIdentity is the catch-all bucket for records whose author/assignee
// does not match any configured DevIdentity. Surfaced in the UI so gaps in the
// `[[devs]]` table are visible rather than silently dropped.
var unknownIdentity = config.DevIdentity{
	JiraAccountID: "",
	DisplayName:   "unknown",
}

// filterUnmapped returns the records left over after mapping. A record is
// "mapped" if it can be attributed to any configured dev; everything else
// piles into one shared Loaded.
func filterUnmapped(data *Loaded, devs []config.DevIdentity) *Loaded {
	ghMapped := map[string]struct{}{}
	jrMapped := map[string]struct{}{}
	for _, d := range devs {
		for _, login := range d.AllGitHubLogins() {
			ghMapped[login] = struct{}{}
		}
		if d.JiraAccountID != "" {
			jrMapped[d.JiraAccountID] = struct{}{}
		}
	}

	out := &Loaded{Months: data.Months}
	for _, iss := range data.Issues {
		// An issue lands in "unknown" if any contributor on it is unmapped.
		// A non-empty assignee/reporter side that's missing from jrMapped
		// counts as an unmapped contributor.
		unknownAssignee := iss.Assignee != ""
		if _, ok := jrMapped[iss.Assignee]; ok {
			unknownAssignee = false
		}
		unknownReporter := iss.Reporter != ""
		if _, ok := jrMapped[iss.Reporter]; ok {
			unknownReporter = false
		}
		if !unknownAssignee && !unknownReporter {
			continue
		}
		out.Issues = append(out.Issues, iss)
	}
	for _, pr := range data.PRs {
		if _, ok := ghMapped[pr.Author]; ok {
			continue
		}
		out.PRs = append(out.PRs, pr)
	}
	for _, c := range data.Commits {
		if _, ok := ghMapped[c.Author]; ok {
			continue
		}
		out.Commits = append(out.Commits, c)
	}
	for _, r := range data.Reviews {
		if _, ok := ghMapped[r.Reviewer]; ok {
			continue
		}
		out.Reviews = append(out.Reviews, r)
	}
	return out
}

// buildDevWindows builds one DevWindowMetrics per configured [[devs]] entry
// plus a synthetic "unknown" entry aggregating everything not covered by the
// configured devs. The window mirrors Current — same start/end as the
// single-user dashboard's primary view — so dev rankings line up visually
// with the headline numbers.
//
// excludes is the union of DefaultBotExcludes and Scoring.Exclude, matched
// case-insensitively against GitHub login/reviewer fields. A mapped dev whose
// every GitHub login matches an exclude pattern is skipped; unmapped records
// whose author/reviewer matches are dropped before they reach the "unknown"
// bucket.
func buildDevWindows(data *Loaded, devIDs []config.DevIdentity, excludes []string, start, end, fullStart, fullEnd cache.Month, ci config.CodeImpactConfig, norm config.NormalizeConfig) []DevWindowMetrics {
	out := make([]DevWindowMetrics, 0, len(devIDs)+1)
	activity := activityCountsByLogin(data)
	// Corpus-wide file churn index for the optional churn-weighting knob. Built
	// once from the full (unfiltered) corpus; nil when the knob is off so the
	// per-file walk is skipped entirely.
	var churn map[string]int
	if ci.ChurnWeighting {
		churn = buildChurnIndex(data)
	}
	for _, id := range devIDs {
		if devExcluded(id, excludes) {
			continue
		}
		w := buildOneDev(data, id, start, end, fullStart, fullEnd, churn, ci, norm)
		w.PrimaryLogin = devs.PrimaryLogin(id, activity)
		out = append(out, w)
	}

	unknown := filterUnmapped(data, devIDs)
	unknown = dropExcludedAuthors(unknown, excludes)
	if hasAny(unknown) {
		out = append(out, buildOneDev(unknown, unknownIdentity, start, end, fullStart, fullEnd, churn, ci, norm))
	}
	return out
}

// activityCountsByLogin sums PRs (authored) + commits (authored) + reviews
// (submitted) per GitHub login across the entire loaded cache. Used by
// PrimaryLogin to pick the busiest handle when a dev claims multiple
// identifiers (real login + git-author-name fallback strings).
func activityCountsByLogin(data *Loaded) map[string]int {
	out := map[string]int{}
	if data == nil {
		return out
	}
	for _, pr := range data.PRs {
		if pr.Author != "" {
			out[pr.Author]++
		}
	}
	for _, c := range data.Commits {
		if c.Author != "" {
			out[c.Author]++
		}
	}
	for _, r := range data.Reviews {
		if r.Reviewer != "" {
			out[r.Reviewer]++
		}
	}
	return out
}

// devExcluded reports whether every GitHub login on id matches an exclude
// pattern. A dev with no GitHub identifiers (Jira-only) is never excluded by
// this list since the patterns target GitHub logins.
func devExcluded(id config.DevIdentity, patterns []string) bool {
	logins := id.AllGitHubLogins()
	if len(logins) == 0 {
		return false
	}
	for _, login := range logins {
		if !config.MatchesBotPattern(login, patterns) {
			return false
		}
	}
	return true
}

// dropExcludedAuthors returns a Loaded with PRs/commits/reviews whose author
// or reviewer matches an exclude pattern removed. Issues are untouched —
// patterns target GitHub logins, not Jira accountIds.
func dropExcludedAuthors(data *Loaded, patterns []string) *Loaded {
	if len(patterns) == 0 || data == nil {
		return data
	}
	out := &Loaded{Months: data.Months, Issues: data.Issues}
	for _, pr := range data.PRs {
		if config.MatchesBotPattern(pr.Author, patterns) {
			continue
		}
		out.PRs = append(out.PRs, pr)
	}
	for _, c := range data.Commits {
		if config.MatchesBotPattern(c.Author, patterns) {
			continue
		}
		out.Commits = append(out.Commits, c)
	}
	for _, r := range data.Reviews {
		if config.MatchesBotPattern(r.Reviewer, patterns) {
			continue
		}
		out.Reviews = append(out.Reviews, r)
	}
	return out
}

func buildOneDev(data *Loaded, id config.DevIdentity, start, end, fullStart, fullEnd cache.Month, churn map[string]int, ci config.CodeImpactConfig, norm config.NormalizeConfig) DevWindowMetrics {
	scoped := data
	if len(id.AllGitHubLogins()) > 0 || id.JiraAccountID != "" {
		scoped = filterForDev(data, id)
	}
	monthly := rollupMonthly(scoped, start, end, ci)
	weekly := rollupWeekly(scoped, start, end, ci)
	totals := totalsFromMonthly(monthly)
	totals.ActiveWeeks = activeWeeksCount(weekly)
	totals.UniqueFilesTouched = uniqueFilesInWindow(scoped, start, end)
	// Per Phase 6.2: code_impact uses the generated-file-dampened cardinality,
	// not the raw count, so dependency dumps don't pad the substance score.
	// Raw int cardinality stays on Totals.UniqueFilesTouched for display.
	effFiles := effectiveUniqueFilesInWindow(scoped, start, end, norm, ci)
	// effLOC equals LOCAdded+LOCDeleted when both code_impact knobs are off, so
	// default behavior is unchanged; churn-weighting / bulk-import dampening only
	// diverge it when explicitly enabled.
	effLOC := effectiveLOCInWindow(scoped, start, end, churn, ci)
	totals.CodeImpact = computeCodeImpactFloat(effFiles, effLOC, totals.PRsMerged, ci)
	var fullHistory []MonthlyRow
	if fullStart.Before(start) || fullStart.Equal(start) {
		fullHistory = rollupMonthly(scoped, fullStart, fullEnd, ci)
	}
	return DevWindowMetrics{
		Dev:                id,
		Totals:             totals,
		Monthly:            monthly,
		Weekly:             weekly,
		FullHistoryMonthly: fullHistory,
		MedianCycleHours:   medianCycleHoursInWindow(scoped, start, end),
		effectiveFiles:     effFiles,
		effectiveLOC:       effLOC,
	}
}

// applyCodeImpactCap re-derives each mapped dev's window Totals.CodeImpact
// using LOC clipped at the team's 95th percentile and the generated-file-
// dampened effective file count stashed during buildOneDev. Run after
// buildDevWindows and before computeContributorScores so the z-score reads
// the post-cap metric. Mirrors the loc_changed dampening in
// computeContributorScores.
//
// Per-row CodeImpact on Monthly/Weekly is intentionally untouched — there's
// no team distribution to cap against at row scope, and the chart should
// reflect the dev's raw substance-of-contribution over time. Per-row also
// keeps the gen-file dampening off so a single lockfile-heavy month doesn't
// disappear from the chart entirely.
func applyCodeImpactCap(devs []DevWindowMetrics, ci config.CodeImpactConfig) {
	scoreable := make([]int, 0, len(devs))
	for i := range devs {
		if devs[i].Dev.DisplayName == "unknown" {
			continue
		}
		scoreable = append(scoreable, i)
	}
	if len(scoreable) == 0 {
		return
	}
	// Cap against the effective-LOC distribution: equals raw LOCAdded+LOCDeleted
	// when the code_impact knobs are off, so the p95 clamp is unchanged by
	// default and stacks correctly on churn-weighted / bulk-damped LOC when on.
	locs := make([]float64, len(scoreable))
	for idx, di := range scoreable {
		locs[idx] = devs[di].effectiveLOC
	}
	capPct := ci.LOCCapPercentile
	if capPct <= 0 {
		capPct = 99
	}
	cap95 := percentile(locs, capPct)
	for _, di := range scoreable {
		loc := devs[di].effectiveLOC
		if loc > cap95 {
			loc = cap95
		}
		devs[di].Totals.CodeImpact = computeCodeImpactFloat(
			devs[di].effectiveFiles,
			loc,
			devs[di].Totals.PRsMerged,
			ci,
		)
	}
}

func hasAny(d *Loaded) bool {
	if d == nil {
		return false
	}
	return len(d.Issues)+len(d.PRs)+len(d.Commits)+len(d.Reviews) > 0
}
