package initflow

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const githubAPIBase = "https://api.github.com"

// githubClient wraps GitHub REST calls init needs. Uses PAT bearer auth.
// Local to initflow — Phase 4 will have its own puller client.
type githubClient struct {
	token string
	http  *http.Client
}

func newGithubClient(token string) *githubClient {
	return &githubClient{
		token: token,
		http:  &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *githubClient) do(ctx context.Context, path string) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPIBase+path, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("github request %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp, nil, fmt.Errorf("read github response %s: %w", path, err)
	}
	return resp, body, nil
}

// githubUser is the subset of /user we need.
type githubUser struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
}

// VerifyAuth confirms the token is valid. Returns the authenticated user's
// login.
func (c *githubClient) VerifyAuth(ctx context.Context) (githubUser, error) {
	resp, body, err := c.do(ctx, "/user")
	if err != nil {
		return githubUser{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return githubUser{}, fmt.Errorf("/user returned %d: %s", resp.StatusCode, truncate(body, 200))
	}
	var u githubUser
	if err := json.Unmarshal(body, &u); err != nil {
		return githubUser{}, fmt.Errorf("parse /user: %w", err)
	}
	if u.Login == "" {
		return githubUser{}, fmt.Errorf("/user returned empty login")
	}
	return u, nil
}

// VerifyOrgVisible confirms the org exists and is visible to the authenticated
// token. Returns an error on 404 (doesn't exist / not visible), or any non-2xx.
//
// We deliberately avoid /user/memberships/orgs/{org}: that endpoint requires
// the `read:org` scope, which most fine-grained PATs don't have. Catching typos
// and unknown orgs is the main goal here; membership itself is proven later
// when `velocity refresh` successfully pulls activity.
func (c *githubClient) VerifyOrgVisible(ctx context.Context, org string) error {
	resp, body, err := c.do(ctx, "/orgs/"+org)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("org %q not found or not visible to this token", org)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("/orgs/%s returned %d: %s", org, resp.StatusCode, truncate(body, 200))
	}
	return nil
}
