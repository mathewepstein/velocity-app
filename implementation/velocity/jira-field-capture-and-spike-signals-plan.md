# Velocity — Jira Field Capture, Custom-Field Mapping & Spike Link Signals

**Created:** 2026-06-08
**Last edited:** 2026-06-08
**Owner:** Mathew Epstein
**Status:** Phases A + B DONE + VALIDATED. **Phase C (the one final crawl) BUILT + LAUNCHED 2026-06-08** — running over the full 23,601-issue corpus (`velocity-backfill --phase jira-fields`, ~85 min, resumable, log at `~/velocity-jira-fields-backfill.log`). Phases D–E not started. Supersedes the narrower `spike-link-signals-plan.md` (folded in as Phase D). Code uncommitted; build/vet/`test ./...` green, 15 pkgs.

## Why this exists

The immediate goal was adding spawned-children / issue-link signals to the spike scorer (`be-rubric-incorporation-plan.md` Phase 4 deferred them — not cached). But capturing them requires a **per-issue crawl of all ~23.6k cached Jira issues**. We have crawled the corpus before (detail backfill: changelog/comments/description). We do not want to do it a third time.

So this plan widens the scope: **one final historical crawl that grabs every relevant field, behind a configurable custom-field→canonical mapping, with a discovery wizard that infers that mapping from recent tickets** — so the next operator (or a fresh org install) configures field signals once and never re-crawls.

## Ground-truth findings (verified 2026-06-08)

