# Velocity — GitHub Corpus Capture: One-Shot Full-Crawl Field Expansion

**Created:** 2026-06-15
**Status:** IMPLEMENTED 2026-06-15 (uncommitted) — code complete, validated. Crawl not yet run.
**Owner:** Mathew Epstein
**Decision (locked 2026-06-15):** We are doing **one more full GitHub corpus crawl and no others.** Backfilling a missing field later means re-crawling, which we will not do. Therefore this crawl must capture **every field we need now or may plausibly need in future** — including the one **new per-PR API call** (`/pulls/{n}/commits`). Scope = store everything below.

**Origin:** `promotion-pr-detection-scope.md` needed `base.ref` (merge target) and PR→commit membership (S4/S6) that the cache doesn't store. Auditing the fetch layer showed those are two of *many* fields we currently fetch-and-discard. Since the crawl is one-shot, the scope widened from "capture base.ref" to "capture the full useful surface of every GitHub endpoint we touch."

---

## 1. Verification status

All field shapes below were **verified live against the GitHub REST API on 2026-06-15** (token crawl, repo `consumerdirect/cd-widgets` PR #228 + org-wide `/search/commits`). This is not asserted from documentation — the key dumps are real. Endpoints sampled:

- `/search/issues?q=type:pr+org:…` (PR list)
- `/repos/{repo}/pulls/{number}` (PR detail)
- `/repos/{repo}/pulls/{number}/commits` (**new** — PR→commit membership)
- `/repos/{repo}/pulls/{number}/files`
- `/repos/{repo}/pulls/{number}/comments` (inline review comments)
- `/repos/{repo}/pulls/{number}/reviews`
- `/search/commits?q=org:…+author-date:…`

---

## 2. Per-endpoint capture audit (verified)

Legend: **KEEP** = already stored; **ADD** = fetched today but discarded, capture it; **NEW CALL** = endpoint not currently hit.

### 2.1 `/search/issues` → `ghSearchIssue` (github.go:528)
KEEP: number, title, body, repository_url, state, created_at, closed_at, user.login.
ADD (present in response, currently dropped):
| field | why capture |
|---|---|
| `labels[]` (name) | PR labels — a `release`/`deploy`/`automated` label is a portable promotion hint; also general categorization signal |
| `draft` | draft PRs are not finished authorship; lets scoring exclude/segment them |
| `updated_at` | last-activity timestamp; staleness / cycle signals |
| `assignees[]` (login) | ownership beyond author |
| `comments` (count) | discussion volume (issue-level) |
| `milestone.title` | release/sprint grouping |
| `author_association` | MEMBER/CONTRIBUTOR/FIRST_TIMER — external-vs-internal author |

### 2.2 `/repos/{repo}/pulls/{number}` → `ghPRDetail` (github.go:541) — richest discard
KEEP: state, merged, merged_at, additions, deletions, head.ref.
ADD (verified present in body we already fetch — **zero new calls**):
| field | why capture |
|---|---|
| **`base.ref`** | merge target branch. Core promotion signal (head long-lived AND base long-lived). |
| **`base.sha`, `head.sha`** | endpoint commit SHAs — partial commit-linkage even without the per-PR commits call |
| `base.repo.full_name`, `head.repo.full_name` | fork detection (head repo ≠ base repo) |
| **`merged_by.login`** | who merged it. Leads self-merging promotions = strong portable promotion signal |
| **`commits`** (count) | # commits in PR; magnitude + merge-up fingerprint (S5) |
| **`changed_files`** (count) | magnitude; cross-check vs files list |
| `merge_commit_sha` | the resulting merge commit — joins PR to the commit graph |
| `requested_reviewers[]` (login) | review-load / review-request signal |
| `labels[]`, `milestone`, `assignees[]`, `draft` | same as search-side (detail is authoritative if they ever diverge) |
| `auto_merge` (bool/non-null) | automated merge — promotion/bot signal |

Skipped deliberately: `mergeable`/`mergeable_state`/`rebeaseable` (transient, meaningless post-merge), `*_url` link fields, `node_id`.

### 2.3 `/repos/{repo}/pulls/{number}/commits` → **NEW CALL** (not fetched today)
The only true new per-PR cost. One paginated GET per merged PR. Verified shape per commit:
| field | why capture |
|---|---|
| `sha` | the PR→commit membership key. Unlocks **S4 (commit-overlap)** and **S6 (author-diversity)** — fully convention-free promotion detection |
| `author.login` | per-commit GitHub author → author-diversity of the diff (S6) |
| `commit.author.{name,email,date}` | fallback attribution when no linked GH account; authored-date |
| `parents[].sha` (count) | merge-commit detection inside the PR |

Store as new child table `pr_commits` keyed to the PR. This is the single highest-leverage portability upgrade in the doc.

### 2.4 `/repos/{repo}/pulls/{number}/files` → `ghPRFileDetail` (pr_file_changes.go:90)
KEEP: filename, status, additions, deletions.
ADD:
| field | why capture |
|---|---|
| `previous_filename` | rename tracking — without it a rename reads as delete+add, distorting LOC/new-vs-modified |
| `sha` (blob) | file content identity; dedupe / overlap detection |
| `changes` | = add+del (derivable, but free; store for convenience) |

Skip: `patch` (the full diff hunk — large, not needed for current signals), `blob_url`/`raw_url`/`contents_url`.

### 2.5 `/repos/{repo}/pulls/{number}/comments` → `ghReviewComment` (pr_comments.go:147)
KEEP: in_reply_to_id, user.login, path, body, created_at.
ADD:
| field | why capture |
|---|---|
| **`id`** | currently parsed transiently for deep-thread counting then **discarded** — without it thread depth can't be recomputed from cache, only at fetch time. Store it. |
| `pull_request_review_id` | groups inline comments into the review they belong to |
| `commit_id`, `original_commit_id` | which commit the comment anchors to |
| `line`, `original_line`, `start_line` | comment position in the diff |
| `updated_at`, `author_association` | edit tracking / author class |

### 2.6 `/repos/{repo}/pulls/{number}/reviews` → `ghReview` (github.go:587)
KEEP: user.login, state, submitted_at.
ADD:
| field | why capture |
|---|---|
| `id` | review identity; joins to inline comments via pull_request_review_id |
| `body` | review summary text — review-effort / substance signal |
| `commit_id` | which commit was reviewed |
| `author_association` | author class |

### 2.7 `/search/commits` → `ghSearchCommit` (github.go:564)
KEEP: sha, commit.message, commit.author.{name,email}, commit.committer.date, author.login, repository.full_name.
ADD (verified present — **zero new calls**):
| field | why capture |
|---|---|
| **`parents[].sha`** (count + shas) | merge-commit detection at the commit level (a merge has ≥2 parents). Sampled commit had 2 parents. Near-direct promotion/merge signal independent of PRs |
| `commit.author.date` | authored date (we bucket by committer date; authored may matter for attribution) |
| `committer.login` | GitHub committer (distinct from author for cherry-picks/rebases) |
| `commit.comment_count` | commit-level discussion |

Skip: `commit.tree.sha`, `commit.verification` (signing — not needed now), `*_url`.

---

## 3. Schema changes

New columns are added idempotently via the existing `addMissingColumns` helper (`sqlite.go:84`, `PRAGMA table_info` + `ALTER TABLE ADD COLUMN`); they **must be nullable or carry a DEFAULT** (existing rows get the default — irrelevant here since every row is re-pulled by the full crawl, but required by the migration contract). New tables come free from `CREATE TABLE IF NOT EXISTS` in `sqliteSchema`.

### 3.1 `github_prs` — new columns
```
base_branch        TEXT
base_sha           TEXT
head_sha           TEXT
head_repo          TEXT
base_repo          TEXT
merged_by          TEXT
commit_count       INTEGER
changed_files      INTEGER
merge_commit_sha   TEXT
requested_reviewers TEXT     -- JSON array of logins, or a child table (see note)
draft              INTEGER NOT NULL DEFAULT 0
auto_merge         INTEGER NOT NULL DEFAULT 0
updated            TEXT
author_association TEXT
-- labels / assignees / milestone: see §3.5 (child tables vs JSON)
```

### 3.2 NEW TABLE `pr_commits` — the PR→commit membership (S4/S6)
```sql
CREATE TABLE IF NOT EXISTS pr_commits (
    scope        TEXT NOT NULL,
    month        TEXT NOT NULL,
    repo         TEXT NOT NULL,
    number       INTEGER NOT NULL,
    ord          INTEGER NOT NULL,
    sha          TEXT NOT NULL,
    author       TEXT,             -- GitHub login when linked
    author_name  TEXT,             -- commit.author.name fallback
    author_email TEXT,
    authored     TEXT,             -- commit.author.date
    parent_count INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_pr_commits_cell ON pr_commits (scope, month);
CREATE INDEX IF NOT EXISTS idx_pr_commits_pr   ON pr_commits (repo, number);
CREATE INDEX IF NOT EXISTS idx_pr_commits_sha  ON pr_commits (sha);
```
The `idx_pr_commits_sha` index makes "has this SHA shipped in an earlier PR?" (S4) a cheap lookup.

### 3.3 `pr_file_changes` — new columns
```
previous_filename TEXT          -- rename source; NULL when not a rename
blob_sha          TEXT
```
(`changes` is derivable from additions+deletions — optional; skip unless wanted.)

### 3.4 `pr_review_comments` — new columns
```
comment_id        INTEGER        -- the API id (currently discarded)
review_id         INTEGER        -- pull_request_review_id
commit_id         TEXT
line              INTEGER
original_line     INTEGER
updated           TEXT
author_association TEXT
```

### 3.5 `github_reviews` — new columns
```
review_id          INTEGER
body               TEXT
commit_id          TEXT
author_association TEXT
```

### 3.6 `github_commits` — new columns
```
authored      TEXT             -- commit.author.date
committer      TEXT             -- committer GitHub login
parent_count  INTEGER NOT NULL DEFAULT 0
comment_count INTEGER NOT NULL DEFAULT 0
```
(If full parent SHAs are wanted, add a `commit_parents(scope,month,repo,sha,ord,parent_sha)` child table. `parent_count` alone is enough for merge detection; full SHAs only matter if we later walk the commit graph. **Recommend: store `parent_count` now, defer the parent-SHA child table** — it's the one place where "store everything" trades real disk for a hypothetical.)

### Labels / assignees / requested_reviewers shape decision
These are multi-valued. Two options, pick one and apply consistently:
- **(A) child tables** (`pr_labels`, `pr_assignees`, `pr_requested_reviewers`) — mirrors the existing `pr_issue_keys` / `pr_files` pattern, queryable, normalized. **Recommended** for consistency with the codebase.
- (B) JSON-encoded TEXT column — fewer tables, but unqueryable without JSON functions, breaks the existing child-table idiom.

---

## 4. Code changes (ingest → cache → store)

1. **Raw API structs** (`internal/pull/`): extend `ghSearchIssue`, `ghPRDetail`, `ghPRFileDetail`, `ghReviewComment`, `ghReview`, `ghSearchCommit` with the ADD fields. Add a new `ghPRCommit` struct + `FetchPRCommits(ctx, repo, number)` paginator modeled on `FetchPRFileChanges` (pr_file_changes.go) — same `ErrPRUnreachable` 404/410/422 sentinel handling.
2. **Cache records** (`internal/cache/records.go`): extend `GitHubPR`, `FileChange`, `ReviewComment`, `GitHubReview`, `GitHubCommit`; add `PRCommit` struct + a `Commits []PRCommit` field on `GitHubPR` (NO omitempty — same nil-vs-empty sentinel as `Files`/`FileChanges` for backfill idempotency).
3. **Puller wiring** (`github.go pullPRs`): after the existing merged-PR file/comment hydration, call `FetchPRCommits` for merged PRs and attach. Thread `base.ref`/`merged_by`/counts from `detail` into the `GitHubPR`.
4. **SQLite store** (`internal/cache/sqlite*.go`): add columns to `sqliteSchema`, register them in `migrateSchema`'s `addMissingColumns` calls, add `pr_commits` to `sqliteSchema` + `allRecordTables` (so Reset truncates it), and extend the PR insert/scan paths. Bump any `files_fetched`-style hydration gate to include a `commits_fetched` flag if commits hydrate separately.
5. **JSON store** (if the JSON cache path is still live alongside SQLite): mirror the struct additions — they round-trip automatically since they're struct fields.

---

## 5. Crawl execution (one-shot)

- This is a **full re-pull**, not an incremental backfill: every PR/commit is re-fetched so the new columns populate for the entire history.
- **New rate-limit cost:** one extra `/pulls/{n}/commits` GET per *merged* PR (open/draft PRs don't hydrate, matching the existing files/comments gate). Estimate: ~12.7k single-use feature PRs + integration PRs ≈ the merged-PR count; budget against the 5k/hr authenticated REST limit + the existing per-page sleep/governor (`UseGovernor`). The crawl already makes 2–3 calls per merged PR (detail + files + comments); this adds a 4th.
- Run behind the existing `RateGovernor` and `sleepBetweenPages`. Confirm the governor accounts for the added call volume before kicking off.

---

## 6. Validation (before declaring the corpus done)

- After the crawl, spot-check a known promotion PR and a known feature PR: confirm `base_branch`, `merged_by`, `commit_count`, and `pr_commits` rows are populated and sane.
- Confirm `pr_commits` SHA coverage: for a sample merge-up PR, verify its commit SHAs also appear in earlier feature PRs' `pr_commits` (the S4 signal is now computable).
- Confirm migration is idempotent: run startup twice, assert no duplicate-column errors and `PRAGMA table_info` matches the spec.
- Confirm `allRecordTables` truncation includes `pr_commits` (Reset correctness).
- Re-run existing analyze + `cmd/velocity-rating-audit snapshot` to confirm **no regression** in current scores (these are additive columns; existing signals must be byte-stable until promotion detection is wired in separately).

---

## 7. Decisions locked vs. open

**Locked (2026-06-15):**
- One more full crawl, no future re-crawl → capture the full surface above.
- Add the `/pulls/{n}/commits` call + `pr_commits` table.
- Store all §2 ADD fields.

**Locked 2026-06-15 (conventional defaults, recommended options taken):**
1. Multi-valued fields (labels/assignees/requested_reviewers): **child tables** — `pr_labels(scope,month,repo,number,ord,name)`, `pr_assignees(…,login)`, `pr_requested_reviewers(…,login)`. Mirrors the existing `pr_issue_keys`/`pr_files` idiom; all three registered in `allRecordTables`.
2. Commit parent SHAs: **`parent_count` integer only** (on `pr_commits` + `github_commits`). Sufficient for merge detection (≥2 = merge); a full parent-SHA child table is deferred — only needed if we later walk the commit graph, and it's the one place "store everything" trades real disk for a hypothetical.
3. `patch`/diff-hunk text on file changes: **skip**. No current or near-term signal needs raw diffs; large storage cost. Revisit only if a content-similarity signal is ever scoped.

---

## 9. Implementation log (2026-06-15, uncommitted)

All layers landed; `go build ./...`, `go vet ./...`, `go test ./...` all green; `gofmt` clean.

- **`internal/cache/records.go`** — extended `GitHubPR` (base/head ref+sha+repo, merged_by, commit/file counts, merge_commit_sha, draft, auto_merge, updated, author_association, Labels/Assignees/RequestedReviewers, `Commits []PRCommit`), `FileChange` (+previous_filename, blob_sha), `ReviewComment` (+id, review_id, commit_id, line, original_line, updated, author_association), `GitHubReview` (+review_id, body, commit_id, author_association), `GitHubCommit` (+authored, committer, parent_count, comment_count). Added `PRCommit` struct. Commits uses the nil-vs-empty sentinel like Files/FileChanges.
- **`internal/pull/github.go`** — expanded `ghPRDetail` (named `ghPRRef` for head/base; merged_by/auto_merge/labels/assignees/requested_reviewers; `ghLabelNames`/`ghUserLogins` helpers), `ghReview`, `ghSearchCommit` (parents, authored date, committer, comment_count); wired all into `pullPRs`/`pullReviews`/`pullCommits`. PR detail labels/assignees read from authoritative `/pulls/{n}` body, not the search row.
- **`internal/pull/pr_commits.go`** (NEW) — `FetchPRCommits` + `ghPRCommit`, modeled on `FetchPRFileChanges` (same `ErrPRUnreachable` 404/410/422 sentinel). Called for merged PRs in `pullPRs`.
- **`internal/pull/pr_file_changes.go` / `pr_comments.go`** — mapped the new file-change + review-comment fields.
- **`internal/cache/sqlite_schema.go`** — new columns on `github_prs`/`pr_file_changes`/`pr_review_comments`/`github_reviews`/`github_commits`; NEW tables `pr_commits` (+idx on sha for S4 lookups), `pr_labels`, `pr_assignees`, `pr_requested_reviewers`; all 4 added to `allRecordTables`.
- **`internal/cache/sqlite.go`** — `migrateSchema` ALTERs every new column (idempotent `addMissingColumns`).
- **`internal/cache/sqlite_github.go`** — write+read paths for all new columns and the 4 child tables; `commits_fetched` sentinel mirrors files/file_changes.
- **JSONStore** — no change needed; generic JSON round-trip picks up the new struct fields automatically.

**Validation done:** copied the live 732MB cache to a temp path, opened it twice through `openSQLiteStore` (a throwaway test, since removed) → migration ALTERed the real old-schema DB cleanly, idempotent on re-open, all new columns + tables present. Full `go test ./...` green (existing PR/commit/review round-trip tests pass against the new write/read).

**Not done (by design):** the crawl itself has not been run — that's the user's call (rate-limit budget: adds a 4th `/pulls/{n}/commits` GET per merged PR). Promotion-PR detection (`promotion-pr-detection-scope.md`) is the separate consuming feature.

---

**Resolved — NOT a gap (corrected 2026-06-15):**
4. Review-fetch window. Initially flagged as a one-shot-stakes concern; **on reading the code it is not one for a full crawl.** `pullReviews` (github.go:254) fetches reviews per-PR for every PR created in month *m*, and `fetchPRReviews` (github.go:283) applies **no date filter** — it pulls every review on the PR regardless of submission date; analyze buckets each by its own `Submitted` timestamp. The github.go:251 "known limitation" only affects *incremental* pulls (un-crawled earlier months → those PRs never visited). A full historical crawl visits every PR in its creation month and captures all its reviews. No window change needed. (The only un-capturable reviews are those submitted *after* the crawl runs — true of any crawl.)
