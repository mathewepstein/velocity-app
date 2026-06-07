// Package devs walks the local cache to enumerate every distinct GitHub login
// and Jira accountId seen across cached records. `velocity devs discover` uses
// the output to propose [[devs]] entries; analyze layers will use it to group
// records per author when multi-dev rollups consume the same data.
package devs

import (
	"errors"
	"fmt"
	"io/fs"
	"sort"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

// Identities is the result of a Scan — the union of every distinct identifier
// seen in the cache, with bot-pattern matches already filtered out. Sorted for
// deterministic CLI output.
type Identities struct {
	GitHubLogins   []string
	JiraAccountIDs []string
}

// Scan walks every entry recorded in the manifest and aggregates distinct
// GitHub logins (from PR + commit authors) and Jira accountIds (from issue
// assignee + reporter). Anyone matching the effective bot-exclude list is
// dropped. Missing on-disk files are skipped silently — manifest may reference
// a month whose JSON has been moved away.
func Scan(profile config.Profile, store cache.Store) (Identities, error) {
	man, err := store.LoadManifest()
	if err != nil {
		return Identities{}, fmt.Errorf("load manifest: %w", err)
	}

	bots := profile.Scoring.EffectiveExcludes()
	gh := map[string]struct{}{}
	jr := map[string]struct{}{}

	for _, entry := range man.Entries {
		month, err := cache.ParseMonth(entry.Month)
		if err != nil {
			// Skip malformed manifest entries rather than abort — discover should
			// give the user as much signal as possible from a healthy cache.
			continue
		}
		switch entry.Source {
		case cache.SourceGitHubPRs:
			prs, err := store.ReadGitHubPRs(entry.Scope, month)
			if isMissing(err) {
				continue
			}
			if err != nil {
				return Identities{}, err
			}
			for _, pr := range prs {
				addIdentity(gh, pr.Author, bots)
			}
		case cache.SourceGitHubCommits:
			cms, err := store.ReadGitHubCommits(entry.Scope, month)
			if isMissing(err) {
				continue
			}
			if err != nil {
				return Identities{}, err
			}
			for _, c := range cms {
				addIdentity(gh, c.Author, bots)
			}
		case cache.SourceGitHubReviews:
			revs, err := store.ReadGitHubReviews(entry.Scope, month)
			if isMissing(err) {
				continue
			}
			if err != nil {
				return Identities{}, err
			}
			for _, r := range revs {
				addIdentity(gh, r.Reviewer, bots)
			}
		case cache.SourceJira:
			issues, err := store.ReadJiraIssues(entry.Scope, month)
			if isMissing(err) {
				continue
			}
			if err != nil {
				return Identities{}, err
			}
			for _, iss := range issues {
				addIdentity(jr, iss.Assignee, bots)
				addIdentity(jr, iss.Reporter, bots)
			}
		}
	}

	return Identities{
		GitHubLogins:   sortedKeys(gh),
		JiraAccountIDs: sortedKeys(jr),
	}, nil
}

// MappedSets returns two lookup sets covering everyone already in profile.Devs:
// gh-logins on the GitHub side (union of every entry's GitHubLogins) and
// jira-accountIds on the Jira side. Discover uses these to suppress entries
// the user has already mapped.
func MappedSets(profile config.Profile) (ghMapped, jrMapped map[string]struct{}) {
	ghMapped = map[string]struct{}{}
	jrMapped = map[string]struct{}{}
	for _, d := range profile.Devs {
		for _, login := range d.AllGitHubLogins() {
			ghMapped[login] = struct{}{}
		}
		if d.JiraAccountID != "" {
			jrMapped[d.JiraAccountID] = struct{}{}
		}
	}
	return ghMapped, jrMapped
}

// Unmapped returns the subset of identities not yet covered by profile.Devs.
func (i Identities) Unmapped(profile config.Profile) Identities {
	gh, jr := MappedSets(profile)
	out := Identities{}
	for _, login := range i.GitHubLogins {
		if _, ok := gh[login]; !ok {
			out.GitHubLogins = append(out.GitHubLogins, login)
		}
	}
	for _, id := range i.JiraAccountIDs {
		if _, ok := jr[id]; !ok {
			out.JiraAccountIDs = append(out.JiraAccountIDs, id)
		}
	}
	return out
}

func addIdentity(seen map[string]struct{}, raw string, bots []string) {
	if raw == "" {
		return
	}
	if config.MatchesBotPattern(raw, bots) {
		return
	}
	seen[raw] = struct{}{}
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func isMissing(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}
