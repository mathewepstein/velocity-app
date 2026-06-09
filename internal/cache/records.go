package cache

import "time"

// Source identifies a data source. Values are stable strings because they end
// up in manifest keys and directory names on disk. GitHub PRs and commits are
// separate sources because they are independently paginated and may come back
// at different rates from the GitHub search API.
type Source string

const (
	SourceJira          Source = "jira"
	SourceGitHubPRs     Source = "github-prs"
	SourceGitHubCommits Source = "github-commits"
	SourceGitHubReviews Source = "github-reviews"
)

// JiraIssue is the canonical in-cache shape for one Jira issue. Pullers in
// Phase 4 derive this from the Jira REST v3 search response; analyzers in
// Phase 6 aggregate over it.
//
// Fields are nullable where the API may omit them (e.g., Resolved on
// unresolved issues). StoryPoints is float64 because Jira returns it as a
// number; missing/unset → 0.
type JiraIssue struct {
	Key         string     `json:"key"`
	Summary     string     `json:"summary"`
	Status      string     `json:"status"`
	Resolution  string     `json:"resolution,omitempty"`
	IssueType   string     `json:"issue_type,omitempty"`
	Created     time.Time  `json:"created"`
	Updated     time.Time  `json:"updated"`
	Resolved    *time.Time `json:"resolved,omitempty"`
	StoryPoints float64    `json:"story_points,omitempty"`
	Assignee    string     `json:"assignee,omitempty"` // accountId
	Reporter    string     `json:"reporter,omitempty"` // accountId
	EpicKey     string     `json:"epic_key,omitempty"` // resolved via configured epic_link field
	Labels      []string   `json:"labels,omitempty"`
	Components  []string   `json:"components,omitempty"`

	// --- Detail-hydration fields (backfill-missing-data-plan). ---
	// Raw signals captured per-issue; additive and forward/backward compatible.
	Description string             `json:"description,omitempty"` // ADF flattened to plain text
	Changelog   []StatusTransition `json:"changelog"`             // NO omitempty: nil=unfetched, []=none
	Comments    []IssueComment     `json:"comments"`              // NO omitempty: nil=unfetched, []=none

	// Resume gate. DetailFetched flips true once changelog+comments+description
	// have been hydrated; resolved issues are then frozen, open ones re-hydrate.
	DetailFetched   bool       `json:"detail_fetched,omitempty"`
	DetailFetchedAt *time.Time `json:"detail_fetched_at,omitempty"`

	// Derived at hydration and cached so analyze/evidence never re-walks the raw
	// changelog/comments.
	FirstInProgress *time.Time `json:"first_in_progress,omitempty"`
	DoneAt          *time.Time `json:"done_at,omitempty"`
	CycleHours      float64    `json:"cycle_hours,omitempty"`
	StatusFlips     int        `json:"status_flips,omitempty"`
	PreCodeComments int        `json:"pre_code_comments,omitempty"`

	// --- Relationship + field-capture fields (jira-field-capture-plan Phase B). ---
	// Links is subtasks + issue links, captured from the base search (free in the
	// issue page) and the historical field-capture crawl. NO omitempty: the
	// nil-vs-empty sentinel (carried by relations_fetched) distinguishes an issue
	// never run through capture (nil) from one with no relationships ([]).
	// Attachments shares that sentinel. FixVersions is omitempty like Labels /
	// Components — it round-trips to nil when empty, no sentinel needed.
	Links       []LinkedIssue `json:"links"`
	Attachments []Attachment  `json:"attachments"`
	FixVersions []string      `json:"fix_versions,omitempty"`

	// RawFields is the catch-all: every populated Jira field captured by the
	// historical `fields=*all` crawl (minus the noise denylist), keyed by field
	// ID with its JSON value. It is the "never crawl Jira again" store — a future
	// signal derives from it without a re-pull. NO omitempty: nil=uncaptured (the
	// crawl hasn't run on this issue), []=captured-but-all-denylisted. Carried by
	// raw_fields_fetched, which is also the crawl's backfill gate.
	RawFields []RawField `json:"raw_fields"`
}

// LinkedIssue is one relationship from an issue: a subtask or an issue link.
// LinkType is the raw Jira type name ("Blocks", "Cloners", "Split", or
// "subtask"); Direction is "inward"/"outward"; Phrase is the human relationship
// text ("is blocked by", "split to") used to classify spawned vs. plain links
// in scoring. Status/IssueType describe the counterpart when the API returns
// them.
type LinkedIssue struct {
	Key       string `json:"key"`
	LinkType  string `json:"link_type"`
	Direction string `json:"direction"`
	Phrase    string `json:"phrase,omitempty"`
	Status    string `json:"status,omitempty"`
	IssueType string `json:"issue_type,omitempty"`
}

// Attachment is one issue attachment. MimeType + Filename let scoring tell a
// design doc / screenshot from a log dump (spike artifact density); the bytes
// are never fetched, only the metadata.
type Attachment struct {
	Filename string    `json:"filename"`
	MimeType string    `json:"mime_type,omitempty"`
	Size     int       `json:"size,omitempty"`
	Created  time.Time `json:"created"`
	Author   string    `json:"author,omitempty"` // accountId
}

