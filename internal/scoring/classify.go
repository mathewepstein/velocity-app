package scoring

import (
	"regexp"
	"strings"

	"github.com/mathewepstein/velocity/internal/cache"
)

// substantiveCommentLen is the body length (chars) at or above which a comment
// counts as "substantive" even without a code fence or link. A built-in floor
// rather than a config knob: it's a coarse heuristic feeding the spike artifact
// axis, not a precisely-tuned score input.
const substantiveCommentLen = 200

// isBugType reports whether a ticket is a bug/regression/hotfix. Bugs lean
// harder into rework inversion (a small diff that bounced is real diagnosis
// effort, not flaky churn). Recognition is by Jira issue type plus the standard
// regression/hotfix label variants — issue-type names are Jira-standard, not an
// org-specific path list, so this normaliser stays in code.
func isBugType(ev *TicketEvidence) bool {
	t := strings.ToLower(strings.TrimSpace(ev.IssueType))
	switch t {
	case "bug", "regression", "hotfix", "defect":
		return true
	}
	for _, l := range ev.Labels {
		switch strings.ToLower(strings.TrimSpace(l)) {
		case "bug", "regression", "hotfix":
			return true
		}
	}
	return false
}

// isSpike reports whether a ticket is a PR-less investigation ticket: an
// `investigate` label, or "spike" (case-insensitive, word-ish) in the summary.
// SQL routing is intentionally out of scope for this path.
func isSpike(ev *TicketEvidence) bool {
	for _, l := range ev.Labels {
		ll := strings.ToLower(strings.TrimSpace(l))
		if ll == "investigate" || ll == "spike" {
			return true
		}
	}
	lt := strings.ToLower(ev.IssueType)
	if lt == "spike" {
		return true
	}
	return spikeWord.MatchString(ev.Summary)
}

// spikeWord matches "spike" as a standalone token in a summary (so it doesn't
// fire on "spiked" or "spikes" mid-word in unrelated prose).
var spikeWord = regexp.MustCompile(`(?i)\bspike\b`)

// urlPattern matches bare http(s) URLs in flattened ADF text.
var urlPattern = regexp.MustCompile(`https?://\S+`)

// docLinkHints are substrings that mark a URL (or path reference) as a
// planning/research artifact rather than an incidental link.
var docLinkHints = []string{
	"confluence", "atlassian.net/wiki", "/wiki/",
	"implementation/", "discovery/",
	"docs.google.com", "miro.com", "figma.com",
}

// mdDocRef matches an in-repo planning-doc path reference like
// `implementation/foo/bar-plan.md` or `discovery/x/y.md` even when it isn't a URL.
var mdDocRef = regexp.MustCompile(`(?i)\b(?:implementation|discovery)/[\w./-]+\.md\b`)

// spikeArtifactSignals derives the spike artifact-density inputs from a ticket's
// description + comment bodies: the count of distinct planning/research artifact
// links, and the count of substantive comments (code fence, URL, or length).
// Computed for every ticket at extraction (cheap string scans); only the spike
// scorer consumes them.
func spikeArtifactSignals(iss *cache.JiraIssue) (links, substantive int) {
	seen := map[string]struct{}{}
	collect := func(text string) {
		for _, u := range urlPattern.FindAllString(text, -1) {
			if isDocLink(u) {
				if _, ok := seen[u]; !ok {
					seen[u] = struct{}{}
				}
			}
		}
		for _, m := range mdDocRef.FindAllString(text, -1) {
			if _, ok := seen[m]; !ok {
				seen[m] = struct{}{}
			}
		}
	}

	collect(iss.Description)
	for _, c := range iss.Comments {
		collect(c.Body)
		if isSubstantiveComment(c.Body) {
			substantive++
		}
	}
	return len(seen), substantive
}

// isDocLink reports whether a URL points at a planning/research artifact.
func isDocLink(u string) bool {
	lu := strings.ToLower(u)
	for _, h := range docLinkHints {
		if strings.Contains(lu, h) {
			return true
		}
	}
	return false
}

// isSubstantiveComment reports whether a comment body carries real content: a
// code fence, an inline/bare URL, or a body at/above the length floor.
func isSubstantiveComment(body string) bool {
	if strings.Contains(body, "```") {
		return true
	}
	if urlPattern.MatchString(body) {
		return true
	}
	return len(strings.TrimSpace(body)) >= substantiveCommentLen
}
