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

// ErrIssueUnreachable is returned by the per-issue hydration when Jira responds
// with a status that won't recover on retry (404 issue moved/deleted, 410
// gone). The backfill phase marks the issue fetched so it isn't retried forever.
var ErrIssueUnreachable = errors.New("issue unreachable")

// HydrateIssue is the single comprehensive per-issue pull. One
// fields=*all&expand=changelog,names GET (changelog + comments paged when Jira
// truncates them on long-lived tickets) captures everything the base search
// doesn't: the mapped description, the status changelog, comments, subtasks +
// issue links, attachments, fix versions, and the raw field catch-all — then
// derives the cycle-time / rework / pre-code signals. It is the one Jira
// hydration path: `velocity refresh` runs it after the base pull, and
// `velocity-backfill --phase jira` runs it over the corpus.
//
// Resume gates: DetailFetched + RawFields, both set non-nil on success. On a
// permanent 404/410 every sentinel is frozen (empty, non-nil) and
// ErrIssueUnreachable is returned so the caller records a perm-skip and never
// retries.
func (p *JiraPuller) HydrateIssue(ctx context.Context, iss *cache.JiraIssue, now time.Time) error {
	esc := url.PathEscape(iss.Key)
	full, err := p.getIssueFull(ctx, esc)
	if err != nil {
		if errors.Is(err, ErrIssueUnreachable) {
			iss.Changelog = []cache.StatusTransition{}
			iss.Comments = []cache.IssueComment{}
			iss.Links = []cache.LinkedIssue{}
			iss.Attachments = []cache.Attachment{}
			iss.RawFields = []cache.RawField{}
			iss.DetailFetched = true
			t := now.UTC()
			iss.DetailFetchedAt = &t
		}
		return err
	}

	// Description: mapped field, falling back to the standard field so an issue
	// whose content only lived in the standard one is never blanked.
	if d := descriptionText(full.Fields, p.descriptionField()); d != "" {
		iss.Description = d
	} else if d := descriptionText(full.Fields, "description"); d != "" {
		iss.Description = d
	}

	// Comments: first page is embedded in the comment field; page the rest.
	cp := extractCommentPage(full.Fields)
	comments := decodeComments(cp.Comments)
	for cp.StartAt+len(cp.Comments) < cp.Total {
		next, err := p.getCommentPage(ctx, esc, cp.StartAt+len(cp.Comments))
		if err != nil {
			return err
		}
		if len(next.Comments) == 0 {
			break // defensive: avoid an infinite loop on a lying Total
		}
		comments = append(comments, decodeComments(next.Comments)...)
		cp = next
	}
	if comments == nil {
		comments = []cache.IssueComment{}
	}
	iss.Comments = comments

	// Changelog: first page is embedded via expand=changelog; page the rest.
	changelog := decodeStatusTransitions(full.Changelog.Histories)
	got := full.Changelog.StartAt + len(full.Changelog.Histories)
	for got < full.Changelog.Total {
		next, err := p.getChangelogPage(ctx, esc, got)
		if err != nil {
			return err
		}
		if len(next.Values) == 0 {
			break
		}
		changelog = append(changelog, decodeStatusTransitions(next.Values)...)
		got = next.StartAt + len(next.Values)
		if next.IsLast {
			break
		}
	}
	if changelog == nil {
		changelog = []cache.StatusTransition{}
	}
	iss.Changelog = changelog

	setRelations(iss, full.Fields)
	iss.RawFields = buildRawFields(full.Fields, full.Names)
	DeriveIssueSignals(iss)

	iss.DetailFetched = true
	t := now.UTC()
	iss.DetailFetchedAt = &t
	return nil
}

// extractCommentPage pulls the first comment page out of the *all field map
// (the `comment` field) by re-decoding it into the typed page shape.
func extractCommentPage(fields map[string]interface{}) jiraCommentPage {
	raw, ok := fields["comment"]
	if !ok {
		return jiraCommentPage{}
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return jiraCommentPage{}
	}
	var cp jiraCommentPage
	_ = json.Unmarshal(b, &cp)
	return cp
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

// getIssueFull does the one comprehensive issue call: every field
// (fields=*all), the field-name map (expand=names), and the inline changelog
// (expand=changelog). Permanent 404/410 is classified as ErrIssueUnreachable.
func (p *JiraPuller) getIssueFull(ctx context.Context, escapedKey string) (issueFull, error) {
	path := fmt.Sprintf("/rest/api/3/issue/%s?fields=*all&expand=changelog,names", escapedKey)
	resp, body, err := p.do(ctx, path)
	if err != nil {
		return issueFull{}, err
	}
	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusGone:
		return issueFull{}, fmt.Errorf("issue %s: %d %s: %w", escapedKey, resp.StatusCode, http.StatusText(resp.StatusCode), ErrIssueUnreachable)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return issueFull{}, fmt.Errorf("GET issue %s → %d: %s", escapedKey, resp.StatusCode, truncate(body, 200))
	}
	var out issueFull
	if err := json.Unmarshal(body, &out); err != nil {
		return issueFull{}, fmt.Errorf("decode issue %s: %w", escapedKey, err)
	}
	return out, nil
}

// issueFull is the comprehensive per-issue response: the full field map (for
// description, relations, and the raw catch-all), the field-name map, and the
// inline changelog.
type issueFull struct {
	Fields    map[string]interface{} `json:"fields"`
	Names     map[string]string      `json:"names"`
	Changelog jiraChangelogEmbedded  `json:"changelog"`
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

// ---------- raw response shapes ----------

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
