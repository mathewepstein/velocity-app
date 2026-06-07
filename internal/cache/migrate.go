package cache

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
)

// MigrateStats summarizes a JSON→SQLite migration.
type MigrateStats struct {
	Cells   int // manifest cells visited
	Skipped int // cells whose JSON partition was missing (manifest references a moved file)
	Issues  int
	PRs     int
	Commits int
	Reviews int
}

// MigrateToSQLite imports the entire JSON-file corpus into a SQLite store at
// dbPath (empty → Root()/velocity.db), preserving the manifest. It is the
// one-time importer for architecture-evolution Step 1b. The JSON corpus is left
// untouched (cold backup until SQLite is proven on a full refresh + analyze).
//
// Per-cell reads route through the JSON store's typed accessors; a manifest
// entry whose on-disk partition is missing is skipped (the manifest can outlive
// a moved file). The destination DB has its cells cleared first so a re-run is
// idempotent.
func MigrateToSQLite(dbPath string, out io.Writer) (MigrateStats, error) {
	var st MigrateStats

	src := JSONStore{}
	manifest, err := src.LoadManifest()
	if err != nil {
		return st, fmt.Errorf("load JSON manifest: %w", err)
	}

	dst, err := openSQLiteStore(dbPath)
	if err != nil {
		return st, err
	}
	defer dst.Close()

	// Wipe the destination so re-running migrate is idempotent rather than
	// doubling rows under the wholesale-rewrite contract.
	if _, err := dst.Reset(io.Discard); err != nil {
		return st, fmt.Errorf("reset destination: %w", err)
	}

	// Deterministic order (source, scope, month) keeps progress output stable
	// and groups a scope's months together.
	keys := make([]string, 0, len(manifest.Entries))
	for k := range manifest.Entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	logf := func(format string, args ...any) {
		if out != nil {
			fmt.Fprintf(out, format, args...)
		}
	}

	for _, k := range keys {
		e := manifest.Entries[k]
		mo, err := ParseMonth(e.Month)
		if err != nil {
			logf("  [skip] bad month %q: %v\n", e.Month, err)
			continue
		}
		st.Cells++

		switch e.Source {
		case SourceJira:
			recs, err := src.ReadJiraIssues(e.Scope, mo)
			if skipMissing(err) {
				st.Skipped++
				continue
			}
			if err != nil {
				return st, fmt.Errorf("read jira %s/%s: %w", e.Scope, mo, err)
			}
			if err := dst.WriteJiraIssues(e.Scope, mo, recs); err != nil {
				return st, fmt.Errorf("write jira %s/%s: %w", e.Scope, mo, err)
			}
			st.Issues += len(recs)
		case SourceGitHubPRs:
			recs, err := src.ReadGitHubPRs(e.Scope, mo)
			if skipMissing(err) {
				st.Skipped++
				continue
			}
			if err != nil {
				return st, fmt.Errorf("read prs %s/%s: %w", e.Scope, mo, err)
			}
			if err := dst.WriteGitHubPRs(e.Scope, mo, recs); err != nil {
				return st, fmt.Errorf("write prs %s/%s: %w", e.Scope, mo, err)
			}
			st.PRs += len(recs)
		case SourceGitHubCommits:
			recs, err := src.ReadGitHubCommits(e.Scope, mo)
			if skipMissing(err) {
				st.Skipped++
				continue
			}
			if err != nil {
				return st, fmt.Errorf("read commits %s/%s: %w", e.Scope, mo, err)
			}
			if err := dst.WriteGitHubCommits(e.Scope, mo, recs); err != nil {
				return st, fmt.Errorf("write commits %s/%s: %w", e.Scope, mo, err)
			}
			st.Commits += len(recs)
		case SourceGitHubReviews:
			recs, err := src.ReadGitHubReviews(e.Scope, mo)
			if skipMissing(err) {
				st.Skipped++
				continue
			}
			if err != nil {
				return st, fmt.Errorf("read reviews %s/%s: %w", e.Scope, mo, err)
			}
			if err := dst.WriteGitHubReviews(e.Scope, mo, recs); err != nil {
				return st, fmt.Errorf("write reviews %s/%s: %w", e.Scope, mo, err)
			}
			st.Reviews += len(recs)
		default:
			logf("  [skip] unknown source %q\n", e.Source)
			continue
		}
		if st.Cells%50 == 0 {
			logf("  ... %d cells migrated\n", st.Cells)
		}
	}

	// Preserve the manifest verbatim so freshness gating (NeedsPull, the two P0
	// coverage-hole fixes) behaves identically on the SQLite substrate.
	if err := dst.SaveManifest(manifest); err != nil {
		return st, fmt.Errorf("save manifest: %w", err)
	}
	return st, nil
}

func skipMissing(err error) bool {
	return err != nil && errors.Is(err, fs.ErrNotExist)
}
