package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ReadMonth loads the cached records for (source, scope, month) as []T.
// Returns os.ErrNotExist (wrapped) if the file is missing. Callers that treat
// missing as "empty" should errors.Is-check.
//
// Go generics note: T is the record type (JiraIssue / GitHubPR / …). The
// cache package stays agnostic — it reads/writes any JSON-shaped slice
// without needing to know what each record looks like.
func ReadMonth[T any](source Source, scope string, m Month) ([]T, error) {
	path, err := MonthPath(source, scope, m)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("cache miss %s/%s/%s: %w", source, scope, m, err)
		}
		return nil, fmt.Errorf("read cache %s: %w", path, err)
	}
	var out []T
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse cache %s: %w", path, err)
	}
	return out, nil
}

// WriteMonth writes records to (source, scope, month) atomically: we write to
// a sibling tmp file then rename. This prevents a crash mid-write from
// leaving a half-written JSON file that would poison every future read.
//
// An empty slice writes "[]" rather than null — easier to diff, and less
// confusing when hand-reading files.
func WriteMonth[T any](source Source, scope string, m Month, records []T) error {
	path, err := MonthPath(source, scope, m)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}

	if records == nil {
		records = []T{}
	}
	// MarshalIndent for hand-readability — the cache exists on a single
	// user's machine; the disk cost is trivial and `jq` on raw files Just Works.
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return fmt.Errorf("encode cache: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		// Rename failures leave the old file intact. Best-effort cleanup.
		_ = os.Remove(tmp)
		return fmt.Errorf("commit cache file %s: %w", path, err)
	}
	return nil
}
