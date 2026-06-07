// Package doctor inspects the velocity installation (config, keychain, cache)
// and reports a structured list of findings. The CLI layer renders them; the
// package itself prints nothing so tests can assert on the result.
package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
	"github.com/mathewepstein/velocity/internal/secrets"
)

// Status is one of three levels. Warn is "noteworthy but not broken" — e.g.,
// an empty cache on a fresh install. Fail means something is actionable.
type Status string

const (
	StatusOK   Status = "ok"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
)

// Check is one finding. Details is optional hint text printed under Message.
type Check struct {
	Name    string
	Status  Status
	Message string
	Details string
}

// Summary aggregates the result of all checks.
type Summary struct {
	Checks []Check
	OK     int
	Warn   int
	Fail   int
}

// Add appends a check and bumps the corresponding counter.
func (s *Summary) Add(c Check) {
	s.Checks = append(s.Checks, c)
	switch c.Status {
	case StatusOK:
		s.OK++
	case StatusWarn:
		s.Warn++
	case StatusFail:
		s.Fail++
	}
}

// Run executes every check in sequence. Never returns a non-nil error —
// failure states surface as Check entries so one broken check doesn't hide
// the rest. now is injected for deterministic tests.
func Run(now time.Time) *Summary {
	s := &Summary{}

	cfgPath, err := config.Path()
	if err != nil {
		s.Add(Check{Name: "Config path", Status: StatusFail, Message: err.Error()})
		return s
	}
	cfg, profile, ok := checkConfig(s, cfgPath)
	if !ok {
		// Without a config there's nothing left to check.
		return s
	}
	_ = cfg // cfg not needed beyond the Check; profile carries the usable shape.

	checkIdentityFields(s, profile)
	checkJiraFields(s, profile)
	checkKeychain(s)
	checkCache(s, profile, now)
	checkMetricsFile(s, now)

	return s
}

func checkConfig(s *Summary, cfgPath string) (*config.Config, config.Profile, bool) {
	cfg, err := config.LoadFrom(cfgPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s.Add(Check{
				Name: "Config", Status: StatusFail,
				Message: fmt.Sprintf("no config at %s", cfgPath),
				Details: "Run `velocity init` to create one.",
			})
		} else {
			s.Add(Check{Name: "Config", Status: StatusFail, Message: err.Error()})
		}
		return nil, config.Profile{}, false
	}
	profile := cfg.ActiveProfile()
	s.Add(Check{
		Name: "Config", Status: StatusOK,
		Message: fmt.Sprintf("loaded from %s", cfgPath),
	})
	return cfg, profile, true
}

func checkIdentityFields(s *Summary, p config.Profile) {
	missing := []string{}
	if p.Jira.BaseURL == "" {
		missing = append(missing, "jira.base_url")
	}
	if p.Jira.Email == "" {
		missing = append(missing, "jira.email")
	}
	if len(p.Jira.Projects) == 0 {
		missing = append(missing, "jira.projects")
	}
	if p.GitHub.Username == "" {
		missing = append(missing, "github.username")
	}
	if len(p.GitHub.Orgs) == 0 {
		missing = append(missing, "github.orgs")
	}
	if len(missing) > 0 {
		s.Add(Check{
			Name:    "Identity fields",
			Status:  StatusFail,
			Message: fmt.Sprintf("missing: %s", strings.Join(missing, ", ")),
			Details: "Re-run `velocity init` or edit config.toml.",
		})
		return
	}
	s.Add(Check{
		Name:    "Identity fields",
		Status:  StatusOK,
		Message: fmt.Sprintf("jira=%s, projects=%v, github=%s, orgs=%v", p.Jira.Email, p.Jira.Projects, p.GitHub.Username, p.GitHub.Orgs),
	})
}

