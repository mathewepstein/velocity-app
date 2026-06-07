package cache

import (
	"database/sql"
	"fmt"
)

func prKey(repo string, number int) string { return fmt.Sprintf("%s#%d", repo, number) }

func (s *sqliteStore) WriteGitHubPRs(scope string, m Month, recs []GitHubPR) error {
	tx, err := s.beginCellWrite(scope, m, "github_prs", "pr_issue_keys", "pr_files", "pr_review_comments", "pr_file_changes")
	if err != nil {
		return err
	}
	defer tx.Rollback()

	insPR, err := tx.Prepare(`INSERT INTO github_prs
		(scope, month, ord, number, repo, title, body, state, author, branch,
		 created, merged, closed, additions, deletions, inline_comments, deep_threads,
		 files_fetched, review_comments_fetched, file_changes_fetched)
		VALUES (?,?,?,?,?,?,?,?,?,?, ?,?,?,?,?,?,?, ?,?,?)`)
	if err != nil {
		return err
	}
	defer insPR.Close()
	insKey, _ := tx.Prepare(`INSERT INTO pr_issue_keys (scope, month, repo, number, ord, value) VALUES (?,?,?,?,?,?)`)
	defer insKey.Close()
	insFile, _ := tx.Prepare(`INSERT INTO pr_files (scope, month, repo, number, ord, path) VALUES (?,?,?,?,?,?)`)
	defer insFile.Close()
	insRC, _ := tx.Prepare(`INSERT INTO pr_review_comments (scope, month, repo, number, ord, author, path, in_reply_to, created, body) VALUES (?,?,?,?,?,?,?,?,?,?)`)
	defer insRC.Close()
	insFC, _ := tx.Prepare(`INSERT INTO pr_file_changes (scope, month, repo, number, ord, path, status, additions, deletions) VALUES (?,?,?,?,?,?,?,?,?)`)
	defer insFC.Close()

	for i := range recs {
		r := &recs[i]
		if _, err := insPR.Exec(scope, m.String(), i, r.Number, r.Repo, r.Title, r.Body, r.State, r.Author, r.Branch,
			fmtTime(r.Created), fmtTimePtr(r.Merged), fmtTimePtr(r.Closed), r.Additions, r.Deletions, r.InlineComments, r.DeepThreads,
			boolToInt(r.Files != nil), boolToInt(r.ReviewComments != nil), boolToInt(r.FileChanges != nil)); err != nil {
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
			if _, err := insRC.Exec(scope, m.String(), r.Repo, r.Number, j, c.Author, c.Path, c.InReplyTo, fmtTime(c.Created), c.Body); err != nil {
				return err
			}
		}
		for j, c := range r.FileChanges {
			if _, err := insFC.Exec(scope, m.String(), r.Repo, r.Number, j, c.Path, c.Status, c.Additions, c.Deletions); err != nil {
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
		files_fetched, review_comments_fetched, file_changes_fetched
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
			ord              int
			created          string
			merged, closed   sql.NullString
			filesF, rcF, fcF int
			r                GitHubPR
		)
		if err := rows.Scan(&ord, &r.Number, &r.Repo, &r.Title, &r.Body, &r.State, &r.Author, &r.Branch,
			&created, &merged, &closed, &r.Additions, &r.Deletions, &r.InlineComments, &r.DeepThreads,
			&filesF, &rcF, &fcF); err != nil {
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
		if filesF != 0 {
			r.Files = []string{}
		}
		if rcF != 0 {
			r.ReviewComments = []ReviewComment{}
		}
		if fcF != 0 {
			r.FileChanges = []FileChange{}
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

	rcRows, err := s.db.Query(`SELECT repo, number, author, path, in_reply_to, created, body
		FROM pr_review_comments WHERE scope=? AND month=? ORDER BY repo, number, ord`, scope, m.String())
	if err != nil {
		return nil, err
	}
	for rcRows.Next() {
		var repo, created string
		var num int
		var c ReviewComment
		if err := rcRows.Scan(&repo, &num, &c.Author, &c.Path, &c.InReplyTo, &created, &c.Body); err != nil {
			rcRows.Close()
			return nil, err
		}
		if c.Created, err = parseTime(created); err != nil {
			rcRows.Close()
			return nil, err
		}
		if i, ok := byKey[prKey(repo, num)]; ok {
			out[i].ReviewComments = append(out[i].ReviewComments, c)
		}
	}
	rcRows.Close()
	if err := rcRows.Err(); err != nil {
		return nil, err
	}

	fcRows, err := s.db.Query(`SELECT repo, number, path, status, additions, deletions
		FROM pr_file_changes WHERE scope=? AND month=? ORDER BY repo, number, ord`, scope, m.String())
	if err != nil {
		return nil, err
	}
	for fcRows.Next() {
		var repo string
		var num int
		var c FileChange
		if err := fcRows.Scan(&repo, &num, &c.Path, &c.Status, &c.Additions, &c.Deletions); err != nil {
			fcRows.Close()
			return nil, err
		}
		if i, ok := byKey[prKey(repo, num)]; ok {
			out[i].FileChanges = append(out[i].FileChanges, c)
		}
	}
	fcRows.Close()
	if err := fcRows.Err(); err != nil {
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
		(scope, month, ord, sha, repo, author, message, committed, additions, deletions)
		VALUES (?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer insC.Close()
	insKey, _ := tx.Prepare(`INSERT INTO commit_issue_keys (scope, month, repo, sha, ord, value) VALUES (?,?,?,?,?,?)`)
	defer insKey.Close()

	for i := range recs {
		r := &recs[i]
		if _, err := insC.Exec(scope, m.String(), i, r.SHA, r.Repo, r.Author, r.Message, fmtTime(r.Committed), r.Additions, r.Deletions); err != nil {
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

	rows, err := s.db.Query(`SELECT ord, sha, repo, author, message, committed, additions, deletions
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
		var r GitHubCommit
		if err := rows.Scan(&ord, &r.SHA, &r.Repo, &r.Author, &r.Message, &committed, &r.Additions, &r.Deletions); err != nil {
			return nil, err
		}
		if r.Committed, err = parseTime(committed); err != nil {
			return nil, err
		}
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

	ins, err := tx.Prepare(`INSERT INTO github_reviews (scope, month, ord, pr_number, repo, reviewer, state, submitted) VALUES (?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer ins.Close()
	for i := range recs {
		r := &recs[i]
		if _, err := ins.Exec(scope, m.String(), i, r.PRNumber, r.Repo, r.Reviewer, r.State, fmtTime(r.Submitted)); err != nil {
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

	rows, err := s.db.Query(`SELECT pr_number, repo, reviewer, state, submitted
		FROM github_reviews WHERE scope=? AND month=? ORDER BY ord`, scope, m.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []GitHubReview{}
	for rows.Next() {
		var submitted string
		var r GitHubReview
		if err := rows.Scan(&r.PRNumber, &r.Repo, &r.Reviewer, &r.State, &submitted); err != nil {
			return nil, err
		}
		if r.Submitted, err = parseTime(submitted); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
