package initflow

import (
	"fmt"
	"io"
	"os"
)

// fieldID is an internal enum naming each init field that can be filled from
// an env var. Kept unexported — callers refer to these by the helper methods.
type fieldID string

const (
	fieldJiraEmail   fieldID = "jira.email"
	fieldJiraToken   fieldID = "jira.token"
	fieldGithubToken fieldID = "github.token"
	fieldGithubOrgs  fieldID = "github.orgs"
)

// envSource pairs the env var name with the value pulled from it.
type envSource struct {
	VarName string
	Value   string
	Secret  bool // when true, banner prints a masked value instead of the raw one
}

// envDetector owns the mapping from init fields to env vars and the
// human-readable metadata (var name, secret-ness).
type envDetector struct {
	picks map[fieldID]envSource
}

// detectEnv reads the environment for known vars. disabled → returns an empty
// detector (every HasField returns false).
func detectEnv(disabled bool) *envDetector {
	d := &envDetector{picks: map[fieldID]envSource{}}
	if disabled {
		return d
	}

	if v := os.Getenv("ATLASSIAN_EMAIL"); v != "" {
		d.picks[fieldJiraEmail] = envSource{VarName: "ATLASSIAN_EMAIL", Value: v}
	}
	if v := os.Getenv("ATLASSIAN_API_TOKEN"); v != "" {
		d.picks[fieldJiraToken] = envSource{VarName: "ATLASSIAN_API_TOKEN", Value: v, Secret: true}
	}
	// GH_TOKEN takes priority over GITHUB_TOKEN because that's what the gh CLI
	// canonicalizes to; but we accept either.
	if v := os.Getenv("GH_TOKEN"); v != "" {
		d.picks[fieldGithubToken] = envSource{VarName: "GH_TOKEN", Value: v, Secret: true}
	} else if v := os.Getenv("GITHUB_TOKEN"); v != "" {
		d.picks[fieldGithubToken] = envSource{VarName: "GITHUB_TOKEN", Value: v, Secret: true}
	}
	if v := os.Getenv("GITHUB_ORG"); v != "" {
		d.picks[fieldGithubOrgs] = envSource{VarName: "GITHUB_ORG", Value: v}
	}
	return d
}

// Has reports whether an env value was detected for id.
func (d *envDetector) Has(id fieldID) bool {
	if d == nil {
		return false
	}
	_, ok := d.picks[id]
	return ok
}

// Get returns the env value and metadata for id. Callers must Has-check first;
// if id is missing Get returns an empty envSource (safe but obviously wrong).
func (d *envDetector) Get(id fieldID) envSource {
	if d == nil {
		return envSource{}
	}
	return d.picks[id]
}

// PrintBanner emits the upfront summary of detected env vars. No-op if nothing
// detected — silence is the best signal of "no magic happening".
func (d *envDetector) PrintBanner(w io.Writer) {
	if d == nil || len(d.picks) == 0 {
		return
	}
	fmt.Fprintln(w, "Detected environment variables (used instead of prompting):")

	// Stable order so the banner doesn't shuffle between runs.
	order := []fieldID{fieldJiraEmail, fieldJiraToken, fieldGithubToken, fieldGithubOrgs}
	for _, id := range order {
		p, ok := d.picks[id]
		if !ok {
			continue
		}
		display := p.Value
		if p.Secret {
			display = maskToken(p.Value)
		}
		fmt.Fprintf(w, "  $%-20s → %s\n", p.VarName, display)
	}
	fmt.Fprintln(w, "Re-run with --no-env to ignore these and prompt for everything.")
	fmt.Fprintln(w)
}

// Announce prints a single "<label>: <value>  (from $VAR)" line for fields
// that came from the environment. Used in place of the regular prompt.
func (d *envDetector) Announce(w io.Writer, id fieldID, label string) {
	p := d.Get(id)
	display := p.Value
	if p.Secret {
		display = maskToken(p.Value)
	}
	fmt.Fprintf(w, "%s: %s  (from $%s)\n", label, display, p.VarName)
}

// maskToken returns "••••<last4>" for any token ≥ 4 chars, otherwise "••••".
// Last 4 is enough to verify a user's using the token they expected without
// leaking the whole secret to the terminal (or to any screenshot they share).
func maskToken(s string) string {
	if len(s) <= 4 {
		return "••••"
	}
	return "••••" + s[len(s)-4:]
}
