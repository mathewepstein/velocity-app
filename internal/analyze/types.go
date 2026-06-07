package analyze

import (
	"time"

	"github.com/mathewepstein/velocity/internal/config"
)

// Result is the full metrics.json payload emitted by Run. Fields are grouped
// so the UI can render each view (current window, comparison overlays,
// full-history sparkline, projects panel) without recomputing anything.
//
// Current/Prior/YoY/Quarters/FullHistory always reflect the configured user's
// activity (filtered by GitHub login + Jira accountId) so the single-user
// dashboard keeps rendering against an org-wide cache.
//
// Devs is the additive multi-dev cohort. Run still computes it and downstream
// code reads it from the in-memory struct (the Elo walker, the in-process
// audits, and ScrubResult's JSON-roundtrip deep-copy on the /api/* incognito
// path), so the field keeps a normal JSON tag. It is NOT written to the
// persisted metrics.json, though — WriteMetrics strips it (see there for why):
// every live page now reads the cohort from /api/contributors|dev, so emitting
// it would only bloat the file with a duplicate of what the query API serves.
type Result struct {
	GeneratedAt   time.Time          `json:"generated_at"`
	BackfillStart string             `json:"backfill_start"`
	CurrentMonth  string             `json:"current_month"`
	Current       WindowMetrics      `json:"current"`
	Prior         WindowMetrics      `json:"prior"`
	YoY           WindowMetrics      `json:"yoy"`
	Quarters      []WindowMetrics    `json:"quarters"`
	FullHistory   HistoryRollup      `json:"full_history"`
	Projects      []Project          `json:"projects"`
	Devs          []DevWindowMetrics `json:"devs,omitempty"`
	QAFlow        QAFlow             `json:"qa_flow"`   // display-only cycle-time / QA diagnostics for Current
	TeamFlow      TeamFlow           `json:"team_flow"` // display-only team-wide macro flow + Claude cut (P4)
	Meta          Meta               `json:"meta"`
}

// WindowMetrics is a self-contained view over one contiguous range of months.
// current/prior/yoy all use the same shape so the UI can swap overlays with
// no code branching.
type WindowMetrics struct {
	Label   string       `json:"label"`
	Window  WindowRange  `json:"window"`
	Totals  Totals       `json:"totals"`
	Monthly []MonthlyRow `json:"monthly"`
	Weekly  []WeeklyRow  `json:"weekly"`
}

// WindowRange identifies a whole-month span, inclusive on both ends.
type WindowRange struct {
	Start        string `json:"start"`         // "YYYY-MM"
	End          string `json:"end"`           // "YYYY-MM"
	LengthMonths int    `json:"length_months"` // inclusive count
}

// Totals are summed over every row inside a window.
// PRsReviewed counts reviews submitted by the subject inside the window —
// bucketed by review submission timestamp, not parent PR creation.
//
// UniqueFilesTouched and CodeImpact are window-level only — UniqueFilesTouched
// is the cardinality of the union of file paths across the window's merged
// PRs (NOT the sum of per-row cardinalities, which would double-count files
// touched in multiple months). CodeImpact is the team-p95-capped composite
// `sqrt(α·F + β·LOC_capped + γ·PRs)`; per-row CodeImpact on MonthlyRow /
// WeeklyRow uses raw uncapped LOC since there's no team distribution to cap
// against at row level.
type Totals struct {
	JiraIssuesTouched     int     `json:"jira_issues_touched"`
	JiraIssuesCreated     int     `json:"jira_issues_created"`
	JiraIssuesResolved    int     `json:"jira_issues_resolved"`
	StoryPoints           float64 `json:"story_points"`            // sum of SP on scored issues resolved in window
	ScoredTicketsResolved int     `json:"scored_tickets_resolved"` // count of resolved-in-window issues with SP > 0 (denominator for avg SP)
	PRsCreated            int     `json:"prs_created"`
	PRsMerged             int     `json:"prs_merged"`
	PRsReviewed           int     `json:"prs_reviewed"`
	Commits               int     `json:"commits"`
	LOCAdded              int     `json:"loc_added"`
	LOCDeleted            int     `json:"loc_deleted"`
	ActiveWeeks           int     `json:"active_weeks"` // ISO weeks in the window with ANY activity
	UniqueFilesTouched    int     `json:"unique_files_touched"`
	CodeImpact            float64 `json:"code_impact"`
}

