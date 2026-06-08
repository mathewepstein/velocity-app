package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// DefaultProfile is the profile name used when no other is selected. The TOML
// supports multiple profiles; v1 only reads this one.
const DefaultProfile = "default"

// Config is the full TOML document. Only one profile is active today, but the
// map-keyed structure keeps the door open for [profiles.work], [profiles.oss],
// etc. without a breaking format change.
type Config struct {
	Profiles map[string]Profile `toml:"profiles"`
	// Cache selects the local cache substrate. Global (not per-profile): there
	// is a single cache under cache.Root() regardless of the active profile.
	Cache CacheConfig `toml:"cache,omitempty"`
}

// CacheConfig selects the storage backend for the month-partitioned record
// cache. Backend is "sqlite" (the default and standard substrate) or "json"
// (the legacy month-partitioned JSON corpus, retained only as an opt-in
// fallback). An empty Backend resolves to sqlite, so a fresh install needs no
// configuration to get the standard substrate. Resolved in cache.OpenStore.
type CacheConfig struct {
	Backend string `toml:"backend,omitempty"`
}

// Profile groups everything about one Atlassian + GitHub setup.
type Profile struct {
	Name        string            `toml:"name"`
	Jira        JiraConfig        `toml:"jira"`
	GitHub      GitHubConfig      `toml:"github"`
	Window      WindowConfig      `toml:"window"`
	Surge       SurgeConfig       `toml:"surge"`
	UI          UIConfig          `toml:"ui"`
	Devs        []DevIdentity     `toml:"devs,omitempty"`
	Scoring     ScoringConfig     `toml:"scoring"`
	StoryPoints StoryPointsConfig `toml:"storypoints"`
}

// StoryPointsConfig parameterises the deterministic story-points band engine
// (storypoints-engine-plan.md, Stage 2). The engine turns a TicketEvidence
// bundle into a Fibonacci band by starting from the rubric's cycle-time × LOC
// quadrant prior, then nudging up by *thinking* signals (rework, review
// contention, touched-area risk) — never by raw size alone. Every knob here is
// tunable in `[profiles.<p>.storypoints]`; zero-values auto-fill from
// DefaultStoryPointsConfig so an absent block still scores.
//
// The band is only a prior. NeedsInsight (see the engine) flags tickets whose
// deterministic band is unreliable — straddling two Fibonacci steps, inflated
// by cycle time with no corroborating rework, or PR-less — for a human/LLM pass.
type StoryPointsConfig struct {
	// Scale is the Fibonacci ladder the band snaps to. Default 1,2,3,5,8,13.
	Scale []int `toml:"scale"`

	// Quadrant cutoffs — the rubric's sanity-check axes.
	LOCThreshold       int     `toml:"loc_threshold"`        // "high LOC" boundary (default 100, net LOC)
	CycleDaysThreshold float64 `toml:"cycle_days_threshold"` // "long cycle" boundary (default 2 days)

	// Quadrant base efforts (continuous, pre-signal). One per quadrant cell.
	// Long-cycle bases are higher because the rubric reads a long cycle on a
	// small diff as hard thinking — but see MinThinkingForHighBand, which flags
	// the case where the long cycle is really just queue latency.
	BaseShortLow  float64 `toml:"base_short_low"`  // short cycle, low LOC  (default 1.5)
	BaseShortHigh float64 `toml:"base_short_high"` // short cycle, high LOC (default 2.5)
	BaseLongLow   float64 `toml:"base_long_low"`   // long cycle, low LOC   (default 6.0)
	BaseLongHigh  float64 `toml:"base_long_high"`  // long cycle, high LOC  (default 8.0)

	// Thinking / process signal weights, in effort points added to the base.
	ReworkWeight      float64 `toml:"rework_weight"`       // per genuine rework bounce (review/QA→dev), default 2.0
	ReviewRoundWeight float64 `toml:"review_round_weight"` // per CHANGES_REQUESTED round (default 1.0)
	DeepThreadWeight  float64 `toml:"deep_thread_weight"`  // per 3+-reply review thread = contention (default 1.5)
	HighRiskBonus     float64 `toml:"high_risk_bonus"`     // touched-area risk = high (default 2.0)
	MediumRiskBonus   float64 `toml:"medium_risk_bonus"`   // touched-area risk = medium (default 0.5)
	CrossRepoBonus    float64 `toml:"cross_repo_bonus"`    // change spans 2+ repos (default 1.0)

	// StraddleFraction: when the raw effort sits within this fraction of the
	// midpoint between two adjacent scale steps, the band is reported as a range
	// and flagged NeedsInsight (default 0.15).
	StraddleFraction float64 `toml:"straddle_fraction"`

	// MinThinkingForHighBand: if the quadrant prior lands at a high band (≥5)
	// but the summed thinking-signal contribution is below this, the band is
	// likely inflated by cycle time alone (e.g. QA-queue latency, not rework) —
	// flag NeedsInsight (default 1.0).
	MinThinkingForHighBand float64 `toml:"min_thinking_for_high_band"`

	// MaxThinkingBonus optionally caps the TOTAL thinking-signal contribution
	// added to the quadrant base, so one runaway signal (e.g. a PR with 8
	// changes-requested rounds) can't max out the band on its own. **Default 0
	// (disabled):** a cap pins capped tickets to a single raw value (base+cap),
	// and when that lands in a wide Fibonacci gap's straddle zone (e.g. 8↔13) it
	// creates a flag pileup — worse than the uncapped distribution. Exposed as a
	// tunable for configs that want it, but off by default.
	MaxThinkingBonus float64 `toml:"max_thinking_bonus"`

	// ReworkMinDwellMins de-noises the rework signal: a backward review/QA→dev
	// bounce counts as real rework only if a commit landed in its window OR the
	// ticket dwelled in the review/QA stage at least this many minutes before
	// bouncing. Below it, a commit-less bounce is treated as status-toggle noise
	// (a board misclick / instantaneous flip). Default 5.
	ReworkMinDwellMins float64 `toml:"rework_min_dwell_mins"`

	// HighBandThinkingShare gates "high" confidence on a high band (points ≥ 5):
	// the thinking-signal contribution must explain at least this fraction of the
	// raw effort, otherwise the band is mostly quadrant base (calendar/LOC) and
	// confidence is downgraded to "medium" rather than overclaiming "high". This
	// is what gives the engine a real low/medium/high spread instead of the
	// binary low-or-high it had at the top. Default 0.5 (thinking must be the
	// majority). An unset value is filled to the default by the config merge; a
	// negative value disables the share test (legacy: every above-floor band is
	// "high").
	HighBandThinkingShare float64 `toml:"high_band_thinking_share"`

	// ReworkCountCap / ReviewRoundCap saturate the rework and review-round
	// contributions: the count used for the weight is capped here, so the 1st and
	// 2nd bounce carry full signal but a runaway 7th barely moves the band. This
	// is the per-signal alternative to the global MaxThinkingBonus (which pinned
	// capped tickets to one raw value and piled them into a straddle zone). The
	// cap is per-signal and integer, so normal-path sums stay on clean Fibonacci
	// steps. 0 disables the cap for that signal (legacy linear count·weight).
	// Defaults: rework 3, review 4.
	ReworkCountCap int `toml:"rework_count_cap"`
	ReviewRoundCap int `toml:"review_round_cap"`

	// SmallDiffLOCFloor / SmallDiffBonusScale apply a size sanity-floor: below the
	// floor (net LOC), a change can't claim the full *rework* bonus — a 1-line
	// edit that bounced many times is flaky-fix churn, not big work. The rework
	// contribution is scaled by SmallDiffBonusScale when NetLOC is in
	// (0, SmallDiffLOCFloor). Risk, review-round, and deep-thread credit are NOT
	// scaled: a tiny diff in a hot file is genuinely risky, and a small diff with
	// real approach debate is legitimately hard (the locked high-risk-small-fix
	// anchor → 5). SmallDiffLOCFloor 0 disables the floor. Defaults: floor 20 LOC,
	// scale 0.5.
	SmallDiffLOCFloor   int     `toml:"small_diff_loc_floor"`
	SmallDiffBonusScale float64 `toml:"small_diff_bonus_scale"`

	// SplitThreshold implements the legend literally at the top of the scale: a
	// true 13 is the band you're *least* sure is a single unit of work, so a
	// silent high-confidence 13 is nearly a contradiction. When the raw effort of
	// a top-band ticket reaches this, the band stays 13 but is flagged
	// NeedsInsight ("effort exceeds a single-ticket scale — likely should have
	// been split; confirm scope") and confidence is capped at "medium" — routing
	// genuine monsters (raw ≫ 13) to a scope/split check rather than rubber-
	// stamping them. Set well above the 13 floor so only clearly-oversized work
	// trips it, not every defensible-contention 13. 0 disables. Default 18.
	SplitThreshold float64 `toml:"split_threshold"`
}

