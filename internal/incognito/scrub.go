package incognito

import (
	"encoding/json"

	"github.com/mathewepstein/velocity/internal/analyze"
	"github.com/mathewepstein/velocity/internal/config"
)

// ScrubResult returns a deep copy of result with every identifying field
// anonymized via m. The caller's analyze.Result is left untouched so the
// server can keep using the real one for downstream lookups.
//
// What gets scrubbed (everything the frontend reads):
//   - dev.display_name → alias display name
//   - dev.github_logins → [alias slug]
//   - dev.jira_account_id → ""
//   - dev.dev.github_login (legacy field) → ""
//   - primary_login → alias slug
//   - projects[].epic_key / summary  (top-level + nested under devs)
//   - devs[].projects[].epic_key / summary
//
// m is mutated as new devs/epics are encountered (EnsureDev/EnsureEpic).
// The caller is responsible for calling m.Save() after ScrubResult returns
// if `bool` is true.
func ScrubResult(result *analyze.Result, m *Mapping) (*analyze.Result, bool) {
	if result == nil {
		return nil, false
	}
	mutated := false

	// Deep-copy via JSON roundtrip. Adequate for this scale (a 31-dev
	// metrics.json is ~4MB) and avoids hand-maintaining a copy function
	// every time analyze.Result grows a field.
	out := cloneViaJSON(result)

	// Build login-to-display-name index from the REAL data so the mapping
	// can resolve URL slugs back to real logins later (server-side /dev/
	// route). Done before any mutation, against the input.
	loginToDisplay := buildLoginIndex(result.Devs)
	m.SetLoginIndex(loginToDisplay)

	// 1. Scrub devs slice.
	for i := range out.Devs {
		d := &out.Devs[i]
		if d.Dev.DisplayName == "" || d.Dev.DisplayName == "unknown" {
			// Leave the synthetic bucket as-is — it's already not identifying
			// and the UI relies on the literal "unknown" string for skip
			// filters.
			continue
		}
		alias, addedDev := m.EnsureDev(d.Dev.DisplayName)
		if addedDev {
			mutated = true
		}
		d.Dev.DisplayName = alias.DisplayName
		d.Dev.GitHubLogins = []string{alias.Slug}
		d.Dev.GitHubLogin = ""
		d.Dev.JiraAccountID = ""
		d.PrimaryLogin = alias.Slug

		for j := range d.Projects {
			ps := &d.Projects[j]
			label, addedEpic := m.EnsureEpic(ps.EpicKey)
			if addedEpic {
				mutated = true
			}
			if label != "" {
				ps.EpicKey = label
			}
			ps.Summary = ""
		}
	}

	// 2. Scrub top-level projects slice. The leaderboard surge panel reads
	// from result.projects directly (separate from devs[].projects).
	for i := range out.Projects {
		p := &out.Projects[i]
		label, addedEpic := m.EnsureEpic(p.EpicKey)
		if addedEpic {
			mutated = true
		}
		if label != "" {
			p.EpicKey = label
		}
		p.Summary = ""
	}

	return out, mutated
}

// buildLoginIndex flattens devs into a real-github-login → real-display-name
// map. Used by the server to resolve the /dev/<slug> URL to a real cohort
// member, then back out into a scrubbed response. Multi-login devs land all
// their logins in the map, all pointing at the same display name.
func buildLoginIndex(devs []analyze.DevWindowMetrics) map[string]string {
	out := map[string]string{}
	for _, d := range devs {
		name := d.Dev.DisplayName
		if name == "" {
			continue
		}
		for _, login := range d.Dev.AllGitHubLogins() {
			if login == "" {
				continue
			}
			out[login] = name
		}
		if d.PrimaryLogin != "" {
			out[d.PrimaryLogin] = name
		}
	}
	return out
}

// cloneViaJSON returns a deep copy of result via JSON marshal/unmarshal.
// Used by ScrubResult so the input isn't mutated. Cheap enough for the
// analyze.Result scale we operate at.
func cloneViaJSON(result *analyze.Result) *analyze.Result {
	data, err := json.Marshal(result)
	if err != nil {
		// JSON marshal of analyze.Result can't fail in practice (the schema
		// is all primitive + slice + map of marshalable types). Returning
		// the input would be incorrect (the caller expects a copy), so we
		// fall back to a shallow assignment — shallow is wrong if used,
		// but the upstream marshal error is itself a programming bug worth
		// surfacing as an empty result rather than a silent leak.
		_ = err
		return &analyze.Result{}
	}
	var out analyze.Result
	if err := json.Unmarshal(data, &out); err != nil {
		return &analyze.Result{}
	}
	return &out
}

// ScrubDevIdentity is a convenience helper for callers that hold one
// DevIdentity outside an analyze.Result (e.g. tests or future endpoints
// that surface raw dev info). Returns the scrubbed view; mutates m if new.
func ScrubDevIdentity(id config.DevIdentity, m *Mapping) (config.DevIdentity, bool) {
	if id.DisplayName == "" || id.DisplayName == "unknown" {
		return id, false
	}
	alias, added := m.EnsureDev(id.DisplayName)
	out := config.DevIdentity{
		DisplayName:   alias.DisplayName,
		GitHubLogins:  []string{alias.Slug},
		JiraAccountID: "",
	}
	return out, added
}
