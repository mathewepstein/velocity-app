package pull

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
	"github.com/mathewepstein/velocity/internal/progress"
)

// JiraPuller pulls one month of issue activity per call. A single puller is
// reused across months for connection reuse and ratelimit-awareness.
type JiraPuller struct {
	baseURL        string
	email          string
	token          string
	storyPointsID  string
	epicLinkID     string
	descriptionID  string // mapped description field; "" → standard "description"
	client         *backoffClient
	pageSize       int
	sleepBtwnPages time.Duration
	reporter       progress.Reporter
}

// NewJiraPuller configures a puller from a resolved profile + keychain token.
// pageSize and sleepBetweenPages are optional knobs; zero → defaults.
func NewJiraPuller(p config.JiraConfig, token string, pageSize int, sleepBetweenPages time.Duration) *JiraPuller {
	if pageSize <= 0 {
		pageSize = 100
	}
	return &JiraPuller{
		baseURL:        strings.TrimRight(p.BaseURL, "/"),
		email:          p.Email,
		token:          token,
		storyPointsID:  p.Fields.StoryPoints,
		epicLinkID:     p.Fields.EpicLink,
		descriptionID:  p.Fields.Description,
		client:         newBackoffClient(),
		pageSize:       pageSize,
		sleepBtwnPages: sleepBetweenPages,
		reporter:       progress.Nop(),
	}
}

// SetReporter routes search pagination progress and backoff waits through rep.
// Defaults to a no-op.
func (p *JiraPuller) SetReporter(rep progress.Reporter) {
	if rep == nil {
		rep = progress.Nop()
	}
	p.reporter = rep
	p.client.reporter = rep
}

// UseGovernor attaches a proactive rate governor so every response feeds its
// rate-limit signal and the backfill runner can pace calls via gov.Wait. Jira
// has no usable remaining-headers, so pacing rides the governor's minInterval
// baseline with a 429's Retry-After as the hard backstop. The refresh path
// leaves this unset.
func (p *JiraPuller) UseGovernor(gov *RateGovernor) {
	p.client.governor = gov
}