// ReworkMinDwell returns ReworkMinDwellMins as a duration for the rework
// de-noiser (analyze.ReworkCountWithCommits).
func (c StoryPointsConfig) ReworkMinDwell() time.Duration {
	return time.Duration(c.ReworkMinDwellMins * float64(time.Minute))
}

// DevIdentity unifies one developer's GitHub identifiers and Jira accountId so
// multi-dev rollups merge cleanly regardless of which side a record came from.
// Populated by `velocity devs discover`; anyone outside this table surfaces
// under "unknown" in the leaderboard.
//
// One human can present as multiple GitHub identifiers in our cache — their
// real GH login plus N git-author-name fallback strings from commits whose
// email isn't linked to a GitHub account (see pull/github.go fallback). The
// GitHubLogins slice absorbs all of them under one identity.
//
// JSON tags mirror the TOML keys so metrics.json stays readable from the web
// UI without a translation layer.
type DevIdentity struct {
	// GitHubLogin is the legacy single-login field. Reads from disk for
	// backward compatibility with v1 configs; applyDefaults migrates the
	// value into GitHubLogins on Load. New writes leave it empty.
	GitHubLogin   string   `toml:"github_login,omitempty" json:"-"`
	GitHubLogins  []string `toml:"github_logins,omitempty" json:"github_logins,omitempty"`
	JiraAccountID string   `toml:"jira_account_id" json:"jira_account_id"`
	DisplayName   string   `toml:"display_name" json:"display_name"`
	ExcludedBot   bool     `toml:"excluded_bot,omitempty" json:"excluded_bot,omitempty"`
}

// AllGitHubLogins returns the set of GitHub identifiers this dev claims. Falls
// back to the legacy singular GitHubLogin field for in-memory DevIdentity
// values that bypass the Load() migration path (tests, ad-hoc construction).
func (d DevIdentity) AllGitHubLogins() []string {
	if len(d.GitHubLogins) > 0 {
		out := make([]string, len(d.GitHubLogins))
		copy(out, d.GitHubLogins)
		return out
	}
	if d.GitHubLogin != "" {
		return []string{d.GitHubLogin}
	}
	return nil
}

// MatchesGitHubLogin reports whether this dev claims the given GitHub
// identifier. Empty input never matches.
func (d DevIdentity) MatchesGitHubLogin(login string) bool {
	if login == "" {
		return false
	}
	for _, l := range d.GitHubLogins {
		if l == login {
			return true
		}
	}
	return d.GitHubLogin == login
}

