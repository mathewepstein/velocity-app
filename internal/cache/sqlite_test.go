package cache

import (
	"errors"
	"io"
	"io/fs"
	"reflect"
	"testing"
	"time"
)

func openTestSQLite(t *testing.T) *sqliteStore {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	st, err := openSQLiteStore("")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	s := st.(*sqliteStore)
	t.Cleanup(func() { s.Close() })
	return s
}

func columnSet(t *testing.T, s *sqliteStore, table string) map[string]bool {
	t.Helper()
	rows, err := s.db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt interface{}
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		cols[name] = true
	}
	return cols
}

func TestMigrateSchema_ColumnsPresentAndIdempotent(t *testing.T) {
	s := openTestSQLite(t)
	// migrateSchema already ran in openSQLiteStore; the new columns must exist.
	cols := columnSet(t, s, "jira_issues")
	for _, c := range []string{"relations_fetched", "raw_fields_fetched"} {
		if !cols[c] {
			t.Errorf("jira_issues missing migrated column %q", c)
		}
	}
	// Re-running must be a no-op (columns already present), not an error.
	if err := migrateSchema(s.db); err != nil {
		t.Fatalf("re-run migrateSchema: %v", err)
	}
}

func TestAddMissingColumns_AddsAndSkips(t *testing.T) {
	s := openTestSQLite(t)
	if _, err := s.db.Exec(`CREATE TABLE scratch (a INTEGER)`); err != nil {
		t.Fatal(err)
	}
	defs := []columnDef{{"a", "INTEGER"}, {"b", "INTEGER NOT NULL DEFAULT 0"}, {"c", "TEXT"}}
	if err := addMissingColumns(s.db, "scratch", defs); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if err := addMissingColumns(s.db, "scratch", defs); err != nil {
		t.Fatalf("idempotent re-add: %v", err)
	}
	cols := columnSet(t, s, "scratch")
	for _, c := range []string{"a", "b", "c"} {
		if !cols[c] {
			t.Errorf("scratch missing column %q after migrate", c)
		}
	}
}

func ut(s string) time.Time {
	tm, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return tm.UTC()
}
func utp(s string) *time.Time { v := ut(s); return &v }

// markPulled records a manifest entry so the cell reads as pulled (not a miss).
func (s *sqliteStore) markPulled(t *testing.T, source Source, scope string, m Month, n int) {
	t.Helper()
	mf, err := s.LoadManifest()
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	mf.Update(source, scope, m, n, ut("2026-06-01T00:00:00Z"))
	if err := s.SaveManifest(mf); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
}

