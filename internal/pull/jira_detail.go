package pull

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
)

// ErrIssueUnreachable is returned by FetchIssueDetail when Jira responds with a
// status that won't recover on retry (404 issue moved/deleted, 410 gone). The
// backfill phase marks the issue DetailFetched so it isn't retried forever.
var ErrIssueUnreachable = errors.New("issue unreachable")

// IssueDetail is the hydrated per-issue detail: the flattened description (plus
// any URLs it references), the status-transition changelog, and the comments.
// Raw signals only — derivation happens in DeriveIssueSignals.
type IssueDetail struct {
	Description string
	DescURLs    []string
	Changelog   []cache.StatusTransition // status field changes only, chronological
	Comments    []cache.IssueComment
}

// FetchIssueDetail pulls one issue's changelog + comments + description in a
// single base call (expand=changelog, fields=comment,description) and pages the
// changelog/comments when Jira truncates them on long-lived tickets, so nothing
// is silently lost. Only status-field changelog entries are kept — that's the
// cycle-time / rework signal; other field edits are noise for our purposes.
func (p *JiraPuller) FetchIssueDetail(ctx context.Context, key string) (IssueDetail, error) {
	esc := url.PathEscape(key)
	raw, err := p.getIssueDetail(ctx, esc)
	if err != nil {
		return IssueDetail{}, err
	}

	var d IssueDetail
	if rawDesc, ok := raw.Fields[p.descriptionField()]; ok {
		var descVal interface{}
		if err := json.Unmarshal(rawDesc, &descVal); err == nil && descVal != nil {
			adf := flattenADF(descVal)
			d.Description = adf.Text
			d.DescURLs = adf.URLs
		}
	}

	// Comments: first page is embedded; page the rest if truncated.
	var comment jiraCommentPage
	if rawCmt, ok := raw.Fields["comment"]; ok {
		if err := json.Unmarshal(rawCmt, &comment); err != nil {
			return IssueDetail{}, fmt.Errorf("decode comments %s: %w", key, err)
		}
	}
	d.Comments = decodeComments(comment.Comments)
	cp := comment
	for cp.StartAt+len(cp.Comments) < cp.Total {
		next, err := p.getCommentPage(ctx, esc, cp.StartAt+len(cp.Comments))
		if err != nil {
			return IssueDetail{}, err
		}
		if len(next.Comments) == 0 {
			break // defensive: avoid an infinite loop on a lying Total
		}
		d.Comments = append(d.Comments, decodeComments(next.Comments)...)
		cp = next
	}

	// Changelog: first page is embedded; page the rest if truncated.
	d.Changelog = decodeStatusTransitions(raw.Changelog.Histories)
	clTotal := raw.Changelog.Total
	got := raw.Changelog.StartAt + len(raw.Changelog.Histories)
	for got < clTotal {
		next, err := p.getChangelogPage(ctx, esc, got)
		if err != nil {
			return IssueDetail{}, err
		}
		if len(next.Values) == 0 {
			break
		}
		d.Changelog = append(d.Changelog, decodeStatusTransitions(next.Values)...)
		got = next.StartAt + len(next.Values)
		if next.IsLast {
			break
		}
	}

	return d, nil
}

// descriptionField is the Jira field the description is read from: the mapped
// custom field when configured, else the built-in "description". At this org
// the real content lives in a custom field, so the mapping is load-bearing —
// the standard field is empty on most issues.
func (p *JiraPuller) descriptionField() string {
	if p.descriptionID != "" {
		return p.descriptionID
	}
	return "description"
}

// getIssueDetail does the base issue call and classifies permanent failures.
func (p *JiraPuller) getIssueDetail(ctx context.Context, escapedKey string) (jiraDetailRaw, error) {
	path := fmt.Sprintf("/rest/api/3/issue/%s?expand=changelog&fields=comment,%s", escapedKey, p.descriptionField())
	resp, body, err := p.do(ctx, path)
	if err != nil {
		return jiraDetailRaw{}, err
	}
	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusGone:
		return jiraDetailRaw{}, fmt.Errorf("issue %s: %d %s: %w", escapedKey, resp.StatusCode, http.StatusText(resp.StatusCode), ErrIssueUnreachable)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return jiraDetailRaw{}, fmt.Errorf("GET issue %s → %d: %s", escapedKey, resp.StatusCode, truncate(body, 200))
	}
	var out jiraDetailRaw
	if err := json.Unmarshal(body, &out); err != nil {
		return jiraDetailRaw{}, fmt.Errorf("decode issue %s: %w", escapedKey, err)
	}
	return out, nil
}

