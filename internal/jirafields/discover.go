// Package jirafields powers `velocity jira fields discover`: a read-only wizard
// that samples recent Jira issues, inventories which fields are actually
// populated, and proposes the [profiles.default.jira.fields] custom-field
// mapping (plus a capture allowlist) so an operator configures field signals
// once instead of guessing field IDs by hand. It proposes — never writes —
// mirroring `devs discover` / `score risk-discover`.
package jirafields

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"
)

// Client is a thin read-only Jira REST v3 helper. Like initflow's client it is
// deliberately self-contained — the discover wizard hits a small, fixed
// endpoint surface (/field, /search/jql, /issue/{key}) and needs no pager,
// governor, or cache wiring.
type Client struct {
	baseURL string
	email   string
	token   string
	http    *http.Client
}

// NewClient builds a discover client. timeout 0 uses a sensible default.
func NewClient(baseURL, email, token string, timeout time.Duration) *Client {
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		email:   email,
		token:   token,
		http:    &http.Client{Timeout: timeout},
	}
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.email, c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jira %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read jira %s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("jira %s %s returned %d: %s", method, path, resp.StatusCode, truncate(b, 200))
	}
	return b, nil
}

// FetchCatalog returns every field's id/name/custom flag from /field.
func (c *Client) FetchCatalog(ctx context.Context) ([]FieldMeta, error) {
	b, err := c.do(ctx, http.MethodGet, "/rest/api/3/field", nil)
	if err != nil {
		return nil, err
	}
	var fields []FieldMeta
	if err := json.Unmarshal(b, &fields); err != nil {
		return nil, fmt.Errorf("parse /field: %w", err)
	}
	return fields, nil
}

type searchBody struct {
	JQL        string   `json:"jql"`
	Fields     []string `json:"fields"`
	MaxResults int      `json:"maxResults"`
}

type searchResponse struct {
	Issues []struct {
		Key string `json:"key"`
	} `json:"issues"`
}

// SampleKeys returns up to n recently-updated issue keys across projects. It
// asks only for the summary field — we just need the keys to then fetch each
// issue's full field set, which avoids relying on `fields=*all` being honored
// by the search endpoint (it is honored by the per-issue GET, verified).
func (c *Client) SampleKeys(ctx context.Context, projects []string, n int) ([]string, error) {
	if len(projects) == 0 {
		return nil, fmt.Errorf("no projects configured")
	}
	jql := fmt.Sprintf("project in (%s) ORDER BY updated DESC", strings.Join(projects, ", "))
	raw, err := json.Marshal(searchBody{JQL: jql, Fields: []string{"summary"}, MaxResults: n})
	if err != nil {
		return nil, err
	}
	b, err := c.do(ctx, http.MethodPost, "/rest/api/3/search/jql", strings.NewReader(string(raw)))
	if err != nil {
		return nil, err
	}
	var resp searchResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, fmt.Errorf("parse /search/jql: %w", err)
	}
	keys := make([]string, 0, len(resp.Issues))
	for _, is := range resp.Issues {
		keys = append(keys, is.Key)
	}
	return keys, nil
}

type issueResponse struct {
	Fields map[string]interface{} `json:"fields"`
}

// FetchIssueFields fetches one issue's full field map (fields=*all).
func (c *Client) FetchIssueFields(ctx context.Context, key string) (map[string]interface{}, error) {
	b, err := c.do(ctx, http.MethodGet, "/rest/api/3/issue/"+url.PathEscape(key)+"?fields=*all", nil)
	if err != nil {
		return nil, err
	}
	var resp issueResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, fmt.Errorf("parse issue %s: %w", key, err)
	}
	return resp.Fields, nil
}

// EditableField is one field that is settable on an issue, per its editmeta.
type EditableField struct {
	Name   string
	Custom string // schema.custom type, when present
}

type editMetaResponse struct {
	Fields map[string]struct {
		Name   string `json:"name"`
		Schema struct {
			Custom string `json:"custom"`
		} `json:"schema"`
		Operations []string `json:"operations"`
	} `json:"fields"`
}

// FetchEditMeta returns the fields settable on an issue (those whose editmeta
// operations include "set"), keyed by field ID. Presence here means Jira will
// accept a write to the field for that issue's project + issue type — the exact
// thing the global /field catalog cannot tell us and the reason a name-matched
// story-points field can 400 on write.
func (c *Client) FetchEditMeta(ctx context.Context, key string) (map[string]EditableField, error) {
	b, err := c.do(ctx, http.MethodGet, "/rest/api/3/issue/"+url.PathEscape(key)+"/editmeta", nil)
	if err != nil {
		return nil, err
	}
	var resp editMetaResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, fmt.Errorf("parse editmeta %s: %w", key, err)
	}
	out := make(map[string]EditableField, len(resp.Fields))
	for id, f := range resp.Fields {
		if !slices.Contains(f.Operations, "set") {
			continue
		}
		out[id] = EditableField{Name: f.Name, Custom: f.Schema.Custom}
	}
	return out, nil
}

// Discover runs the full read-only wizard: catalog → sample keys → per-issue
// fields → report. sleep spaces the per-issue GETs to stay polite (0 = none).
func (c *Client) Discover(ctx context.Context, projects []string, sample int, sleep time.Duration) (*Report, error) {
	catalog, err := c.FetchCatalog(ctx)
	if err != nil {
		return nil, err
	}
	keys, err := c.SampleKeys(ctx, projects, sample)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no issues found for project(s) %s", strings.Join(projects, ", "))
	}
	issues := make([]map[string]interface{}, 0, len(keys))
	settable := map[string]SettableField{}
	for i, k := range keys {
		f, err := c.FetchIssueFields(ctx, k)
		if err != nil {
			return nil, err
		}
		issues = append(issues, f)
		// editmeta is best-effort: a per-issue failure (e.g. the token can't read
		// this issue's editmeta) reduces settability evidence but must not abort
		// the wizard — story_points then falls back to the catalog name match.
		if em, err := c.FetchEditMeta(ctx, k); err == nil {
			for id, ef := range em {
				sf := settable[id]
				sf.Name, sf.Custom = ef.Name, ef.Custom
				sf.Count++
				settable[id] = sf
			}
		}
		if sleep > 0 && i < len(keys)-1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(sleep):
			}
		}
	}
	// Keys come back updated-desc; sort for stable, readable output.
	sort.Strings(keys)
	return buildReport(catalog, keys, issues, settable), nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
