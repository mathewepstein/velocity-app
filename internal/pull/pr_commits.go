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

// HydratePRMeta backfills the PR-detail-body fields (base/head ref+sha+repo,
// merged_by, commit/file counts, merge_commit_sha, draft, auto_merge, updated,
// author_association, labels, assignees, requested_reviewers) and the PR→commit
// membership onto an already-cached merged PR — the lean alternative to a full
// re-crawl. Exactly two API calls: /pulls/{n}/commits and /pulls/{n}; it does
// NOT re-fetch the files/comments/commit-search payloads the original crawl
// already stored.
//
// Atomicity contract (the backfill runner checkpoints the whole slice on a
// transient error, so a partial mutation would be skipped forever on resume):
// pr is mutated ONLY on full success, or — for a permanently-unreachable PR —
// the Commits sentinel alone is set and ErrPRUnreachable returned (PermSkip). A
// transient error leaves pr untouched so the next run retries it. Commits is
// fetched first as the reachability gate (it cleanly maps 404/410/422 to
// ErrPRUnreachable, unlike the detail call).
func (p *GithubPuller) HydratePRMeta(ctx context.Context, pr *cache.GitHubPR) error {
	commits, err := p.FetchPRCommits(ctx, pr.Repo, pr.Number)
	if err != nil {
		if errors.Is(err, ErrPRUnreachable) {
			pr.Commits = []cache.PRCommit{} // permanent: mark so it's never retried
		}
		return err // transient: pr left untouched for a clean resume
	}
	detail, err := p.fetchPRDetail(ctx, pr.Repo, pr.Number)
	if err != nil {
		return err // transient (commits just succeeded → PR reachable); pr untouched
	}

	// Both calls succeeded — apply atomically.
	if commits == nil {
		commits = []cache.PRCommit{}
	}
	pr.Commits = commits
	pr.BaseBranch = detail.Base.Ref
	pr.BaseSHA = detail.Base.SHA
	pr.HeadSHA = detail.Head.SHA
	pr.HeadRepo = detail.Head.repoName()
	pr.BaseRepo = detail.Base.repoName()
	pr.CommitCount = detail.Commits
	pr.ChangedFiles = detail.ChangedFiles
	pr.MergeCommitSHA = detail.MergeCommit
	pr.Draft = detail.Draft
	pr.AutoMerge = detail.AutoMerge != nil
	pr.AuthorAssociation = detail.AuthorAssoc
	pr.Labels = ghLabelNames(detail.Labels)
	pr.Assignees = ghUserLogins(detail.Assignees)
	pr.RequestedReviewers = ghUserLogins(detail.RequestedReviewers)
	if detail.MergedBy != nil {
		pr.MergedBy = detail.MergedBy.Login
	}
	if detail.UpdatedAt != nil {
		t := detail.UpdatedAt.UTC()
		pr.Updated = &t
	}
	return nil
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