// RawField is one entry in the catch-all: a Jira field's ID, human name, and
// JSON-encoded value, exactly as the field-capture crawl saw it.
type RawField struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	Value string `json:"value"` // JSON-encoded raw field value
}

// StatusTransition is one entry from a Jira issue's changelog: a field change
// (usually "status") at a point in time. Drives cycle-time and rework signals.
type StatusTransition struct {
	At     time.Time `json:"at"`
	Author string    `json:"author,omitempty"` // accountId
	From   string    `json:"from,omitempty"`
	To     string    `json:"to,omitempty"`
	Field  string    `json:"field,omitempty"` // usually "status"
}

// IssueComment is one comment on a Jira issue, body flattened from ADF.
type IssueComment struct {
	Author  string    `json:"author,omitempty"` // accountId
	Created time.Time `json:"created"`
	Body    string    `json:"body,omitempty"` // ADF flattened to plain text
}

// GitHubPR is the canonical in-cache shape for one pull request.
// IssueKeys is extracted client-side from title/body/branch (regex
// [A-Z]+-\d+) and stored pre-computed so analysis doesn't re-scan strings.
//
// Files is the list of file paths changed by the PR, populated only for
// merged PRs. Nil means "not yet fetched" (lazy backfill candidate); an
// empty non-nil slice means "fetched and confirmed empty" — either a PR
// that genuinely changes zero files (extremely rare) or one that returned
// a permanent error from the GitHub API (404/410/422 — deleted, renamed,
// unprocessable). Drafts and closed-without-merge PRs stay nil since their
// files don't contribute to scoring.
//
// NOTE: no `omitempty` on this field. The nil-vs-empty distinction is
// load-bearing for the backfill script's idempotent resume — round-tripping
// an empty slice through omitempty serializes as absent, which would deserialize
// back to nil and cause unreachable PRs to be re-fetched every run.
type GitHubPR struct {
	Number    int        `json:"number"`
	Repo      string     `json:"repo"` // "org/name"
	Title     string     `json:"title"`
	Body      string     `json:"body,omitempty"`
	State     string     `json:"state"` // open | closed | merged
	Author    string     `json:"author"`
	Branch    string     `json:"branch,omitempty"`
	Created   time.Time  `json:"created"`
	Merged    *time.Time `json:"merged,omitempty"`
	Closed    *time.Time `json:"closed,omitempty"`
	Additions int        `json:"additions"`
	Deletions int        `json:"deletions"`
	IssueKeys []string   `json:"issue_keys,omitempty"`
	Files     []string   `json:"files"`

	// --- Detail-hydration fields (backfill-missing-data-plan). ---
	// ReviewComments are inline (diff-anchored) review comments, populated only
	// for merged PRs. Same nil-vs-empty sentinel contract as Files: nil=unfetched
	// (backfill candidate), []=fetched-empty or permanent error. NO omitempty.
	ReviewComments []ReviewComment `json:"review_comments"`
	InlineComments int             `json:"inline_comments,omitempty"` // derived count
	DeepThreads    int             `json:"deep_threads,omitempty"`    // threads w/ 3+ replies (in_reply_to chains)

	// FileChanges supersedes Files with per-file status + LOC. NO omitempty
	// (same sentinel). Files is kept for backward compat with existing records.
	FileChanges []FileChange `json:"file_changes"`
}

// ReviewComment is one inline pull-request review comment. InReplyTo links a
// reply to its parent comment's id, so reply chains (thread depth) can be
// reconstructed without a second API shape.
type ReviewComment struct {
	Author    string    `json:"author"`
	Path      string    `json:"path,omitempty"`
	InReplyTo int       `json:"in_reply_to,omitempty"`
	Created   time.Time `json:"created"`
	Body      string    `json:"body,omitempty"`
}

// FileChange is one file touched by a PR, with its add/modify/remove status —
// the new-vs-modified-code signal that the path-only Files list can't express.
type FileChange struct {
	Path      string `json:"path"`
	Status    string `json:"status"` // added | modified | removed | renamed
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

// GitHubCommit is the canonical in-cache shape for one commit. Note: a
// commit's month bucket is determined by Committed (not Authored) to avoid
// replay of historical commits rewriting old cache partitions.
type GitHubCommit struct {
	SHA       string    `json:"sha"`
	Repo      string    `json:"repo"` // "org/name"
	Author    string    `json:"author"`
	Message   string    `json:"message"`
	Committed time.Time `json:"committed"`
	Additions int       `json:"additions"`
	Deletions int       `json:"deletions"`
	IssueKeys []string  `json:"issue_keys,omitempty"`
}

// GitHubReview is the canonical in-cache shape for one pull-request review.
// Bucketed by Submitted (UTC) so a review left months after the PR was opened
// lands in the month the review actually happened — that's the correct signal
// for reviewer credit in scoring.
type GitHubReview struct {
	PRNumber  int       `json:"pr_number"`
	Repo      string    `json:"repo"` // "org/name"
	Reviewer  string    `json:"reviewer"`
	State     string    `json:"state"` // APPROVED | CHANGES_REQUESTED | COMMENTED | DISMISSED
	Submitted time.Time `json:"submitted"`
}
