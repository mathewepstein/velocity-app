package devs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mathewepstein/velocity/internal/config"
)

// jiraBulkBatchSize is the per-request cap on accountId query params. The
// Atlassian REST v3 user/bulk endpoint accepts up to 200, but 50 keeps URLs
// short and request budget predictable when called interactively.
const jiraBulkBatchSize = 50

// jiraBulkTimeout is the per-batch HTTP deadline. Short — the endpoint is
// lightweight; if any single batch takes longer the user is better off seeing
// an error than waiting.
var jiraBulkTimeout = 15 * time.Second

// ResolveJiraNames maps each accountId to its current displayName. Empty input
// returns an empty map. Account IDs that the API doesn't recognize are
// silently omitted — caller treats absence as "unknown" and falls back to the
// raw accountId in the UI.
//
// The bulk endpoint paginates its response; we follow `nextPage` links until
// every accountId in the batch has been visited.
func ResolveJiraNames(ctx context.Context, j config.JiraConfig, token string, accountIDs []string) (map[string]string, error) {
	if len(accountIDs) == 0 {
		return map[string]string{}, nil
	}
	if j.BaseURL == "" || j.Email == "" {
		return nil, errors.New("jira base_url or email missing from config")
	}
	if token == "" {
		return nil, errors.New("jira token missing")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	base := strings.TrimRight(j.BaseURL, "/")
	out := make(map[string]string, len(accountIDs))

	for _, batch := range chunkStrings(uniqueNonEmpty(accountIDs), jiraBulkBatchSize) {
		if err := fetchJiraBulk(ctx, base, j.Email, token, batch, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func fetchJiraBulk(ctx context.Context, baseURL, email, token string, batch []string, out map[string]string) error {
	q := url.Values{}
	for _, id := range batch {
		q.Add("accountId", id)
	}
	endpoint := baseURL + "/rest/api/3/user/bulk?" + q.Encode()

	for {
		body, next, err := getJiraPage(ctx, endpoint, email, token)
		if err != nil {
			return err
		}
		for _, u := range body.Values {
			if u.AccountID == "" {
				continue
			}
			name := u.DisplayName
			if name == "" {
				name = u.AccountID
			}
			out[u.AccountID] = name
		}
		if body.IsLast || next == "" {
			return nil
		}
		endpoint = next
	}
}

func getJiraPage(ctx context.Context, endpoint, email, token string) (jiraBulkUserResponse, string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, jiraBulkTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return jiraBulkUserResponse{}, "", err
	}
	req.SetBasicAuth(email, token)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return jiraBulkUserResponse{}, "", err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return jiraBulkUserResponse{}, "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return jiraBulkUserResponse{}, "", fmt.Errorf("jira user/bulk returned %d: %s", resp.StatusCode, truncateBytes(raw, 200))
	}

	var body jiraBulkUserResponse
	if err := json.Unmarshal(raw, &body); err != nil {
		return jiraBulkUserResponse{}, "", fmt.Errorf("parse user/bulk: %w", err)
	}
	return body, body.NextPage, nil
}

// jiraBulkUserResponse mirrors only the fields we consume from
// /rest/api/3/user/bulk. The endpoint paginates via a fully-qualified
// nextPage URL the response includes when more results remain.
type jiraBulkUserResponse struct {
	Values   []jiraBulkUser `json:"values"`
	IsLast   bool           `json:"isLast"`
	NextPage string         `json:"nextPage"`
}

type jiraBulkUser struct {
	AccountID   string `json:"accountId"`
	DisplayName string `json:"displayName"`
	Active      bool   `json:"active"`
}

func uniqueNonEmpty(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func chunkStrings(in []string, size int) [][]string {
	if size <= 0 {
		return [][]string{in}
	}
	out := make([][]string, 0, (len(in)+size-1)/size)
	for i := 0; i < len(in); i += size {
		end := i + size
		if end > len(in) {
			end = len(in)
		}
		out = append(out, in[i:end])
	}
	return out
}

func truncateBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
