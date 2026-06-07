package pull

import "regexp"

// issueKeyRE matches Jira-style ticket keys: uppercase project prefix, dash,
// digits. Used to surface references in PR titles / bodies / branch names /
// commit messages so analyze can attribute GitHub activity to epics.
//
// Word-boundary anchors keep us from picking up substrings like "ABC-123" in
// the middle of a longer identifier.
var issueKeyRE = regexp.MustCompile(`\b[A-Z][A-Z0-9]+-\d+\b`)

// ExtractIssueKeys pulls all Jira-style keys out of one or more text blobs,
// de-duplicating while preserving first-seen order.
func ExtractIssueKeys(texts ...string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, t := range texts {
		if t == "" {
			continue
		}
		for _, m := range issueKeyRE.FindAllString(t, -1) {
			if _, ok := seen[m]; ok {
				continue
			}
			seen[m] = struct{}{}
			out = append(out, m)
		}
	}
	return out
}
