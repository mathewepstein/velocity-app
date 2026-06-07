package analyze

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

// Loaded is the flat in-memory view of the cache used by every aggregator.
// Holding all records in memory is fine: even an org-wide 6-year history is
// well under 100 MB, and every aggregator wants full-history sweeps anyway.
type Loaded struct {
	Issues  []cache.JiraIssue
	PRs     []cache.GitHubPR
	Commits []cache.GitHubCommit
	Reviews []cache.GitHubReview
	Months  []cache.Month // inclusive, chronological
}

// LoadAll reads every month in [BackfillStart, currentMonth] across every
// configured project + org from store. Missing cache cells are skipped silently
// — the backfill can have zero-record months (leave, pre-employment gaps).
func LoadAll(p config.Profile, currentMonth cache.Month, store cache.Store) (*Loaded, error) {
	start, err := cache.ParseMonth(p.Window.BackfillStart)
	if err != nil {
		return nil, fmt.Errorf("invalid backfill_start %q: %w", p.Window.BackfillStart, err)
	}
	if currentMonth.Before(start) {
		return nil, fmt.Errorf("current month %s precedes backfill_start %s", currentMonth, start)
	}

	out := &Loaded{Months: cache.MonthsInRange(start, currentMonth)}
	for _, m := range out.Months {
		for _, project := range p.Jira.Projects {
			issues, err := store.ReadJiraIssues(project, m)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				return nil, fmt.Errorf("load jira/%s/%s: %w", project, m, err)
			}
			out.Issues = append(out.Issues, issues...)
		}
		for _, org := range p.GitHub.Orgs {
			prs, err := store.ReadGitHubPRs(org, m)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("load github-prs/%s/%s: %w", org, m, err)
			}
			out.PRs = append(out.PRs, prs...)

			commits, err := store.ReadGitHubCommits(org, m)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("load github-commits/%s/%s: %w", org, m, err)
			}
			out.Commits = append(out.Commits, commits...)

			reviews, err := store.ReadGitHubReviews(org, m)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("load github-reviews/%s/%s: %w", org, m, err)
			}
			out.Reviews = append(out.Reviews, reviews...)
		}
	}
	return out, nil
}
