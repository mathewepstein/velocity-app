package pull

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
)

// FetchPRCommits walks /repos/{owner}/{repo}/pulls/{number}/commits and returns
// the PR's commit membership: the SHAs (+ per-commit author and parent count)
// that the PR shipped. This is the data the cache never stored before — it
// unlocks commit-overlap (S4: did this PR re-ship already-merged commits?) and
// author-diversity (S6: how many distinct authors does the diff carry?) for
// promotion-PR detection. Callers should only invoke it for merged PRs.
//
// On a definitively-not-fetchable response (404 / 410 / 422) it returns
// ErrPRUnreachable so the caller can persist an empty sentinel and never retry,
// mirroring FetchPRFiles / FetchPRFileChanges.
func (p *GithubPuller) FetchPRCommits(ctx context.Context, repo string, number int) ([]cache.PRCommit, error) {
	const perPage = 100
	var all []cache.PRCommit
	for page := 1; ; page++ {
		params := url.Values{}
		params.Set("per_page", fmt.Sprintf("%d", perPage))
		params.Set("page", fmt.Sprintf("%d", page))
		req, err := p.newReq(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d/commits?%s", repo, number, params.Encode()))
		if err != nil {
			return nil, err
		}
		resp, body, err := p.client.do(ctx, req, nil)
		if err != nil {
			return nil, err
		}
		switch resp.StatusCode {
		case http.StatusNotFound, http.StatusGone, http.StatusUnprocessableEntity:
			return nil, fmt.Errorf("%s/pulls/%d/commits: %d %s: %w", repo, number, resp.StatusCode, http.StatusText(resp.StatusCode), ErrPRUnreachable)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("GET /repos/%s/pulls/%d/commits → %d: %s", repo, number, resp.StatusCode, truncate(body, 200))
		}
		var batch []ghPRCommit
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, fmt.Errorf("decode pr commits %s/#%d: %w", repo, number, err)
		}
		for _, c := range batch {
			pc := cache.PRCommit{
				SHA:         c.SHA,
				Author:      c.Author.Login,
				AuthorName:  c.Commit.Author.Name,
				AuthorEmail: c.Commit.Author.Email,
				Authored:    c.Commit.Author.Date.UTC(),
				ParentCount: len(c.Parents),
			}
			all = append(all, pc)
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

// ghPRCommit is the subset of the /pulls/{n}/commits response we keep: the SHA,
// the linked GitHub author (when one exists), the raw commit-author identity as
// fallback, and the parent shas (count distinguishes a merge commit).
type ghPRCommit struct {
	SHA    string `json:"sha"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	Commit struct {
		Author struct {
			Name  string    `json:"name"`
			Email string    `json:"email"`
			Date  time.Time `json:"date"`
		} `json:"author"`
	} `json:"commit"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
}
