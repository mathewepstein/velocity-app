package cache

// sqliteSchema is the relational mirror of the records.go structs (Step 1b).
//
// Design notes:
//   - Every record table carries (scope, month, ord): the cache is
//     month-partitioned and a cell is rewritten wholesale, so ord preserves the
//     in-slice order the JSON store produced — required for byte-identical
//     metrics.json reconstruction.
//   - Struct-slice and string-slice fields become child tables keyed back to the
//     parent's natural key. The nil-vs-empty sentinel (load-bearing for backfill
//     resume) is carried on the parent as a *_fetched flag: a fetched-but-empty
//     slice has the flag set with zero child rows; an unfetched slice leaves the
//     flag clear and reads back as nil. Slices declared `omitempty` in JSON
//     (labels, components, issue_keys) have no sentinel — they round-trip to nil
//     when empty regardless, so zero child rows simply read back as nil.
//   - time.Time is stored as RFC3339Nano text; nullable (*time.Time) columns are
//     NULL when nil.
//   - Indexes on (scope, month) serve the Step 1 whole-cell reads; the author /
//     epic / repo indexes are seeded now for the Step 2 query layer.
const sqliteSchema = `
CREATE TABLE IF NOT EXISTS manifest (
    source    TEXT NOT NULL,
    scope     TEXT NOT NULL,
    month     TEXT NOT NULL,
    pulled_at TEXT NOT NULL,
    records   INTEGER NOT NULL,
    PRIMARY KEY (source, scope, month)
);

CREATE TABLE IF NOT EXISTS jira_issues (
    scope             TEXT NOT NULL,
    month             TEXT NOT NULL,
    ord               INTEGER NOT NULL,
    key               TEXT NOT NULL,
    summary           TEXT,
    status            TEXT,
    resolution        TEXT,
    issue_type        TEXT,
    created           TEXT NOT NULL,
    updated           TEXT NOT NULL,
    resolved          TEXT,
    story_points      REAL,
    assignee          TEXT,
    reporter          TEXT,
    epic_key          TEXT,
    description       TEXT,
    detail_fetched    INTEGER NOT NULL DEFAULT 0,
    detail_fetched_at TEXT,
    first_in_progress TEXT,
    done_at           TEXT,
    cycle_hours       REAL,
    status_flips      INTEGER,
    pre_code_comments INTEGER,
    changelog_fetched INTEGER NOT NULL DEFAULT 0,
    comments_fetched  INTEGER NOT NULL DEFAULT 0,
    relations_fetched  INTEGER NOT NULL DEFAULT 0,
    raw_fields_fetched INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (scope, month, key)
);
CREATE INDEX IF NOT EXISTS idx_jira_cell ON jira_issues (scope, month);
CREATE INDEX IF NOT EXISTS idx_jira_assignee ON jira_issues (assignee);
CREATE INDEX IF NOT EXISTS idx_jira_epic ON jira_issues (epic_key);

CREATE TABLE IF NOT EXISTS jira_labels (
    scope TEXT NOT NULL, month TEXT NOT NULL, issue_key TEXT NOT NULL,
    ord INTEGER NOT NULL, value TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_jira_labels_cell ON jira_labels (scope, month);

CREATE TABLE IF NOT EXISTS jira_components (
    scope TEXT NOT NULL, month TEXT NOT NULL, issue_key TEXT NOT NULL,
    ord INTEGER NOT NULL, value TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_jira_components_cell ON jira_components (scope, month);

CREATE TABLE IF NOT EXISTS jira_changelog (
    scope TEXT NOT NULL, month TEXT NOT NULL, issue_key TEXT NOT NULL, ord INTEGER NOT NULL,
    at TEXT NOT NULL, author TEXT, from_status TEXT, to_status TEXT, field TEXT
);
CREATE INDEX IF NOT EXISTS idx_jira_changelog_cell ON jira_changelog (scope, month);

CREATE TABLE IF NOT EXISTS jira_comments (
    scope TEXT NOT NULL, month TEXT NOT NULL, issue_key TEXT NOT NULL, ord INTEGER NOT NULL,
    author TEXT, created TEXT NOT NULL, body TEXT
);
CREATE INDEX IF NOT EXISTS idx_jira_comments_cell ON jira_comments (scope, month);

CREATE TABLE IF NOT EXISTS jira_issue_links (
    scope TEXT NOT NULL, month TEXT NOT NULL, issue_key TEXT NOT NULL, ord INTEGER NOT NULL,
    counterpart_key TEXT NOT NULL, link_type TEXT, direction TEXT, phrase TEXT, status TEXT, issue_type TEXT
);
CREATE INDEX IF NOT EXISTS idx_jira_issue_links_cell ON jira_issue_links (scope, month);

CREATE TABLE IF NOT EXISTS jira_attachments (
    scope TEXT NOT NULL, month TEXT NOT NULL, issue_key TEXT NOT NULL, ord INTEGER NOT NULL,
    filename TEXT NOT NULL, mime_type TEXT, size INTEGER NOT NULL DEFAULT 0, created TEXT, author TEXT
);
CREATE INDEX IF NOT EXISTS idx_jira_attachments_cell ON jira_attachments (scope, month);

CREATE TABLE IF NOT EXISTS jira_fix_versions (
    scope TEXT NOT NULL, month TEXT NOT NULL, issue_key TEXT NOT NULL,
    ord INTEGER NOT NULL, value TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_jira_fix_versions_cell ON jira_fix_versions (scope, month);

CREATE TABLE IF NOT EXISTS jira_issue_fields (
    scope TEXT NOT NULL, month TEXT NOT NULL, issue_key TEXT NOT NULL, ord INTEGER NOT NULL,
    field_id TEXT NOT NULL, field_name TEXT, value_json TEXT
);
CREATE INDEX IF NOT EXISTS idx_jira_issue_fields_cell ON jira_issue_fields (scope, month);

CREATE TABLE IF NOT EXISTS github_prs (
    scope                   TEXT NOT NULL,
    month                   TEXT NOT NULL,
    ord                     INTEGER NOT NULL,
    number                  INTEGER NOT NULL,
    repo                    TEXT NOT NULL,
    title                   TEXT,
    body                    TEXT,
    state                   TEXT,
    author                  TEXT,
    branch                  TEXT,
    created                 TEXT NOT NULL,
    merged                  TEXT,
    closed                  TEXT,
    additions               INTEGER NOT NULL,
    deletions               INTEGER NOT NULL,
    inline_comments         INTEGER,
    deep_threads            INTEGER,
    files_fetched           INTEGER NOT NULL DEFAULT 0,
    review_comments_fetched INTEGER NOT NULL DEFAULT 0,
    file_changes_fetched    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (scope, month, repo, number)
);
CREATE INDEX IF NOT EXISTS idx_prs_cell ON github_prs (scope, month);
CREATE INDEX IF NOT EXISTS idx_prs_author ON github_prs (author);

CREATE TABLE IF NOT EXISTS pr_issue_keys (
    scope TEXT NOT NULL, month TEXT NOT NULL, repo TEXT NOT NULL, number INTEGER NOT NULL,
    ord INTEGER NOT NULL, value TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_pr_issue_keys_cell ON pr_issue_keys (scope, month);

CREATE TABLE IF NOT EXISTS pr_files (
    scope TEXT NOT NULL, month TEXT NOT NULL, repo TEXT NOT NULL, number INTEGER NOT NULL,
    ord INTEGER NOT NULL, path TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_pr_files_cell ON pr_files (scope, month);

CREATE TABLE IF NOT EXISTS pr_review_comments (
    scope TEXT NOT NULL, month TEXT NOT NULL, repo TEXT NOT NULL, number INTEGER NOT NULL, ord INTEGER NOT NULL,
    author TEXT, path TEXT, in_reply_to INTEGER, created TEXT NOT NULL, body TEXT
);
CREATE INDEX IF NOT EXISTS idx_pr_review_comments_cell ON pr_review_comments (scope, month);

CREATE TABLE IF NOT EXISTS pr_file_changes (
    scope TEXT NOT NULL, month TEXT NOT NULL, repo TEXT NOT NULL, number INTEGER NOT NULL, ord INTEGER NOT NULL,
    path TEXT NOT NULL, status TEXT, additions INTEGER NOT NULL, deletions INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_pr_file_changes_cell ON pr_file_changes (scope, month);

CREATE TABLE IF NOT EXISTS github_commits (
    scope     TEXT NOT NULL,
    month     TEXT NOT NULL,
    ord       INTEGER NOT NULL,
    sha       TEXT NOT NULL,
    repo      TEXT NOT NULL,
    author    TEXT,
    message   TEXT,
    committed TEXT NOT NULL,
    additions INTEGER NOT NULL,
    deletions INTEGER NOT NULL,
    PRIMARY KEY (scope, month, sha, repo)
);
CREATE INDEX IF NOT EXISTS idx_commits_cell ON github_commits (scope, month);
CREATE INDEX IF NOT EXISTS idx_commits_author ON github_commits (author);

CREATE TABLE IF NOT EXISTS commit_issue_keys (
    scope TEXT NOT NULL, month TEXT NOT NULL, repo TEXT NOT NULL, sha TEXT NOT NULL,
    ord INTEGER NOT NULL, value TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_commit_issue_keys_cell ON commit_issue_keys (scope, month);

CREATE TABLE IF NOT EXISTS github_reviews (
    scope     TEXT NOT NULL,
    month     TEXT NOT NULL,
    ord       INTEGER NOT NULL,
    pr_number INTEGER NOT NULL,
    repo      TEXT NOT NULL,
    reviewer  TEXT,
    state     TEXT,
    submitted TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_reviews_cell ON github_reviews (scope, month);
CREATE INDEX IF NOT EXISTS idx_reviews_reviewer ON github_reviews (reviewer);
`

// allRecordTables is every table Reset truncates (manifest + records + children),
// ordered children-then-parents — irrelevant without FKs but kept tidy.
var allRecordTables = []string{
	"jira_labels", "jira_components", "jira_changelog", "jira_comments",
	"jira_issue_links", "jira_attachments", "jira_fix_versions", "jira_issue_fields", "jira_issues",
	"pr_issue_keys", "pr_files", "pr_review_comments", "pr_file_changes", "github_prs",
	"commit_issue_keys", "github_commits",
	"github_reviews",
	"manifest",
}
