package main

import (
	"reflect"
	"sort"
	"testing"

	"github.com/mathewepstein/velocity/internal/config"
)

func TestMergePendingExtendsExistingByJiraAccountID(t *testing.T) {
	existing := []config.DevIdentity{
		{GitHubLogins: []string{"mathewepstein"}, JiraAccountID: "acct-mat", DisplayName: "Mathew Epstein"},
		{GitHubLogins: []string{"bsmith"}, JiraAccountID: "acct-bob", DisplayName: "Bob Smith"},
	}
	pending := []config.DevIdentity{
		// Attach-to-existing: same jiraAccountID as Mathew, adds a fallback identifier.
		{GitHubLogins: []string{"Mathew Epstein"}, JiraAccountID: "acct-mat", DisplayName: "Mathew Epstein"},
		// New entry: no match in existing.
		{GitHubLogins: []string{"alice"}, JiraAccountID: "acct-alice", DisplayName: "Alice"},
	}

	merged, ext, app := mergePending(existing, pending)
	if ext != 1 || app != 1 {
		t.Errorf("extended/appended = %d/%d, want 1/1", ext, app)
	}
	if len(merged) != 3 {
		t.Fatalf("merged len = %d, want 3", len(merged))
	}
	// Mathew should now hold both identifiers.
	wantLogins := []string{"Mathew Epstein", "mathewepstein"}
	got := append([]string(nil), merged[0].AllGitHubLogins()...)
	sort.Strings(got)
	sort.Strings(wantLogins)
	if !reflect.DeepEqual(got, wantLogins) {
		t.Errorf("Mathew's GitHubLogins = %v, want %v", got, wantLogins)
	}
	if merged[0].DisplayName != "Mathew Epstein" {
		t.Errorf("Mathew's DisplayName changed: %q", merged[0].DisplayName)
	}
	// Bob is untouched.
	if !reflect.DeepEqual(merged[1].GitHubLogins, []string{"bsmith"}) {
		t.Errorf("Bob's GitHubLogins changed: %v", merged[1].GitHubLogins)
	}
	// Alice is appended verbatim.
	if !reflect.DeepEqual(merged[2].GitHubLogins, []string{"alice"}) {
		t.Errorf("Alice's GitHubLogins = %v, want [alice]", merged[2].GitHubLogins)
	}
}

func TestMergePendingDeduplicatesGitHubLogins(t *testing.T) {
	// User accidentally re-runs discover and confirms the same pairing twice.
	// The merged entry must not gain a duplicate identifier.
	existing := []config.DevIdentity{
		{GitHubLogins: []string{"alice"}, JiraAccountID: "acct-alice", DisplayName: "Alice"},
	}
	pending := []config.DevIdentity{
		{GitHubLogins: []string{"alice"}, JiraAccountID: "acct-alice", DisplayName: "Alice"},
	}
	merged, ext, app := mergePending(existing, pending)
	if ext != 1 || app != 0 {
		t.Errorf("extended/appended = %d/%d, want 1/0", ext, app)
	}
	if !reflect.DeepEqual(merged[0].GitHubLogins, []string{"alice"}) {
		t.Errorf("Alice's GitHubLogins = %v, want [alice] (no dupe)", merged[0].GitHubLogins)
	}
}

func TestMergePendingHandlesLegacySingularField(t *testing.T) {
	// Existing entry stored under the legacy singular field (e.g. a config
	// loaded mid-migration). Attach-to-existing must still find it AND must
	// convert it to plural form so the saved output uses the new schema.
	existing := []config.DevIdentity{
		{GitHubLogin: "mathewepstein", JiraAccountID: "acct-mat", DisplayName: "Mathew Epstein"},
	}
	pending := []config.DevIdentity{
		{GitHubLogins: []string{"Mathew Epstein"}, JiraAccountID: "acct-mat"},
	}
	merged, _, _ := mergePending(existing, pending)
	if merged[0].GitHubLogin != "" {
		t.Errorf("legacy singular field should be cleared after merge, got %q", merged[0].GitHubLogin)
	}
	want := []string{"Mathew Epstein", "mathewepstein"}
	got := append([]string(nil), merged[0].GitHubLogins...)
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("merged GitHubLogins = %v, want %v", got, want)
	}
}

func TestMergePendingAppendsJiraOnlyEntries(t *testing.T) {
	// Pending entry has no jira_account_id — append-as-new regardless.
	existing := []config.DevIdentity{
		{GitHubLogins: []string{"alice"}, JiraAccountID: "acct-alice", DisplayName: "Alice"},
	}
	pending := []config.DevIdentity{
		{GitHubLogins: []string{"bob"}, DisplayName: "bob"},
	}
	merged, ext, app := mergePending(existing, pending)
	if ext != 0 || app != 1 {
		t.Errorf("extended/appended = %d/%d, want 0/1", ext, app)
	}
	if len(merged) != 2 {
		t.Fatalf("merged len = %d, want 2", len(merged))
	}
}