func (p *JiraPuller) getCommentPage(ctx context.Context, escapedKey string, startAt int) (jiraCommentPage, error) {
	path := fmt.Sprintf("/rest/api/3/issue/%s/comment?startAt=%d&maxResults=%d", escapedKey, startAt, p.pageSize)
	resp, body, err := p.do(ctx, path)
	if err != nil {
		return jiraCommentPage{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return jiraCommentPage{}, fmt.Errorf("GET comments %s → %d: %s", escapedKey, resp.StatusCode, truncate(body, 200))
	}
	var out jiraCommentPage
	if err := json.Unmarshal(body, &out); err != nil {
		return jiraCommentPage{}, fmt.Errorf("decode comments %s: %w", escapedKey, err)
	}
	return out, nil
}

func (p *JiraPuller) getChangelogPage(ctx context.Context, escapedKey string, startAt int) (jiraChangelogPageBean, error) {
	path := fmt.Sprintf("/rest/api/3/issue/%s/changelog?startAt=%d&maxResults=%d", escapedKey, startAt, p.pageSize)
	resp, body, err := p.do(ctx, path)
	if err != nil {
		return jiraChangelogPageBean{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return jiraChangelogPageBean{}, fmt.Errorf("GET changelog %s → %d: %s", escapedKey, resp.StatusCode, truncate(body, 200))
	}
	var out jiraChangelogPageBean
	if err := json.Unmarshal(body, &out); err != nil {
		return jiraChangelogPageBean{}, fmt.Errorf("decode changelog %s: %w", escapedKey, err)
	}
	return out, nil
}

// do issues an authenticated GET and returns the response + body. Shared by the
// detail/comment/changelog calls; the backoffClient handles retries + the
// governor underneath.
func (p *JiraPuller) do(ctx context.Context, path string) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+path, nil)
	if err != nil {
		return nil, nil, err
	}
	req.SetBasicAuth(p.email, p.token)
	req.Header.Set("Accept", "application/json")
	return p.client.do(ctx, req, nil)
}

// decodeComments flattens each comment body from ADF to plain text.
func decodeComments(raw []jiraCommentRaw) []cache.IssueComment {
	out := make([]cache.IssueComment, 0, len(raw))
	for _, c := range raw {
		out = append(out, cache.IssueComment{
			Author:  c.Author.AccountID,
			Created: parseJiraTime(c.Created),
			Body:    flattenADF(c.Body).Text,
		})
	}
	return out
}

// decodeStatusTransitions keeps only status-field changes, one StatusTransition
// per history entry that touched the status field.
func decodeStatusTransitions(histories []jiraHistoryRaw) []cache.StatusTransition {
	var out []cache.StatusTransition
	for _, h := range histories {
		for _, it := range h.Items {
			if it.Field != "status" {
				continue
			}
			out = append(out, cache.StatusTransition{
				At:     parseJiraTime(h.Created),
				Author: h.Author.AccountID,
				From:   it.FromString,
				To:     it.ToString,
				Field:  it.Field,
			})
		}
	}
	return out
}

// DeriveIssueSignals computes the cached cycle-time / rework / pre-code signals
// from an issue's raw changelog + comments, mutating iss in place. Definitions
// are deliberately structural (no hardcoded status-name taxonomy) so they hold
// across teams; the raw changelog/comments stay stored (B1) so these can be
// refined later without re-fetching:
//
//   - FirstInProgress: when the issue first left its created status (first
//     status transition). A lower-bound "work started" marker.
//   - DoneAt: resolutiondate if set, else the last status transition.
//   - CycleHours: DoneAt − FirstInProgress when both exist and DoneAt is later.
//   - StatusFlips: status transitions that re-entered an already-visited status
//     (rework / churn signal).
//   - PreCodeComments: comments created at or before FirstInProgress
//     (discussion/coordination before active work began).
func DeriveIssueSignals(iss *cache.JiraIssue) {
	sort.SliceStable(iss.Changelog, func(i, j int) bool {
		return iss.Changelog[i].At.Before(iss.Changelog[j].At)
	})

	iss.FirstInProgress = nil
	iss.DoneAt = nil
	iss.CycleHours = 0
	iss.StatusFlips = 0
	iss.PreCodeComments = 0

	if len(iss.Changelog) > 0 {
		first := iss.Changelog[0].At
		if !first.IsZero() {
			iss.FirstInProgress = &first
		}
	}

	switch {
	case iss.Resolved != nil:
		iss.DoneAt = iss.Resolved
	case len(iss.Changelog) > 0:
		last := iss.Changelog[len(iss.Changelog)-1].At
		if !last.IsZero() {
			iss.DoneAt = &last
		}
	}

	if iss.FirstInProgress != nil && iss.DoneAt != nil {
		if d := iss.DoneAt.Sub(*iss.FirstInProgress); d > 0 {
			iss.CycleHours = d.Hours()
		}
	}

	visited := map[string]bool{}
	if len(iss.Changelog) > 0 && iss.Changelog[0].From != "" {
		visited[iss.Changelog[0].From] = true // the created status
	}
	for _, tr := range iss.Changelog {
		if tr.To == "" {
			continue
		}
		if visited[tr.To] {
			iss.StatusFlips++
		}
		visited[tr.To] = true
	}

	if iss.FirstInProgress != nil {
		for _, c := range iss.Comments {
			if !c.Created.IsZero() && !c.Created.After(*iss.FirstInProgress) {
				iss.PreCodeComments++
			}
		}
	}
}