func checkJiraFields(s *Summary, p config.Profile) {
	if p.Jira.Fields.EpicLink == "" {
		s.Add(Check{
			Name: "Jira custom fields", Status: StatusWarn,
			Message: "epic_link not resolved",
			Details: "Surge detection needs the epic link field. Re-run `velocity init` to auto-discover.",
		})
		return
	}
	sp := p.Jira.Fields.StoryPoints
	if sp == "" {
		sp = "(none)"
	}
	s.Add(Check{
		Name: "Jira custom fields", Status: StatusOK,
		Message: fmt.Sprintf("story_points=%s, epic_link=%s", sp, p.Jira.Fields.EpicLink),
	})
}

func checkKeychain(s *Summary) {
	for _, svc := range secrets.KnownServices {
		has, err := secrets.Has(config.DefaultProfile, svc)
		if err != nil {
			s.Add(Check{
				Name: fmt.Sprintf("Keychain: %s", svc), Status: StatusFail,
				Message: err.Error(),
			})
			continue
		}
		if !has {
			s.Add(Check{
				Name: fmt.Sprintf("Keychain: %s", svc), Status: StatusFail,
				Message: "no token stored",
				Details: fmt.Sprintf("Run `velocity auth set %s`.", svc),
			})
			continue
		}
		s.Add(Check{
			Name: fmt.Sprintf("Keychain: %s", svc), Status: StatusOK,
			Message: fmt.Sprintf("token present at %s", secrets.Key(config.DefaultProfile, svc)),
		})
	}
}

func checkCache(s *Summary, p config.Profile, now time.Time) {
	root, err := cache.Root()
	if err != nil {
		s.Add(Check{Name: "Cache root", Status: StatusFail, Message: err.Error()})
		return
	}
	info, err := os.Stat(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s.Add(Check{
				Name: "Cache root", Status: StatusWarn,
				Message: fmt.Sprintf("does not exist at %s", root),
				Details: "Run `velocity refresh` to populate.",
			})
		} else {
			s.Add(Check{Name: "Cache root", Status: StatusFail, Message: err.Error()})
		}
		return
	}
	if !info.IsDir() {
		s.Add(Check{Name: "Cache root", Status: StatusFail, Message: fmt.Sprintf("%s is not a directory", root)})
		return
	}
	size, err := dirSize(root)
	if err != nil {
		s.Add(Check{Name: "Cache root", Status: StatusWarn, Message: err.Error()})
	} else {
		s.Add(Check{
			Name: "Cache root", Status: StatusOK,
			Message: fmt.Sprintf("%s (%s)", root, humanSize(size)),
		})
	}

	manifest, err := cache.LoadManifest()
	if err != nil {
		s.Add(Check{Name: "Manifest", Status: StatusFail, Message: err.Error()})
		return
	}
	if len(manifest.Entries) == 0 {
		s.Add(Check{
			Name: "Manifest", Status: StatusWarn,
			Message: "empty",
			Details: "Run `velocity refresh` to pull data.",
		})
		return
	}
	s.Add(Check{
		Name: "Manifest", Status: StatusOK,
		Message: fmt.Sprintf("%d entries", len(manifest.Entries)),
	})

	checkManifestedFilesExist(s, manifest)
	checkOrphanFiles(s, manifest, p)
}

// checkManifestedFilesExist verifies every cell in the manifest has its JSON
// file on disk. A missing file usually means somebody `rm`'d a cache file
// but not the manifest; `velocity refresh --force` is the fix.
func checkManifestedFilesExist(s *Summary, manifest *cache.Manifest) {
	missing := []string{}
	unreadable := []string{}
	for _, e := range manifest.Entries {
		m, err := cache.ParseMonth(e.Month)
		if err != nil {
			continue
		}
		path, err := cache.MonthPath(e.Source, e.Scope, m)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				missing = append(missing, fmt.Sprintf("%s/%s/%s", e.Source, e.Scope, e.Month))
			} else {
				unreadable = append(unreadable, path)
			}
			continue
		}
		// Sanity-check JSON parseability — a truncated file would corrupt the
		// analyzer silently otherwise.
		var probe []json.RawMessage
		if err := json.Unmarshal(data, &probe); err != nil {
			unreadable = append(unreadable, path)
		}
	}
	if len(missing) > 0 {
		s.Add(Check{
			Name: "Manifested files present", Status: StatusFail,
			Message: fmt.Sprintf("%d file(s) missing", len(missing)),
			Details: summarizeList(missing, 5) + "\n    Fix: `velocity refresh --force`",
		})
	} else {
		s.Add(Check{
			Name: "Manifested files present", Status: StatusOK,
			Message: "all files on disk",
		})
	}
	if len(unreadable) > 0 {
		s.Add(Check{
			Name: "Cache file integrity", Status: StatusFail,
			Message: fmt.Sprintf("%d file(s) corrupt or unparseable", len(unreadable)),
			Details: summarizeList(unreadable, 5) + "\n    Fix: delete the files and `velocity refresh --force`",
		})
	} else {
		s.Add(Check{
			Name: "Cache file integrity", Status: StatusOK,
			Message: "all files parse as JSON arrays",
		})
	}
}

