package devs

import (
	"sort"
	"strings"
	"unicode"
)

// Match scores how likely a GitHub identifier (login or git-author-name) and a
// Jira display name belong to the same person. Output is a 0–100 integer
// confidence — higher is better — produced by the first tier that fires:
//
//	100 — normalized strings are equal
//	 95 — initial+lastname pattern (e.g. jswinth ↔ Jon Swinth, kweckwerth-cd ↔ Kevin Weckwerth)
//	60–90 — token-set Jaccard >= 0.5
//	40–70 — partial substring containment across tokens
//	  0 — nothing identifiable
//
// Both inputs are normalized (lowercased, stripped to alphanumerics) before
// matching. Tokens split on whitespace, dash, underscore, dot.
func Match(githubID, jiraName string) int {
	a := normalizeIdent(githubID)
	b := normalizeIdent(jiraName)
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 100
	}

	aTokens := tokenize(githubID)
	bTokens := tokenize(jiraName)
	if len(aTokens) == 0 || len(bTokens) == 0 {
		return 0
	}

	if initialLastname(aTokens, bTokens) || initialLastname(bTokens, aTokens) {
		return 95
	}

	j := jaccard(aTokens, bTokens)
	if j >= 0.5 {
		score := 60 + int(j*30)
		if score > 90 {
			score = 90
		}
		return score
	}

	sub := substringOverlap(aTokens, bTokens)
	if sub > 0 {
		denom := len(bTokens)
		if denom == 0 {
			denom = 1
		}
		ratio := float64(sub) / float64(denom)
		score := 40 + int(ratio*30)
		if score > 70 {
			score = 70
		}
		return score
	}

	return 0
}

// MatchCandidate names one Jira accountId proposed for a GitHub identifier,
// with the score that earned the slot. Sorted descending by score.
type MatchCandidate struct {
	JiraAccountID string
	DisplayName   string
	Score         int
}

// Propose ranks every Jira candidate against one GitHub identifier and returns
// the top-N above minScore, descending by Score. Stable ordering: when two
// candidates tie on score, the lower accountId sorts first so output is
// deterministic across runs.
func Propose(githubID string, candidates map[string]string, topN, minScore int) []MatchCandidate {
	if len(candidates) == 0 || topN <= 0 {
		return nil
	}
	out := make([]MatchCandidate, 0, len(candidates))
	for acctID, name := range candidates {
		s := Match(githubID, name)
		if s < minScore {
			continue
		}
		out = append(out, MatchCandidate{JiraAccountID: acctID, DisplayName: name, Score: s})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].JiraAccountID < out[j].JiraAccountID
	})
	if len(out) > topN {
		out = out[:topN]
	}
	return out
}

// normalizeIdent collapses an identifier to lowercase alphanumerics. Used for
// the exact-match tier so spaces, punctuation, and case don't block obvious
// hits ("Mathew Epstein" ↔ "mathewepstein").
func normalizeIdent(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// tokenize splits on whitespace, dash, underscore, and dot, then lowercases and
// drops empty fragments. Order preserved.
func tokenize(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		switch r {
		case ' ', '\t', '-', '_', '.', ',', '(', ')':
			return true
		}
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// initialLastname tests whether some token in `from` looks like
// "<firstinitial><lastname>" relative to tokens in `to`. Matches when one
// `from` token T satisfies: T[0] == first char of some token in `to` AND
// T[1:] equals another token in `to` exactly. The "equals" (not contains)
// rule is load-bearing: an earlier version used substring containment and
// produced false positives like FPC-Jer ↔ Josh Berman ("er" inside "berman")
// or jasonconsumerdirect ↔ John O'Neill ("o" inside "asonconsumerdirect").
// Strict equality also requires rest ≥ 3 chars so a 2-char lastname token
// can't anchor the match.
//
// Examples that fire:
//
//	from=["jswinth","cd"], to=["jon","swinth"]      → "jswinth" = "j"+"swinth"
//	from=["kweckwerth","cd"], to=["kevin","weckwerth"] → "kweckwerth"
//	from=["jbrongust"], to=["jared","brongust"]     → "jbrongust"
func initialLastname(from, to []string) bool {
	if len(to) < 2 {
		return false
	}
	for _, t := range from {
		if len(t) < 4 {
			continue // need at least initial + 3-char lastname to be meaningful
		}
		initial := rune(t[0])
		rest := t[1:]
		if len(rest) < 3 {
			continue
		}
		for i, firstTok := range to {
			if len(firstTok) == 0 || rune(firstTok[0]) != initial {
				continue
			}
			for j, lastTok := range to {
				if i == j {
					continue
				}
				if lastTok == rest {
					return true
				}
			}
		}
	}
	return false
}

// jaccard returns |A∩B| / |A∪B| over the two token sets. Returns 0 on empty
// inputs.
func jaccard(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	aSet := setOf(a)
	bSet := setOf(b)
	inter := 0
	for t := range aSet {
		if _, ok := bSet[t]; ok {
			inter++
		}
	}
	union := len(aSet) + len(bSet) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// substringOverlap counts tokens in b that appear as a substring of some token
// in a, or vice versa. Min length 3 to avoid spurious 2-char hits.
func substringOverlap(a, b []string) int {
	count := 0
	for _, bt := range b {
		if len(bt) < 3 {
			continue
		}
		for _, at := range a {
			if len(at) < 3 {
				continue
			}
			if strings.Contains(at, bt) || strings.Contains(bt, at) {
				count++
				break
			}
		}
	}
	return count
}

func setOf(tokens []string) map[string]struct{} {
	out := make(map[string]struct{}, len(tokens))
	for _, t := range tokens {
		out[t] = struct{}{}
	}
	return out
}