// ScoringConfig holds the contributor-scoring and Elo-rating knobs. Numeric
// zero-values auto-fill from DefaultScoringConfig() so a config that omits the
// block entirely still parses with sensible defaults.
//
// KTiers supersedes the binary KFactorNew/KFactorEst/NewThreshold triple as of
// Phase 7. Older fields are retained for one cycle so existing velocity.toml
// files still parse; applyDefaults synthesizes a two-tier KTiers from them
// when the new field is absent.
type ScoringConfig struct {
	Weights                 map[string]float64 `toml:"weights"`
	CodeImpact              CodeImpactConfig   `toml:"code_impact"`
	Normalize               NormalizeConfig    `toml:"normalize"`
	KTiers                  []KTier            `toml:"k_tiers"`
	KFactorNew              int                `toml:"k_factor_new"`             // Deprecated: superseded by KTiers
	KFactorEst              int                `toml:"k_factor_established"`     // Deprecated: superseded by KTiers
	NewThreshold            int                `toml:"new_dev_period_threshold"` // Deprecated: superseded by KTiers
	IdleDecayAfter          int                `toml:"idle_decay_after"`
	IdleDecayDelta          float64            `toml:"idle_decay_delta"`
	ProvisionalUntilPeriods int                `toml:"provisional_until_periods"`
	PeriodWeeks             int                `toml:"period_weeks"`
	Exclude                 []string           `toml:"exclude"`
}

// CodeImpactConfig parameterises the `code_impact` composite metric. The
// formula is `sqrt(Alpha·unique_files + Beta·loc_capped + Gamma·merged_prs)`
// where loc_capped is `min(loc_delta, team_p95)` at window scope (no cap at
// row scope). All three coefficients tunable in
// `[profiles.<p>.scoring.code_impact]`.
type CodeImpactConfig struct {
	Alpha float64 `toml:"alpha"`
	Beta  float64 `toml:"beta"`
	Gamma float64 `toml:"gamma"`

	// ChurnWeighting weights each file's LOC by how often it's revisited
	// across the corpus, so add-once boilerplate counts less than
	// repeatedly-edited files. OFF by default — when off, code_impact is
	// computed exactly as before (raw LOC), so enabling it is an opt-in
	// re-ranking. Requires FileChange data (backfill `file-changes` phase).
	//   - ChurnFloor: LOC weight for a file touched once (default 0.5).
	//   - ChurnFullAt: corpus touch count at which weight reaches 1.0 (default 4).
	ChurnWeighting bool    `toml:"churn_weighting"`
	ChurnFloor     float64 `toml:"churn_floor"`
	ChurnFullAt    int     `toml:"churn_full_at"`

	// BulkImportDampening detects boilerplate/vendor dumps from FileChange
	// (huge added LOC, almost entirely additions, across many added-status
	// files) and damps that PR's LOC contribution to code_impact. OFF by
	// default. A structural replacement for the reactive p95 LOC cap.
	//   - BulkImportMinLOC: min added LOC for a PR to qualify (default 5000).
	//   - BulkImportAddRatio: additions/(additions+deletions) ≥ this (default 0.95).
	//   - BulkImportMinFiles: min added-status files (default 20).
	//   - BulkImportWeight: LOC weight applied to a qualifying PR (default 0.25).
	BulkImportDampening bool    `toml:"bulk_import_dampening"`
	BulkImportMinLOC    int     `toml:"bulk_import_min_loc"`
	BulkImportAddRatio  float64 `toml:"bulk_import_add_ratio"`
	BulkImportMinFiles  int     `toml:"bulk_import_min_files"`
	BulkImportWeight    float64 `toml:"bulk_import_weight"`

	// Bulk-data-dump dampening (ON by default). A single PR dominated by one
	// serialized-data extension (JSON/CSV/XML/… — a fixture or export dump) has
	// those data files' contribution to BOTH the LOC and file-count terms scaled
	// by DumpWeight. Detected from the path list (extension dominance + scale),
	// so it's truncation-proof and never flags a source-code PR or a small data
	// PR. This is what stops a multi-million-line JSON dump from reading as
	// code_impact; set DisableDumpDampening to turn it off.
	//   - DumpWeight:    weight for data files in a dump (default 0 = no credit).
	//   - DumpDominance: min single-data-extension fraction (default 0.9).
	//   - DumpMinFiles:  min files for a PR to qualify as a dump (default 50).
	DisableDumpDampening bool    `toml:"disable_dump_dampening"`
	DumpWeight           float64 `toml:"dump_weight"`
	DumpDominance        float64 `toml:"dump_dominance"`
	DumpMinFiles         int     `toml:"dump_min_files"`

	// LOCCapPercentile is the team-wide LOC percentile that caps the loc term in
	// applyCodeImpactCap. Raised from the old hardcoded 95 to 99 so a legitimate
	// high-output contributor isn't truncated to the cohort's 95th percentile —
	// bulk dumps are now handled at the source by DumpWeight, so the blunt cap no
	// longer has to do that job.
	LOCCapPercentile float64 `toml:"loc_cap_percentile"`
}