// checkOrphanFiles walks the cache dirs and reports files that aren't in the
// manifest. These usually accumulate from aborted pulls or manual tinkering;
// they aren't harmful but they waste space and can confuse later cleanup.
func checkOrphanFiles(s *Summary, manifest *cache.Manifest, p config.Profile) {
	inManifest := map[string]bool{}
	for _, e := range manifest.Entries {
		m, err := cache.ParseMonth(e.Month)
		if err != nil {
			continue
		}
		path, err := cache.MonthPath(e.Source, e.Scope, m)
		if err != nil {
			continue
		}
		inManifest[path] = true
	}

	root, err := cache.Root()
	if err != nil {
		return
	}
	var orphans []string
	sources := []cache.Source{cache.SourceJira, cache.SourceGitHubPRs, cache.SourceGitHubCommits}
	for _, src := range sources {
		dir := filepath.Join(root, string(src))
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".json") {
				return nil
			}
			if strings.HasSuffix(path, ".tmp") {
				// Leftover from a crashed write — worth flagging.
				orphans = append(orphans, path+" (stale tmp)")
				return nil
			}
			if !inManifest[path] {
				orphans = append(orphans, path)
			}
			return nil
		})
	}

	if len(orphans) > 0 {
		s.Add(Check{
			Name: "Orphan cache files", Status: StatusWarn,
			Message: fmt.Sprintf("%d file(s) not in manifest", len(orphans)),
			Details: summarizeList(orphans, 5),
		})
	} else {
		s.Add(Check{
			Name: "Orphan cache files", Status: StatusOK,
			Message: "none",
		})
	}
}

func checkMetricsFile(s *Summary, now time.Time) {
	root, err := cache.Root()
	if err != nil {
		return
	}
	path := filepath.Join(root, cache.MetricsFile)
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s.Add(Check{
				Name: "metrics.json", Status: StatusWarn,
				Message: "not generated yet",
				Details: "Run `velocity analyze` to create it.",
			})
			return
		}
		s.Add(Check{Name: "metrics.json", Status: StatusFail, Message: err.Error()})
		return
	}
	age := now.Sub(info.ModTime())
	ageStr := humanDuration(age)
	if age > 24*time.Hour {
		s.Add(Check{
			Name: "metrics.json", Status: StatusWarn,
			Message: fmt.Sprintf("%s (%s old)", humanSize(info.Size()), ageStr),
			Details: "Consider re-running `velocity analyze`.",
		})
		return
	}
	s.Add(Check{
		Name: "metrics.json", Status: StatusOK,
		Message: fmt.Sprintf("%s (generated %s ago)", humanSize(info.Size()), ageStr),
	})
}

// dirSize sums the size of every regular file under root. Errors are
// returned to the caller — a failed walk is diagnostic info.
func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func humanSize(n int64) string {
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
	)
	switch {
	case n >= gb:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(gb))
	case n >= mb:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(mb))
	case n >= kb:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(kb))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func summarizeList(items []string, max int) string {
	if len(items) <= max {
		return "    - " + strings.Join(items, "\n    - ")
	}
	shown := items[:max]
	return "    - " + strings.Join(shown, "\n    - ") + fmt.Sprintf("\n    … and %d more", len(items)-max)
}