// PullMonth returns every issue on the given project key whose `updated`
// timestamp falls inside m. Over-reports vs. "resolved in month" on purpose:
// analyze uses updated-in-month as the activity signal and resolutiondate for
// throughput calculations. No author filter — per-author rollups happen
// downstream from assignee/reporter on each record.
func (p *JiraPuller) PullMonth(ctx context.Context, project string, m cache.Month) ([]cache.JiraIssue, error) {
	start := m.Start()
	end := m.Add(1).Start()
	jql := BuildJiraJQL(project, start, end)

	fields := jiraBaseFields
	if p.storyPointsID != "" {
		fields = append(fields, p.storyPointsID)
	}
	if p.epicLinkID != "" && p.epicLinkID != "parent" {
		// "parent" is always implied on issues; only add custom fields.
		fields = append(fields, p.epicLinkID)
	}

	var out []cache.JiraIssue
	var nextToken string
	for page := 0; ; page++ {
		body := jiraSearchBody{
			JQL:           jql,
			Fields:        fields,
			MaxResults:    p.pageSize,
			NextPageToken: nextToken,
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/rest/api/3/search/jql", bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.SetBasicAuth(p.email, p.token)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")

		var resp jiraSearchResponse
		if err := p.client.doJSON(ctx, req, raw, &resp); err != nil {
			return nil, fmt.Errorf("jira search page %d: %w", page, err)
		}

		for _, issue := range resp.Issues {
			out = append(out, p.decodeIssue(issue))
		}
		p.reporter.Page(page+1, len(out))

		if resp.IsLast || resp.NextPageToken == "" {
			break
		}
		nextToken = resp.NextPageToken

		if p.sleepBtwnPages > 0 {
			if err := sleepOrCancel(ctx, p.sleepBtwnPages); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// BuildJiraJQL returns the JQL for "every issue updated during [start, end)
// on this project". Exported for tests.
func BuildJiraJQL(project string, start, end time.Time) string {
	return fmt.Sprintf(
		`project = %s AND updated >= "%s" AND updated < "%s"`,
		project,
		start.UTC().Format("2006-01-02 15:04"),
		end.UTC().Format("2006-01-02 15:04"),
	)
}

// jiraBaseFields is the list of fields every pull requests. Custom fields
// (story points, epic link) are appended at call time.
var jiraBaseFields = []string{
	"summary",
	"status",
	"resolution",
	"issuetype",
	"created",
	"updated",
	"resolutiondate",
	"assignee",
	"reporter",
	"labels",
	"components",
	"parent",
	"issuelinks",
	"subtasks",
	"attachment",
	"fixVersions",
}

type jiraSearchBody struct {
	JQL           string   `json:"jql"`
	Fields        []string `json:"fields,omitempty"`
	MaxResults    int      `json:"maxResults"`
	NextPageToken string   `json:"nextPageToken,omitempty"`
}

type jiraSearchResponse struct {
	Issues        []jiraIssueRaw `json:"issues"`
	NextPageToken string         `json:"nextPageToken"`
	IsLast        bool           `json:"isLast"`
}

// jiraIssueRaw is a minimal unmarshal target — only the fields we care about,
// everything else goes into the "extra" map so custom field IDs (resolved at
// runtime from config) can be read out.
type jiraIssueRaw struct {
	Key    string                 `json:"key"`
	Fields map[string]interface{} `json:"fields"`
}

// decodeIssue converts a raw Jira issue JSON object into our cache shape.
func (p *JiraPuller) decodeIssue(r jiraIssueRaw) cache.JiraIssue {
	f := r.Fields
	iss := cache.JiraIssue{
		Key:     r.Key,
		Summary: stringField(f, "summary"),
	}

	if s, ok := f["status"].(map[string]interface{}); ok {
		iss.Status = stringField(s, "name")
	}
	if s, ok := f["resolution"].(map[string]interface{}); ok {
		iss.Resolution = stringField(s, "name")
	}
	if s, ok := f["issuetype"].(map[string]interface{}); ok {
		iss.IssueType = stringField(s, "name")
	}
	if s, ok := f["assignee"].(map[string]interface{}); ok {
		iss.Assignee = stringField(s, "accountId")
	}
	if s, ok := f["reporter"].(map[string]interface{}); ok {
		iss.Reporter = stringField(s, "accountId")
	}

	iss.Created = parseJiraTime(stringField(f, "created"))
	iss.Updated = parseJiraTime(stringField(f, "updated"))
	if res := stringField(f, "resolutiondate"); res != "" {
		t := parseJiraTime(res)
		if !t.IsZero() {
			iss.Resolved = &t
		}
	}

	if p.storyPointsID != "" {
		iss.StoryPoints = floatField(f, p.storyPointsID)
	}

	// Epic link: prefer the configured custom field; fall back to "parent".
	if p.epicLinkID != "" && p.epicLinkID != "parent" {
		iss.EpicKey = stringField(f, p.epicLinkID)
	}
	if iss.EpicKey == "" {
		if parent, ok := f["parent"].(map[string]interface{}); ok {
			iss.EpicKey = stringField(parent, "key")
		}
	}

	if arr, ok := f["labels"].([]interface{}); ok {
		for _, a := range arr {
			if s, ok := a.(string); ok {
				iss.Labels = append(iss.Labels, s)
			}
		}
	}
	if arr, ok := f["components"].([]interface{}); ok {
		for _, a := range arr {
			if m, ok := a.(map[string]interface{}); ok {
				if name := stringField(m, "name"); name != "" {
					iss.Components = append(iss.Components, name)
				}
			}
		}
	}

	setRelations(&iss, f)
	return iss
}

// setRelations parses subtasks + issue links, attachments, and fix versions
// from a Jira issue `fields` map onto iss. Shared by the base-search decode and
// the `fields=*all` field-capture hydration. The Links + Attachments sentinel
// slices are initialized non-nil (and FixVersions reset), so an issue run
// through this is "relations captured" even with none — distinct from a
// pre-capture historical issue that reads back nil.
func setRelations(iss *cache.JiraIssue, f map[string]interface{}) {
	iss.Links = []cache.LinkedIssue{}
	iss.Attachments = []cache.Attachment{}
	iss.FixVersions = nil

	if arr, ok := f["subtasks"].([]interface{}); ok {
		for _, a := range arr {
			if m, ok := a.(map[string]interface{}); ok {
				iss.Links = append(iss.Links, linkedFrom(m, "subtask", "outward", "subtask"))
			}
		}
	}
	if arr, ok := f["issuelinks"].([]interface{}); ok {
		for _, a := range arr {
			m, ok := a.(map[string]interface{})
			if !ok {
				continue
			}
			typ, _ := m["type"].(map[string]interface{})
			linkType := stringField(typ, "name")
			if out, ok := m["outwardIssue"].(map[string]interface{}); ok {
				iss.Links = append(iss.Links, linkedFrom(out, linkType, "outward", stringField(typ, "outward")))
			} else if in, ok := m["inwardIssue"].(map[string]interface{}); ok {
				iss.Links = append(iss.Links, linkedFrom(in, linkType, "inward", stringField(typ, "inward")))
			}
		}
	}
	if arr, ok := f["attachment"].([]interface{}); ok {
		for _, a := range arr {
			if m, ok := a.(map[string]interface{}); ok {
				att := cache.Attachment{
					Filename: stringField(m, "filename"),
					MimeType: stringField(m, "mimeType"),
					Size:     int(floatField(m, "size")),
					Created:  parseJiraTime(stringField(m, "created")),
				}
				if au, ok := m["author"].(map[string]interface{}); ok {
					att.Author = stringField(au, "accountId")
				}
				iss.Attachments = append(iss.Attachments, att)
			}
		}
	}
	if arr, ok := f["fixVersions"].([]interface{}); ok {
		for _, a := range arr {
			if m, ok := a.(map[string]interface{}); ok {
				if name := stringField(m, "name"); name != "" {
					iss.FixVersions = append(iss.FixVersions, name)
				}
			}
		}
	}
}

// linkedFrom builds a LinkedIssue from a counterpart issue object (subtask or
// the inward/outward side of an issue link), reading the counterpart's status
// and type when the API embedded them.
func linkedFrom(issue map[string]interface{}, linkType, direction, phrase string) cache.LinkedIssue {
	l := cache.LinkedIssue{Key: stringField(issue, "key"), LinkType: linkType, Direction: direction, Phrase: phrase}
	if fl, ok := issue["fields"].(map[string]interface{}); ok {
		l.Status = nestedName(fl, "status")
		l.IssueType = nestedName(fl, "issuetype")
	}
	return l
}

// nestedName returns m[key].name when m[key] is an object, else "".
func nestedName(m map[string]interface{}, key string) string {
	if sub, ok := m[key].(map[string]interface{}); ok {
		return stringField(sub, "name")
	}
	return ""
}

func stringField(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func floatField(m map[string]interface{}, key string) float64 {
	if v, ok := m[key]; ok {
		if f, ok := v.(float64); ok {
			return f
		}
	}
	return 0
}

// parseJiraTime parses Jira's "2026-04-15T08:33:21.000-0700" format.
// Returns zero time on empty / unparseable input — callers check IsZero.
func parseJiraTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05.000-0700",
		"2006-01-02T15:04:05-0700",
		time.RFC3339Nano,
		time.RFC3339,
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
