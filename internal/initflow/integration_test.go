//go:build integration

package initflow

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// These tests hit real APIs when run with:
//   go test -tags=integration -run=TestLive_ -v ./internal/initflow
// and the env vars ATLASSIAN_EMAIL / ATLASSIAN_API_TOKEN / GH_TOKEN / GITHUB_ORG.
// They are build-tagged so `go test ./...` skips them by default.

func TestLive_JiraAuth(t *testing.T) {
	email := os.Getenv("ATLASSIAN_EMAIL")
	token := os.Getenv("ATLASSIAN_API_TOKEN")
	if email == "" || token == "" {
		t.Skip("ATLASSIAN_EMAIL / ATLASSIAN_API_TOKEN not set")
	}
	baseURL := "https://consumerdirect.atlassian.net"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c := newJiraClient(baseURL, email, token)
	m, err := c.VerifyAuth(ctx)
	if err != nil {
		t.Fatalf("VerifyAuth: %v", err)
	}
	if m.AccountID == "" {
		t.Fatal("empty accountId")
	}
	if !strings.EqualFold(m.EmailAddress, email) {
		t.Logf("note: /myself email %q != input %q (can happen if accounts have aliases)", m.EmailAddress, email)
	}
	t.Logf("accountId=%s display=%q", m.AccountID, m.DisplayName)
}

func TestLive_JiraProject(t *testing.T) {
	email := os.Getenv("ATLASSIAN_EMAIL")
	token := os.Getenv("ATLASSIAN_API_TOKEN")
	if email == "" || token == "" {
		t.Skip("ATLASSIAN_EMAIL / ATLASSIAN_API_TOKEN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c := newJiraClient("https://consumerdirect.atlassian.net", email, token)
	if err := c.VerifyProject(ctx, "CD"); err != nil {
		t.Fatalf("VerifyProject CD: %v", err)
	}
	// Negative test.
	if err := c.VerifyProject(ctx, "ZZZZZZ"); err == nil {
		t.Fatal("expected error for bogus project key, got nil")
	}
}

func TestLive_JiraFields(t *testing.T) {
	email := os.Getenv("ATLASSIAN_EMAIL")
	token := os.Getenv("ATLASSIAN_API_TOKEN")
	if email == "" || token == "" {
		t.Skip("ATLASSIAN_EMAIL / ATLASSIAN_API_TOKEN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c := newJiraClient("https://consumerdirect.atlassian.net", email, token)
	res, err := c.DiscoverFields(ctx)
	if err != nil {
		t.Fatalf("DiscoverFields: %v", err)
	}
	t.Logf("StoryPoints ID: %q candidates: %d", res.StoryPointsID, len(res.StoryPointsCandidates))
	for _, f := range res.StoryPointsCandidates {
		t.Logf("  SP candidate: %s (%s)", f.ID, f.Name)
	}
	t.Logf("EpicLink ID: %q candidates: %d", res.EpicLinkID, len(res.EpicLinkCandidates))
	for _, f := range res.EpicLinkCandidates {
		t.Logf("  EL candidate: %s (%s)", f.ID, f.Name)
	}
	if res.StoryPointsID == "" && len(res.StoryPointsCandidates) == 0 {
		t.Error("no Story Points field resolved")
	}
	if res.EpicLinkID == "" {
		t.Error("no Epic Link field resolved (expected at least 'parent' fallback)")
	}
}

func TestLive_GithubAuth(t *testing.T) {
	token := os.Getenv("GH_TOKEN")
	if token == "" {
		t.Skip("GH_TOKEN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c := newGithubClient(token)
	u, err := c.VerifyAuth(ctx)
	if err != nil {
		t.Fatalf("VerifyAuth: %v", err)
	}
	if u.Login == "" {
		t.Fatal("empty login")
	}
	t.Logf("login=%s id=%d", u.Login, u.ID)
}

func TestLive_GithubOrg(t *testing.T) {
	token := os.Getenv("GH_TOKEN")
	org := os.Getenv("GITHUB_ORG")
	if token == "" || org == "" {
		t.Skip("GH_TOKEN or GITHUB_ORG not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c := newGithubClient(token)
	if err := c.VerifyOrgVisible(ctx, org); err != nil {
		t.Fatalf("VerifyOrgVisible %s: %v", org, err)
	}
	// Negative test.
	if err := c.VerifyOrgVisible(ctx, "definitely-not-a-real-org-xyz-123"); err == nil {
		t.Fatal("expected error for bogus org, got nil")
	}
}
