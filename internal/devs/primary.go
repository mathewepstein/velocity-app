package devs

import (
	"sort"

	"github.com/mathewepstein/velocity/internal/config"
)

// PrimaryLogin picks the canonical GitHub login for d from the activity map,
// which counts PRs + commits + reviews per login across the entire cache.
// The dev's busiest login wins; ties are broken alphabetically so the value
// is deterministic across runs. Returns "" only when the dev has no GitHub
// identifiers at all (Jira-only stubs).
//
// Used to canonicalize /dev/<login> URLs: a single human can present as
// multiple GitHub identifiers in the cache (real login + git-author-name
// fallbacks from commits whose email isn't linked), and the leaderboard
// needs one stable handle per dev.
func PrimaryLogin(d config.DevIdentity, activity map[string]int) string {
	logins := d.AllGitHubLogins()
	if len(logins) == 0 {
		return ""
	}
	if len(logins) == 1 {
		return logins[0]
	}
	best := ""
	bestCount := -1
	// Sort logins alphabetically first so the tie-break is deterministic
	// regardless of the input order coming out of config / cache iteration.
	sorted := append([]string(nil), logins...)
	sort.Strings(sorted)
	for _, l := range sorted {
		c := activity[l]
		if c > bestCount {
			best = l
			bestCount = c
		}
	}
	return best
}