- **Description is the wrong field.** The detail fetch hardcodes `fields=comment,description`. CD's real description is **`customfield_11140` ("Description - V2")**. Cache evidence: 23,601 issues, all `detail_fetched`, but **only 6,825 (29%) have a non-empty standard `description`.** The mapping must let `description` point at a custom field.
- **`JiraFields` already maps two fields** (`story_points`, `epic_link`), resolved at `velocity init`, consumed in `decodeIssue` via `p.storyPointsID`/`p.epicLinkID` with a `parent` fallback. This is the pattern to generalize, not invent.
- **Sprint is not used at this org** — `customfield_10120` is empty on samples; **dropped from capture** per Mathew.
- **Valuable per Mathew:** components, labels, parent (already captured), **fix versions** (new), plus links/subtasks/attachments for classification, relating, and spike artifact-density.
- Field inventory (per-field shapes, custom-field IDs, what's populated vs empty) is in the jira-agent inventory from this session — `customfield_11102`=story points, `customfield_11140`=description, `customfield_11158`=steps to reproduce, `customfield_10126`=flagged, `customfield_10120`=sprint(unused), `attachment`, `issuelinks`, `subtasks`, `fixVersions`/`versions`, time-tracking in seconds.

## Locked decisions

1. **Generalize the field mapping** — `[profiles.default.jira.fields]` grows from `{story_points, epic_link}` to a full canonical-signal→field-id map (`description`, `steps_to_reproduce`, … extensible). Empty/absent entries fall back to the standard field or are skipped. No org-specific IDs in the binary — all config.
2. **Drop Sprint.** Not used here.
3. **Capture broad + future-proof — typed + raw catch-all (LOCKED 2026-06-08).**
   - **Typed/child tables** for the fields scoring reads on the hot path: links/subtasks (`jira_issue_links`), attachments (count + `jira_attachments`), fix versions (`jira_fix_versions`), mapped description, flagged, time-tracking scalars.
   - **Raw catch-all** `jira_issue_fields(scope, month, issue_key, field_id, field_name, value_json)` storing every populated field from `fields=*all` **minus a noise denylist** (JSM/HR/finance custom fields — Request Type, Approvals, SLA timers, onboarding/offboarding, budget/deal, satisfaction). Any future signal derives from this table with **zero re-crawl** — the literal "last backfill" guarantee.
   - Cost: bounded by the denylist; est. sub-GB for ~23.6k issues × ~30 relevant fields × small JSON. Acceptable for a local analytics cache; gzip `value_json` if it bloats.
4. **Spawned/links scoring:** separate nudge + driver on the spike path.
5. **Discovery wizard:** `velocity jira fields discover` — pulls recent tickets, inventories populated fields (`expand=names`), proposes the `[jira.fields]` mapping + capture allowlist, **proposes-not-writes** (mirrors `devs discover` / `risk-discover`).

---

## Phase A — Generalized field mapping + discovery wizard — DONE 2026-06-08

**Built:**
- **Config (`internal/config/config.go`):** `JiraFields` gained `Description string \`toml:"description,omitempty"\`` and `Extra map[string]string \`toml:"extra,omitempty"\``; kept `StoryPoints`/`EpicLink`. **Correction (per Mathew):** dropped the originally-planned `StepsToReproduce`/`Flagged` named fields — a field earns a *named* mapping entry only if it has an engine consumer AND lives in a non-standard custom field (today: story_points, epic_link, description). Standard fields (components/labels/parent/fixVersions) are NOT mapped here — ingest reads them by fixed name. Consumer-less custom fields ride `Extra` or the raw catch-all.
- **Package `internal/jirafields`** (`report.go` pure logic + `discover.go` client). Read-only flow: GET `/field` catalog → POST `/search/jql` for recent keys → GET `/issue/{key}?fields=*all` per key → `buildReport`. Per-issue `*all` GET (not search `*all`, which we didn't want to assume the endpoint honors). Population tally + shape descriptor; proposals + Extra/Denylist buckets.
- **Proposal heuristics:** `story_points`/`epic_link` matched against the **field catalog by name** (these are routinely empty on recent tickets — population-only match misses them; this gap was caught in the live smoke test and fixed). `description` matched by **population** among description-named fields (the signal that distinguishes the sparse standard field from the custom one actually used). Noise denylist by name keyword (JSM/HR/finance/SLA), org-agnostic.
- **Command `velocity jira fields discover`** (`cmd/velocity/jira.go`, registered in `main.go`): paste-ready `[profiles.default.jira.fields]` block + Extra candidates (commented) + denylist summary + full population-evidence table. `--tickets`(20) `--sleep-ms`(200) `--json`. Proposes-not-writes (mirrors `devs discover`/`score risk-discover`).
- **Tests:** `internal/jirafields/report_test.go` — isPopulated/shapeOf, description-picks-most-populated-custom (the CD case), story_points-from-catalog-when-unpopulated (smoke-test regression), epic fallback to parent, noise denylist vs Extra bucket, stats sort/count.

**Live smoke (read-only, 12 recent CD tickets):** correctly proposed `description = customfield_11140` (12/12) over standard `Description` (2/12), `story_points = customfield_11102` (0/12, from catalog), `epic_link = customfield_10121` (the org's legacy custom Epic Link field — user to curate vs `parent`). Surfaced Steps-to-Reproduce/Development/QA-Story-Points etc. as Extra candidates; denylisted `[CHART] Date of First Response`.

**Validation:** `go build ./...`, `go vet ./...`, `go test ./...` (15 pkgs) all green. No `velocity.db` mutation; no Jira writes. Pending commit + `make install`.

## Phase B — Broaden ingest + cache (mapping-driven) — DONE 2026-06-08

**Typed boundary (LOCKED by Mathew):** typed storage only for fields with a real use — **links, attachments, fixVersions**. Flagged, time-tracking, priority, etc. ride the raw catch-all (no consumer-less typed columns — same rule as the StepsToReproduce drop).

**Built:**
- **Records (`internal/cache/records.go`):** new types `LinkedIssue` (key/link_type/direction/phrase/status/issue_type), `Attachment` (filename/mime_type/size/created/author), `RawField` (id/name/value-JSON). New `JiraIssue` fields: `Links []LinkedIssue` + `Attachments []Attachment` (sentinel, no omitempty), `FixVersions []string` (omitempty like Labels), `RawFields []RawField` (sentinel, Phase-C-populated).
- **Schema (`sqlite_schema.go`):** child tables `jira_issue_links`, `jira_attachments`, `jira_fix_versions`, `jira_issue_fields` (raw catch-all). Two flag columns on `jira_issues`: `relations_fetched` (gates Links+Attachments), `raw_fields_fetched` (gates RawFields — also Phase C's backfill gate). All registered in `allRecordTables` (children before parent).
- **Migration (`sqlite.go`):** no migration framework existed (schema is `CREATE TABLE IF NOT EXISTS` only). New tables auto-create; the two new columns are added by a new idempotent `migrateSchema`/`addMissingColumns` (PRAGMA `table_info` → `ALTER TABLE ADD COLUMN`), called in `openSQLiteStore` after the base schema. Safe on the live 23.6k-issue DB: existing rows default to 0 → read back as uncaptured (nil).
- **Store I/O (`sqlite_jira.go`):** `WriteJiraIssues`/`ReadJiraIssues` write+read all four child tables + the two flag columns, round-tripping the nil-vs-empty sentinels exactly like `Changelog`/`Comments`.
- **Ingest (`pull/jira.go`):** `JiraPuller` gained `descriptionID` from `Fields.Description`. `jiraBaseFields` gained `issuelinks`, `subtasks`, `attachment`, `fixVersions` (free in the search page). `decodeIssue` parses all four into `Links`/`Attachments`/`FixVersions`, initializing Links+Attachments non-nil (forward pulls = "relations captured"). Sprint: nothing to drop (was never requested). `versions` (affects-versions) skipped — only fixVersions per Mathew.
- **Description remap (`pull/jira_detail.go`):** detail fetch path now `fields=comment,<descriptionField()>` (mapped id, else standard `description`); `jiraDetailRaw.Fields` refactored to `map[string]json.RawMessage` so the description reads from a per-org custom field. Flows through both `refresh.go` (base) and `detail.go` (hydration) since both build the puller from `profile.Jira`.
- **Tests:** `sqlite_test.go` — `TestSQLite_JiraRelationsAndFieldsRoundTrip` (full round-trip + nil/empty sentinels for links/attachments/raw), `TestMigrateSchema_ColumnsPresentAndIdempotent`, `TestAddMissingColumns_AddsAndSkips`. Existing pull/detail tests still green.

**Validation:** `go build ./...`, `go vet ./...`, `go test ./...` (15 pkgs) all green. No live `velocity.db` mutation (migration deliberately not triggered on the real DB — runs on `make install` + next command). Pending commit + `make install`.

**Note for Phase C:** the raw catch-all (`RawFields`/`jira_issue_fields`) and the description-remap of *historical* (already-`detail_fetched`, frozen) issues are populated by the Phase C `*all` crawl — the detail-path remap only fixes *forward* hydration. `raw_fields_fetched` is the crawl's `NeedsWork` gate.

## Phase C — One final historical backfill — BUILT + LAUNCHED 2026-06-08

**Built:**
- **`pull.HydrateIssueFields`** (`internal/pull/jira_fields.go`): GET `/issue/{key}?fields=*all&expand=names` → `setRelations` (extracted from `decodeIssue`, shared) + remapped description (mapped field, falls back to standard so it never blanks) + `buildRawFields` (every populated field minus the noise denylist, JSON-encoded, ID-sorted for byte-stable rows). On permanent 404/410 → freezes sentinels (`RawFields=[]`) + returns `ErrIssueUnreachable`.
- **Denylist reuse:** exported `jirafields.IsNoiseFieldName` + `IsPopulated` so the crawl applies the exact same noise filter as the wizard (one source of truth).
- **`detail.JiraFieldsPhase`**: `NeedsWork = RawFields == nil` (fetch once, frozen — raw fields don't drift like changelog), perm-skip on unreachable. Wired into `cmd/velocity-backfill` as `--phase jira-fields` (generalized `runJiraDetail` → `runJiraPhase`).
- **Config:** `description = "customfield_11140"` added to `~/.config/velocity/config.toml` (was unset). `story_points` left at `customfield_10128` (band engine calibrated on it; wizard's `customfield_11102` is a parallel field — both land in the raw catch-all; flagged for later curation).
- **Tests:** `pull/jira_fields_test.go` — `setRelations` (subtask + inward/outward links + attachments + fixVersions), non-nil-empty sentinel, `buildRawFields` (noise/null/zero excluded, JSON values, ID-sorted).

**Validated live (read-only-then-bounded):** dry-run = 23,601 candidates. `--limit 5` live run hydrated 5 — DB confirmed `relations_fetched=1`/`raw_fields_fetched=1`, **descriptions populated on 2019 issues that had empty standard descriptions** (the remap works on frozen historical issues), 27–30 raw fields each, fix versions captured.

**Running:** full corpus launched (background, ~85 min, ~5 issues/s). Resumable — a re-run continues from `raw_fields_fetched`. Note: `comment` and `Sprint` land in the raw catch-all (comment bodies duplicate `jira_comments`; watch DB growth, grow denylist if needed).

## Phase D — Spike link/child signals (original ask)

- **Evidence (`internal/scoring/evidence.go`):** `spikeLinkSignals(iss)` → `SpawnedCount` (subtasks + creation-flavored links: `split to`/`cloned to`/`created from`) + `LinkBreadth` (distinct linked counterparts). Set on `TicketEvidence` alongside `ArtifactLinks`/`SubstantiveComments` (~line 253). **Attachments** also feed `spikeArtifactSignals` now (design docs = artifact density).
- **Score (`internal/scoring/spike.go`):** quadrant base unchanged; add a config-weighted nudge with distinct drivers (`Spawned %d follow-up ticket(s)`, `Linked to %d related ticket(s)`). Suppress the "multi-day, zero-artifact = dormancy?" low-confidence flag when `SpawnedCount > 0` (spawned work is real investigation output). `SpikeConfig` gains `SpawnedWeight`/`BreadthWeight`/`BreadthThreshold`, conservative defaults.
- Confirm the spike link-type set against the corpus (`issuelinks` type names actually present) before hardcoding spawned-vs-plain classification.

## Phase E — Calibration

- Backfill → re-extract → re-band spikes. Read-only before/after sweep on spike band distribution + flag%. Validate Christian's spike anchors as golden tests. Confirm description-coverage lift improved the doc-URL artifact signal. Tune weights only if under-banded.

## Validation gate (every phase)

`go build ./...`, `go vet`, full `go test ./...` green. No live `velocity.db` mutation during calibration. After commit + `make install`: `velocity jira fields discover` → curate config → `velocity refresh` → backfill → `velocity score generate`. (`make install` required — cron runs the installed binary.)

## Sequencing

A (mapping + wizard) → B (ingest/cache, mapping-driven) → C (one crawl) → D (spike signals) → E (calibrate). A unblocks the correct description mapping that C depends on, so it must come first.

## Open items

- **Noise denylist** — enumerate the JSM/HR/finance custom-field IDs to exclude from the catch-all (from the inventory; verify against a corpus field-frequency query so we exclude noise, not signal).
- **Migration mechanics** — read the existing `changelog_fetched`/table migration before writing new ones.
- **Wizard depth** — N recent tickets default; confirm ~20 covers field variation for this org.