// NormalizeConfig drives the silent anti-gaming layer applied to commits,
// loc_changed, and code_impact before z-scoring. None of these knobs surface
// in metrics.json — the multiplier is recomputed each analyze and only the
// dampened values feed scoring.
//
//   - SpamThreshold / SpamPenalty — commit-spam dampening triggers when
//     commits_per_unique_file_in_PRs exceeds SpamThreshold; multiplier shrinks
//     by SpamPenalty per unit above the threshold.
//   - StuffPenalty — LOC-stuffing dampening triggers when a dev's
//     loc_per_unique_file ratio lands above the team's p90; multiplier shrinks
//     by StuffPenalty per overflow unit.
//   - MultiplierFloor — combined multiplier never drops below this (default 0.5).
//   - GeneratedFilePatterns / GeneratedFileWeight — files matching one of the
//     patterns count fractionally (default 0.25) toward the EFFECTIVE unique
//     files used inside code_impact. Totals.UniqueFilesTouched remains the raw
//     cardinality for display.
type NormalizeConfig struct {
	SpamThreshold         float64  `toml:"spam_threshold"`
	SpamPenalty           float64  `toml:"spam_penalty"`
	StuffPenalty          float64  `toml:"stuff_penalty"`
	MultiplierFloor       float64  `toml:"multiplier_floor"`
	GeneratedFilePatterns []string `toml:"generated_file_patterns"`
	GeneratedFileWeight   float64  `toml:"generated_file_weight"`
}

// KTier is one row in the K-factor ramp: from MinPeriods (inclusive) up to
// the next tier's MinPeriods (exclusive), apply this K. Sort ascending by
// MinPeriods; the first tier should start at 0 so brand-new devs get a K.
type KTier struct {
	MinPeriods int `toml:"min_periods"`
	K          int `toml:"k"`
}

type JiraConfig struct {
	BaseURL   string     `toml:"base_url"`
	Email     string     `toml:"email"`
	Projects  []string   `toml:"projects"`
	AccountID string     `toml:"account_id"`
	Fields    JiraFields `toml:"fields"`
}

// JiraFields holds the resolved custom-field IDs for the Jira instance.
// Discovered during `velocity init` so analyze never has to guess.
type JiraFields struct {
	StoryPoints string `toml:"story_points"`
	EpicLink    string `toml:"epic_link"`
}

type GitHubConfig struct {
	Username string   `toml:"username"`
	Orgs     []string `toml:"orgs"`
}

type WindowConfig struct {
	BackfillStart       string `toml:"backfill_start"`
	DefaultLengthMonths int    `toml:"default_length_months"`
}

type SurgeConfig struct {
	// Legacy static lifetime thresholds. Retained for config-file compatibility
	// but no longer consulted — detectProjects switched to momentum detection
	// (recent-vs-baseline activity rate) on 2026-06-04. Safe to leave in a
	// config; they're ignored.
	MinStoryPoints int `toml:"min_story_points"`
	MinActiveWeeks int `toml:"min_active_weeks"`
	MinPRs         int `toml:"min_prs"`
	MinCommits     int `toml:"min_commits"`
	MinLOC         int `toml:"min_loc"`

	// Momentum detection. An epic's momentum = its recent-window weekly
	// activity rate ÷ its trailing-baseline weekly rate. RecentWeeks and
	// BaselineWeeks define the two windows (anchored at the latest active week
	// in the cache). MinRecentActivity is the PRs+commits floor an epic must
	// clear in the recent window to appear at all (drops dormant/trivial
	// epics). HotRatio/RisingRatio/CoolingRatio bucket the momentum into a
	// direction label.
	RecentWeeks       int     `toml:"recent_weeks"`
	BaselineWeeks     int     `toml:"baseline_weeks"`
	MinRecentActivity int     `toml:"min_recent_activity"`
	HotRatio          float64 `toml:"hot_ratio"`
	RisingRatio       float64 `toml:"rising_ratio"`
	CoolingRatio      float64 `toml:"cooling_ratio"`
}

type UIConfig struct {
	// DefaultComparison is one of: prior, yoy, qoq, none.
	DefaultComparison string `toml:"default_comparison"`
}

// DefaultProfileConfig returns a Profile populated with the numeric/behavioral
// defaults locked in the plan. Identity fields (email, account_id, etc.) are
// left blank — those come from the user.
func DefaultProfileConfig() Profile {
	return Profile{
		Name: "default",
		Window: WindowConfig{
			BackfillStart:       "2019-11",
			DefaultLengthMonths: 3,
		},
		Surge: SurgeConfig{
			MinStoryPoints:    5,
			MinActiveWeeks:    3,
			MinPRs:            3,
			MinCommits:        20,
			MinLOC:            1000,
			RecentWeeks:       2,
			BaselineWeeks:     8,
			MinRecentActivity: 3,
			HotRatio:          2.0,
			RisingRatio:       1.2,
			CoolingRatio:      0.8,
		},
		UI: UIConfig{
			DefaultComparison: "prior",
		},
		Scoring:     DefaultScoringConfig(),
		StoryPoints: DefaultStoryPointsConfig(),
	}
}

