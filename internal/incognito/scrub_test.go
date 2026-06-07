package incognito

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mathewepstein/velocity/internal/analyze"
	"github.com/mathewepstein/velocity/internal/config"
)

func TestScrubResultRemovesRealNamesAndLogins(t *testing.T) {
	m := newEmptyMapping()
	real := sampleResult()
	scrubbed, mutated := ScrubResult(real, m)
	if !mutated {
		t.Errorf("first scrub should report mutated = true (minted new entries)")
	}

	// Real names must not appear anywhere in the scrubbed JSON.
	out, err := json.Marshal(scrubbed)
	if err != nil {
		t.Fatalf("marshal scrubbed: %v", err)
	}
	body := string(out)

	for _, secret := range []string{
		"Mathew Epstein", "Christian Corey", "Karl Weckwerth",
		"mathewepstein", "christiancorey", "karlweckwerth",
		"5dee8b7006eaff0e54523a77",
		"CD-100", "CD-200",
		"Auth migration", "Compliance project",
	} {
		if strings.Contains(body, secret) {
			t.Errorf("scrubbed response leaks real identifier %q", secret)
		}
	}

	// Scrubbed names must include the aliases.
	for _, d := range scrubbed.Devs {
		if d.Dev.DisplayName == "unknown" {
			continue
		}
		if d.Dev.DisplayName == "" {
			t.Errorf("dev display_name empty after scrub: %+v", d)
		}
		if len(d.Dev.GitHubLogins) != 1 {
			t.Errorf("scrubbed dev should have exactly one anon login, got %d", len(d.Dev.GitHubLogins))
		}
		if d.Dev.JiraAccountID != "" {
			t.Errorf("jira_account_id should be blanked, got %q", d.Dev.JiraAccountID)
		}
	}
}

func TestScrubResultPreservesUnknownBucket(t *testing.T) {
	m := newEmptyMapping()
	real := &analyze.Result{
		Devs: []analyze.DevWindowMetrics{
			{Dev: config.DevIdentity{DisplayName: "unknown"}},
			{Dev: config.DevIdentity{DisplayName: "Mathew Epstein", GitHubLogins: []string{"mathewepstein"}}},
		},
	}
	scrubbed, _ := ScrubResult(real, m)
	if scrubbed.Devs[0].Dev.DisplayName != "unknown" {
		t.Errorf("unknown bucket got scrubbed: %q", scrubbed.Devs[0].Dev.DisplayName)
	}
	if scrubbed.Devs[1].Dev.DisplayName == "Mathew Epstein" {
		t.Errorf("Mathew should have been anonymized")
	}
}

func TestScrubResultIsIdempotent(t *testing.T) {
	// Calling Scrub twice with the same mapping should produce the same
	// scrubbed output the second time (no further mutation, no name drift).
	m := newEmptyMapping()
	real := sampleResult()
	first, _ := ScrubResult(real, m)
	// We re-scrub the ORIGINAL (not first), simulating a second incoming
	// HTTP request that re-reads metrics.json from disk.
	second, mutated := ScrubResult(real, m)
	if mutated {
		t.Errorf("second scrub should not mutate the mapping")
	}
	if first.Devs[0].Dev.DisplayName != second.Devs[0].Dev.DisplayName {
		t.Errorf("alias drifted across scrubs: %q vs %q",
			first.Devs[0].Dev.DisplayName, second.Devs[0].Dev.DisplayName)
	}
}

func TestScrubResultDoesNotMutateInput(t *testing.T) {
	m := newEmptyMapping()
	real := sampleResult()
	originalFirstDev := real.Devs[0].Dev.DisplayName
	originalProjects := real.Devs[0].Projects[0].EpicKey
	_, _ = ScrubResult(real, m)
	if real.Devs[0].Dev.DisplayName != originalFirstDev {
		t.Errorf("input was mutated: dev name = %q, want %q", real.Devs[0].Dev.DisplayName, originalFirstDev)
	}
	if real.Devs[0].Projects[0].EpicKey != originalProjects {
		t.Errorf("input was mutated: epic key = %q, want %q", real.Devs[0].Projects[0].EpicKey, originalProjects)
	}
}

func TestScrubResultScrubsTopLevelProjects(t *testing.T) {
	m := newEmptyMapping()
	real := &analyze.Result{
		Projects: []analyze.Project{
			{EpicKey: "CD-100", Summary: "Sensitive Project Title"},
		},
	}
	scrubbed, _ := ScrubResult(real, m)
	if scrubbed.Projects[0].EpicKey == "CD-100" {
		t.Errorf("top-level epic key not scrubbed")
	}
	if scrubbed.Projects[0].Summary != "" {
		t.Errorf("top-level summary not blanked: %q", scrubbed.Projects[0].Summary)
	}
}

// sampleResult returns a small but representative analyze.Result for tests.
// Has the shapes the scrubber touches (devs, devs[].projects, top-level
// projects) without dragging in full fixture machinery.
func sampleResult() *analyze.Result {
	return &analyze.Result{
		Devs: []analyze.DevWindowMetrics{
			{
				Dev: config.DevIdentity{
					DisplayName:   "Mathew Epstein",
					GitHubLogins:  []string{"mathewepstein"},
					JiraAccountID: "5dee8b7006eaff0e54523a77",
				},
				PrimaryLogin: "mathewepstein",
				Projects: []analyze.ProjectShare{
					{EpicKey: "CD-100", Summary: "Auth migration"},
					{EpicKey: "CD-200", Summary: "Compliance project"},
				},
			},
			{
				Dev: config.DevIdentity{
					DisplayName:  "Christian Corey",
					GitHubLogins: []string{"christiancorey"},
				},
				PrimaryLogin: "christiancorey",
				Projects: []analyze.ProjectShare{
					{EpicKey: "CD-200", Summary: "Compliance project"},
				},
			},
			{
				Dev: config.DevIdentity{
					DisplayName:  "Karl Weckwerth",
					GitHubLogins: []string{"karlweckwerth"},
				},
				PrimaryLogin: "karlweckwerth",
			},
		},
		Projects: []analyze.Project{
			{EpicKey: "CD-100", Summary: "Auth migration"},
			{EpicKey: "CD-200", Summary: "Compliance project"},
		},
	}
}
