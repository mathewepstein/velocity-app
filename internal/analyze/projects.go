package analyze

import (
	"fmt"
	"sort"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

// detectProjects groups activity by Jira epic and returns the set that clears
// any surge threshold. Attribution:
//   - An issue is attributed to issue.EpicKey (if set).
//   - A PR/commit is attributed to the epic of any ticket key it references.
//     PRs/commits with no issue keys — or issue keys whose issues aren't in
//     the cache — are skipped. This is deliberate: the point is to cluster
//     "this PR is part of Project X", and without a Jira link we can't say.
//
// A PR or commit that spans multiple epics is counted once per epic; the
// epic panel is about presence-of-activity, not exclusive ownership.
func detectProjects(data *Loaded, surge config.SurgeConfig) []Project {
	issueByKey := make(map[string]cache.JiraIssue, len(data.Issues))
	for _, i := range data.Issues {
		issueByKey[i.Key] = i
	}

	aggregates := map[string]*epicAggregator{}

	ensure := func(epicKey string) *epicAggregator {
		a, ok := aggregates[epicKey]
		if ok {
			return a
		}
		a = &epicAggregator{
			epicKey: epicKey,
			weeks:   map[string]*ProjectWeekRow{},
		}
		// Use the epic issue's own summary if we have it cached. Sub-issues
		// don't get used as a summary source — their titles aren't the epic.
		if epic, ok := issueByKey[epicKey]; ok && epic.IssueType == "Epic" {
			a.summary = epic.Summary
		}
		aggregates[epicKey] = a
		return a
	}

	for _, i := range data.Issues {
		if i.EpicKey == "" {
			continue
		}
		a := ensure(i.EpicKey)
		week := isoWeek(i.Updated)
		row := a.week(week)
		row.IssuesTouched++
		row.StoryPoints += i.StoryPoints
	}

	for _, pr := range data.PRs {
		epics := epicsForKeys(pr.IssueKeys, issueByKey)
		if len(epics) == 0 {
			continue
		}
		// Attribute PR events to merged-at when available (the shipping
		// signal); fall back to created-at for open/closed-unmerged PRs.
		when := pr.Created
		if pr.Merged != nil {
			when = *pr.Merged
		}
		week := isoWeek(when)
		for epicKey := range epics {
			a := ensure(epicKey)
			row := a.week(week)
			row.PRs++
			row.LOCAdded += pr.Additions
			row.LOCDeleted += pr.Deletions
		}
	}

	for _, c := range data.Commits {
		epics := epicsForKeys(c.IssueKeys, issueByKey)
		if len(epics) == 0 {
			continue
		}
		week := isoWeek(c.Committed)
		for epicKey := range epics {
			a := ensure(epicKey)
			row := a.week(week)
			row.Commits++
			// c.Additions / c.Deletions are 0 in v1 (known gap documented in
			// the plan). Still sum them so the field lights up the day we
			// add per-commit stats.
			row.LOCAdded += c.Additions
			row.LOCDeleted += c.Deletions
		}
	}

	// Clamp the momentum knobs so a partially-specified config (or a test that
	// only sets a couple fields) can't divide by zero or misbehave.
	if surge.RecentWeeks < 1 {
		surge.RecentWeeks = 2
	}
	if surge.BaselineWeeks < 1 {
		surge.BaselineWeeks = 8
	}
	if surge.HotRatio <= 0 {
		surge.HotRatio = 2.0
	}
	if surge.RisingRatio <= 0 {
		surge.RisingRatio = 1.2
	}
	if surge.CoolingRatio <= 0 {
		surge.CoolingRatio = 0.8
	}

	// "Now" is the latest active week anywhere in the cache, so every epic's
	// momentum is measured against the same recent window (not its own last
	// week — that would make every epic look freshly active).
	anchorOrd := -1
	for _, a := range aggregates {
		for w := range a.weeks {
			if ord, ok := weekOrdinal(w); ok && ord > anchorOrd {
				anchorOrd = ord
			}
		}
	}
	if anchorOrd < 0 {
		return nil
	}

	projects := make([]Project, 0, len(aggregates))
	for _, a := range aggregates {
		p, included := a.finalize(surge, anchorOrd)
		if !included {
			continue
		}
		projects = append(projects, p)
	}

	// Hottest first: highest momentum, then most recent-window activity, then
	// epic key for a stable order.
	sort.Slice(projects, func(i, j int) bool {
		if projects[i].Momentum != projects[j].Momentum {
			return projects[i].Momentum > projects[j].Momentum
		}
		ai := projects[i].RecentPRs + projects[i].RecentCommits
		aj := projects[j].RecentPRs + projects[j].RecentCommits
		if ai != aj {
			return ai > aj
		}
		return projects[i].EpicKey < projects[j].EpicKey
	})
	return projects
}

// weekOrdinal converts an ISO-week label ("YYYY-Www") to a monotonic week
// number (Mondays since the Unix epoch / 7) so two weeks can be differenced
// exactly across year boundaries. Returns false for an unparseable label.
func weekOrdinal(label string) (int, bool) {
	var y, w int
	if _, err := fmt.Sscanf(label, "%d-W%d", &y, &w); err != nil {
		return 0, false
	}
	monday := isoWeekStartMonday(y, w)
	return int(monday.Unix() / (7 * 86400)), true
}

// buildProjectShares computes per-dev project participation for every
// detected epic. Walks PRs / commits / reviews once, indexing contributions
// by (login, epic) so each mapped dev's ProjectShare slice can be assembled
// in O(epics × shares) instead of re-scanning the full cache per dev.
//
// Team totals span the whole loaded cache (matching what Project.Totals
// shows), so a dev's share row reads "this dev contributed N of the M
// commits this epic has accumulated over its lifetime in the cache."
//
// Reviews use the (repo, pr_number) → epics index built from the PRs list;
// a review on a PR not in the cache (rare — happens at the edge of the
// pull window) is silently skipped.
func buildProjectShares(data *Loaded, projects []Project, ci config.CodeImpactConfig) map[string]map[string]*ProjectShare {
	issueByKey := make(map[string]cache.JiraIssue, len(data.Issues))
	for _, i := range data.Issues {
		issueByKey[i.Key] = i
	}

	// epicMeta carries the team-wide summary + triggers we need to populate
	// each ProjectShare. Only epics that fired surge thresholds are eligible
	// — every other epic stays invisible in the per-dev view, matching the
	// org-wide projects panel.
	type epicMeta struct {
		summary  string
		triggers []string
	}
	relevant := make(map[string]epicMeta, len(projects))
	for _, p := range projects {
		relevant[p.EpicKey] = epicMeta{summary: p.Summary, triggers: p.Triggers}
	}

	// Pre-bucket PRs and commits by epic so the per-(login, epic) walk is
	// cheap, and so we have a (repo, pr_number) → epics index for reviews.
	type prKey struct {
		Repo   string
		Number int
	}
	prEpicsByKey := make(map[prKey]map[string]struct{}, len(data.PRs))

	// dev[login][epic] → counter. team[epic] → counter. Files is the union
	// of file paths across the (dev/team, epic)'s merged PRs — used to derive
	// code_impact per share. Lazily allocated; nil-checks at access time keep
	// the bookkeeping cheap when an epic has no merged-PR activity yet.
	type counter struct {
		prs, commits, reviews int
		loc                   int
		merged                int
		files                 map[string]struct{}
	}
	teamByEpic := map[string]*counter{}
	devByLoginEpic := map[string]map[string]*counter{}

	ensureTeam := func(epic string) *counter {
		c, ok := teamByEpic[epic]
		if !ok {
			c = &counter{}
			teamByEpic[epic] = c
		}
		return c
	}
	ensureDev := func(login, epic string) *counter {
		byEpic, ok := devByLoginEpic[login]
		if !ok {
			byEpic = map[string]*counter{}
			devByLoginEpic[login] = byEpic
		}
		c, ok := byEpic[epic]
		if !ok {
			c = &counter{}
			byEpic[epic] = c
		}
		return c
	}
	addFiles := func(c *counter, files []string) {
		if len(files) == 0 {
			return
		}
		if c.files == nil {
			c.files = map[string]struct{}{}
		}
		for _, f := range files {
			c.files[f] = struct{}{}
		}
	}

	for _, pr := range data.PRs {
		epics := epicsForKeys(pr.IssueKeys, issueByKey)
		if len(epics) == 0 {
			continue
		}
		prEpicsByKey[prKey{Repo: pr.Repo, Number: pr.Number}] = epics
		for epic := range epics {
			if _, ok := relevant[epic]; !ok {
				continue
			}
			tc := ensureTeam(epic)
			tc.prs++
			if pr.Merged != nil {
				tc.merged++
				tc.loc += pr.Additions + pr.Deletions
				addFiles(tc, pr.Files)
			}
			if pr.Author != "" {
				dc := ensureDev(pr.Author, epic)
				dc.prs++
				if pr.Merged != nil {
					dc.merged++
					dc.loc += pr.Additions + pr.Deletions
					addFiles(dc, pr.Files)
				}
			}
		}
	}

	for _, c := range data.Commits {
		epics := epicsForKeys(c.IssueKeys, issueByKey)
		if len(epics) == 0 {
			continue
		}
		for epic := range epics {
			if _, ok := relevant[epic]; !ok {
				continue
			}
			ensureTeam(epic).commits++
			if c.Author != "" {
				ensureDev(c.Author, epic).commits++
			}
		}
	}

	for _, r := range data.Reviews {
		epics, ok := prEpicsByKey[prKey{Repo: r.Repo, Number: r.PRNumber}]
		if !ok {
			continue
		}
		for epic := range epics {
			if _, ok := relevant[epic]; !ok {
				continue
			}
			ensureTeam(epic).reviews++
			if r.Reviewer != "" {
				ensureDev(r.Reviewer, epic).reviews++
			}
		}
	}

	// Assemble per-login ProjectShare slices. We key by login (not by
	// DevIdentity) so attachProjectShares can fold across a dev's multiple
	// github_logins efficiently.
	out := make(map[string]map[string]*ProjectShare, len(devByLoginEpic))
	for login, byEpic := range devByLoginEpic {
		shares := make(map[string]*ProjectShare, len(byEpic))
		for epic, dc := range byEpic {
			if dc.prs+dc.commits+dc.reviews == 0 {
				continue
			}
			tc := teamByEpic[epic]
			meta := relevant[epic]
			shares[epic] = &ProjectShare{
				EpicKey:        epic,
				Summary:        meta.summary,
				DevPRs:         dc.prs,
				TeamPRs:        tc.prs,
				DevCommits:     dc.commits,
				TeamCommits:    tc.commits,
				DevReviews:     dc.reviews,
				TeamReviews:    tc.reviews,
				DevCodeImpact:  computeCodeImpact(len(dc.files), dc.loc, dc.merged, ci),
				TeamCodeImpact: computeCodeImpact(len(tc.files), tc.loc, tc.merged, ci),
				Triggers:       meta.triggers,
			}
		}
		out[login] = shares
	}
	return out
}

// attachProjectShares folds per-login ProjectShare records into each
// DevWindowMetrics. Devs claiming multiple github_logins (real handle +
// git-author-name fallback) sum their share counts across logins so the
// rendered list doesn't double-list the same epic. Empty share slices are
// left nil so `omitempty` keeps metrics.json compact.
func attachProjectShares(devs []DevWindowMetrics, sharesByLogin map[string]map[string]*ProjectShare) []DevWindowMetrics {
	for i := range devs {
		logins := devs[i].Dev.AllGitHubLogins()
		if len(logins) == 0 {
			continue
		}
		merged := map[string]*ProjectShare{}
		for _, login := range logins {
			byEpic, ok := sharesByLogin[login]
			if !ok {
				continue
			}
			for epic, s := range byEpic {
				dst, ok := merged[epic]
				if !ok {
					// Copy so the source map is left untouched for any
					// other dev that happens to claim the same login.
					cp := *s
					merged[epic] = &cp
					continue
				}
				dst.DevPRs += s.DevPRs
				dst.DevCommits += s.DevCommits
				dst.DevReviews += s.DevReviews
				// Code impact is sqrt(sum-of-weighted-inputs), not additive.
				// For devs claiming multiple github_logins the underlying
				// PR sets are disjoint (one PR has one author), so summing
				// the post-sqrt values overstates only when both logins
				// have substantial contributions to the same epic — rare
				// enough that the simpler additive merge is fine, and the
				// team total (which uses the union directly) is exact.
				dst.DevCodeImpact += s.DevCodeImpact
			}
		}
		if len(merged) == 0 {
			continue
		}
		out := make([]ProjectShare, 0, len(merged))
		for _, s := range merged {
			out = append(out, *s)
		}
		// Sort by combined dev activity descending; tie-break alpha by
		// epic key so the order is stable across runs.
		sort.Slice(out, func(a, b int) bool {
			ia := out[a].DevPRs + out[a].DevCommits + out[a].DevReviews
			ib := out[b].DevPRs + out[b].DevCommits + out[b].DevReviews
			if ia != ib {
				return ia > ib
			}
			return out[a].EpicKey < out[b].EpicKey
		})
		devs[i].Projects = out
	}
	return devs
}

// epicsForKeys returns the set of epic keys for a PR/commit's issue_keys,
// via the issueByKey index. Deduped so one PR isn't double-counted when it
// tags two sub-issues of the same epic.
func epicsForKeys(keys []string, issueByKey map[string]cache.JiraIssue) map[string]struct{} {
	if len(keys) == 0 {
		return nil
	}
	out := map[string]struct{}{}
	for _, k := range keys {
		issue, ok := issueByKey[k]
		if !ok {
			continue
		}
		if issue.EpicKey == "" {
			// The PR might tag the epic itself (rare — usually sub-tasks
			// carry the key), in which case the issue IS the epic.
			if issue.IssueType == "Epic" {
				out[issue.Key] = struct{}{}
			}
			continue
		}
		out[issue.EpicKey] = struct{}{}
	}
	return out
}

// epicAggregator accumulates per-week signals for one epic during detection.
type epicAggregator struct {
	epicKey string
	summary string
	weeks   map[string]*ProjectWeekRow
}

// week returns the weekly row for the given label, creating it on demand.
func (a *epicAggregator) week(label string) *ProjectWeekRow {
	row, ok := a.weeks[label]
	if !ok {
		row = &ProjectWeekRow{Week: label}
		a.weeks[label] = row
	}
	return row
}

// finalize computes totals + the recent/baseline momentum signal for one epic,
// anchored at anchorOrd (the cache's latest active week). It returns the
// assembled Project and whether the epic clears the recent-activity floor — the
// only inclusion gate, so dormant and trivial epics drop out and what remains
// is "currently-active initiatives, ranked by how they're trending."
func (a *epicAggregator) finalize(surge config.SurgeConfig, anchorOrd int) (Project, bool) {
	weeks := make([]string, 0, len(a.weeks))
	for w := range a.weeks {
		weeks = append(weeks, w)
	}
	sort.Strings(weeks)

	// Recent window = [anchor-(RecentWeeks-1), anchor]; baseline = the
	// BaselineWeeks immediately before it. Calendar weeks (not active weeks),
	// so an epic that went quiet shows a low baseline → a fresh burst reads hot.
	recentStartOrd := anchorOrd - (surge.RecentWeeks - 1)
	baselineStartOrd := recentStartOrd - surge.BaselineWeeks

	weekly := make([]ProjectWeekRow, 0, len(weeks))
	totals := ProjectTotals{}
	peakWeek := ""
	peakSignal := -1
	var recentSignal, recentPRs, recentCommits, baselineSignal int
	for _, w := range weeks {
		row := a.weeks[w]
		row.CombinedSignal = row.IssuesTouched + row.PRs + row.Commits
		weekly = append(weekly, *row)
		totals.Issues += row.IssuesTouched
		totals.StoryPoints += row.StoryPoints
		totals.PRs += row.PRs
		totals.Commits += row.Commits
		totals.LOCAdded += row.LOCAdded
		totals.LOCDeleted += row.LOCDeleted
		if row.CombinedSignal > peakSignal {
			peakSignal = row.CombinedSignal
			peakWeek = w
		}
		if ord, ok := weekOrdinal(w); ok {
			switch {
			case ord >= recentStartOrd && ord <= anchorOrd:
				recentSignal += row.CombinedSignal
				recentPRs += row.PRs
				recentCommits += row.Commits
			case ord >= baselineStartOrd && ord < recentStartOrd:
				baselineSignal += row.CombinedSignal
			}
		}
	}

	// Activity floor: must be doing real work (PRs/commits, not just issue
	// touches) in the recent window to count as an active initiative.
	if recentPRs+recentCommits < surge.MinRecentActivity {
		return Project{}, false
	}

	recentRate := float64(recentSignal) / float64(surge.RecentWeeks)
	baselineRate := float64(baselineSignal) / float64(surge.BaselineWeeks)

	var momentum float64
	var direction string
	if baselineRate == 0 {
		// No prior baseline — a brand-new initiative. Rank it at the hot tier
		// rather than reporting an infinite ratio.
		momentum = surge.HotRatio
		direction = "new"
	} else {
		momentum = recentRate / baselineRate
		switch {
		case momentum >= surge.HotRatio:
			direction = "hot"
		case momentum >= surge.RisingRatio:
			direction = "rising"
		case momentum <= surge.CoolingRatio:
			direction = "cooling"
		default:
			direction = "steady"
		}
	}

	// Triggers keeps carrying a single token (the direction) for the notable
	// states so the per-dev project panel (ProjectShare) can still flag them;
	// steady/cooling carry none, matching its "notable projects only" intent.
	var triggers []string
	if direction == "new" || direction == "hot" || direction == "rising" {
		triggers = []string{direction}
	}

	first := ""
	last := ""
	if len(weeks) > 0 {
		first = weeks[0]
		last = weeks[len(weeks)-1]
	}
	return Project{
		EpicKey:       a.epicKey,
		Summary:       a.summary,
		FirstSeenWeek: first,
		LastSeenWeek:  last,
		PeakWeek:      peakWeek,
		ActiveWeeks:   len(weeks),
		Totals:        totals,
		Momentum:      momentum,
		Direction:     direction,
		BaselineRate:  baselineRate,
		RecentSignal:  recentSignal,
		RecentPRs:     recentPRs,
		RecentCommits: recentCommits,
		Triggers:      triggers,
		Weekly:        weekly,
	}, true
}