// DefaultStoryPointsConfig returns the locked defaults for the band engine. The
// quadrant bases + signal weights are calibrated against the rubric's anchor
// examples (a 3-line high-risk auth fix with 2 review rounds → 5; a 400-LOC
// scaffold with one rubber-stamp review → 2–3; a 50-LOC change reverted+rewritten
// → 5–8).
func DefaultStoryPointsConfig() StoryPointsConfig {
	return StoryPointsConfig{
		Scale:        []int{1, 2, 3, 5, 8, 13},
		LOCThreshold: 100,
		// Calibrated to CD's active-cycle distribution (In-Progress→Done minus
		// QA-queue wait): median ≈ 8d, p75 ≈ 24d. 14 days puts "long cycle" at
		// roughly the top third, so it discriminates instead of catching nearly
		// everything (see discovery/velocity/storypoints-band-calibration.md).
		CycleDaysThreshold: 14,
		// Bases are the LOW end of each rubric quadrant cell's range (short/low
		// 1–2, short/high 2–3, long/* 5–8/5–13) and land ON scale steps, so a
		// ticket with no thinking nudge snaps cleanly to a confident floor instead
		// of straddling. Thinking signals push UP within the cell's range from there.
		BaseShortLow:  1.0,
		BaseShortHigh: 2.0,
		BaseLongLow:   5.0,
		BaseLongHigh:  6.0,
		// Rework is the strongest complexity signal in the rubric (a revert/rewrite
		// means the approach was wrong), so flips and changes-requested rounds carry
		// real weight — enough that a small diff that bounced is a 5, not a 2.
		// Weights are integers on purpose: half-points added to integer bases land
		// sums exactly on Fibonacci midpoints (1.5, 2.5, …), which the straddle
		// detector then flags spuriously. Integer sums snap cleanly except a true
		// 4 (the 3↔5 midpoint), which genuinely warrants a straddle.
		ReworkWeight:           2.0,
		ReviewRoundWeight:      1.0,
		DeepThreadWeight:       2.0,
		HighRiskBonus:          2.0,
		MediumRiskBonus:        1.0,
		CrossRepoBonus:         1.0,
		StraddleFraction:       0.15,
		MinThinkingForHighBand: 1.0,
		MaxThinkingBonus:       0, // disabled by default — see field doc
		ReworkMinDwellMins:     5,
		HighBandThinkingShare:  0.5,
		ReworkCountCap:         3,
		ReviewRoundCap:         4,
		SmallDiffLOCFloor:      20,
		SmallDiffBonusScale:    0.5,
		SplitThreshold:         18,
	}
}

// DefaultScoringConfig returns the contributor-score defaults: the weights map
// each metric contributes to the composite via weighted z-score, plus the Elo
// knobs used by the bi-weekly rolling rating. LOC is pre-dampened (p95 cap +
// sqrt) at analyze time, so the 0.25 weight assumes that dampening has
// already been applied.
func DefaultScoringConfig() ScoringConfig {
	return ScoringConfig{
		Weights: map[string]float64{
			"prs_merged":           3.0,
			"jira_issues_resolved": 2.0,
			"code_impact":          1.5,
			"prs_reviewed":         1.0,
			"prs_created":          0.5,
			"jira_issues_touched":  0.5,
			"active_weeks":         0.5,
			"story_points":         0.5,
			"loc_changed":          0.25,
		},
		CodeImpact: CodeImpactConfig{
			Alpha: 1.0,
			Beta:  0.5,
			Gamma: 2.0,
			// Churn-weighting + bulk-import dampening default OFF (the bools);
			// these tunables only take effect once a knob is enabled. Defaults
			// preserve the pre-patch code_impact exactly.
			ChurnFloor:         0.5,
			ChurnFullAt:        4,
			BulkImportMinLOC:   5000,
			BulkImportAddRatio: 0.95,
			BulkImportMinFiles: 20,
			BulkImportWeight:   0.25,
			// Bulk-data-dump dampening: ON by default (DisableDumpDampening
			// false), data files in a detected dump credited at DumpWeight=0.
			DumpDominance:    0.9,
			DumpMinFiles:     50,
			LOCCapPercentile: 99,
		},
		Normalize: NormalizeConfig{
			SpamThreshold:       1.5,
			SpamPenalty:         0.25,
			StuffPenalty:        0.25,
			MultiplierFloor:     0.5,
			GeneratedFileWeight: 0.25,
			GeneratedFilePatterns: []string{
				"*.lock",
				"package-lock.json",
				"yarn.lock",
				"pnpm-lock.yaml",
				"go.sum",
				"composer.lock",
				"gemfile.lock",
				"*.min.js",
				"*.min.css",
				"*.pb.go",
				"*_pb2.py",
				"*.generated.*",
				"*/generated/*",
				"*/.next/*",
				"*/dist/*",
				"*/build/*",
				"*/vendor/*",
				"*/node_modules/*",
			},
		},
		KTiers: []KTier{
			{MinPeriods: 0, K: 32},
			{MinPeriods: 4, K: 28},
			{MinPeriods: 9, K: 28},
			{MinPeriods: 17, K: 30},
		},
		KFactorNew:              32,
		KFactorEst:              16,
		NewThreshold:            6,
		IdleDecayAfter:          3,
		IdleDecayDelta:          8.0,
		ProvisionalUntilPeriods: 12,
		PeriodWeeks:             2,
	}
}

// DefaultBotExcludes is the built-in suppression list merged with
// ScoringConfig.Exclude at runtime. Patterns support a single trailing or
// leading `*` wildcard; everything else is matched literally (case-insensitive).
// Resolved by MatchesBotPattern.
var DefaultBotExcludes = []string{
	"*[bot]",
	"dependabot",
	"renovate",
	"github-actions",
	"claude*",
}

// MatchesBotPattern reports whether login matches any of the given patterns.
// Patterns support a single leading or trailing `*` wildcard. Comparison is
// case-insensitive.
func MatchesBotPattern(login string, patterns []string) bool {
	l := strings.ToLower(login)
	for _, raw := range patterns {
		p := strings.ToLower(strings.TrimSpace(raw))
		if p == "" {
			continue
		}
		switch {
		case strings.HasPrefix(p, "*") && strings.HasSuffix(p, "*") && len(p) >= 2:
			if strings.Contains(l, strings.Trim(p, "*")) {
				return true
			}
		case strings.HasPrefix(p, "*"):
			if strings.HasSuffix(l, strings.TrimPrefix(p, "*")) {
				return true
			}
		case strings.HasSuffix(p, "*"):
			if strings.HasPrefix(l, strings.TrimSuffix(p, "*")) {
				return true
			}
		default:
			if l == p {
				return true
			}
		}
	}
	return false
}

