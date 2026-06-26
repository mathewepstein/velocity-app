package analyze

import (
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
	"github.com/mathewepstein/velocity/internal/devs"
)

// resolvedCreditedTo reports whether issue i's resolution — and the story
// points that ride it — should be credited to dev id. B1/B3: assignee-only.
// The reporter filed the ticket; the assignee did the work, so a reporter who
// never owned the ticket must not inherit its resolution. Shared by the
// composite path (filterForDev) and the Elo path (periodTotals) so the two
// attribution sites can't drift.
func resolvedCreditedTo(i cache.JiraIssue, id config.DevIdentity) bool {
	return id.JiraAccountID != "" && i.Assignee == id.JiraAccountID
}

// devProgressedIssues counts DISTINCT issues that dev id personally advanced
// through the build pipeline within the window tested by inWindow — i.e. the
// dev AUTHORED at least one status transition INTO a dev-pipeline stage
// (In Progress / Code Review / Ready QA; workflowRank 1-3) whose event time
// passes inWindow. B2's replacement for jira_issues_touched: actor- AND
// time-correct (it matches changelog.Author against the event's own At), so it
// cannot be inherited the way assignee/reporter attribution can. QA pickup
// (In QA), closes (Done/Closed/Resolved), and backlog/triage moves (rank 0) are
// excluded as not-dev-progress. The distinct-issue (not raw-transition) shape
// rewards throughput, resists churn-gaming, and is robust to the changelog's
// snapshot duplication (the same key in several month cells counts once).
//
// Scans the FULL corpus, not a per-dev scoped slice: a dev can advance a ticket
// they neither own nor reported, and actor-attribution must catch that.
func devProgressedIssues(data *Loaded, id config.DevIdentity, inWindow func(time.Time) bool) int {
	if id.JiraAccountID == "" {
		return 0
	}
	seen := map[string]struct{}{}
	for _, iss := range data.Issues {
		if _, ok := seen[iss.Key]; ok {
			continue
		}
		for _, tr := range iss.Changelog {
			if tr.Author != id.JiraAccountID {
				continue
			}
			if tr.Field != "" && tr.Field != "status" {
				continue
			}
			if r := workflowRank(tr.To); r < 1 || r > 3 {
				continue
			}
			if !inWindow(tr.At) {
				continue
			}
			seen[iss.Key] = struct{}{}
			break
		}
	}
	return len(seen)
}

// devProgressedByBucket returns, per time-bucket key (from bucketOf applied to
// each transition's timestamp), the count of DISTINCT issues dev id personally
// advanced into the build pipeline (workflowRank 1-3 via a dev-authored status
// transition). Distinctness is per-bucket — an issue advanced in two different
// months counts once in each — which is the right semantic for the per-month /
// per-week history rows.
//
// This is the bucketed companion to devProgressedIssues: the rollup rows can't
// reuse the window-total (its cross-window dedup means month counts wouldn't sum
// back to it), and rollupMonthly/Weekly can't compute it themselves because
// progress is actor-attributed (needs id + the full, unscoped changelog), not
// derivable from the dev-scoped issue set. buildOneDev calls this once with
// monthKey and once with isoWeek and assigns the results onto the rows.
func devProgressedByBucket(data *Loaded, id config.DevIdentity, bucketOf func(time.Time) string) map[string]int {
	if id.JiraAccountID == "" {
		return nil
	}
	sets := map[string]map[string]struct{}{}
	for _, iss := range data.Issues {
		for _, tr := range iss.Changelog {
			if tr.Author != id.JiraAccountID {
				continue
			}
			if tr.Field != "" && tr.Field != "status" {
				continue
			}
			if r := workflowRank(tr.To); r < 1 || r > 3 {
				continue
			}
			b := bucketOf(tr.At)
			s := sets[b]
			if s == nil {
				s = map[string]struct{}{}
				sets[b] = s
			}
			s[iss.Key] = struct{}{}
		}
	}
	out := make(map[string]int, len(sets))
	for b, s := range sets {
		out[b] = len(s)
	}
	return out
}

