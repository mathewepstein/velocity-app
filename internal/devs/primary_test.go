package devs

import (
	"testing"

	"github.com/mathewepstein/velocity/internal/config"
)

func TestPrimaryLogin_PicksBusiestLogin(t *testing.T) {
	d := config.DevIdentity{GitHubLogins: []string{"alice-author-name", "alice"}}
	activity := map[string]int{
		"alice":             420,
		"alice-author-name": 3,
	}
	if got := PrimaryLogin(d, activity); got != "alice" {
		t.Errorf("PrimaryLogin = %q, want %q (busiest)", got, "alice")
	}
}

func TestPrimaryLogin_TieBreaksAlphabetically(t *testing.T) {
	d := config.DevIdentity{GitHubLogins: []string{"zeta", "alpha", "mike"}}
	activity := map[string]int{"alpha": 10, "mike": 10, "zeta": 10}
	if got := PrimaryLogin(d, activity); got != "alpha" {
		t.Errorf("PrimaryLogin = %q, want %q (alpha-sort tie-break)", got, "alpha")
	}
}

func TestPrimaryLogin_NoActivityFallsBackToFirstAlpha(t *testing.T) {
	// A dev with logins but zero recorded activity still needs a stable
	// canonical handle — alphabetical first wins.
	d := config.DevIdentity{GitHubLogins: []string{"zeta", "alpha"}}
	if got := PrimaryLogin(d, map[string]int{}); got != "alpha" {
		t.Errorf("PrimaryLogin = %q, want %q (alpha fallback)", got, "alpha")
	}
}

func TestPrimaryLogin_SingleLoginShortCircuits(t *testing.T) {
	d := config.DevIdentity{GitHubLogins: []string{"solo"}}
	if got := PrimaryLogin(d, nil); got != "solo" {
		t.Errorf("PrimaryLogin = %q, want %q (only login)", got, "solo")
	}
}

func TestPrimaryLogin_NoGitHubLoginsReturnsEmpty(t *testing.T) {
	d := config.DevIdentity{JiraAccountID: "jira-only"}
	if got := PrimaryLogin(d, nil); got != "" {
		t.Errorf("PrimaryLogin = %q, want empty (Jira-only stub)", got)
	}
}

func TestPrimaryLogin_LegacySingularField(t *testing.T) {
	// In-memory DevIdentity that bypassed Load's migration still resolves
	// via AllGitHubLogins() falling back to the legacy GitHubLogin field.
	d := config.DevIdentity{GitHubLogin: "legacy-handle"}
	if got := PrimaryLogin(d, map[string]int{"legacy-handle": 9}); got != "legacy-handle" {
		t.Errorf("PrimaryLogin = %q, want %q (legacy singular field)", got, "legacy-handle")
	}
}
