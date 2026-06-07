package initflow

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// jiraClient is a thin HTTP helper for the endpoints init needs. It is
// intentionally local to this package — Phase 4 pullers hit a different
// endpoint surface and will bring their own client.
type jiraClient struct {
	baseURL string
	email   string
	token   string
	http    *http.Client
}

func newJiraClient(baseURL, email, token string) *jiraClient {
	return &jiraClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		email:   email,
		token:   token,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// do issues a GET against the Jira REST v3 API with basic auth. Returns the
// raw body on 2xx; wraps the status + body on non-2xx.
func (c *jiraClient) do(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.email, c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jira request %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read jira response %s: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("jira %s returned %d: %s", path, resp.StatusCode, truncate(body, 200))
	}
	return body, nil
}

// jiraMyself is the subset of /rest/api/3/myself we need.
type jiraMyself struct {
	AccountID    string `json:"accountId"`
	EmailAddress string `json:"emailAddress"`
	DisplayName  string `json:"displayName"`
}

// VerifyJiraAuth confirms (baseURL, email, token) are valid by calling
// /myself. Returns the resolved accountId.
func (c *jiraClient) VerifyAuth(ctx context.Context) (jiraMyself, error) {
	body, err := c.do(ctx, "/rest/api/3/myself")
	if err != nil {
		return jiraMyself{}, err
	}
	var m jiraMyself
	if err := json.Unmarshal(body, &m); err != nil {
		return jiraMyself{}, fmt.Errorf("parse /myself: %w", err)
	}
	if m.AccountID == "" {
		return jiraMyself{}, fmt.Errorf("/myself returned empty accountId")
	}
	return m, nil
}

// VerifyProject returns nil if the project key exists and the user can see it.
// Jira returns 404 for nonexistent OR permission-denied, so the error message
// intentionally mentions both.
func (c *jiraClient) VerifyProject(ctx context.Context, key string) error {
	_, err := c.do(ctx, "/rest/api/3/project/"+key)
	if err != nil {
		return fmt.Errorf("project %q not found or not visible: %w", key, err)
	}
	return nil
}

// jiraField is the subset of /rest/api/3/field we need.
type jiraField struct {
	ID     string   `json:"id"`
	Key    string   `json:"key"`
	Name   string   `json:"name"`
	Custom bool     `json:"custom"`
	Schema struct{} `json:"schema"` // unused, kept to document the shape
}

// FieldResolution names the two custom fields init needs to resolve.
type FieldResolution struct {
	StoryPointsID string
	StoryPointsCandidates []jiraField // populated when >1 candidate, so caller can prompt

	EpicLinkID string
	EpicLinkCandidates []jiraField
}

// DiscoverFields calls /field and resolves Story Points + Epic Link.
//
// Story Points: matches fields named "Story Points" or "Story point estimate"
// (Jira renamed the default in team-managed projects). If both exist we return
// both as candidates for the caller to disambiguate.
//
// Epic Link: company-managed projects use the built-in "parent" field;
// team-managed uses a "Epic Link" custom field (historically customfield_10014).
// If a dedicated "Epic Link" custom field exists we return it; otherwise we
// default to "parent" which is always valid for company-managed.
func (c *jiraClient) DiscoverFields(ctx context.Context) (FieldResolution, error) {
	body, err := c.do(ctx, "/rest/api/3/field")
	if err != nil {
		return FieldResolution{}, err
	}
	var fields []jiraField
	if err := json.Unmarshal(body, &fields); err != nil {
		return FieldResolution{}, fmt.Errorf("parse /field: %w", err)
	}

	var spCandidates []jiraField
	var elCandidates []jiraField
	for _, f := range fields {
		name := strings.ToLower(strings.TrimSpace(f.Name))
		switch name {
		case "story points", "story point estimate":
			spCandidates = append(spCandidates, f)
		case "epic link":
			elCandidates = append(elCandidates, f)
		}
	}

	res := FieldResolution{
		StoryPointsCandidates: spCandidates,
		EpicLinkCandidates:    elCandidates,
	}
	if len(spCandidates) == 1 {
		res.StoryPointsID = spCandidates[0].ID
	}
	if len(elCandidates) == 1 {
		res.EpicLinkID = elCandidates[0].ID
	} else if len(elCandidates) == 0 {
		// No custom Epic Link field → company-managed default.
		res.EpicLinkID = "parent"
	}
	return res, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
