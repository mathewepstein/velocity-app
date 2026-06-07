package cache

import (
	"fmt"
	"io"
	"io/fs"
	"os"
)

// Reset wipes every cached source tree plus the manifest and the computed
// metrics file. Anything else under the data root (e.g. a future ratings.json
// that records Elo history) survives — that's by design.
//
// Returns a list of removed paths so callers can show the user what changed.
// Missing paths are not errors; reset is idempotent.
func Reset(out io.Writer) ([]string, error) {
	var removed []string

	mp, err := ManifestPath()
	if err != nil {
		return nil, err
	}
	metrics, err := MetricsPath()
	if err != nil {
		return nil, err
	}

	targets := []string{mp, metrics}
	for _, s := range AllSources {
		dir, err := SourceDir(s)
		if err != nil {
			return nil, err
		}
		targets = append(targets, dir)
	}

	for _, path := range targets {
		_, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			// fs.ErrNotExist is what some platforms return through err.
			if isMissing(err) {
				continue
			}
			return nil, fmt.Errorf("stat %s: %w", path, err)
		}
		if err := os.RemoveAll(path); err != nil {
			return nil, fmt.Errorf("remove %s: %w", path, err)
		}
		removed = append(removed, path)
		if out != nil {
			fmt.Fprintf(out, "  removed %s\n", path)
		}
	}
	return removed, nil
}

func isMissing(err error) bool {
	if err == nil {
		return false
	}
	if e, ok := err.(*fs.PathError); ok {
		return os.IsNotExist(e.Err)
	}
	return false
}
