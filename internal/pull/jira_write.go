package pull

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
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
	baseURL string
	email   string
	token   string
	// storyPointsOverride is an OPTIONAL pin from config. The write field is
	// resolved per ticket from the issue's edit screen (editmeta), which is the
	// real source of truth and can differ by project/issue type; this only forces
	// a specific field when set AND settable, for multi-field instances.
	storyPointsOverride string
	client              *backoffClient
}

// NewJiraWriter builds a writer from the Jira config + keychain token. The Story
// Points field is resolved per ticket at write time from the issue's editmeta;
// j.Fields.StoryPoints is kept only as an optional override, so an empty or
// even stale config value no longer blocks posting.
func NewJiraWriter(j config.JiraConfig, token string) *JiraWriter {
	return &JiraWriter{
		baseURL:             strings.TrimRight(j.BaseURL, "/"),
		email:               j.Email,
		token:               token,
		storyPointsOverride: j.Fields.StoryPoints,
		client:              newBackoffClient(),
	}
}

func (w *JiraWriter) auth(req *http.Request) {
	req.SetBasicAuth(w.email, w.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
}

// SetStoryPoints writes points to issue key's Story Points field
// (PUT /rest/api/3/issue/{key}, 204 on success). The field is resolved from the
// issue's own edit screen, so it works across projects/issue types without any
// configuration and without the operator hand-mapping field IDs.
func (w *JiraWriter) SetStoryPoints(ctx context.Context, key string, points float64) error {
	fieldID, err := w.resolveStoryPointsField(ctx, key)
	if err != nil {
		return fmt.Errorf("set story points on %s: %w", key, err)
	}
	payload := map[string]any{
		"fields": map[string]any{fieldID: points},
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
		return fmt.Errorf("set story points on %s (field %s): %w", key, fieldID, err)
	}
	return nil
}

// resolveStoryPointsField determines which field to write story points to on an
// issue. The issue's edit screen (editmeta) is the source of truth — the correct
// field varies by project and issue type, so a single configured ID is only a
// hint. Precedence: a configured override that is actually settable wins;
// otherwise the sole settable Story Points field on the issue. An unreadable
// editmeta falls back to the configured override. "None settable" and genuine
// ambiguity (several settable Story Points fields, none configured) are explicit,
// actionable errors rather than a raw downstream 400.
func (w *JiraWriter) resolveStoryPointsField(ctx context.Context, key string) (string, error) {
	settable, ok := w.fetchEditMeta(ctx, key)
	if !ok {
		if w.storyPointsOverride != "" {
			return w.storyPointsOverride, nil // can't introspect; trust the config hint
		}
		return "", fmt.Errorf("could not read %s's edit screen to locate the Story Points field, and no [jira.fields].story_points override is set", key)
	}
	// An explicit, still-valid config override wins — lets a user pin a specific
	// field in a multi-field instance.
	if w.storyPointsOverride != "" {
		if _, valid := settable[w.storyPointsOverride]; valid {
			return w.storyPointsOverride, nil
		}
	}
	ids := storyPointsFieldIDs(settable)
	switch len(ids) {
	case 1:
		return ids[0], nil
	case 0:
		note := ""
		if w.storyPointsOverride != "" {
			note = fmt.Sprintf(" (configured %s is not on it)", w.storyPointsOverride)
		}
		return "", fmt.Errorf("no Story Points field is settable on %s's edit screen%s; check the project's field configuration", key, note)
	default:
		labels := make([]string, len(ids))
		for i, id := range ids {
			labels[i] = fieldLabel(id, settable[id])
		}
		return "", fmt.Errorf("%s has multiple settable Story Points fields (%s); set [jira.fields].story_points to pick one", key, strings.Join(labels, ", "))
	}
}

type editMetaField struct {
	name   string
	custom string // schema.custom type, when present
}

// fetchEditMeta returns the fields settable on an issue (editmeta operations
// include "set"), keyed by field ID. ok is false on any read/parse failure so
// callers fall back gracefully rather than masking the original error.
func (w *JiraWriter) fetchEditMeta(ctx context.Context, key string) (map[string]editMetaField, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.baseURL+"/rest/api/3/issue/"+url.PathEscape(key)+"/editmeta", nil)
	if err != nil {
		return nil, false
	}
	w.auth(req)
	var resp struct {
		Fields map[string]struct {
			Name   string `json:"name"`
			Schema struct {
				Custom string `json:"custom"`
			} `json:"schema"`
			Operations []string `json:"operations"`
		} `json:"fields"`
	}
	if err := w.client.doJSON(ctx, req, nil, &resp); err != nil {
		return nil, false
	}
	out := make(map[string]editMetaField, len(resp.Fields))
	for id, f := range resp.Fields {
		if !slices.Contains(f.Operations, "set") {
			continue
		}
		out[id] = editMetaField{name: f.Name, custom: f.Schema.Custom}
	}
	return out, true
}

// storyPointsFieldIDs returns the settable field IDs that look like a Story
// Points field — by standard name ("Story Points" / "Story point estimate") or
// by the story-points schema custom type — sorted for stable, deterministic
// selection and error output.
func storyPointsFieldIDs(settable map[string]editMetaField) []string {
	var ids []string
	for id, f := range settable {
		n := strings.ToLower(strings.TrimSpace(f.name))
		if n == "story points" || n == "story point estimate" || strings.Contains(f.custom, "story-points") {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// fieldLabel renders a field as `id ("Name")` for disambiguation messages.
func fieldLabel(id string, f editMetaField) string {
	return fmt.Sprintf("%s (%q)", id, f.name)
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
