package pull

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
)

// deepThreadMinComments is the comment count (root + replies) at which a review
// thread counts as a "deep thread" — a genuine back-and-forth, not a drive-by
// comment with a single ack. Root + 2 replies = 3 comments.
const deepThreadMinComments = 3

// ReviewCommentData is the hydrated inline-review-comment signal for one PR:
// the mapped comments plus the derived deep-thread count (computed from the raw
// API ids, which the cache shape doesn't retain).
type ReviewCommentData struct {
	Comments    []cache.ReviewComment
	DeepThreads int
}

// FetchReviewComments walks GitHub's paginated
// /repos/{owner}/{repo}/pulls/{number}/comments endpoint (inline, diff-anchored
// review comments — distinct from issue-level PR comments) and returns the
// mapped comments plus the number of deep threads. Callers should only invoke
// it for merged PRs.
//
// On a definitively-not-fetchable response (404 / 410 / 422) it returns
// ErrPRUnreachable so the caller can persist an empty sentinel and never retry.
func (p *GithubPuller) FetchReviewComments(ctx context.Context, repo string, number int) (ReviewCommentData, error) {
	const perPage = 100
	var raw []ghReviewComment
	for page := 1; ; page++ {
		params := url.Values{}
		params.Set("per_page", fmt.Sprintf("%d", perPage))
		params.Set("page", fmt.Sprintf("%d", page))
		req, err := p.newReq(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d/comments?%s", repo, number, params.Encode()))
		if err != nil {
			return ReviewCommentData{}, err
		}
		resp, body, err := p.client.do(ctx, req, nil)
		if err != nil {
			return ReviewCommentData{}, err
		}
		switch resp.StatusCode {
		case http.StatusNotFound, http.StatusGone, http.StatusUnprocessableEntity:
			return ReviewCommentData{}, fmt.Errorf("%s/pulls/%d/comments: %d %s: %w", repo, number, resp.StatusCode, http.StatusText(resp.StatusCode), ErrPRUnreachable)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return ReviewCommentData{}, fmt.Errorf("GET /repos/%s/pulls/%d/comments → %d: %s", repo, number, resp.StatusCode, truncate(body, 200))
		}
		var batch []ghReviewComment
		if err := json.Unmarshal(body, &batch); err != nil {
			return ReviewCommentData{}, fmt.Errorf("decode review comments %s/#%d: %w", repo, number, err)
		}
		raw = append(raw, batch...)
		if len(batch) < perPage {
			break
		}
		if p.sleepBtwnPages > 0 {
			if err := sleepOrCancel(ctx, p.sleepBtwnPages); err != nil {
				return ReviewCommentData{}, err
			}
		}
	}

	out := make([]cache.ReviewComment, 0, len(raw))
	for _, c := range raw {
		out = append(out, cache.ReviewComment{
			Author:    c.User.Login,
			Path:      c.Path,
			InReplyTo: c.InReplyTo,
			Created:   c.CreatedAt,
			Body:      c.Body,
		})
	}
	return ReviewCommentData{Comments: out, DeepThreads: countDeepThreads(raw)}, nil
}

// HydrateReviewComments fetches inline review comments for one merged PR and
// writes the raw comments + derived counts into pr, using the same nil-vs-empty
// sentinel as Files: a permanent (unreachable) error persists an empty
// non-nil slice so the PR isn't retried forever, and returns ErrPRUnreachable
// so the caller can classify it as a perm-skip.
func (p *GithubPuller) HydrateReviewComments(ctx context.Context, pr *cache.GitHubPR) error {
	data, err := p.FetchReviewComments(ctx, pr.Repo, pr.Number)
	if err != nil {
		if errors.Is(err, ErrPRUnreachable) {
			pr.ReviewComments = []cache.ReviewComment{}
			pr.InlineComments = 0
			pr.DeepThreads = 0
		}
		return err
	}
	if pr.ReviewComments = data.Comments; pr.ReviewComments == nil {
		pr.ReviewComments = []cache.ReviewComment{}
	}
	pr.InlineComments = len(pr.ReviewComments)
	pr.DeepThreads = data.DeepThreads
	return nil
}

// countDeepThreads groups review comments into threads by following
// in_reply_to_id chains to each thread's root, then counts threads with
// deepThreadMinComments or more comments. Needs the raw comment ids, which the
// cache shape doesn't retain — so it runs on the API response, not the cache.
func countDeepThreads(raw []ghReviewComment) int {
	if len(raw) == 0 {
		return 0
	}
	parentOf := make(map[int]int, len(raw))
	for _, c := range raw {
		parentOf[c.ID] = c.InReplyTo
	}
	root := func(id int) int {
		seen := map[int]bool{}
		for {
			p, ok := parentOf[id]
			if !ok || p == 0 || seen[id] {
				return id
			}
			seen[id] = true
			id = p
		}
	}
	counts := map[int]int{}
	for _, c := range raw {
		counts[root(c.ID)]++
	}
	deep := 0
	for _, n := range counts {
		if n >= deepThreadMinComments {
			deep++
		}
	}
	return deep
}

// ghReviewComment is the subset of the /pulls/{n}/comments response we need.
// in_reply_to_id is 0 (absent) on a thread's root comment.
type ghReviewComment struct {
	ID        int `json:"id"`
	InReplyTo int `json:"in_reply_to_id"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
	Path      string    `json:"path"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}
