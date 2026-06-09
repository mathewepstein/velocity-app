package pull

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/mathewepstein/velocity/internal/config"
)

// JiraWriter performs the story-points engine's write-backs: set the Story
// Points field and post one calibration comment. It is deliberately separate
// from JiraPuller (the read path) but shares the same auth + backoff plumbing.
// Low-volume by design — the caller (scoring.PostScores) gates every write
// behind a dry-run preview and a posted_to_jira idempotency check, so there is
// no bulk pacing governor here; the backoff client's 429/5xx retry is enough.
type JiraWriter struct {
	baseURL       string
	email         string
	token         string
	storyPointsID string
	client        *backoffClient
}

// NewJiraWriter builds a writer from the Jira config + keychain token. The
// Story Points field ID comes from config (discovered at `velocity init`), so
// there is no per-call field-discovery round trip.
func NewJiraWriter(j config.JiraConfig, token string) *JiraWriter {
	return &JiraWriter{
		baseURL:       strings.TrimRight(j.BaseURL, "/"),
		email:         j.Email,
		token:         token,
		storyPointsID: j.Fields.StoryPoints,
		client:        newBackoffClient(),
	}
}

func (w *JiraWriter) auth(req *http.Request) {
	req.SetBasicAuth(w.email, w.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
}

// SetStoryPoints writes points to the instance's Story Points field on issue
// key (PUT /rest/api/3/issue/{key}, 204 on success). Errors if the field is not
// mapped in config, so the caller never silently no-ops.
func (w *JiraWriter) SetStoryPoints(ctx context.Context, key string, points float64) error {
	if w.storyPointsID == "" {
		return fmt.Errorf("story points field is not mapped in config (run `velocity init` or set [jira.fields].story_points)")
	}
	payload := map[string]any{
		"fields": map[string]any{w.storyPointsID: points},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, w.baseURL+"/rest/api/3/issue/"+url.PathEscape(key), bytes.NewReader(raw))
	if err != nil {
		return err
	}
	w.auth(req)
	// 204 No Content on success → out=nil, doJSON just checks the 2xx status.
	if err := w.client.doJSON(ctx, req, raw, nil); err != nil {
		return fmt.Errorf("set story points on %s: %w", key, err)
	}
	return nil
}

// AddComment posts one comment to issue key. lines render to ADF: a line equal
// to "---" becomes a horizontal rule; every other non-empty line becomes its
// own paragraph. Mirrors the /score-ticket Step 6 calibration comment.
func (w *JiraWriter) AddComment(ctx context.Context, key string, lines []string) error {
	payload := map[string]any{"body": linesToADF(lines)}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.baseURL+"/rest/api/3/issue/"+url.PathEscape(key)+"/comment", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	w.auth(req)
	if err := w.client.doJSON(ctx, req, raw, nil); err != nil {
		return fmt.Errorf("add comment to %s: %w", key, err)
	}
	return nil
}

// VerifyWriteScope confirms the token can edit issues and add comments without
// any side effect (GET /rest/api/3/mypermissions). Returns a descriptive error
// when a required permission is missing so the UI can keep the post button off.
// This is a best-effort preflight — Jira evaluates project permissions without
// a project context conservatively — so the authoritative check remains the
// per-ticket post, whose 403 is recorded in the report.
func (w *JiraWriter) VerifyWriteScope(ctx context.Context) error {
	perms := []string{"EDIT_ISSUES", "ADD_COMMENTS"}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		w.baseURL+"/rest/api/3/mypermissions?permissions="+strings.Join(perms, ","), nil)
	if err != nil {
		return err
	}
	w.auth(req)
	var out struct {
		Permissions map[string]struct {
			HavePermission bool `json:"havePermission"`
		} `json:"permissions"`
	}
	if err := w.client.doJSON(ctx, req, nil, &out); err != nil {
		return fmt.Errorf("check jira permissions: %w", err)
	}
	for _, p := range perms {
		if !out.Permissions[p].HavePermission {
			return fmt.Errorf("jira token lacks the %s permission", p)
		}
	}
	return nil
}

// linesToADF wraps plain-text lines in a minimal Atlassian Document Format doc:
// "---" → a rule node, blank lines dropped, every other line → a paragraph.
func linesToADF(lines []string) map[string]any {
	content := make([]any, 0, len(lines))
	for _, ln := range lines {
		switch {
		case strings.TrimSpace(ln) == "---":
			content = append(content, map[string]any{"type": "rule"})
		case strings.TrimSpace(ln) == "":
			// drop empty lines
		default:
			content = append(content, map[string]any{
				"type":    "paragraph",
				"content": []any{map[string]any{"type": "text", "text": ln}},
			})
		}
	}
	if len(content) == 0 {
		// ADF requires at least one node — never post a structurally empty doc.
		content = append(content, map[string]any{"type": "paragraph"})
	}
	return map[string]any{"type": "doc", "version": 1, "content": content}
}