func TestSQLite_CellMissIsNotExist(t *testing.T) {
	s := openTestSQLite(t)
	_, err := s.ReadJiraIssues("CD", MustParseMonth("2026-01"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("want fs.ErrNotExist for never-pulled cell, got %v", err)
	}
}

func TestSQLite_ManifestRoundTrip(t *testing.T) {
	s := openTestSQLite(t)
	mf := NewManifest()
	mf.Update(SourceJira, "CD", MustParseMonth("2026-01"), 7, ut("2026-02-01T12:00:00Z"))
	mf.Update(SourceGitHubPRs, "org", MustParseMonth("2026-03"), 3, ut("2026-03-15T08:30:00Z"))
	if err := s.SaveManifest(mf); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := s.LoadManifest()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !reflect.DeepEqual(got.Entries, mf.Entries) {
		t.Fatalf("manifest round-trip mismatch:\n got=%+v\nwant=%+v", got.Entries, mf.Entries)
	}
}

func TestSQLite_JiraIssueRoundTrip(t *testing.T) {
	s := openTestSQLite(t)
	m := MustParseMonth("2026-01")
	issues := []JiraIssue{
		{ // fully unfetched: Changelog/Comments nil, no labels
			Key: "CD-1", Summary: "a", Status: "Done", Resolution: "Fixed", IssueType: "Bug",
			Created: ut("2026-01-02T03:04:05Z"), Updated: ut("2026-01-10T00:00:00Z"),
			Resolved: utp("2026-01-09T00:00:00Z"), StoryPoints: 3, Assignee: "acc1", Reporter: "acc2",
			EpicKey: "CD-100", Labels: []string{"x", "y"}, Components: []string{"c1"},
		},
		{ // detail-fetched but empty changelog/comments (sentinel: non-nil empty)
			Key: "CD-2", Summary: "b", Status: "Open",
			Created: ut("2026-01-05T00:00:00Z"), Updated: ut("2026-01-06T00:00:00Z"),
			DetailFetched: true, DetailFetchedAt: utp("2026-01-07T00:00:00Z"),
			Changelog: []StatusTransition{}, Comments: []IssueComment{},
		},
		{ // populated changelog + comments, derived fields
			Key: "CD-3", Summary: "c", Status: "In QA",
			Created: ut("2026-01-08T00:00:00Z"), Updated: ut("2026-01-09T00:00:00Z"),
			DetailFetched: true, DetailFetchedAt: utp("2026-01-09T01:00:00Z"),
			FirstInProgress: utp("2026-01-08T06:00:00Z"), DoneAt: utp("2026-01-09T00:00:00Z"),
			CycleHours: 18.5, StatusFlips: 2, PreCodeComments: 1,
			Changelog: []StatusTransition{
				{At: ut("2026-01-08T06:00:00Z"), Author: "acc1", From: "Open", To: "In Progress", Field: "status"},
				{At: ut("2026-01-08T20:00:00Z"), Author: "acc1", From: "In Progress", To: "In QA", Field: "status"},
			},
			Comments: []IssueComment{
				{Author: "acc2", Created: ut("2026-01-08T07:00:00Z"), Body: "lgtm"},
			},
		},
	}
	if err := s.WriteJiraIssues("CD", m, issues); err != nil {
		t.Fatalf("write: %v", err)
	}
	s.markPulled(t, SourceJira, "CD", m, len(issues))
	got, err := s.ReadJiraIssues("CD", m)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !reflect.DeepEqual(got, issues) {
		t.Fatalf("jira round-trip mismatch:\n got=%#v\nwant=%#v", got, issues)
	}
	// Explicit sentinel assertions (DeepEqual already covers, but make intent loud).
	if got[0].Changelog != nil || got[0].Comments != nil {
		t.Fatalf("unfetched issue should have nil changelog/comments, got %#v / %#v", got[0].Changelog, got[0].Comments)
	}
	if got[1].Changelog == nil || len(got[1].Changelog) != 0 {
		t.Fatalf("fetched-empty issue should have non-nil empty changelog, got %#v", got[1].Changelog)
	}
}

func TestSQLite_JiraRelationsAndFieldsRoundTrip(t *testing.T) {
	s := openTestSQLite(t)
	m := MustParseMonth("2026-03")
	issues := []JiraIssue{
		{ // never run through field capture: Links/Attachments/RawFields nil
			Key: "CD-10", Summary: "uncaptured",
			Created: ut("2026-03-01T00:00:00Z"), Updated: ut("2026-03-01T00:00:00Z"),
		},
		{ // relations captured but empty (sentinel: non-nil empty); raw fields captured empty
			Key: "CD-11", Summary: "captured-empty",
			Created:     ut("2026-03-02T00:00:00Z"), Updated: ut("2026-03-02T00:00:00Z"),
			Links:       []LinkedIssue{}, Attachments: []Attachment{}, RawFields: []RawField{},
		},
		{ // fully populated relationships + fix versions + raw catch-all
			Key: "CD-12", Summary: "populated",
			Created: ut("2026-03-03T00:00:00Z"), Updated: ut("2026-03-03T00:00:00Z"),
			Links: []LinkedIssue{
				{Key: "CD-99", LinkType: "subtask", Direction: "outward", Phrase: "subtask"},
				{Key: "CD-88", LinkType: "Cloners", Direction: "outward", Phrase: "split to", Status: "Open", IssueType: "Task"},
			},
			Attachments: []Attachment{
				{Filename: "design.png", MimeType: "image/png", Size: 4096, Created: ut("2026-03-03T01:00:00Z"), Author: "acc1"},
			},
			FixVersions: []string{"1.20", "1.21"},
			RawFields: []RawField{
				{ID: "customfield_10126", Name: "Flagged", Value: `[{"value":"Impediment"}]`},
				{ID: "priority", Name: "Priority", Value: `{"name":"High"}`},
			},
		},
	}
	if err := s.WriteJiraIssues("CD", m, issues); err != nil {
		t.Fatalf("write: %v", err)
	}
	s.markPulled(t, SourceJira, "CD", m, len(issues))
	got, err := s.ReadJiraIssues("CD", m)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !reflect.DeepEqual(got, issues) {
		t.Fatalf("relations round-trip mismatch:\n got=%#v\nwant=%#v", got, issues)
	}
	if got[0].Links != nil || got[0].Attachments != nil || got[0].RawFields != nil {
		t.Fatalf("uncaptured issue should have nil links/attachments/raw, got %#v / %#v / %#v", got[0].Links, got[0].Attachments, got[0].RawFields)
	}
	if got[1].Links == nil || len(got[1].Links) != 0 || got[1].RawFields == nil || len(got[1].RawFields) != 0 {
		t.Fatalf("captured-empty issue should have non-nil empty links/raw, got %#v / %#v", got[1].Links, got[1].RawFields)
	}
}

func TestSQLite_GitHubPRRoundTrip(t *testing.T) {
	s := openTestSQLite(t)
	m := MustParseMonth("2026-02")
	prs := []GitHubPR{
		{ // open PR, nothing hydrated: Files/ReviewComments/FileChanges nil
			Number: 1, Repo: "org/a", Title: "t1", State: "open", Author: "alice",
			Created: ut("2026-02-01T00:00:00Z"), Additions: 10, Deletions: 2,
			IssueKeys: []string{"CD-1"},
		},
		{ // merged PR, files fetched-empty, review comments populated, file changes fetched-empty
			Number: 2, Repo: "org/a", Title: "t2", State: "merged", Author: "bob", Branch: "feat",
			Created: ut("2026-02-02T00:00:00Z"), Merged: utp("2026-02-03T00:00:00Z"),
			Additions: 100, Deletions: 50, InlineComments: 1, DeepThreads: 0,
			Files:          []string{},
			ReviewComments: []ReviewComment{{Author: "carol", Path: "x.go", InReplyTo: 0, Created: ut("2026-02-02T12:00:00Z"), Body: "nit"}},
			FileChanges:    []FileChange{},
		},
		{ // merged PR, files + file changes populated
			Number: 3, Repo: "org/b", Title: "t3", State: "merged", Author: "alice",
			Created: ut("2026-02-04T00:00:00Z"), Merged: utp("2026-02-05T00:00:00Z"),
			Closed: utp("2026-02-05T00:00:00Z"), Additions: 7, Deletions: 1,
			Files:       []string{"a.go", "b.go"},
			FileChanges: []FileChange{{Path: "a.go", Status: "added", Additions: 5, Deletions: 0}, {Path: "b.go", Status: "modified", Additions: 2, Deletions: 1}},
		},
	}
	if err := s.WriteGitHubPRs("org", m, prs); err != nil {
		t.Fatalf("write: %v", err)
	}
	s.markPulled(t, SourceGitHubPRs, "org", m, len(prs))
	got, err := s.ReadGitHubPRs("org", m)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !reflect.DeepEqual(got, prs) {
		t.Fatalf("PR round-trip mismatch:\n got=%#v\nwant=%#v", got, prs)
	}
	if got[0].Files != nil || got[0].ReviewComments != nil || got[0].FileChanges != nil {
		t.Fatalf("unhydrated PR should have nil slices, got %#v", got[0])
	}
	if got[1].Files == nil || len(got[1].Files) != 0 {
		t.Fatalf("fetched-empty Files should be non-nil empty, got %#v", got[1].Files)
	}
}

func TestSQLite_CommitsAndReviewsRoundTrip(t *testing.T) {
	s := openTestSQLite(t)
	m := MustParseMonth("2026-03")
	commits := []GitHubCommit{
		{SHA: "abc", Repo: "org/a", Author: "alice", Message: "fix", Committed: ut("2026-03-01T00:00:00Z"), Additions: 3, Deletions: 1, IssueKeys: []string{"CD-9"}},
		{SHA: "def", Repo: "org/a", Author: "bob", Message: "feat", Committed: ut("2026-03-02T00:00:00Z"), Additions: 9, Deletions: 0},
	}
	if err := s.WriteGitHubCommits("org", m, commits); err != nil {
		t.Fatalf("write commits: %v", err)
	}
	s.markPulled(t, SourceGitHubCommits, "org", m, len(commits))
	gotC, err := s.ReadGitHubCommits("org", m)
	if err != nil {
		t.Fatalf("read commits: %v", err)
	}
	if !reflect.DeepEqual(gotC, commits) {
		t.Fatalf("commit round-trip mismatch:\n got=%#v\nwant=%#v", gotC, commits)
	}

	reviews := []GitHubReview{
		{PRNumber: 1, Repo: "org/a", Reviewer: "carol", State: "APPROVED", Submitted: ut("2026-03-03T00:00:00Z")},
		{PRNumber: 1, Repo: "org/a", Reviewer: "dave", State: "COMMENTED", Submitted: ut("2026-03-03T01:00:00Z")},
	}
	if err := s.WriteGitHubReviews("org", m, reviews); err != nil {
		t.Fatalf("write reviews: %v", err)
	}
	s.markPulled(t, SourceGitHubReviews, "org", m, len(reviews))
	gotR, err := s.ReadGitHubReviews("org", m)
	if err != nil {
		t.Fatalf("read reviews: %v", err)
	}
	if !reflect.DeepEqual(gotR, reviews) {
		t.Fatalf("review round-trip mismatch:\n got=%#v\nwant=%#v", gotR, reviews)
	}
}

func TestSQLite_RewriteCellReplaces(t *testing.T) {
	s := openTestSQLite(t)
	m := MustParseMonth("2026-01")
	s.markPulled(t, SourceJira, "CD", m, 1)
	if err := s.WriteJiraIssues("CD", m, []JiraIssue{{Key: "CD-1", Created: ut("2026-01-01T00:00:00Z"), Updated: ut("2026-01-01T00:00:00Z"), Labels: []string{"old"}}}); err != nil {
		t.Fatal(err)
	}
	// Rewrite the same cell with different content; old rows + children must be gone.
	if err := s.WriteJiraIssues("CD", m, []JiraIssue{{Key: "CD-2", Created: ut("2026-01-02T00:00:00Z"), Updated: ut("2026-01-02T00:00:00Z")}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadJiraIssues("CD", m)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != "CD-2" || got[0].Labels != nil {
		t.Fatalf("cell rewrite did not replace cleanly: %#v", got)
	}
}

func TestSQLite_ResetTruncates(t *testing.T) {
	s := openTestSQLite(t)
	m := MustParseMonth("2026-01")
	s.markPulled(t, SourceJira, "CD", m, 1)
	if err := s.WriteJiraIssues("CD", m, []JiraIssue{{Key: "CD-1", Created: ut("2026-01-01T00:00:00Z"), Updated: ut("2026-01-01T00:00:00Z")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reset(io.Discard); err != nil {
		t.Fatalf("reset: %v", err)
	}
	mf, err := s.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	if len(mf.Entries) != 0 {
		t.Fatalf("manifest not cleared after reset: %+v", mf.Entries)
	}
	if _, err := s.ReadJiraIssues("CD", m); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("cell should be a miss after reset, got %v", err)
	}
}
