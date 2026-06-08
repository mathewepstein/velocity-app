package analyze

import (
	"strings"

	"github.com/mathewepstein/velocity/internal/cache"
)

// Team-wide macro flow for the "Velocity" page — DISPLAY-ONLY.
//
// Unlike full_history (which is self-filtered to the configured user), this is
// the whole team's throughput, trended per month, plus a Claude-attribution
// cut. None of it feeds the composite score or Elo — it's the CEO-facing
// "what's our output / what is Claude doing for our velocity" view (D3). There
// is deliberately NO single composite "velocity number" (Goodhart).

// TeamFlow is the macro view: a full-history monthly series + a current-window
// Claude-attribution headline.
type TeamFlow struct {
	Monthly []TeamFlowMonth `json:"monthly"` // backfill_start → current, zero-filled, chronological
	Claude  ClaudeCut       `json:"claude"`  // current-window attribution headline
}

// TeamFlowMonth is one month of team-wide flow. Counts are bucketed by the
// event's own timestamp (created/resolved/merged); MedianCycleHours is the
// median active dev/review time (In Progress + Code Review, dormancy excluded —
// see ActiveDevHours) over tickets RESOLVED that month (heavily skewed → median
// not mean).
type TeamFlowMonth struct {
	Month                string  `json:"month"`
	IssuesCreated        int     `json:"issues_created"`
	IssuesResolved       int     `json:"issues_resolved"`
	PRsCreated           int     `json:"prs_created"`
	PRsMerged            int     `json:"prs_merged"`
	MedianCycleHours     float64 `json:"median_cycle_hours"`
	StoryPoints          float64 `json:"story_points"`           // team SP completed (sum) — dormant until SP coverage
	ClaudeIssuesResolved int     `json:"claude_issues_resolved"` // CLAUDE_GEN-labeled, resolved that month
}

// ClaudeCut is the "what is Claude doing for our velocity" headline for the
// current window: share of resolved tickets carrying a CLAUDE_GEN label, and
// the cycle-time split between Claude-assisted and other tickets.
type ClaudeCut struct {
	WindowStart            string  `json:"window_start"`
	WindowEnd              string  `json:"window_end"`
	IssuesResolved         int     `json:"issues_resolved"`
	ClaudeIssuesResolved   int     `json:"claude_issues_resolved"`
	MedianCycleHoursClaude float64 `json:"median_cycle_hours_claude"`
	MedianCycleHoursOther  float64 `json:"median_cycle_hours_other"`
}

// hasClaudeLabel reports whether any label marks the issue as Claude-generated.
// Matches the CLAUDE_GEN label across its punctuation/case variants
// (CLAUDE_GEN, Claude-Gen) by normalizing to "claudegen". Deliberately does
// NOT match claude-ready (a request, not authorship) or CODEX_GEN (a different
// tool).
func hasClaudeLabel(labels []string) bool {
	for _, l := range labels {
		norm := strings.ToLower(strings.NewReplacer("_", "", "-", "", ".", "", " ", "").Replace(l))
		if norm == "claudegen" {
			return true
		}
	}
	return false
}

// deriveTeamFlow builds the team-wide monthly series over [histStart, current]
// and the Claude cut over the current window [curStart, curEnd].
func deriveTeamFlow(data *Loaded, histStart, current, curStart, curEnd cache.Month) TeamFlow {
	months := cache.MonthsInRange(histStart, current)
	idx := make(map[string]int, len(months))
	rows := make([]TeamFlowMonth, len(months))
	for i, m := range months {
		rows[i] = TeamFlowMonth{Month: m.String()}
		idx[m.String()] = i
	}
	cycleByMonth := map[string][]float64{}

	for _, iss := range data.Issues {
		if i, ok := idx[monthKey(iss.Created)]; ok {
			rows[i].IssuesCreated++
		}
		if iss.Resolved == nil {
			continue
		}
		i, ok := idx[monthKey(*iss.Resolved)]
		if !ok {
			continue
		}
		rows[i].IssuesResolved++
		if iss.StoryPoints > 0 {
			rows[i].StoryPoints += iss.StoryPoints
		}
		if hasClaudeLabel(iss.Labels) {
			rows[i].ClaudeIssuesResolved++
		}
		if c := ActiveDevHours(iss); c > 0 {
			cycleByMonth[rows[i].Month] = append(cycleByMonth[rows[i].Month], c)
		}
	}
	for _, pr := range data.PRs {
		if i, ok := idx[monthKey(pr.Created)]; ok {
			rows[i].PRsCreated++
		}
		if pr.Merged != nil {
			if i, ok := idx[monthKey(*pr.Merged)]; ok {
				rows[i].PRsMerged++
			}
		}
	}
	for month, xs := range cycleByMonth {
		rows[idx[month]].MedianCycleHours = percentile(xs, 50)
	}

	return TeamFlow{
		Monthly: rows,
		Claude:  deriveClaudeCut(data, curStart, curEnd),
	}
}

// deriveClaudeCut computes the current-window Claude-attribution headline.
func deriveClaudeCut(data *Loaded, start, end cache.Month) ClaudeCut {
	var claudeCyc, otherCyc []float64
	resolved, claudeResolved := 0, 0
	for _, iss := range data.Issues {
		if iss.Resolved == nil || !monthInRange(monthKey(*iss.Resolved), start, end) {
			continue
		}
		resolved++
		isClaude := hasClaudeLabel(iss.Labels)
		if isClaude {
			claudeResolved++
		}
		if c := ActiveDevHours(iss); c > 0 {
			if isClaude {
				claudeCyc = append(claudeCyc, c)
			} else {
				otherCyc = append(otherCyc, c)
			}
		}
	}
	return ClaudeCut{
		WindowStart:            start.String(),
		WindowEnd:              end.String(),
		IssuesResolved:         resolved,
		ClaudeIssuesResolved:   claudeResolved,
		MedianCycleHoursClaude: percentile(claudeCyc, 50),
		MedianCycleHoursOther:  percentile(otherCyc, 50),
	}
}
