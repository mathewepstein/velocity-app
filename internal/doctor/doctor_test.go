package doctor

import (
	"strings"
	"testing"
	"time"

	"github.com/mathewepstein/velocity/internal/config"
)

func TestSummary_AddCountsByStatus(t *testing.T) {
	s := &Summary{}
	s.Add(Check{Status: StatusOK})
	s.Add(Check{Status: StatusOK})
	s.Add(Check{Status: StatusWarn})
	s.Add(Check{Status: StatusFail})
	if s.OK != 2 || s.Warn != 1 || s.Fail != 1 {
		t.Errorf("got OK=%d Warn=%d Fail=%d, want 2/1/1", s.OK, s.Warn, s.Fail)
	}
	if len(s.Checks) != 4 {
		t.Errorf("want 4 checks, got %d", len(s.Checks))
	}
}

func TestCheckIdentityFields_MissingSurfaces(t *testing.T) {
	s := &Summary{}
	// Profile with everything blank.
	checkIdentityFields(s, config.Profile{})
	if len(s.Checks) != 1 {
		t.Fatalf("want 1 check, got %d", len(s.Checks))
	}
	c := s.Checks[0]
	if c.Status != StatusFail {
		t.Errorf("status: %s", c.Status)
	}
	// Should name every missing field.
	for _, want := range []string{"jira.base_url", "jira.email", "jira.projects", "github.username", "github.orgs"} {
		if !strings.Contains(c.Message, want) {
			t.Errorf("message missing %q: %s", want, c.Message)
		}
	}
}

func TestCheckIdentityFields_FullProfilePasses(t *testing.T) {
	s := &Summary{}
	checkIdentityFields(s, config.Profile{
		Jira: config.JiraConfig{
			BaseURL: "https://example.atlassian.net", Email: "me@example.com",
			Projects: []string{"PRJ"},
		},
		GitHub: config.GitHubConfig{Username: "me", Orgs: []string{"org"}},
	})
	if s.Checks[0].Status != StatusOK {
		t.Errorf("status: %s (msg=%s)", s.Checks[0].Status, s.Checks[0].Message)
	}
}

func TestCheckJiraFields_EmptyEpicLinkWarns(t *testing.T) {
	s := &Summary{}
	checkJiraFields(s, config.Profile{})
	if s.Checks[0].Status != StatusWarn {
		t.Errorf("want warn for missing epic_link, got %s", s.Checks[0].Status)
	}
}

func TestHumanSize(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1_500_000, "1.4 MB"},
		{2 * 1024 * 1024 * 1024, "2.0 GB"},
	}
	for _, tc := range tests {
		if got := humanSize(tc.n); got != tc.want {
			t.Errorf("humanSize(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5m"},
		{3 * time.Hour, "3h"},
		{50 * time.Hour, "2d"},
	}
	for _, tc := range tests {
		if got := humanDuration(tc.d); got != tc.want {
			t.Errorf("humanDuration(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestSummarizeList_TruncatesPastMax(t *testing.T) {
	items := []string{"a", "b", "c", "d", "e", "f", "g"}
	got := summarizeList(items, 3)
	if !strings.Contains(got, "- a") || !strings.Contains(got, "- b") || !strings.Contains(got, "- c") {
		t.Errorf("first 3 items not shown: %s", got)
	}
	if strings.Contains(got, "- d") {
		t.Errorf("item d should be hidden: %s", got)
	}
	if !strings.Contains(got, "and 4 more") {
		t.Errorf("truncation suffix missing: %s", got)
	}
}

func TestSummarizeList_WithinMaxShowsAll(t *testing.T) {
	items := []string{"a", "b"}
	got := summarizeList(items, 5)
	if !strings.Contains(got, "- a") || !strings.Contains(got, "- b") {
		t.Errorf("got: %s", got)
	}
	if strings.Contains(got, "more") {
		t.Errorf("unexpected truncation suffix: %s", got)
	}
}