// EffectiveExcludes returns the union of DefaultBotExcludes and the configured
// Scoring.Exclude list, preserving order and dropping duplicates. Use this when
// deciding whether to drop an author from the contributor-score pipeline.
func (s ScoringConfig) EffectiveExcludes() []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(DefaultBotExcludes)+len(s.Exclude))
	for _, p := range DefaultBotExcludes {
		key := strings.ToLower(p)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, p)
	}
	for _, p := range s.Exclude {
		key := strings.ToLower(p)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, p)
	}
	return out
}

// Load reads and parses the config file at Path(). Returns os.ErrNotExist
// (wrapped) if the file is absent, so callers can offer `velocity init`.
// Missing numeric/behavioral fields are filled in from DefaultProfileConfig so
// older configs don't break when new fields land.
func Load() (*Config, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	return LoadFrom(p)
}

// LoadFrom reads and parses a config file from an explicit path. Useful for
// tests and for honoring $VELOCITY_CONFIG without re-resolving.
func LoadFrom(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("config file not found at %s (run `velocity init`): %w", path, err)
		}
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.applyDefaults()
	return &cfg, nil
}

// Save writes the config to Path(), creating parents and using 0o600 perms.
func (c *Config) Save() error {
	p, err := Path()
	if err != nil {
		return err
	}
	return c.SaveTo(p)
}

