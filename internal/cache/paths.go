package cache

import (
	"fmt"
	"path/filepath"

	"github.com/mathewepstein/velocity/internal/config"
)

// ManifestFile is the manifest filename at the cache root.
const ManifestFile = "manifest.json"

// MetricsFile is the computed-metrics filename at the cache root (written by
// analyze in Phase 6, served by the web UI).
const MetricsFile = "metrics.json"

// Root returns the cache root directory (honors $XDG_DATA_HOME).
// Defined here so other cache files don't import internal/config directly.
func Root() (string, error) {
	return config.DataDir()
}

// ManifestPath returns the absolute path to manifest.json.
func ManifestPath() (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, ManifestFile), nil
}

// MetricsPath returns the absolute path to metrics.json.
func MetricsPath() (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, MetricsFile), nil
}

// SourceDir returns the absolute path to one source's per-scope directory
// tree, e.g. $DATA/jira or $DATA/github-prs. Used by Reset to enumerate the
// trees that should be wiped without touching siblings like ratings.json that
// must survive a reset.
func SourceDir(source Source) (string, error) {
	if source == "" {
		return "", fmt.Errorf("cache: source required")
	}
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, string(source)), nil
}

// AllSources lists every Source the cache knows about. Reset uses it; future
// pullers should append here so a reset still picks them up.
var AllSources = []Source{
	SourceJira,
	SourceGitHubPRs,
	SourceGitHubCommits,
	SourceGitHubReviews,
}

// MonthPath returns the absolute path for a given (source, scope, month).
// Example: (jira, "CD", 2024-01) → $DATA/jira/CD/2024-01.json
//
// Scope sanity-check: scope is a project key or org name and must not contain
// path separators. This is defense in depth — callers already validate, but
// catching it here prevents "../" escapes from ever reaching the filesystem.
func MonthPath(source Source, scope string, m Month) (string, error) {
	if source == "" {
		return "", fmt.Errorf("cache: source required")
	}
	if scope == "" {
		return "", fmt.Errorf("cache: scope required")
	}
	if filepath.Base(scope) != scope {
		return "", fmt.Errorf("cache: invalid scope %q (path separators not allowed)", scope)
	}
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, string(source), scope, m.String()+".json"), nil
}
