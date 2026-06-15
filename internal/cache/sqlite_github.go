package cache

import (
	"database/sql"
	"fmt"
)

func prKey(repo string, number int) string { return fmt.Sprintf("%s#%d", repo, number) }

func (s *sqliteStore) WriteGitHubPRs(scope string, m Month, recs []GitHubPR) error {
	tx, err := s.beginCellWrite(scope, m, "github_prs", "pr_issue_keys", "pr_files", "pr_review_comments", "pr_file_changes",
		"pr_commits", "pr_labels", "pr_assignees", "pr_requested_reviewers")
	if err != nil {
		return err
	}
	defer tx.Rollback()

	insPR, err := tx.Prepare(`INSERT INTO github_prs
		(scope, month, ord, number, repo, title, body, state, author, branch,
		 created, merged, closed, additions, deletions, inline_comments, deep_threads,
		 files_fetched, review_comments_fetched, file_changes_fetched,
		 base_branch, base_sha, head_sha, head_repo, base_repo, merged_by, commit_count,
		 changed_files, merge_commit_sha, draft, auto_merge, updated, author_association, commits_fetched)
		VALUES (?,?,?,?,?,?,?,?,?,?, ?,?,?,?,?,?,?, ?,?,?, ?,?,?,?,?,?,?, ?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer insPR.Close()
	insKey, _ := tx.Prepare(`INSERT INTO pr_issue_keys (scope, month, repo, number, ord, value) VALUES (?,?,?,?,?,?)`)
	defer insKey.Close()
	insFile, _ := tx.Prepare(`INSERT INTO pr_files (scope, month, repo, number, ord, path) VALUES (?,?,?,?,?,?)`)
	defer insFile.Close()
	insRC, _ := tx.Prepare(`INSERT INTO pr_review_comments
		(scope, month, repo, number, ord, author, path, in_reply_to, created, body,
		 comment_id, review_id, commit_id, line, original_line, updated, author_association)
		VALUES (?,?,?,?,?,?,?,?,?,?, ?,?,?,?,?,?,?)`)
	defer insRC.Close()
	insFC, _ := tx.Prepare(`INSERT INTO pr_file_changes
		(scope, month, repo, number, ord, path, status, additions, deletions, previous_filename, blob_sha)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`)
	defer insFC.Close()
	insCommit, _ := tx.Prepare(`INSERT INTO pr_commits
		(scope, month, repo, number, ord, sha, author, author_name, author_email, authored, parent_count)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`)
	defer insCommit.Close()
	insLabel, _ := tx.Prepare(`INSERT INTO pr_labels (scope, month, repo, number, ord, value) VALUES (?,?,?,?,?,?)`)
	defer insLabel.Close()
	insAssignee, _ := tx.Prepare(`INSERT INTO pr_assignees (scope, month, repo, number, ord, value) VALUES (?,?,?,?,?,?)`)
	defer insAssignee.Close()
	insReviewer, _ := tx.Prepare(`INSERT INTO pr_requested_reviewers (scope, month, repo, number, ord, value) VALUES (?,?,?,?,?,?)`)
	defer insReviewer.Close()

	for i := range recs {
		r := &recs[i]
		if _, err := insPR.Exec(scope, m.String(), i, r.Number, r.Repo, r.Title, r.Body, r.State, r.Author, r.Branch,
			fmtTime(r.Created), fmtTimePtr(r.Merged), fmtTimePtr(r.Closed), r.Additions, r.Deletions, r.InlineComments, r.DeepThreads,
			boolToInt(r.Files != nil), boolToInt(r.ReviewComments != nil), boolToInt(r.FileChanges != nil),
			r.BaseBranch, r.BaseSHA, r.HeadSHA, r.HeadRepo, r.BaseRepo, r.MergedBy, r.CommitCount,
			r.ChangedFiles, r.MergeCommitSHA, boolToInt(r.Draft), boolToInt(r.AutoMerge), fmtTimePtr(r.Updated), r.AuthorAssociation,
			boolToInt(r.Commits != nil)); err != nil {
			return fmt.Errorf("insert PR %s#%d: %w", r.Repo, r.Number, err)
		}
		for j, v := range r.IssueKeys {
			if _, err := insKey.Exec(scope, m.String(), r.Repo, r.Number, j, v); err != nil {
				return err
			}
		}
		for j, v := range r.Files {
			if _, err := insFile.Exec(scope, m.String(), r.Repo, r.Number, j, v); err != nil {
				return err
			}
		}
		for j, c := range r.ReviewComments {
			if _, err := insRC.Exec(scope, m.String(), r.Repo, r.Number, j, c.Author, c.Path, c.InReplyTo, fmtTime(c.Created), c.Body,
				c.ID, c.ReviewID, c.CommitID, c.Line, c.OriginalLine, fmtTimePtr(c.Updated), c.AuthorAssociation); err != nil {
				return err
			}
		}
		for j, c := range r.FileChanges {
			if _, err := insFC.Exec(scope, m.String(), r.Repo, r.Number, j, c.Path, c.Status, c.Additions, c.Deletions,
				c.PreviousFilename, c.BlobSHA); err != nil {
				return err
			}
		}
		for j, c := range r.Commits {
			if _, err := insCommit.Exec(scope, m.String(), r.Repo, r.Number, j, c.SHA, c.Author, c.AuthorName, c.AuthorEmail,
				fmtTime(c.Authored), c.ParentCount); err != nil {
				return err
			}
		}
		for j, v := range r.Labels {
			if _, err := insLabel.Exec(scope, m.String(), r.Repo, r.Number, j, v); err != nil {
				return err
			}
		}
		for j, v := range r.Assignees {
			if _, err := insAssignee.Exec(scope, m.String(), r.Repo, r.Number, j, v); err != nil {
				return err
			}
		}
		for j, v := range r.RequestedReviewers {
			if _, err := insReviewer.Exec(scope, m.String(), r.Repo, r.Number, j, v); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *sqliteStore) ReadGitHubPRs(scope string, m Month) ([]GitHubPR, error) {
	ok, err := s.cellExists(SourceGitHubPRs, scope, m)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, missErr(SourceGitHubPRs, scope, m)
	}

	rows, err := s.db.Query(`SELECT ord, number, repo, title, body, state, author, branch,
		created, merged, closed, additions, deletions, inline_comments, deep_threads,
		files_fetched, review_comments_fetched, file_changes_fetched,
		base_branch, base_sha, head_sha, head_repo, base_repo, merged_by, commit_count,
		changed_files, merge_commit_sha, draft, auto_merge, updated, author_association, commits_fetched
		FROM github_prs WHERE scope=? AND month=? ORDER BY ord`, scope, m.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []GitHubPR{}
	// Index, not pointer (out grows via append; see ReadJiraIssues).
	byKey := map[string]int{}
	for rows.Next() {
		var (
			ord                          int
			created                      string
			merged, closed, updated      sql.NullString
			baseBranch, baseSHA, headSHA sql.NullString
			headRepo, baseRepo, mergedBy sql.NullString
			mergeCommitSHA, authorAssoc  sql.NullString
			filesF, rcF, fcF, commitsF   int
			draft, autoMerge             int
			r                            GitHubPR
		)
		if err := rows.Scan(&ord, &r.Number, &r.Repo, &r.Title, &r.Body, &r.State, &r.Author, &r.Branch,
			&created, &merged, &closed, &r.Additions, &r.Deletions, &r.InlineComments, &r.DeepThreads,
			&filesF, &rcF, &fcF,
			&baseBranch, &baseSHA, &headSHA, &headRepo, &baseRepo, &mergedBy, &r.CommitCount,
			&r.ChangedFiles, &mergeCommitSHA, &draft, &autoMerge, &updated, &authorAssoc, &commitsF); err != nil {
			return nil, err
		}
		if r.Created, err = parseTime(created); err != nil {
			return nil, err
		}
		if r.Merged, err = parseTimePtr(merged); err != nil {
			return nil, err
		}
		if r.Closed, err = parseTimePtr(closed); err != nil {
			return nil, err
		}
		if r.Updated, err = parseTimePtr(updated); err != nil {
			return nil, err
		}
		r.BaseBranch = baseBranch.String
		r.BaseSHA = baseSHA.String
		r.HeadSHA = headSHA.String
		r.HeadRepo = headRepo.String
		r.BaseRepo = baseRepo.String
		r.MergedBy = mergedBy.String
		r.MergeCommitSHA = mergeCommitSHA.String
		r.AuthorAssociation = authorAssoc.String
		r.Draft = draft != 0
		r.AutoMerge = autoMerge != 0
		if filesF != 0 {
			r.Files = []string{}
		}
		if rcF != 0 {
			r.ReviewComments = []ReviewComment{}
		}
		if fcF != 0 {
			r.FileChanges = []FileChange{}
		}
		if commitsF != 0 {
			r.Commits = []PRCommit{}
		}
		out = append(out, r)
		byKey[prKey(r.Repo, r.Number)] = len(out) - 1
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// IssueKeys (omitempty: nil when empty) + Files (sentinel, pre-seeded).
	if err := s.loadPRStrings(`SELECT repo, number, value FROM pr_issue_keys WHERE scope=? AND month=? ORDER BY repo, number, ord`,
		scope, m, out, byKey, func(p *GitHubPR, v string) { p.IssueKeys = append(p.IssueKeys, v) }); err != nil {
		return nil, err
	}
	if err := s.loadPRStrings(`SELECT repo, number, path FROM pr_files WHERE scope=? AND month=? ORDER BY repo, number, ord`,
		scope, m, out, byKey, func(p *GitHubPR, v string) { p.Files = append(p.Files, v) }); err != nil {
		return nil, err
	}
	if err := s.loadPRStrings(`SELECT repo, number, value FROM pr_labels WHERE scope=? AND month=? ORDER BY repo, number, ord`,
		scope, m, out, byKey, func(p *GitHubPR, v string) { p.Labels = append(p.Labels, v) }); err != nil {
		return nil, err
	}
	if err := s.loadPRStrings(`SELECT repo, number, value FROM pr_assignees WHERE scope=? AND month=? ORDER BY repo, number, ord`,
		scope, m, out, byKey, func(p *GitHubPR, v string) { p.Assignees = append(p.Assignees, v) }); err != nil {
		return nil, err
	}
	if err := s.loadPRStrings(`SELECT repo, number, value FROM pr_requested_reviewers WHERE scope=? AND month=? ORDER BY repo, number, ord`,
		scope, m, out, byKey, func(p *GitHubPR, v string) { p.RequestedReviewers = append(p.RequestedReviewers, v) }); err != nil {
		return nil, err
	}

	rcRows, err := s.db.Query(`SELECT repo, number, author, path, in_reply_to, created, body,
		comment_id, review_id, commit_id, line, original_line, updated, author_association
		FROM pr_review_comments WHERE scope=? AND month=? ORDER BY repo, number, ord`, scope, m.String())
	if err != nil {
		return nil, err
	}
	for rcRows.Next() {
		var repo, created string
		var num int
		var commitID, authorAssoc, updated sql.NullString
		var commentID, reviewID, line, origLine sql.NullInt64
		var c ReviewComment
		if err := rcRows.Scan(&repo, &num, &c.Author, &c.Path, &c.InReplyTo, &created, &c.Body,
			&commentID, &reviewID, &commitID, &line, &origLine, &updated, &authorAssoc); err != nil {
			rcRows.Close()
			return nil, err
		}
		if c.Created, err = parseTime(created); err != nil {
			rcRows.Close()
			return nil, err
		}
		if c.Updated, err = parseTimePtr(updated); err != nil {
			rcRows.Close()
			return nil, err
		}
		c.ID = int(commentID.Int64)
		c.ReviewID = int(reviewID.Int64)
		c.CommitID = commitID.String
		c.Line = int(line.Int64)
		c.OriginalLine = int(origLine.Int64)
		c.AuthorAssociation = authorAssoc.String
		if i, ok := byKey[prKey(repo, num)]; ok {
			out[i].ReviewComments = append(out[i].ReviewComments, c)
		}
	}
	rcRows.Close()
	if err := rcRows.Err(); err != nil {
		return nil, err
	}

	fcRows, err := s.db.Query(`SELECT repo, number, path, status, additions, deletions, previous_filename, blob_sha
		FROM pr_file_changes WHERE scope=? AND month=? ORDER BY repo, number, ord`, scope, m.String())
	if err != nil {
		return nil, err
	}
	for fcRows.Next() {
		var repo string
		var num int
		var prevName, blobSHA sql.NullString
		var c FileChange
		if err := fcRows.Scan(&repo, &num, &c.Path, &c.Status, &c.Additions, &c.Deletions, &prevName, &blobSHA); err != nil {
			fcRows.Close()
			return nil, err
		}
		c.PreviousFilename = prevName.String
		c.BlobSHA = blobSHA.String
		if i, ok := byKey[prKey(repo, num)]; ok {
			out[i].FileChanges = append(out[i].FileChanges, c)
		}
	}
	fcRows.Close()
	if err := fcRows.Err(); err != nil {
		return nil, err
	}

	pcRows, err := s.db.Query(`SELECT repo, number, sha, author, author_name, author_email, authored, parent_count
		FROM pr_commits WHERE scope=? AND month=? ORDER BY repo, number, ord`, scope, m.String())
	if err != nil {
		return nil, err
	}
	for pcRows.Next() {
		var repo string
		var num int
		var author, authorName, authorEmail, authored sql.NullString
		var c PRCommit
		if err := pcRows.Scan(&repo, &num, &c.SHA, &author, &authorName, &authorEmail, &authored, &c.ParentCount); err != nil {
			pcRows.Close()
			return nil, err
		}
		c.Author = author.String
		c.AuthorName = authorName.String
		c.AuthorEmail = authorEmail.String
		if authored.Valid && authored.String != "" {
			if c.Authored, err = parseTime(authored.String); err != nil {
				pcRows.Close()
				return nil, err
			}
		}
		if i, ok := byKey[prKey(repo, num)]; ok {
			out[i].Commits = append(out[i].Commits, c)
		}
	}
	pcRows.Close()
	if err := pcRows.Err(); err != nil {
		return nil, err
	}

	return out, nil
}

// loadPRStrings runs a (repo, number, value) query and feeds each row to add via
// the matching parent PR. out must be the finalized (non-growing) slice so
// &out[i] is stable.
func (s *sqliteStore) loadPRStrings(query, scope string, m Month, out []GitHubPR, byKey map[string]int, add func(p *GitHubPR, value string)) error {
	rows, err := s.db.Query(query, scope, m.String())
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var repo, v string
		var num int
		if err := rows.Scan(&repo, &num, &v); err != nil {
			return err
		}
		if i, ok := byKey[prKey(repo, num)]; ok {
			add(&out[i], v)
		}
	}
	return rows.Err()
}

func (s *sqliteStore) WriteGitHubCommits(scope string, m Month, recs []GitHubCommit) error {
	tx, err := s.beginCellWrite(scope, m, "github_commits", "commit_issue_keys")
	if err != nil {
		return err
	}
	defer tx.Rollback()

	insC, err := tx.Prepare(`INSERT INTO github_commits
		(scope, month, ord, sha, repo, author, message, committed, additions, deletions,
		 authored, committer, parent_count, comment_count)
		VALUES (?,?,?,?,?,?,?,?,?,?, ?,?,?,?)`)
	if err != nil {
		return err
	}
	defer insC.Close()
	insKey, _ := tx.Prepare(`INSERT INTO commit_issue_keys (scope, month, repo, sha, ord, value) VALUES (?,?,?,?,?,?)`)
	defer insKey.Close()

	for i := range recs {
		r := &recs[i]
		if _, err := insC.Exec(scope, m.String(), i, r.SHA, r.Repo, r.Author, r.Message, fmtTime(r.Committed), r.Additions, r.Deletions,
			fmtTimePtr(r.Authored), r.Committer, r.ParentCount, r.CommentCount); err != nil {
			return fmt.Errorf("insert commit %s: %w", r.SHA, err)
		}
		for j, v := range r.IssueKeys {
			if _, err := insKey.Exec(scope, m.String(), r.Repo, r.SHA, j, v); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *sqliteStore) ReadGitHubCommits(scope string, m Month) ([]GitHubCommit, error) {
	ok, err := s.cellExists(SourceGitHubCommits, scope, m)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, missErr(SourceGitHubCommits, scope, m)
	}

	rows, err := s.db.Query(`SELECT ord, sha, repo, author, message, committed, additions, deletions,
		authored, committer, parent_count, comment_count
		FROM github_commits WHERE scope=? AND month=? ORDER BY ord`, scope, m.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []GitHubCommit{}
	bySHA := map[string]int{} // index, not pointer (out grows via append)
	for rows.Next() {
		var ord int
		var committed string
		var authored, committer sql.NullString
		var r GitHubCommit
		if err := rows.Scan(&ord, &r.SHA, &r.Repo, &r.Author, &r.Message, &committed, &r.Additions, &r.Deletions,
			&authored, &committer, &r.ParentCount, &r.CommentCount); err != nil {
			return nil, err
		}
		if r.Committed, err = parseTime(committed); err != nil {
			return nil, err
		}
		if r.Authored, err = parseTimePtr(authored); err != nil {
			return nil, err
		}
		r.Committer = committer.String
		out = append(out, r)
		bySHA[prKey(r.Repo, 0)+r.SHA] = len(out) - 1
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	kRows, err := s.db.Query(`SELECT repo, sha, value FROM commit_issue_keys WHERE scope=? AND month=? ORDER BY repo, sha, ord`, scope, m.String())
	if err != nil {
		return nil, err
	}
	defer kRows.Close()
	for kRows.Next() {
		var repo, sha, v string
		if err := kRows.Scan(&repo, &sha, &v); err != nil {
			return nil, err
		}
		if i, ok := bySHA[prKey(repo, 0)+sha]; ok {
			out[i].IssueKeys = append(out[i].IssueKeys, v)
		}
	}
	if err := kRows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *sqliteStore) WriteGitHubReviews(scope string, m Month, recs []GitHubReview) error {
	tx, err := s.beginCellWrite(scope, m, "github_reviews")
	if err != nil {
		return err
	}
	defer tx.Rollback()

	ins, err := tx.Prepare(`INSERT INTO github_reviews
		(scope, month, ord, pr_number, repo, reviewer, state, submitted, review_id, body, commit_id, author_association)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer ins.Close()
	for i := range recs {
		r := &recs[i]
		if _, err := ins.Exec(scope, m.String(), i, r.PRNumber, r.Repo, r.Reviewer, r.State, fmtTime(r.Submitted),
			r.ReviewID, r.Body, r.CommitID, r.AuthorAssociation); err != nil {
			return fmt.Errorf("insert review %s#%d: %w", r.Repo, r.PRNumber, err)
		}
	}
	return tx.Commit()
}

func (s *sqliteStore) ReadGitHubReviews(scope string, m Month) ([]GitHubReview, error) {
	ok, err := s.cellExists(SourceGitHubReviews, scope, m)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, missErr(SourceGitHubReviews, scope, m)
	}

	rows, err := s.db.Query(`SELECT pr_number, repo, reviewer, state, submitted,
		review_id, body, commit_id, author_association
		FROM github_reviews WHERE scope=? AND month=? ORDER BY ord`, scope, m.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []GitHubReview{}
	for rows.Next() {
		var submitted string
		var reviewID sql.NullInt64
		var body, commitID, authorAssoc sql.NullString
		var r GitHubReview
		if err := rows.Scan(&r.PRNumber, &r.Repo, &r.Reviewer, &r.State, &submitted,
			&reviewID, &body, &commitID, &authorAssoc); err != nil {
			return nil, err
		}
		if r.Submitted, err = parseTime(submitted); err != nil {
			return nil, err
		}
		r.ReviewID = int(reviewID.Int64)
		r.Body = body.String
		r.CommitID = commitID.String
		r.AuthorAssociation = authorAssoc.String
		out = append(out, r)
	}
	return out, rows.Err()
}