// devCreatedIssues counts DISTINCT issues dev id REPORTED (filed) with a
// creation time passing inWindow. Reporter-attribution is correct here — the
// reporter IS the author/planner — the mirror image of why it's wrong for
// resolved (resolvedCreditedTo). B5's planning-credit signal; sqrt-saturated +
// low-weighted downstream so it rewards "planning happened", never filing
// volume.
func devCreatedIssues(data *Loaded, id config.DevIdentity, inWindow func(time.Time) bool) int {
	if id.JiraAccountID == "" {
		return 0
	}
	seen := map[string]struct{}{}
	for _, iss := range data.Issues {
		if iss.Reporter != id.JiraAccountID {
			continue
		}
		if _, ok := seen[iss.Key]; ok {
			continue
		}
		if inWindow(iss.Created) {
			seen[iss.Key] = struct{}{}
		}
	}
	return len(seen)
}

// filterForDev returns a Loaded containing only records attributed to id.
//
// Attribution rules:
//   - PRs:     Author claimed by id.AllGitHubLogins()
//   - Commits: Author claimed by id.AllGitHubLogins()
//   - Reviews: Reviewer claimed by id.AllGitHubLogins()
//   - Issues:  Assignee == id.JiraAccountID OR Reporter == id.JiraAccountID,
//     EXCEPT resolution + story points, which are assignee-only (see
//     resolvedCreditedTo). A reporter-only issue still counts toward touched /
//     created but has its Resolved cleared so the rollup can't credit it.
//
// An empty id field on either side disables that side's filter — a Jira-only
// stub dev (no GitHub identifiers) still attributes its issues correctly.
// Months is copied through so downstream zero-fill keeps working.
func filterForDev(data *Loaded, id config.DevIdentity) *Loaded {
	out := &Loaded{Months: data.Months}

	if id.JiraAccountID != "" {
		for _, iss := range data.Issues {
			if iss.Assignee != id.JiraAccountID && iss.Reporter != id.JiraAccountID {
				continue
			}
			// B1/B3: resolution + SP are assignee-only. iss is a value copy, so
			// clearing Resolved here only affects this dev's scoped Loaded; the
			// rollup gates both the resolved count and StoryPoints on Resolved
			// != nil, so this zeroes both for reporter-only issues.
			if !resolvedCreditedTo(iss, id) {
				iss.Resolved = nil
			}
			out.Issues = append(out.Issues, iss)
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
func buildDevWindows(data *Loaded, devIDs []config.DevIdentity, excludes, excludedRoles []string, start, end, fullStart, fullEnd cache.Month, ci config.CodeImpactConfig, norm config.NormalizeConfig, iw prIntegrationWeight) []DevWindowMetrics {
	out := make([]DevWindowMetrics, 0, len(devIDs)+1)
	activity := activityCountsByLogin(data)
	// Corpus-wide file churn index for the optional churn-weighting knob. Built
	// once from the full (unfiltered) corpus; nil when the knob is off so the
	// per-file walk is skipped entirely.
	var churn map[string]int
	if ci.ChurnWeighting {
		churn = buildChurnIndex(data)
	}
	// Corpus-wide copy-paste family index for boilerplate dampening (ON by
	// default). Built once from the full corpus; nil when disabled or when
	// nothing qualifies, so the per-file walk is skipped.
	var family map[string]float64
	if !ci.DisableBoilerplateDampening {
		family = buildFamilyIndex(data, ci)
	}
	for _, id := range devIDs {
		// Skip bot/login excludes AND non-scored roles (qa/exec/excluded). The
		// dev stays in devIDs so filterUnmapped still treats their records as
		// mapped — excluded means "off the board", not "reattributed to unknown".
		if devExcluded(id, excludes) || config.RoleExcluded(id.EffectiveRole(), excludedRoles) {
			continue
		}
		w := buildOneDev(data, id, start, end, fullStart, fullEnd, churn, family, ci, norm, iw)
		w.PrimaryLogin = devs.PrimaryLogin(id, activity)
		out = append(out, w)
	}

	unknown := filterUnmapped(data, devIDs)
	unknown = dropExcludedAuthors(unknown, excludes)
	if hasAny(unknown) {
		out = append(out, buildOneDev(unknown, unknownIdentity, start, end, fullStart, fullEnd, churn, family, ci, norm, iw))
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

func buildOneDev(data *Loaded, id config.DevIdentity, start, end, fullStart, fullEnd cache.Month, churn map[string]int, family map[string]float64, ci config.CodeImpactConfig, norm config.NormalizeConfig, iw prIntegrationWeight) DevWindowMetrics {
	scoped := data
	if len(id.AllGitHubLogins()) > 0 || id.JiraAccountID != "" {
		scoped = filterForDev(data, id)
	}
	monthly := rollupMonthly(scoped, start, end, ci, norm)
	weekly := rollupWeekly(scoped, start, end, ci, norm)
	// B2: populate the per-row scored Jira-progress signal. rollupMonthly/Weekly
	// can't — progress is actor-attributed via the changelog (needs id + full,
	// unscoped data), not derivable from the dev-scoped issue rollup. Bucketed
	// here so the monthly/weekly history rows carry it (the window total below
	// uses cross-window dedup and so can't be summed out of the rows).
	progByMonth := devProgressedByBucket(data, id, monthKey)
	for i := range monthly {
		monthly[i].JiraIssuesProgressed = progByMonth[monthly[i].Month]
	}
	progByWeek := devProgressedByBucket(data, id, isoWeek)
	for i := range weekly {
		weekly[i].JiraIssuesProgressed = progByWeek[weekly[i].Week]
	}
	totals := totalsFromMonthly(monthly)
	totals.ActiveWeeks = activeWeeksCount(weekly)
	// B2: scored Jira-progress signal — distinct issues this dev personally
	// advanced through the build pipeline in-window (dev-authored changelog
	// transitions). Scans full data (not scoped) since it's actor-attributed,
	// not ownership-attributed. Authoritative window total (cross-month dedup),
	// set after totalsFromMonthly so the per-row population above can't skew it.
	totals.JiraIssuesProgressed = devProgressedIssues(data, id, func(t time.Time) bool {
		return monthInRange(monthKey(t), start, end)
	})
	// B5 planning credit: reporter-attributed distinct issues filed in-window.
	totals.JiraIssuesCreated = devCreatedIssues(data, id, func(t time.Time) bool {
		return monthInRange(monthKey(t), start, end)
	})
	totals.UniqueFilesTouched = uniqueFilesInWindow(scoped, start, end)
	// Per Phase 6.2: code_impact uses the generated-file-dampened cardinality,
	// not the raw count, so dependency dumps don't pad the substance score.
	// Raw int cardinality stays on Totals.UniqueFilesTouched for display.
	effFiles := effectiveUniqueFilesInWindow(scoped, start, end, norm, ci, family, iw)
	// effLOC equals LOCAdded+LOCDeleted when both code_impact knobs are off, so
	// default behavior is unchanged; churn-weighting / bulk-import dampening only
	// diverge it when explicitly enabled. effFiles/effLOC are also integration-
	// down-weighted (via iw) when the feature is on.
	effLOC := effectiveLOCInWindow(scoped, start, end, churn, family, ci, norm, iw)
	// Integration scoring (iw != nil): stash the down-weighted prs_*/loc inputs
	// for computeContributorScores and use the down-weighted merged count for the
	// γ·merged term of code_impact. When iw is nil, scored stays nil and the raw
	// merged count is used — byte-identical to the pre-feature path.
	var scored *scoredMetrics
	mergedForImpact := float64(totals.PRsMerged)
	if iw != nil {
		sm, flagged := scopedIntegrationScoring(scoped, start, end, iw)
		scored = &sm
		mergedForImpact = sm.prsMerged
		totals.IntegrationPRs = flagged
	}
	totals.CodeImpact = codeImpactFormula(effFiles, effLOC, mergedForImpact, ci)
	var fullHistory []MonthlyRow
	if fullStart.Before(start) || fullStart.Equal(start) {
		fullHistory = rollupMonthly(scoped, fullStart, fullEnd, ci, norm)
		// progByMonth spans all of data (no window filter), so it covers the
		// full-history months too.
		for i := range fullHistory {
			fullHistory[i].JiraIssuesProgressed = progByMonth[fullHistory[i].Month]
		}
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
		scored:             scored,
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
		// γ·merged uses the integration-down-weighted merged count when present
		// (scored, set in buildOneDev), else the raw count — matching how effLOC
		// above is already down-weighted in the effective* walks.
		merged := float64(devs[di].Totals.PRsMerged)
		if devs[di].scored != nil {
			merged = devs[di].scored.prsMerged
		}
		devs[di].Totals.CodeImpact = codeImpactFormula(
			devs[di].effectiveFiles,
			loc,
			merged,
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
