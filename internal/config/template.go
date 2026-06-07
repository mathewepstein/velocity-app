package config

// Template returns a commented TOML scaffold for hand-editing. Used by
// `velocity init --template` when the user wants to skip the interactive flow.
//
// The string is a raw literal — no Sprintf — so the file shipped to disk is
// byte-identical to what the reader sees here.
func Template() string {
	return templateContents
}

const templateContents = `# velocity config
# Location: $XDG_CONFIG_HOME/velocity/config.toml (or ~/.config/velocity/config.toml)
# Override the path with $VELOCITY_CONFIG.
#
# Tokens do NOT live here. They are stored in the OS keychain.
# Set them with:
#   velocity auth set jira
#   velocity auth set github

# Local cache backend. "sqlite" (default) is a single velocity.db under the
# data dir; "json" is the legacy month-partitioned JSON corpus. Omit this
# section entirely to use sqlite — it's the standard substrate.
[cache]
backend = "sqlite"

[profiles.default]
name = "default"

  [profiles.default.jira]
  # Your Atlassian Cloud instance URL (no trailing slash).
  base_url = "https://YOUR-ORG.atlassian.net"
  # Email associated with your Atlassian account.
  email = "you@example.com"
  # Project keys to pull issues from. At least one required.
  projects = ["PROJ"]
  # Resolved during init — your Atlassian accountId.
  account_id = ""

    [profiles.default.jira.fields]
    # Custom field IDs resolved during init. Leave blank and init will fill.
    # Company-managed projects usually use "parent" for epic_link.
    # Team-managed projects usually use "customfield_10014".
    story_points = ""
    epic_link    = ""

  [profiles.default.github]
  username = "your-github-username"
  # Orgs to search for your PRs and commits. At least one required.
  orgs = ["your-org"]

  [profiles.default.window]
  # Oldest month to backfill when running 'velocity refresh --since'.
  backfill_start = "2019-11"
  # Default window length (months) when no --since is passed.
  default_length_months = 4

  [profiles.default.surge]
  # Initiatives are ranked by MOMENTUM: an epic's recent-window weekly activity
  # rate vs its own trailing baseline. recent_weeks/baseline_weeks set the two
  # windows; min_recent_activity is the PRs+commits floor to appear at all;
  # the *_ratio knobs bucket momentum into hot/rising/steady/cooling.
  recent_weeks        = 2
  baseline_weeks      = 8
  min_recent_activity = 3
  hot_ratio           = 2.0
  rising_ratio        = 1.2
  cooling_ratio       = 0.8
  # Legacy static thresholds — no longer used (kept for older configs).
  min_story_points = 5
  min_active_weeks = 3
  min_prs          = 3
  min_commits      = 20
  min_loc          = 1000

  [profiles.default.ui]
  # Default comparison overlay on page load: prior | yoy | qoq | none.
  default_comparison = "prior"

  [profiles.default.scoring]
  # Bi-weekly rating period. Two weeks = ~26 updates/year.
  period_weeks = 2
  # Elo K-factor. Higher = bigger per-period rating swings.
  k_factor_new          = 32   # devs with fewer than new_dev_period_threshold periods of history
  k_factor_established  = 16
  new_dev_period_threshold = 6
  # Extra logins/account-ids to exclude from scoring, merged with the built-in
  # bot list (*[bot], dependabot, renovate, github-actions, claude*). Supports
  # a single leading or trailing '*' wildcard.
  exclude = []

    [profiles.default.scoring.weights]
    # Weighted z-score: each metric contributes z(metric) * weight. Tune in TOML;
    # zero a weight to remove its contribution. Keys not listed fall back to the
    # built-in defaults below.
    prs_merged           = 3.0
    jira_issues_resolved = 2.0
    code_impact          = 1.5   # see [scoring.code_impact] for the formula
    prs_reviewed         = 1.0
    prs_created          = 0.5
    jira_issues_touched  = 0.5
    active_weeks         = 0.5
    story_points         = 0.5
    loc_changed          = 0.25  # already p95-capped + sqrt-dampened at analyze time

    [profiles.default.scoring.code_impact]
    # code_impact = sqrt(alpha*unique_files + beta*loc_capped + gamma*merged_prs)
    # where loc_capped = min(loc_added + loc_deleted, team_pN) at window scope.
    # Per-row code_impact on monthly/weekly trend charts uses uncapped LOC.
    alpha = 1.0
    beta  = 0.5
    gamma = 2.0
    # Bulk-data-dump dampening (ON by default). A single PR dominated by one
    # serialized-data extension (json/csv/xml/… — a fixture/export dump) has
    # those data files credited at dump_weight in BOTH the LOC and file-count
    # terms, so a multi-million-line JSON dump stops reading as code_impact.
    # Source code and small data PRs are never flagged. disable_dump_dampening
    # = true turns it off.
    dump_weight    = 0.0   # weight for data files in a detected dump (0 = no credit)
    dump_dominance = 0.9   # min fraction of one data extension to qualify
    dump_min_files = 50    # min files for a PR to qualify as a dump
    # Team LOC-cap percentile (raised from 95 → 99 so a legitimate high-output
    # contributor isn't truncated; dumps are handled by dump_weight above).
    loc_cap_percentile = 99

    [profiles.default.scoring.normalize]
    # Silent anti-gaming. Never surfaced in metrics.json. Dampens commits /
    # loc_changed / code_impact before z-scoring when a dev's window pattern
    # looks like commit-spam or LOC-stuffing.
    spam_threshold        = 1.5   # commits/unique_files above this triggers dampening
    spam_penalty          = 0.25  # multiplier shrinks by this per ratio unit above threshold
    stuff_penalty         = 0.25  # multiplier shrinks by this per overflow unit above team p90
    multiplier_floor      = 0.5   # combined multiplier never drops below this
    generated_file_weight = 0.25  # generated files count fractionally toward unique_files
    # generated_file_patterns:
    #   single trailing/leading '*' wildcards supported. Set to [] to disable.
    # generated_file_patterns = ["*.lock", "package-lock.json", "*.min.js", ...]

# Identity table populated by 'velocity devs discover'. Each entry maps a
# GitHub login to a Jira accountId so per-dev rollups merge across sources.
# Anyone not in this table is bucketed under "unknown" in the leaderboard.
# [[profiles.default.devs]]
# github_login    = "octocat"
# jira_account_id = "5b10ac8d82e05b22cc7d4ef5"
# display_name    = "Octo Cat"
# excluded_bot    = false
`
