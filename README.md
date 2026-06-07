# Velocity

Personal engineering velocity dashboard. Pulls your Jira + GitHub activity into a local cache and serves a web UI showing trends, comparisons, and auto-detected project surges.

Ships as a single static Go binary. Anyone at any company runs `velocity init`, answers a few prompts, and has their own dashboard. Nothing is hard-coded to one employer, one user, or one Jira instance.

![dashboard screenshot — to add]()

## What you get

- **Local cache** of every Jira issue you've touched and every PR / commit you've authored, back to whatever month you tell it to backfill to. Stored in a single SQLite database under `$XDG_DATA_HOME/velocity/` (the legacy JSON-file backend is still available — see [Config reference](#config-reference)).
- **Contributor leaderboard** — once you map your team with `velocity devs discover`, per-dev rollups, a composite score, and an Elo-style rating across the whole org cache (leaderboard / contributors / per-dev / macro velocity pages).
- **Cheap refresh** — only re-pulls the current month (and the last-closed month during days 1–7 of the next month to catch late resolutions). A monthly `velocity refresh` takes seconds.
- **Rich comparisons** — Prior, YoY, QoQ overlays on every view. Windows are one-click switchable: default / YTD / 1Y / All.
- **Project surge detection** — groups PRs + commits + issues by Jira epic, flags clusters that clear any of the configured thresholds (PRs, commits, LoC, active weeks, story points). Surfaced in a sortable panel and annotated on the full-history chart.
- **Offline / dependency-free UI** — dashboard is vanilla HTML + CSS + SVG, embedded into the binary via `//go:embed`. No Chart.js, no D3, no CDN calls.
- **Secrets in the OS keychain** — Jira + GitHub tokens live in macOS Keychain (`velocity:{profile}:{service}`). Never written to disk in cleartext.

## Requirements

- Go 1.22+
- macOS (v1 — keychain integration is Darwin-only; Linux / Windows support would be a small follow-up via `go-keyring`'s other backends)
- An Atlassian API token (create at https://id.atlassian.com/manage-profile/security/api-tokens)
- A GitHub personal access token with `repo` scope (classic) or equivalent fine-grained permissions

## Install

```bash
git clone https://github.com/mathewepstein/velocity.git
cd velocity
make install      # go install → $GOBIN (velocity now on PATH)
```

Or if you prefer to keep the binary in the repo:

```bash
make              # = make build, drops ./velocity in the repo root
```

After pulling updates, re-run `make install` (or `make build`) to refresh the binary. `make help` lists every target.

## Quick start

```bash
# 1. First-time setup — interactive, validates every entry against the live APIs.
velocity init

# 2. Backfill your full history. Takes a few minutes the first time;
#    subsequent monthly refreshes take seconds.
velocity refresh --since 2019-11      # or any other YYYY-MM start month

# 3. Build the contributor roster. REQUIRED before the leaderboard works —
#    maps each GitHub login to a Jira accountId. Run once after the first
#    backfill; re-run later to pick up new teammates.
velocity devs discover

# 4. Compute metrics from the cache.
velocity analyze

# 5. Open the dashboard.
velocity serve --open
```

> **Don't skip step 3.** `init` sets up *your* single-user view, but the
> leaderboard / contributors / velocity pages are driven by the `[[devs]]`
> identity table, which `init` does **not** populate. Until you run
> `velocity devs discover`, every contributor is bucketed under "unknown" and
> those pages render empty. `analyze` and `serve` print a reminder when the
> roster is empty.

After that, a typical weekly check-in is just:

```bash
velocity refresh && velocity analyze && velocity serve --open
```

## Commands

| Command | Purpose |
|---|---|
| `velocity init [--template]` | Interactive setup, or drop a commented TOML scaffold for hand-editing. Auto-discovers Jira custom fields (Story Points, Epic Link) by hitting `/rest/api/3/field`. Initializes the SQLite cache. Does **not** populate the contributor roster — run `devs discover` for that. |
| `velocity refresh [--since YYYY-MM] [--force] [--dry-run] [--sleep-between-pages N]` | Pull missing months from Jira + GitHub. Honors freshness rules (current month always re-pulled; last-closed month re-pulled on days 1–7 of the next month). |
| `velocity devs discover [--apply-all] [--dry-run]` | Scan the cache and build the `[[devs]]` identity table — maps GitHub logins to Jira accountIds so per-dev rollups merge across sources. **Required before the leaderboard works.** Run after the first `refresh`; re-run to pick up new teammates. |
| `velocity analyze` | Read the cache, recompute `metrics.json`. No network. |
| `velocity cache migrate [--db PATH]` | One-time import of a legacy JSON corpus into the SQLite database. Only needed if you ran an older build on the `json` backend and want to switch. |
| `velocity serve [--port 8000] [--open]` | Serve the embedded web UI + `metrics.json` on localhost. `--open` launches the browser. |
| `velocity auth set <jira\|github>` | Store a token in the keychain. Prompts with no-echo on a TTY. |
| `velocity auth show` | List known services + whether each has a stored token (never prints the token). |
| `velocity auth delete <jira\|github>` | Remove a keychain entry. Idempotent. |
| `velocity doctor [--no-color]` | Validate config + keychain + cache integrity. Non-zero exit on any failure. |

## Config reference

Config lives at `$XDG_CONFIG_HOME/velocity/config.toml` (default `~/.config/velocity/config.toml`). Override with `$VELOCITY_CONFIG=/path/to/config.toml`.

```toml
# Cache backend. "sqlite" (default) is a single velocity.db; "json" is the
# legacy month-partitioned corpus. Omit this section entirely to use sqlite.
[cache]
backend = "sqlite"

[profiles.default]
name = "work"

  [profiles.default.jira]
  base_url = "https://your-instance.atlassian.net"
  email    = "you@example.com"
  projects = ["PRJ"]                # Jira project key(s) — activity scoped to these
  account_id = "5dee8b..."          # resolved during init, used in JQL
  fields.story_points = "customfield_10016"
  fields.epic_link    = "parent"    # or customfield_10014 (team-managed)

  [profiles.default.github]
  username = "yourusername"
  orgs     = ["your-org"]           # GitHub org(s) — activity scoped to these

  [profiles.default.window]
  backfill_start = "2019-11"        # oldest month to ever consider
  default_length_months = 4         # default current-window length

  [profiles.default.surge]
  min_story_points = 5
  min_active_weeks = 3
  min_prs          = 3
  min_commits      = 20
  min_loc          = 1000           # added + deleted

  [profiles.default.ui]
  default_comparison = "prior"      # prior | yoy | qoq | none

# Contributor identity table — populated by `velocity devs discover`, not by
# init. One entry per person, mapping a GitHub login to a Jira accountId so
# per-dev rollups merge across both sources. Anyone not listed is bucketed
# under "unknown" and excluded from the leaderboard.
  [[profiles.default.devs]]
  github_login    = "octocat"
  jira_account_id = "5b10ac8d82e05b22cc7d4ef5"
  display_name    = "Octo Cat"
  excluded_bot    = false
```

### Credentials

Tokens are stored in the OS keychain under `velocity:{profile}:{service}` and never written to disk. `velocity init` prompts for them during setup. To rotate:

```bash
velocity auth set jira       # enter new token (no-echo prompt)
velocity auth set github
```

## Cache layout

Default (`sqlite` backend):

```
$XDG_DATA_HOME/velocity/               # default ~/.local/share/velocity
├── velocity.db                         # all cached records + the pull manifest (SQLite)
├── metrics.json                        # computed, served to the UI
└── ratings.json                        # persisted Elo rating state
```

The records (Jira issues, PRs, commits, reviews) and the pull manifest live in
`velocity.db`. `metrics.json` (the analyze output served to the UI) and
`ratings.json` (the Elo precompute) are plain files regardless of backend.

With the opt-in `json` backend (`[cache] backend = "json"`), records are stored
as month-partitioned JSON files instead, alongside a `manifest.json`:

```
├── manifest.json                       # (source, scope, month) → pulled_at + record count
├── jira/{PROJECT}/2019-11.json …
├── github-prs/{ORG}/2019-11.json …
└── github-commits/{ORG}/2019-11.json …
```

Freshness rules enforced by `velocity refresh`:
- Months **before the current month** are frozen. Never re-pulled unless `--force`.
- The **current month** is always re-pulled.
- The **last closed month** is re-pulled on days 1–7 of the next month (grace window for late resolutions).

## Troubleshooting

| Symptom | Check |
|---|---|
| `velocity refresh` says "fetch jira token" | Run `velocity auth set jira`. |
| Dashboard shows the error banner | Cache is empty or `metrics.json` wasn't generated. Run `velocity refresh && velocity analyze`. |
| Some months have zero activity and you expect data | Check the date range with `velocity doctor`. Zero-record months (leave, pre-employment) are preserved intentionally — the analyzer zero-fills them so the chart x-axis is continuous. |
| Surge detection fires on too many projects | Raise the thresholds in `[profiles.default.surge]`, or toggle "Hide small" in the UI (hides projects with fewer than 3 PRs + commits). |
| Epic shows "Epic not in cache" | The epic predates your `backfill_start`, or was never touched by you. Issues under it are still attributed, but the summary field is only populated when the epic itself is cached. |
| Tokens feel stale / API returns 401 | `velocity auth set <service>` rotates without touching the rest of the config. |

`velocity doctor` is the first thing to run if anything feels off — it validates every required field, confirms tokens are in the keychain, walks the cache, flags missing/corrupt files and stale `metrics.json`.

## Project layout

```
cmd/velocity/        CLI entry (Cobra-based, one file per subcommand)
internal/config/     TOML load/save, XDG paths, $VELOCITY_CONFIG override
internal/secrets/    go-keyring wrapper, closed set of known services
internal/cache/      Month type, manifest, atomic JSON I/O, freshness rules
internal/pull/       Jira + GitHub pullers, shared backoff client, Refresh orchestrator
internal/analyze/    metrics, prior/YoY/QoQ/full-history slicing, epic-surge detection
internal/server/     embedded static file server + /metrics.json passthrough
internal/initflow/   interactive init prompts + live API validation
internal/doctor/     config/keychain/cache integrity checks
web/                 embedded frontend (HTML + CSS + ES2020 JS, no external libraries)
_legacy/             original Python prototype (preserved for reference)
```

## Design notes

- **Go, not Python.** The legacy prototype was Python; the rewrite is Go so the whole tool ships as one binary. `//go:embed` bundles the web assets. No runtime Python interpreter, no venv, no pip install step.
- **SQLite by default, JSON opt-in.** The cache started as month-partitioned JSON (stdlib-only, `jq`-friendly) but moved to a single pure-Go SQLite db (`modernc.org/sqlite`, no CGO — still one static binary) as the org-wide corpus grew. A `Store` interface hid the storage so the switch didn't touch the aggregation, pull, or backfill layers; the JSON store remains behind `[cache] backend = "json"` as a fallback.
- **Client-side derivation.** The server emits `metrics.json` once per analyze; the UI computes every window / overlay / metric-switch view from `full_history.monthly` in the browser. No round trip per click.
- **Vanilla SVG charts.** No Chart.js / D3. The chart code is small (~150 lines) and keeps the UI fully offline.

## License

MIT.
