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

// IsSpike reports whether a ticket is a PR-less investigation ticket: an
// `investigate` label, or "spike" (case-insensitive, word-ish) in the summary.
// SQL routing is intentionally out of scope for this path. Exported so the
// spike-audit calibration tool routes tickets identically to the band engine.
func IsSpike(ev *TicketEvidence) bool {
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
//
// Attachments are deliberately NOT counted. They are usually reporter-supplied
// screenshots demonstrating the problem, not investigator-produced artifacts —
// their count tracks how much the reporter screenshotted, not investigation
// effort (CD-15865: 26 images of one blank screen). A doc-type, assignee-added
// attachment would be a real signal, but separating that needs author/timing
// analysis we don't do here.
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

// spawnLinkMarkers are substrings of an outward relationship phrase that mark a
// link as follow-up work this issue produced (a spawned ticket), distinct from a
// plain relate/block/depend link. Confirmed against the corpus: the only
// creation-flavored issue link present is the "Defect" type's outward "created"
// phrase; the clone/split markers are kept for portability to orgs that use them.
// Subtasks are handled separately (by link type) since they carry no phrase.
var spawnLinkMarkers = []string{"created", "clone", "split"}

// spikeLinkSignals derives the spike relationship inputs from a ticket's captured
// links: SpawnedCount counts follow-up work the investigation produced (subtasks
// plus outward creation-flavored links), and LinkBreadth counts the distinct
// linked counterparts (how widely the investigation reached). Both are inert on
// the standard band path; only the spike scorer consumes them.
func spikeLinkSignals(iss *cache.JiraIssue) (spawned, breadth int) {
	counterparts := map[string]struct{}{}
	for _, l := range iss.Links {
		if l.Key != "" {
			counterparts[strings.ToUpper(l.Key)] = struct{}{}
		}
		if isSpawnLink(l) {
			spawned++
		}
	}
	return spawned, len(counterparts)
}

// isSpawnLink reports whether a link represents follow-up work spawned by this
// issue: a subtask (any direction — captured outward from the parent), or an
// outward link whose phrase reads as creation ("created", "clones", "split to").
// Inward creation phrases ("created by", "is cloned from") describe the issue's
// own origin, not work it spawned, and are excluded by the outward gate.
func isSpawnLink(l cache.LinkedIssue) bool {
	if strings.EqualFold(l.LinkType, "subtask") {
		return true
	}
	if !strings.EqualFold(l.Direction, "outward") {
		return false
	}
	p := strings.ToLower(l.Phrase)
	for _, m := range spawnLinkMarkers {
		if strings.Contains(p, m) {
			return true
		}
	}
	return false
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

// isSubstantiveComment reports whether a comment body carries real investigation
// content: a code fence, a planning/research doc link, or a body at/above the
// length floor. Only doc-links count — a bare Jira-ticket link is a breadth
// pointer (already captured by the link signals), not comment depth, so it must
// not by itself make a comment substantive (mirrors the artifact-link counter's
// isDocLink filter rather than matching any URL).
func isSubstantiveComment(body string) bool {
	if strings.Contains(body, "```") {
		return true
	}
	for _, u := range urlPattern.FindAllString(body, -1) {
		if isDocLink(u) {
			return true
		}
	}
	return len(strings.TrimSpace(body)) >= substantiveCommentLen
}
