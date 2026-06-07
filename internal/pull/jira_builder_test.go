package pull

import (
	"strings"
	"testing"
	"time"
)

func TestBuildJiraJQL(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	jql := BuildJiraJQL("CD", start, end)

	wantContains := []string{
		`project = CD`,
		`updated >= "2024-01-01 00:00"`,
		`updated < "2024-02-01 00:00"`,
	}
	for _, want := range wantContains {
		if !strings.Contains(jql, want) {
			t.Errorf("missing %q in JQL:\n%s", want, jql)
		}
	}

	// Author filtering must NOT appear — per-author rollups happen downstream.
	for _, forbidden := range []string{"assignee", "reporter", "currentUser"} {
		if strings.Contains(jql, forbidden) {
			t.Errorf("JQL still contains user filter %q:\n%s", forbidden, jql)
		}
	}
}
