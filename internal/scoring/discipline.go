package scoring

import "strings"

// Discipline is the engineering discipline a ticket belongs to, derived from its
// Jira labels. It is the axis the scoring page's discipline filter narrows on,
// and the seam a future per-discipline config / hotlist / engine would branch
// on. Today every discipline is scored by the same engine and config; this type
// only carries the classification, it does not change scoring.
type Discipline string

const (
	DisciplineFE     Discipline = "FE"
	DisciplineBE     Discipline = "BE"
	DisciplineDevOps Discipline = "DevOps"
)

// disciplineByLabel maps a case-folded Jira label to its Discipline. It is the
// single source of truth for the mapping: the list query builds its label
// prefilter from these keys (see DisciplineLabelKeys) so the SQL and this
// classifier can't drift. "backend" folds to BE to absorb the stray full-word
// label seen in the corpus alongside "BE".
var disciplineByLabel = map[string]Discipline{
	"fe":      DisciplineFE,
	"be":      DisciplineBE,
	"backend": DisciplineBE,
	"devops":  DisciplineDevOps,
}

// Disciplines returns the distinct disciplines a ticket's labels place it in, in
// a stable order (FE, BE, DevOps). An empty result means untagged — the ticket
// carries none of the FE/BE/DevOps labels (it may still carry other labels). A
// ticket may belong to more than one discipline (e.g. a fullstack ticket tagged
// both FE and BE); all matches are returned.
func Disciplines(labels []string) []Discipline {
	var fe, be, devops bool
	for _, l := range labels {
		switch disciplineByLabel[strings.ToLower(strings.TrimSpace(l))] {
		case DisciplineFE:
			fe = true
		case DisciplineBE:
			be = true
		case DisciplineDevOps:
			devops = true
		}
	}
	out := make([]Discipline, 0, 3)
	if fe {
		out = append(out, DisciplineFE)
	}
	if be {
		out = append(out, DisciplineBE)
	}
	if devops {
		out = append(out, DisciplineDevOps)
	}
	return out
}

// DisciplineLabelKeys returns the case-folded label strings the classifier
// recognizes, so a SQL prefilter over jira_labels stays in sync with Disciplines.
func DisciplineLabelKeys() []string {
	out := make([]string, 0, len(disciplineByLabel))
	for k := range disciplineByLabel {
		out = append(out, k)
	}
	return out
}
