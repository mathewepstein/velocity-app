package pull

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/mathewepstein/velocity/internal/cache"
)

// FetchPRFileChanges walks the same /repos/{owner}/{repo}/pulls/{number}/files
// endpoint as FetchPRFiles, but keeps the per-file status (added / modified /
// removed / renamed) and add/delete counts that the path-only Files list drops
// — the new-vs-modified-code signal. Files is deliberately left untouched for
// backward compat with the ~22k existing records; this is the richer parallel.
//
// On a definitively-not-fetchable response (404 / 410 / 422) it returns
// ErrPRUnreachable so the caller can persist an empty sentinel and never retry.
func (p *GithubPuller) FetchPRFileChanges(ctx context.Context, repo string, number int) ([]cache.FileChange, error) {
	const perPage = 100
	var all []cache.FileChange
	for page := 1; ; page++ {
		params := url.Values{}
		params.Set("per_page", fmt.Sprintf("%d", perPage))
		params.Set("page", fmt.Sprintf("%d", page))
		req, err := p.newReq(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d/files?%s", repo, number, params.Encode()))
		if err != nil {
			return nil, err
		}
		resp, body, err := p.client.do(ctx, req, nil)
		if err != nil {
			return nil, err
		}
		switch resp.StatusCode {
		case http.StatusNotFound, http.StatusGone, http.StatusUnprocessableEntity:
			return nil, fmt.Errorf("%s/pulls/%d: %d %s: %w", repo, number, resp.StatusCode, http.StatusText(resp.StatusCode), ErrPRUnreachable)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("GET /repos/%s/pulls/%d/files → %d: %s", repo, number, resp.StatusCode, truncate(body, 200))
		}
		var batch []ghPRFileDetail
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, fmt.Errorf("decode pr file changes %s/#%d: %w", repo, number, err)
		}
		for _, f := range batch {
			if f.Filename == "" {
				continue
			}
			all = append(all, cache.FileChange{
				Path:      f.Filename,
				Status:    f.Status,
				Additions: f.Additions,
				Deletions: f.Deletions,
			})
		}
		if len(batch) < perPage {
			return all, nil
		}
		if p.sleepBtwnPages > 0 {
			if err := sleepOrCancel(ctx, p.sleepBtwnPages); err != nil {
				return nil, err
			}
		}
	}
}

// HydratePRFileChanges fetches per-file change detail for one merged PR and
// writes it into pr, using the same nil-vs-empty Files sentinel: a permanent
// (unreachable) error persists an empty non-nil slice so the PR isn't retried
// forever, and returns ErrPRUnreachable so the caller can perm-skip it.
func (p *GithubPuller) HydratePRFileChanges(ctx context.Context, pr *cache.GitHubPR) error {
	changes, err := p.FetchPRFileChanges(ctx, pr.Repo, pr.Number)
	if err != nil {
		if errors.Is(err, ErrPRUnreachable) {
			pr.FileChanges = []cache.FileChange{}
		}
		return err
	}
	if pr.FileChanges = changes; pr.FileChanges == nil {
		pr.FileChanges = []cache.FileChange{}
	}
	return nil
}

// ghPRFileDetail is the richer subset of the /pulls/{number}/files response:
// path plus status and per-file add/delete counts.
type ghPRFileDetail struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"` // added | modified | removed | renamed | copied | changed | unchanged
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}