// HydrateIssueDetail fetches detail for one issue and writes the raw signals +
// derived fields into iss, flipping the DetailFetched resume gate. On a
// permanent (unreachable) error it still marks the issue fetched — with empty
// (non-nil) changelog/comments — so resolved issues aren't retried forever; it
// returns ErrIssueUnreachable so the caller can classify it as a perm-skip.
func (p *JiraPuller) HydrateIssueDetail(ctx context.Context, iss *cache.JiraIssue, now time.Time) error {
	d, err := p.FetchIssueDetail(ctx, iss.Key)
	if err != nil {
		if errors.Is(err, ErrIssueUnreachable) {
			iss.Changelog = []cache.StatusTransition{}
			iss.Comments = []cache.IssueComment{}
			iss.DetailFetched = true
			t := now.UTC()
			iss.DetailFetchedAt = &t
		}
		return err
	}

	iss.Description = d.Description
	if iss.Changelog = d.Changelog; iss.Changelog == nil {
		iss.Changelog = []cache.StatusTransition{}
	}
	if iss.Comments = d.Comments; iss.Comments == nil {
		iss.Comments = []cache.IssueComment{}
	}
	DeriveIssueSignals(iss)
	iss.DetailFetched = true
	t := now.UTC()
	iss.DetailFetchedAt = &t
	return nil
}

// ---------- raw response shapes ----------

// jiraDetailRaw keeps Fields as a keyed map so the description can be read from
// whichever (possibly custom) field the mapping points at — a fixed struct tag
// can't express a per-org custom field ID. comment is always the standard key.
type jiraDetailRaw struct {
	Fields    map[string]json.RawMessage `json:"fields"`
	Changelog jiraChangelogEmbedded      `json:"changelog"`
}

type jiraCommentPage struct {
	Comments   []jiraCommentRaw `json:"comments"`
	StartAt    int              `json:"startAt"`
	MaxResults int              `json:"maxResults"`
	Total      int              `json:"total"`
}

type jiraCommentRaw struct {
	Author struct {
		AccountID string `json:"accountId"`
	} `json:"author"`
	Created string      `json:"created"`
	Body    interface{} `json:"body"`
}

// jiraChangelogEmbedded is the changelog object returned inline via
// expand=changelog (uses "histories").
type jiraChangelogEmbedded struct {
	Histories  []jiraHistoryRaw `json:"histories"`
	StartAt    int              `json:"startAt"`
	MaxResults int              `json:"maxResults"`
	Total      int              `json:"total"`
}

// jiraChangelogPageBean is the standalone /issue/{key}/changelog page bean
// (uses "values" + isLast).
type jiraChangelogPageBean struct {
	Values     []jiraHistoryRaw `json:"values"`
	StartAt    int              `json:"startAt"`
	MaxResults int              `json:"maxResults"`
	Total      int              `json:"total"`
	IsLast     bool             `json:"isLast"`
}

type jiraHistoryRaw struct {
	Created string `json:"created"`
	Author  struct {
		AccountID string `json:"accountId"`
	} `json:"author"`
	Items []struct {
		Field      string `json:"field"`
		FromString string `json:"fromString"`
		ToString   string `json:"toString"`
	} `json:"items"`
}
