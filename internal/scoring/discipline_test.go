package scoring

import (
	"reflect"
	"testing"
)

func TestDisciplines(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   []Discipline
	}{
		{"empty is untagged", nil, []Discipline{}},
		{"unrelated labels are untagged", []string{"PCI", "Technical_Debt"}, []Discipline{}},
		{"FE", []string{"FE"}, []Discipline{DisciplineFE}},
		{"case-folded fe", []string{"fe"}, []Discipline{DisciplineFE}},
		{"Backend folds to BE", []string{"Backend"}, []Discipline{DisciplineBE}},
		{"stray-cased devops", []string{"DEVOPS"}, []Discipline{DisciplineDevOps}},
		{"whitespace tolerated", []string{"  be  "}, []Discipline{DisciplineBE}},
		{"multi-membership keeps stable order", []string{"DevOps", "FE", "BE"}, []Discipline{DisciplineFE, DisciplineBE, DisciplineDevOps}},
		{"dupes collapse", []string{"FE", "fe", "FE"}, []Discipline{DisciplineFE}},
		{"discipline labels mixed with others", []string{"PCI", "BE"}, []Discipline{DisciplineBE}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Disciplines(c.labels)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Disciplines(%v) = %v, want %v", c.labels, got, c.want)
			}
		})
	}
}

// seedLabels creates the jira_labels table (mirroring the cache schema, which
// co-resides in velocity.db) and inserts (issue_key, value) pairs so the List
// join has something to read.
func seedLabels(t *testing.T, ss *ScoreStore, byKey map[string][]string) {
	t.Helper()
	if _, err := ss.db.Exec(`CREATE TABLE IF NOT EXISTS jira_labels (
		scope TEXT NOT NULL, month TEXT NOT NULL, issue_key TEXT NOT NULL,
		ord INTEGER NOT NULL, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("create jira_labels: %v", err)
	}
	for key, vals := range byKey {
		for i, v := range vals {
			if _, err := ss.db.Exec(
				`INSERT INTO jira_labels (scope, month, issue_key, ord, value) VALUES (?,?,?,?,?)`,
				"org", "2026-06", key, i, v); err != nil {
				t.Fatalf("insert label: %v", err)
			}
		}
	}
}

func disciplinesOf(recs []ScoreRecord, ticket string) []Discipline {
	for _, r := range recs {
		if r.Ticket == ticket {
			return r.Disciplines
		}
	}
	return nil
}

func TestScoreStore_ListAttachesDisciplines(t *testing.T) {
	ss := tmpStore(t)
	// A fullstack ticket (FE+BE, with month duplication to exercise DISTINCT), a
	// DevOps ticket via a stray casing, and an untagged ticket.
	seedLabels(t, ss, map[string][]string{
		"CD-1": {"FE", "BE", "PCI"},
		"CD-2": {"devops"},
		"CD-3": {"Documentation"},
	})
	// Duplicate the FE row in a second month cell — DISTINCT must collapse it.
	if _, err := ss.db.Exec(
		`INSERT INTO jira_labels (scope, month, issue_key, ord, value) VALUES (?,?,?,?,?)`,
		"org", "2026-05", "CD-1", 0, "FE"); err != nil {
		t.Fatalf("dup label: %v", err)
	}
	for _, k := range []string{"CD-1", "CD-2", "CD-3"} {
		if _, err := ss.SaveAuto(autoRec(k, "h-"+k, 5)); err != nil {
			t.Fatalf("save %s: %v", k, err)
		}
	}

	recs, err := ss.List(ScoreFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got, want := disciplinesOf(recs, "CD-1"), []Discipline{DisciplineFE, DisciplineBE}; !reflect.DeepEqual(got, want) {
		t.Errorf("CD-1 disciplines = %v, want %v", got, want)
	}
	if got, want := disciplinesOf(recs, "CD-2"), []Discipline{DisciplineDevOps}; !reflect.DeepEqual(got, want) {
		t.Errorf("CD-2 disciplines = %v, want %v", got, want)
	}
	if got := disciplinesOf(recs, "CD-3"); len(got) != 0 {
		t.Errorf("CD-3 (untagged) disciplines = %v, want empty", got)
	}
}

func TestScoreStore_ListNoLabelsTableIsUntagged(t *testing.T) {
	ss := tmpStore(t) // scores-only DB, no jira_labels table
	if _, err := ss.SaveAuto(autoRec("CD-1", "h1", 5)); err != nil {
		t.Fatalf("save: %v", err)
	}
	recs, err := ss.List(ScoreFilter{})
	if err != nil {
		t.Fatalf("list must not error when jira_labels is absent: %v", err)
	}
	if got := disciplinesOf(recs, "CD-1"); len(got) != 0 {
		t.Errorf("disciplines = %v, want empty when no corpus labels", got)
	}
}