// SaveTo writes the config to an explicit path. The write is atomic: contents
// land in a tempfile in the same directory, get fsynced, and only then replace
// the existing file via rename. If a previous config exists, it's also copied
// to "<path>.bak" before the rename so a corrupted save (or a regretted edit)
// stays one cp away from recovery.
func (c *Config) SaveTo(path string) error {
	if err := EnsureDir(path); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".velocity-config-*.toml.tmp")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpPath := tmp.Name()
	cleanupTmp := func() { _ = os.Remove(tmpPath) }

	if err := os.Chmod(tmpPath, 0o600); err != nil {
		_ = tmp.Close()
		cleanupTmp()
		return fmt.Errorf("chmod temp config: %w", err)
	}
	if err := toml.NewEncoder(tmp).Encode(c); err != nil {
		_ = tmp.Close()
		cleanupTmp()
		return fmt.Errorf("encode config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanupTmp()
		return fmt.Errorf("fsync temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanupTmp()
		return fmt.Errorf("close temp config: %w", err)
	}

	// Best-effort backup of the pre-save file. A backup failure is logged via
	// the returned error only if it would block the save — here we treat it as
	// non-fatal so a missing/locked .bak target doesn't strand the user.
	if _, statErr := os.Stat(path); statErr == nil {
		_ = copyFile(path, path+".bak")
	}

	if err := os.Rename(tmpPath, path); err != nil {
		cleanupTmp()
		return fmt.Errorf("rename temp into place: %w", err)
	}
	return nil
}

// copyFile is a small dependency-free `cp src dst`. Used only for the on-save
// backup; not exported because the only sensible caller is SaveTo.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// ActiveProfile returns the profile selected for v1. Creates a defaulted
// profile on the fly if the config is missing one (so a half-written config
// doesn't panic downstream).
func (c *Config) ActiveProfile() Profile {
	if c == nil {
		return DefaultProfileConfig()
	}
	if p, ok := c.Profiles[DefaultProfile]; ok {
		return p
	}
	return DefaultProfileConfig()
}

// applyDefaults fills zero-valued numeric/behavioral fields with defaults from
// DefaultProfileConfig. Only fields safe to auto-fill are touched; identity
// fields stay empty so validation can flag them.
func (c *Config) applyDefaults() {
	if c.Profiles == nil {
		c.Profiles = map[string]Profile{}
	}
	defaults := DefaultProfileConfig()

	p, ok := c.Profiles[DefaultProfile]
	if !ok {
		return // Nothing to merge into; Load() will surface validation errors later.
	}

	if p.Window.BackfillStart == "" {
		p.Window.BackfillStart = defaults.Window.BackfillStart
	}
	if p.Window.DefaultLengthMonths == 0 {
		p.Window.DefaultLengthMonths = defaults.Window.DefaultLengthMonths
	}
	if p.Surge.MinStoryPoints == 0 {
		p.Surge.MinStoryPoints = defaults.Surge.MinStoryPoints
	}
	if p.Surge.MinActiveWeeks == 0 {
		p.Surge.MinActiveWeeks = defaults.Surge.MinActiveWeeks
	}
	if p.Surge.MinPRs == 0 {
		p.Surge.MinPRs = defaults.Surge.MinPRs
	}
	if p.Surge.MinCommits == 0 {
		p.Surge.MinCommits = defaults.Surge.MinCommits
	}
	if p.Surge.MinLOC == 0 {
		p.Surge.MinLOC = defaults.Surge.MinLOC
	}
	if p.Surge.RecentWeeks == 0 {
		p.Surge.RecentWeeks = defaults.Surge.RecentWeeks
	}
	if p.Surge.BaselineWeeks == 0 {
		p.Surge.BaselineWeeks = defaults.Surge.BaselineWeeks
	}
	if p.Surge.MinRecentActivity == 0 {
		p.Surge.MinRecentActivity = defaults.Surge.MinRecentActivity
	}
	if p.Surge.HotRatio == 0 {
		p.Surge.HotRatio = defaults.Surge.HotRatio
	}
	if p.Surge.RisingRatio == 0 {
		p.Surge.RisingRatio = defaults.Surge.RisingRatio
	}
	if p.Surge.CoolingRatio == 0 {
		p.Surge.CoolingRatio = defaults.Surge.CoolingRatio
	}
	if p.UI.DefaultComparison == "" {
		p.UI.DefaultComparison = defaults.UI.DefaultComparison
	}

	// Scoring block: fill any missing knob with the locked default. Weights are
	// filled key-by-key so a user can override one metric without re-declaring
	// the whole map.
	if p.Scoring.Weights == nil {
		p.Scoring.Weights = map[string]float64{}
	}
	for metric, w := range defaults.Scoring.Weights {
		if _, ok := p.Scoring.Weights[metric]; !ok {
			p.Scoring.Weights[metric] = w
		}
	}
	if p.Scoring.KFactorNew == 0 {
		p.Scoring.KFactorNew = defaults.Scoring.KFactorNew
	}
	if p.Scoring.KFactorEst == 0 {
		p.Scoring.KFactorEst = defaults.Scoring.KFactorEst
	}
	if p.Scoring.NewThreshold == 0 {
		p.Scoring.NewThreshold = defaults.Scoring.NewThreshold
	}
	if p.Scoring.PeriodWeeks == 0 {
		p.Scoring.PeriodWeeks = defaults.Scoring.PeriodWeeks
	}
	if p.Scoring.IdleDecayAfter == 0 {
		p.Scoring.IdleDecayAfter = defaults.Scoring.IdleDecayAfter
	}
	if p.Scoring.IdleDecayDelta == 0 {
		p.Scoring.IdleDecayDelta = defaults.Scoring.IdleDecayDelta
	}
	if p.Scoring.ProvisionalUntilPeriods == 0 {
		p.Scoring.ProvisionalUntilPeriods = defaults.Scoring.ProvisionalUntilPeriods
	}
	// CodeImpact coefficients fill in field-by-field so a user can override
	// one without re-declaring all three.
	if p.Scoring.CodeImpact.Alpha == 0 {
		p.Scoring.CodeImpact.Alpha = defaults.Scoring.CodeImpact.Alpha
	}
	if p.Scoring.CodeImpact.Beta == 0 {
		p.Scoring.CodeImpact.Beta = defaults.Scoring.CodeImpact.Beta
	}
	if p.Scoring.CodeImpact.Gamma == 0 {
		p.Scoring.CodeImpact.Gamma = defaults.Scoring.CodeImpact.Gamma
	}
	// Churn / bulk-import tunables fill in only when omitted (zero). The bool
	// toggles intentionally have no fill — their zero value (false / OFF) is
	// the intended default that preserves the pre-patch code_impact.
	if p.Scoring.CodeImpact.ChurnFloor == 0 {
		p.Scoring.CodeImpact.ChurnFloor = defaults.Scoring.CodeImpact.ChurnFloor
	}
	if p.Scoring.CodeImpact.ChurnFullAt == 0 {
		p.Scoring.CodeImpact.ChurnFullAt = defaults.Scoring.CodeImpact.ChurnFullAt
	}
	if p.Scoring.CodeImpact.BulkImportMinLOC == 0 {
		p.Scoring.CodeImpact.BulkImportMinLOC = defaults.Scoring.CodeImpact.BulkImportMinLOC
	}
	if p.Scoring.CodeImpact.BulkImportAddRatio == 0 {
		p.Scoring.CodeImpact.BulkImportAddRatio = defaults.Scoring.CodeImpact.BulkImportAddRatio
	}
	if p.Scoring.CodeImpact.BulkImportMinFiles == 0 {
		p.Scoring.CodeImpact.BulkImportMinFiles = defaults.Scoring.CodeImpact.BulkImportMinFiles
	}
	if p.Scoring.CodeImpact.BulkImportWeight == 0 {
		p.Scoring.CodeImpact.BulkImportWeight = defaults.Scoring.CodeImpact.BulkImportWeight
	}
	// Bulk-data-dump knobs. DumpWeight intentionally has no fill — its zero value
	// (0 = no credit for a dump) IS the intended default; DisableDumpDampening's
	// zero value (false = dampening ON) is likewise intended.
	if p.Scoring.CodeImpact.DumpDominance == 0 {
		p.Scoring.CodeImpact.DumpDominance = defaults.Scoring.CodeImpact.DumpDominance
	}
	if p.Scoring.CodeImpact.DumpMinFiles == 0 {
		p.Scoring.CodeImpact.DumpMinFiles = defaults.Scoring.CodeImpact.DumpMinFiles
	}
	if p.Scoring.CodeImpact.LOCCapPercentile == 0 {
		p.Scoring.CodeImpact.LOCCapPercentile = defaults.Scoring.CodeImpact.LOCCapPercentile
	}
	// Normalize block: same field-by-field fill so partial overrides work.
	// GeneratedFilePatterns falls back to the default list only if the user
	// left it unset entirely; an explicit empty slice in TOML disables the
	// auto-generated dampening (`generated_file_patterns = []`).
	if p.Scoring.Normalize.SpamThreshold == 0 {
		p.Scoring.Normalize.SpamThreshold = defaults.Scoring.Normalize.SpamThreshold
	}
	if p.Scoring.Normalize.SpamPenalty == 0 {
		p.Scoring.Normalize.SpamPenalty = defaults.Scoring.Normalize.SpamPenalty
	}
	if p.Scoring.Normalize.StuffPenalty == 0 {
		p.Scoring.Normalize.StuffPenalty = defaults.Scoring.Normalize.StuffPenalty
	}
	if p.Scoring.Normalize.MultiplierFloor == 0 {
		p.Scoring.Normalize.MultiplierFloor = defaults.Scoring.Normalize.MultiplierFloor
	}
	if p.Scoring.Normalize.GeneratedFileWeight == 0 {
		p.Scoring.Normalize.GeneratedFileWeight = defaults.Scoring.Normalize.GeneratedFileWeight
	}
	if p.Scoring.Normalize.GeneratedFilePatterns == nil {
		p.Scoring.Normalize.GeneratedFilePatterns = defaults.Scoring.Normalize.GeneratedFilePatterns
	}
	// KTiers migration: if a config predates the Phase 7 tier table, synthesize
	// a two-tier ramp from the legacy KFactorNew/KFactorEst/NewThreshold values
	// so the math keeps matching the user's existing intent. A config that
	// omits both KTiers and the legacy triple falls through to the four-tier
	// default. Configs that already define KTiers are left untouched.
	if len(p.Scoring.KTiers) == 0 {
		if p.Scoring.KFactorNew != defaults.Scoring.KFactorNew ||
			p.Scoring.KFactorEst != defaults.Scoring.KFactorEst ||
			p.Scoring.NewThreshold != defaults.Scoring.NewThreshold {
			p.Scoring.KTiers = []KTier{
				{MinPeriods: 0, K: p.Scoring.KFactorNew},
				{MinPeriods: p.Scoring.NewThreshold, K: p.Scoring.KFactorEst},
			}
		} else {
			p.Scoring.KTiers = defaults.Scoring.KTiers
		}
	}

	// StoryPoints block: fill each band-engine knob field-by-field so a partial
	// override works. Scale falls back to the default ladder only if left unset.
	spDef := defaults.StoryPoints
	if p.StoryPoints.Scale == nil {
		p.StoryPoints.Scale = spDef.Scale
	}
	if p.StoryPoints.LOCThreshold == 0 {
		p.StoryPoints.LOCThreshold = spDef.LOCThreshold
	}
	if p.StoryPoints.CycleDaysThreshold == 0 {
		p.StoryPoints.CycleDaysThreshold = spDef.CycleDaysThreshold
	}
	if p.StoryPoints.BaseShortLow == 0 {
		p.StoryPoints.BaseShortLow = spDef.BaseShortLow
	}
	if p.StoryPoints.BaseShortHigh == 0 {
		p.StoryPoints.BaseShortHigh = spDef.BaseShortHigh
	}
	if p.StoryPoints.BaseLongLow == 0 {
		p.StoryPoints.BaseLongLow = spDef.BaseLongLow
	}
	if p.StoryPoints.BaseLongHigh == 0 {
		p.StoryPoints.BaseLongHigh = spDef.BaseLongHigh
	}
	if p.StoryPoints.ReworkWeight == 0 {
		p.StoryPoints.ReworkWeight = spDef.ReworkWeight
	}
	if p.StoryPoints.ReviewRoundWeight == 0 {
		p.StoryPoints.ReviewRoundWeight = spDef.ReviewRoundWeight
	}
	if p.StoryPoints.DeepThreadWeight == 0 {
		p.StoryPoints.DeepThreadWeight = spDef.DeepThreadWeight
	}
	if p.StoryPoints.HighRiskBonus == 0 {
		p.StoryPoints.HighRiskBonus = spDef.HighRiskBonus
	}
	if p.StoryPoints.MediumRiskBonus == 0 {
		p.StoryPoints.MediumRiskBonus = spDef.MediumRiskBonus
	}
	if p.StoryPoints.CrossRepoBonus == 0 {
		p.StoryPoints.CrossRepoBonus = spDef.CrossRepoBonus
	}
	if p.StoryPoints.StraddleFraction == 0 {
		p.StoryPoints.StraddleFraction = spDef.StraddleFraction
	}
	if p.StoryPoints.MinThinkingForHighBand == 0 {
		p.StoryPoints.MinThinkingForHighBand = spDef.MinThinkingForHighBand
	}
	if p.StoryPoints.ReworkMinDwellMins == 0 {
		p.StoryPoints.ReworkMinDwellMins = spDef.ReworkMinDwellMins
	}
	if p.StoryPoints.HighBandThinkingShare == 0 {
		p.StoryPoints.HighBandThinkingShare = spDef.HighBandThinkingShare
	}
	if p.StoryPoints.ReworkCountCap == 0 {
		p.StoryPoints.ReworkCountCap = spDef.ReworkCountCap
	}
	if p.StoryPoints.ReviewRoundCap == 0 {
		p.StoryPoints.ReviewRoundCap = spDef.ReviewRoundCap
	}
	if p.StoryPoints.SmallDiffLOCFloor == 0 {
		p.StoryPoints.SmallDiffLOCFloor = spDef.SmallDiffLOCFloor
	}
	if p.StoryPoints.SmallDiffBonusScale == 0 {
		p.StoryPoints.SmallDiffBonusScale = spDef.SmallDiffBonusScale
	}
	if p.StoryPoints.SplitThreshold == 0 {
		p.StoryPoints.SplitThreshold = spDef.SplitThreshold
	}
	// MaxThinkingBonus has no fill: its zero value (disabled) is the intended
	// default, like the dump/churn toggles in CodeImpactConfig.

	// Migrate legacy single-login DevIdentity entries to the plural form.
	// Idempotent: entries already on the new schema are left alone.
	for i, d := range p.Devs {
		if len(d.GitHubLogins) == 0 && d.GitHubLogin != "" {
			d.GitHubLogins = []string{d.GitHubLogin}
		}
		d.GitHubLogin = "" // clear so SaveTo emits only the plural form
		p.Devs[i] = d
	}

	c.Profiles[DefaultProfile] = p
}
