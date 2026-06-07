package cache

import "io"

// Store is the substrate the rest of velocity reads cached primitives from and
// writes them back to. It abstracts the month-partitioned record cache plus the
// pull manifest so the backing store can move from JSON files to SQLite without
// touching the aggregation, pull, or backfill layers (architecture-evolution
// plan, Step 1).
//
// Why typed methods instead of the generic ReadMonth[T]/WriteMonth[T] free
// functions: a relational SQLite store needs per-source column mapping, and Go
// has no generic interface methods. The four record sources are a closed set,
// so a typed method per source is the honest seam. The generic free functions
// remain (JSONStore is implemented on top of them) but call sites go through
// the interface.
//
// Out of scope for the Store (still plain files in Step 1): ratings.json (Elo
// is a stateful precompute, not an on-demand query — E6), metrics.json (the
// serving blob, retired gradually in Step 2), and incognito-names.json. Those
// continue to live under cache.Root().
type Store interface {
	// Per-source typed record access for one (scope, month) cell. Read returns
	// a wrapped fs.ErrNotExist when the cell was never pulled (cache miss),
	// which callers errors.Is-check to treat as "skip". A pulled-but-empty cell
	// returns an empty slice and a nil error.
	ReadJiraIssues(scope string, m Month) ([]JiraIssue, error)
	WriteJiraIssues(scope string, m Month, recs []JiraIssue) error
	ReadGitHubPRs(scope string, m Month) ([]GitHubPR, error)
	WriteGitHubPRs(scope string, m Month, recs []GitHubPR) error
	ReadGitHubCommits(scope string, m Month) ([]GitHubCommit, error)
	WriteGitHubCommits(scope string, m Month, recs []GitHubCommit) error
	ReadGitHubReviews(scope string, m Month) ([]GitHubReview, error)
	WriteGitHubReviews(scope string, m Month, recs []GitHubReview) error

	// Manifest persistence. The in-memory Manifest struct (and its pure query
	// methods — FirstCachedMonth, LatestCachedMonth, Entry, Update) is
	// store-agnostic; only where it persists moves behind the Store.
	LoadManifest() (*Manifest, error)
	SaveManifest(m *Manifest) error

	// Reset wipes every cached record + the manifest, preserving siblings that
	// must survive a reset (ratings.json). Returns a human-readable list of what
	// was cleared. Idempotent.
	Reset(out io.Writer) ([]string, error)

	// Close releases any held resources (a no-op for the JSON store; closes the
	// database handle for SQLite).
	Close() error
}

// JSONStore is the original month-partitioned-JSON implementation of Store,
// implemented directly on top of the package's free functions so its behavior
// is byte-identical to the pre-Store cache. It holds no state.
type JSONStore struct{}

// compile-time assertion that JSONStore satisfies Store.
var _ Store = JSONStore{}

func (JSONStore) ReadJiraIssues(scope string, m Month) ([]JiraIssue, error) {
	return ReadMonth[JiraIssue](SourceJira, scope, m)
}
func (JSONStore) WriteJiraIssues(scope string, m Month, recs []JiraIssue) error {
	return WriteMonth(SourceJira, scope, m, recs)
}
func (JSONStore) ReadGitHubPRs(scope string, m Month) ([]GitHubPR, error) {
	return ReadMonth[GitHubPR](SourceGitHubPRs, scope, m)
}
func (JSONStore) WriteGitHubPRs(scope string, m Month, recs []GitHubPR) error {
	return WriteMonth(SourceGitHubPRs, scope, m, recs)
}
func (JSONStore) ReadGitHubCommits(scope string, m Month) ([]GitHubCommit, error) {
	return ReadMonth[GitHubCommit](SourceGitHubCommits, scope, m)
}
func (JSONStore) WriteGitHubCommits(scope string, m Month, recs []GitHubCommit) error {
	return WriteMonth(SourceGitHubCommits, scope, m, recs)
}
func (JSONStore) ReadGitHubReviews(scope string, m Month) ([]GitHubReview, error) {
	return ReadMonth[GitHubReview](SourceGitHubReviews, scope, m)
}
func (JSONStore) WriteGitHubReviews(scope string, m Month, recs []GitHubReview) error {
	return WriteMonth(SourceGitHubReviews, scope, m, recs)
}
func (JSONStore) LoadManifest() (*Manifest, error) { return LoadManifest() }
func (JSONStore) SaveManifest(m *Manifest) error   { return m.Save() }
func (JSONStore) Reset(out io.Writer) ([]string, error) {
	return Reset(out)
}
func (JSONStore) Close() error { return nil }