// MonthlyRow is the per-month rollup used inside windows and full history.
// A month with zero activity still appears with zeroed counts so the UI can
// render a continuous x-axis.
type MonthlyRow struct {
	Month                 string  `json:"month"`
	JiraIssuesTouched     int     `json:"jira_issues_touched"`
	JiraIssuesCreated     int     `json:"jira_issues_created"`
	JiraIssuesResolved    int     `json:"jira_issues_resolved"`
	StoryPoints           float64 `json:"story_points"`
	ScoredTicketsResolved int     `json:"scored_tickets_resolved"`
	PRsCreated            int     `json:"prs_created"`
	PRsMerged             int     `json:"prs_merged"`
	PRsReviewed           int     `json:"prs_reviewed"`
	Commits               int     `json:"commits"`
	LOCAdded              int     `json:"loc_added"`
	LOCDeleted            int     `json:"loc_deleted"`
	UniqueFilesTouched    int     `json:"unique_files_touched"`
	CodeImpact            float64 `json:"code_impact"`
}

// WeeklyRow is the per-ISO-week rollup. Label is "YYYY-Www" (ISO week date
// year, which can differ from calendar year at year boundaries).
type WeeklyRow struct {
	Week                  string  `json:"week"`
	JiraIssuesTouched     int     `json:"jira_issues_touched"`
	JiraIssuesCreated     int     `json:"jira_issues_created"`
	JiraIssuesResolved    int     `json:"jira_issues_resolved"`
	StoryPoints           float64 `json:"story_points"`
	ScoredTicketsResolved int     `json:"scored_tickets_resolved"`
	PRsCreated            int     `json:"prs_created"`
	PRsMerged             int     `json:"prs_merged"`
	PRsReviewed           int     `json:"prs_reviewed"`
	Commits               int     `json:"commits"`
	LOCAdded              int     `json:"loc_added"`
	LOCDeleted            int     `json:"loc_deleted"`
	UniqueFilesTouched    int     `json:"unique_files_touched"`
	CodeImpact            float64 `json:"code_impact"`
}

// DevWindowMetrics carries one developer's view over the current window.
// Score is populated for mapped devs by Phase 3; Rating is populated for
// mapped devs by Phase 4. The synthetic "unknown" bucket leaves both nil
// so the leaderboard can skip it cleanly.
type DevWindowMetrics struct {
	Dev                config.DevIdentity `json:"dev"`
	PrimaryLogin       string             `json:"primary_login,omitempty"`
	Totals             Totals             `json:"totals"`
	Monthly            []MonthlyRow       `json:"monthly"`
	Weekly             []WeeklyRow        `json:"weekly"`
	FullHistoryMonthly []MonthlyRow       `json:"full_history_monthly,omitempty"`
	Score              *ContributorScore  `json:"score,omitempty"`
	Rating             *EloRating         `json:"rating,omitempty"`
	Projects           []ProjectShare     `json:"projects,omitempty"`
	// MedianCycleHours is this dev's median ticket cycle time (FirstInProgress→
	// DoneAt) over tickets resolved in the window. Display-only; never scored.
	MedianCycleHours float64 `json:"median_cycle_hours,omitempty"`

	// effectiveFiles is the generated-file-dampened cardinality of the
	// window's merged-PR file set. Used as the F input to code_impact at
	// scoring time so dependency dumps don't pad substance-of-contribution.
	// Unexported on purpose: the plan §6.2 mandates that the anti-gaming
	// layer never surfaces in metrics.json.
	effectiveFiles float64

	// effectiveLOC is the L input to code_impact after the optional D4 patch
	// (churn-weighting + bulk-import dampening). Equals raw LOCAdded+LOCDeleted
	// when both knobs are off, so default behavior is unchanged. Only read by
	// applyCodeImpactCap when a knob is enabled. Unexported for the same
	// anti-gaming reason as effectiveFiles.
	effectiveLOC float64
}

// ProjectShare is one dev's footprint on one detected epic. The dev/* fields
// count contributions by this dev only; the team/* fields are the project's
// org-wide totals over its lifetime in the cache, so the UI can render a
// "20 / 480 commits" share row directly. Triggers mirror the team-scoped
// project's triggers — a future slice may add dev-scoped surge detection.
type ProjectShare struct {
	EpicKey        string   `json:"epic_key"`
	Summary        string   `json:"summary,omitempty"`
	DevPRs         int      `json:"dev_prs"`
	TeamPRs        int      `json:"team_prs"`
	DevCommits     int      `json:"dev_commits"`
	TeamCommits    int      `json:"team_commits"`
	DevReviews     int      `json:"dev_reviews"`
	TeamReviews    int      `json:"team_reviews"`
	DevCodeImpact  float64  `json:"dev_code_impact"`
	TeamCodeImpact float64  `json:"team_code_impact"`
	Triggers       []string `json:"triggers,omitempty"`
}

// ContributorScore is the A4 composite: a weighted sum of per-metric
// z-scores across the team in one window. Breakdown is the per-metric
// contribution (weight × z), so the UI can render a stacked bar showing
// which signals drove the total. Rank is 1-indexed, sorted by Total desc.
type ContributorScore struct {
	Total     float64            `json:"total"`
	Breakdown map[string]float64 `json:"breakdown"`
	Rank      int                `json:"rank"`
}

