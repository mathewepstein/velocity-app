package cache

import (
	"database/sql"
	"fmt"
)

func (s *sqliteStore) WriteJiraIssues(scope string, m Month, recs []JiraIssue) error {
	tx, err := s.beginCellWrite(scope, m, "jira_issues", "jira_labels", "jira_components", "jira_changelog", "jira_comments",
		"jira_issue_links", "jira_attachments", "jira_fix_versions", "jira_issue_fields")
	if err != nil {
		return err
	}
	defer tx.Rollback()

	insIssue, err := tx.Prepare(`INSERT INTO jira_issues
		(scope, month, ord, key, summary, status, resolution, issue_type,
		 created, updated, resolved, story_points, assignee, reporter, epic_key,
		 description, detail_fetched, detail_fetched_at, first_in_progress, done_at,
		 cycle_hours, status_flips, pre_code_comments, changelog_fetched, comments_fetched,
		 relations_fetched, raw_fields_fetched)
		VALUES (?,?,?,?,?,?,?,?, ?,?,?,?,?,?,?, ?,?,?,?,?, ?,?,?,?,?, ?,?)`)
	if err != nil {
		return err
	}
	defer insIssue.Close()
	insLabel, _ := tx.Prepare(`INSERT INTO jira_labels (scope, month, issue_key, ord, value) VALUES (?,?,?,?,?)`)
	defer insLabel.Close()
	insComp, _ := tx.Prepare(`INSERT INTO jira_components (scope, month, issue_key, ord, value) VALUES (?,?,?,?,?)`)
	defer insComp.Close()
	insChg, _ := tx.Prepare(`INSERT INTO jira_changelog (scope, month, issue_key, ord, at, author, from_status, to_status, field) VALUES (?,?,?,?,?,?,?,?,?)`)
	defer insChg.Close()
	insCmt, _ := tx.Prepare(`INSERT INTO jira_comments (scope, month, issue_key, ord, author, created, body) VALUES (?,?,?,?,?,?,?)`)
	defer insCmt.Close()
	insLink, _ := tx.Prepare(`INSERT INTO jira_issue_links (scope, month, issue_key, ord, counterpart_key, link_type, direction, phrase, status, issue_type) VALUES (?,?,?,?,?,?,?,?,?,?)`)
	defer insLink.Close()
	insAtt, _ := tx.Prepare(`INSERT INTO jira_attachments (scope, month, issue_key, ord, filename, mime_type, size, created, author) VALUES (?,?,?,?,?,?,?,?,?)`)
	defer insAtt.Close()
	insFix, _ := tx.Prepare(`INSERT INTO jira_fix_versions (scope, month, issue_key, ord, value) VALUES (?,?,?,?,?)`)
	defer insFix.Close()
	insRaw, _ := tx.Prepare(`INSERT INTO jira_issue_fields (scope, month, issue_key, ord, field_id, field_name, value_json) VALUES (?,?,?,?,?,?,?)`)
	defer insRaw.Close()

	for i := range recs {
		r := &recs[i]
		// relations_fetched gates the Links + Attachments sentinels (both set
		// together by the same ingest paths); raw_fields_fetched gates RawFields.
		relationsFetched := r.Links != nil || r.Attachments != nil
		if _, err := insIssue.Exec(scope, m.String(), i, r.Key, r.Summary, r.Status, r.Resolution, r.IssueType,
			fmtTime(r.Created), fmtTime(r.Updated), fmtTimePtr(r.Resolved), r.StoryPoints, r.Assignee, r.Reporter, r.EpicKey,
			r.Description, boolToInt(r.DetailFetched), fmtTimePtr(r.DetailFetchedAt), fmtTimePtr(r.FirstInProgress), fmtTimePtr(r.DoneAt),
			r.CycleHours, r.StatusFlips, r.PreCodeComments, boolToInt(r.Changelog != nil), boolToInt(r.Comments != nil),
			boolToInt(relationsFetched), boolToInt(r.RawFields != nil)); err != nil {
			return fmt.Errorf("insert jira issue %s: %w", r.Key, err)
		}
		for j, v := range r.Labels {
			if _, err := insLabel.Exec(scope, m.String(), r.Key, j, v); err != nil {
				return err
			}
		}
		for j, v := range r.Components {
			if _, err := insComp.Exec(scope, m.String(), r.Key, j, v); err != nil {
				return err
			}
		}
		for j, c := range r.Changelog {
			if _, err := insChg.Exec(scope, m.String(), r.Key, j, fmtTime(c.At), c.Author, c.From, c.To, c.Field); err != nil {
				return err
			}
		}
		for j, c := range r.Comments {
			if _, err := insCmt.Exec(scope, m.String(), r.Key, j, c.Author, fmtTime(c.Created), c.Body); err != nil {
				return err
			}
		}
		for j, l := range r.Links {
			if _, err := insLink.Exec(scope, m.String(), r.Key, j, l.Key, l.LinkType, l.Direction, l.Phrase, l.Status, l.IssueType); err != nil {
				return err
			}
		}
		for j, a := range r.Attachments {
			if _, err := insAtt.Exec(scope, m.String(), r.Key, j, a.Filename, a.MimeType, a.Size, fmtTime(a.Created), a.Author); err != nil {
				return err
			}
		}
		for j, v := range r.FixVersions {
			if _, err := insFix.Exec(scope, m.String(), r.Key, j, v); err != nil {
				return err
			}
		}
		for j, f := range r.RawFields {
			if _, err := insRaw.Exec(scope, m.String(), r.Key, j, f.ID, f.Name, f.Value); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *sqliteStore) ReadJiraIssues(scope string, m Month) ([]JiraIssue, error) {
	ok, err := s.cellExists(SourceJira, scope, m)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, missErr(SourceJira, scope, m)
	}

	rows, err := s.db.Query(`SELECT ord, key, summary, status, resolution, issue_type,
		created, updated, resolved, story_points, assignee, reporter, epic_key,
		description, detail_fetched, detail_fetched_at, first_in_progress, done_at,
		cycle_hours, status_flips, pre_code_comments, changelog_fetched, comments_fetched,
		relations_fetched, raw_fields_fetched
		FROM jira_issues WHERE scope=? AND month=? ORDER BY ord`, scope, m.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []JiraIssue{}
	// Index, not pointer: out is still growing via append below, so any &out[i]
	// captured here would dangle after a reallocation. Children attach by index
	// once out is final.
	byKey := map[string]int{}
	for rows.Next() {
		var (
			ord                                              int
			created, updated                                 string
			resolved, detailAt, firstInProg, doneAt          sql.NullString
			detailFetched, changelogFetched, commentsFetched int
			relationsFetched, rawFieldsFetched               int
			r                                                JiraIssue
		)
		if err := rows.Scan(&ord, &r.Key, &r.Summary, &r.Status, &r.Resolution, &r.IssueType,
			&created, &updated, &resolved, &r.StoryPoints, &r.Assignee, &r.Reporter, &r.EpicKey,
			&r.Description, &detailFetched, &detailAt, &firstInProg, &doneAt,
			&r.CycleHours, &r.StatusFlips, &r.PreCodeComments, &changelogFetched, &commentsFetched,
			&relationsFetched, &rawFieldsFetched); err != nil {
			return nil, err
		}
		if r.Created, err = parseTime(created); err != nil {
			return nil, err
		}
		if r.Updated, err = parseTime(updated); err != nil {
			return nil, err
		}
		if r.Resolved, err = parseTimePtr(resolved); err != nil {
			return nil, err
		}
		if r.DetailFetchedAt, err = parseTimePtr(detailAt); err != nil {
			return nil, err
		}
		if r.FirstInProgress, err = parseTimePtr(firstInProg); err != nil {
			return nil, err
		}
		if r.DoneAt, err = parseTimePtr(doneAt); err != nil {
			return nil, err
		}
		r.DetailFetched = detailFetched != 0
		// Sentinel: a fetched-but-empty slice is non-nil empty; unfetched is nil.
		if changelogFetched != 0 {
			r.Changelog = []StatusTransition{}
		}
		if commentsFetched != 0 {
			r.Comments = []IssueComment{}
		}
		if relationsFetched != 0 {
			r.Links = []LinkedIssue{}
			r.Attachments = []Attachment{}
		}
		if rawFieldsFetched != 0 {
			r.RawFields = []RawField{}
		}
		out = append(out, r)
		byKey[r.Key] = len(out) - 1
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Child slices, in ord order. Append-to-nil yields a non-nil slice, which is
	// correct for the omitempty string slices (labels/components: nil when empty)
	// and harmless for the sentinel slices (already pre-seeded to non-nil empty).
	if err := s.loadStringChildren(`SELECT issue_key, value FROM jira_labels WHERE scope=? AND month=? ORDER BY issue_key, ord`,
		scope, m, func(key, v string) {
			if i, ok := byKey[key]; ok {
				out[i].Labels = append(out[i].Labels, v)
			}
		}); err != nil {
		return nil, err
	}
	if err := s.loadStringChildren(`SELECT issue_key, value FROM jira_components WHERE scope=? AND month=? ORDER BY issue_key, ord`,
		scope, m, func(key, v string) {
			if i, ok := byKey[key]; ok {
				out[i].Components = append(out[i].Components, v)
			}
		}); err != nil {
		return nil, err
	}

	chgRows, err := s.db.Query(`SELECT issue_key, at, author, from_status, to_status, field
		FROM jira_changelog WHERE scope=? AND month=? ORDER BY issue_key, ord`, scope, m.String())
	if err != nil {
		return nil, err
	}
	for chgRows.Next() {
		var key, at string
		var c StatusTransition
		if err := chgRows.Scan(&key, &at, &c.Author, &c.From, &c.To, &c.Field); err != nil {
			chgRows.Close()
			return nil, err
		}
		if c.At, err = parseTime(at); err != nil {
			chgRows.Close()
			return nil, err
		}
		if i, ok := byKey[key]; ok {
			out[i].Changelog = append(out[i].Changelog, c)
		}
	}
	chgRows.Close()
	if err := chgRows.Err(); err != nil {
		return nil, err
	}

	cmtRows, err := s.db.Query(`SELECT issue_key, author, created, body
		FROM jira_comments WHERE scope=? AND month=? ORDER BY issue_key, ord`, scope, m.String())
	if err != nil {
		return nil, err
	}
	for cmtRows.Next() {
		var key, created string
		var c IssueComment
		if err := cmtRows.Scan(&key, &c.Author, &created, &c.Body); err != nil {
			cmtRows.Close()
			return nil, err
		}
		if c.Created, err = parseTime(created); err != nil {
			cmtRows.Close()
			return nil, err
		}
		if i, ok := byKey[key]; ok {
			out[i].Comments = append(out[i].Comments, c)
		}
	}
	cmtRows.Close()
	if err := cmtRows.Err(); err != nil {
		return nil, err
	}

	// Fix versions: string child like labels/components (nil when empty).
	if err := s.loadStringChildren(`SELECT issue_key, value FROM jira_fix_versions WHERE scope=? AND month=? ORDER BY issue_key, ord`,
		scope, m, func(key, v string) {
			if i, ok := byKey[key]; ok {
				out[i].FixVersions = append(out[i].FixVersions, v)
			}
		}); err != nil {
		return nil, err
	}

	linkRows, err := s.db.Query(`SELECT issue_key, counterpart_key, link_type, direction, phrase, status, issue_type
		FROM jira_issue_links WHERE scope=? AND month=? ORDER BY issue_key, ord`, scope, m.String())
	if err != nil {
		return nil, err
	}
	for linkRows.Next() {
		var key string
		var l LinkedIssue
		if err := linkRows.Scan(&key, &l.Key, &l.LinkType, &l.Direction, &l.Phrase, &l.Status, &l.IssueType); err != nil {
			linkRows.Close()
			return nil, err
		}
		if i, ok := byKey[key]; ok {
			out[i].Links = append(out[i].Links, l)
		}
	}
	linkRows.Close()
	if err := linkRows.Err(); err != nil {
		return nil, err
	}

	attRows, err := s.db.Query(`SELECT issue_key, filename, mime_type, size, created, author
		FROM jira_attachments WHERE scope=? AND month=? ORDER BY issue_key, ord`, scope, m.String())
	if err != nil {
		return nil, err
	}
	for attRows.Next() {
		var key, created string
		var a Attachment
		if err := attRows.Scan(&key, &a.Filename, &a.MimeType, &a.Size, &created, &a.Author); err != nil {
			attRows.Close()
			return nil, err
		}
		if a.Created, err = parseTime(created); err != nil {
			attRows.Close()
			return nil, err
		}
		if i, ok := byKey[key]; ok {
			out[i].Attachments = append(out[i].Attachments, a)
		}
	}
	attRows.Close()
	if err := attRows.Err(); err != nil {
		return nil, err
	}

	rawRows, err := s.db.Query(`SELECT issue_key, field_id, field_name, value_json
		FROM jira_issue_fields WHERE scope=? AND month=? ORDER BY issue_key, ord`, scope, m.String())
	if err != nil {
		return nil, err
	}
	for rawRows.Next() {
		var key string
		var f RawField
		if err := rawRows.Scan(&key, &f.ID, &f.Name, &f.Value); err != nil {
			rawRows.Close()
			return nil, err
		}
		if i, ok := byKey[key]; ok {
			out[i].RawFields = append(out[i].RawFields, f)
		}
	}
	rawRows.Close()
	if err := rawRows.Err(); err != nil {
		return nil, err
	}

	return out, nil
}

// loadStringChildren runs a (parent-key, value) query and feeds each row to add.
func (s *sqliteStore) loadStringChildren(query, scope string, m Month, add func(key, value string)) error {
	rows, err := s.db.Query(query, scope, m.String())
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key, v string
		if err := rows.Scan(&key, &v); err != nil {
			return err
		}
		add(key, v)
	}
	return rows.Err()
}