// EloRating is the per-dev bi-weekly rating snapshot exposed on the
// leaderboard. Current is the rating after the most recently completed
// period; DeltaPeriod is that period's change (zero if the dev sat out).
// History is the rating trajectory in chronological order, one entry per
// period the dev played in — used to render the sparkline. Provisional is
// true when the dev hasn't played enough periods for the rating to be
// trustworthy (controlled by ScoringConfig.ProvisionalUntilPeriods).
type EloRating struct {
	Current       float64   `json:"current"`
	DeltaPeriod   float64   `json:"delta_period"`
	PeriodsPlayed int       `json:"periods_played"`
	Provisional   bool      `json:"provisional"`
	History       []float64 `json:"history"`
	// HistoryDates carries the period START date (YYYY-MM-DD, the UTC Monday
	// opening the bi-weekly period) for each History entry, same length and
	// order as History. It lets the frontend label the Elo chart with real
	// dates instead of opaque "Pn" and clip the trajectory to a month window.
	// Empty for never-played devs (no history). Parallel to History rather
	// than folded into it so consumers reading History as a plain number
	// series (e.g. the leaderboard top-N trend) are unaffected.
	HistoryDates []string `json:"history_dates,omitempty"`
}

// HistoryRollup is the full-history sparkline: every month from
// backfill_start through current_month, zeros included.
type HistoryRollup struct {
	Monthly []MonthlyRow `json:"monthly"`
}

// Project is one detected epic-scale initiative, ranked by momentum (recent
// activity rate vs its own trailing baseline) rather than static lifetime
// thresholds. Momentum is the recent ÷ baseline weekly-rate ratio; Direction
// buckets it (new|hot|rising|steady|cooling). Recent* are the recent-window
// counts that the panel surfaces and that the activity floor gates on.
type Project struct {
	EpicKey       string           `json:"epic_key"`
	Summary       string           `json:"summary"`
	FirstSeenWeek string           `json:"first_seen_week"`
	LastSeenWeek  string           `json:"last_seen_week"`
	PeakWeek      string           `json:"peak_week"`
	ActiveWeeks   int              `json:"active_weeks"`
	Totals        ProjectTotals    `json:"totals"`
	Momentum      float64          `json:"momentum"`
	Direction     string           `json:"direction"`
	BaselineRate  float64          `json:"baseline_rate"`
	RecentSignal  int              `json:"recent_signal"`
	RecentPRs     int              `json:"recent_prs"`
	RecentCommits int              `json:"recent_commits"`
	Triggers      []string         `json:"triggers"`
	Weekly        []ProjectWeekRow `json:"weekly"`
}

// ProjectTotals mirrors the surge-detection signals, kept separate from
// Totals so the UI's project panel doesn't carry fields that don't apply
// (e.g., a project has no "issues created" vs "touched" distinction —
// attribution is by epic link, not calendar state).
type ProjectTotals struct {
	Issues      int     `json:"issues"`
	StoryPoints float64 `json:"story_points"`
	PRs         int     `json:"prs"`
	Commits     int     `json:"commits"`
	LOCAdded    int     `json:"loc_added"`
	LOCDeleted  int     `json:"loc_deleted"`
}

// ProjectWeekRow is the per-week time series for one project. CombinedSignal
// is the simple sum used to pick peak_week (LOC excluded — it dwarfs discrete
// counts numerically and would always pick the week with one big refactor).
type ProjectWeekRow struct {
	Week           string  `json:"week"`
	IssuesTouched  int     `json:"issues_touched"`
	StoryPoints    float64 `json:"story_points"`
	PRs            int     `json:"prs"`
	Commits        int     `json:"commits"`
	LOCAdded       int     `json:"loc_added"`
	LOCDeleted     int     `json:"loc_deleted"`
	CombinedSignal int     `json:"combined_signal"`
}

// Meta surfaces analyzer-level diagnostics: how many records went into the
// result, how many months had any data. Useful for `velocity doctor` and for
// UI footers that say "based on 78 months of history".
type Meta struct {
	JiraIssuesLoaded     int     `json:"jira_issues_loaded"`
	PRsLoaded            int     `json:"prs_loaded"`
	CommitsLoaded        int     `json:"commits_loaded"`
	ReviewsLoaded        int     `json:"reviews_loaded"`
	MonthsLoaded         int     `json:"months_loaded"`
	ProjectsDetected     int     `json:"projects_detected"`
	DevsMapped           int     `json:"devs_mapped"`
	EloCompositeSpearman float64 `json:"elo_composite_spearman"`
}
