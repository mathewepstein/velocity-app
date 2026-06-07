package devs

import "testing"

func TestMatch(t *testing.T) {
	cases := []struct {
		name     string
		gh, jira string
		min, max int
	}{
		// Tier 1 — exact normalized.
		{"exact same casing", "Mathew Epstein", "Mathew Epstein", 100, 100},
		{"exact different casing", "mathewepstein", "Mathew Epstein", 100, 100},
		{"exact with punctuation", "jon.swinth", "Jon Swinth", 100, 100},

		// Tier 2 — initial + lastname (target the 95 band).
		{"initial+lastname with company suffix", "jswinth-cd", "Jon Swinth", 95, 95},
		{"initial+lastname kweckwerth", "kweckwerth-cd", "Kevin Weckwerth", 95, 95},
		{"initial+lastname jbrongust", "jbrongust", "Jared Brongust", 95, 95},
		{"initial+lastname swapped order", "Jon Swinth", "jswinth-cd", 95, 95},

		// Tier 3 — token jaccard >= 0.5.
		{"token overlap one of two", "alice", "Alice Williams", 60, 90},
		{"token overlap broken display", "Nivethitha Shanmugam(UST", "Nivethitha Shanmugam", 60, 90},

		// Tier 4 — substring fallback. Two tokens, only one of which contains
		// a substring of the other; doesn't satisfy initial+lastname (no shared
		// first-char anchor) and jaccard is 0 because no token is identical.
		{"substring partial", "swinthproject", "Jon Swinth", 40, 70},

		// No match.
		{"unrelated names", "alice", "Bob Smith", 0, 0},
		{"empty inputs", "", "Bob Smith", 0, 0},
		{"both empty", "", "", 0, 0},
		{"too short token", "ab", "Bob Smith", 0, 0},

		// Regressions from the 2026-05-20 false-positive batch — these all
		// scored 95 under the old substring-tolerant initialLastname and
		// must NEVER auto-apply under --apply-all. Bound to <90 so the
		// confidence-threshold gate keeps them out of auto-apply.
		{"FP jasonconsumerdirect vs John O'Neill", "jasonconsumerdirect", "John O'Neill", 0, 89},
		{"FP Matthew-R-Rohr vs Maria Shahid", "Matthew-R-Rohr", "Maria Shahid", 0, 89},
		{"FP JustinConsumerDirect vs Javier.MotaBayardo.ust", "JustinConsumerDirect", "Javier.MotaBayardo.ust", 0, 89},
		{"FP FPC-Jer vs Josh Berman", "FPC-Jer", "Josh Berman", 0, 89},
		{"FP JE-UST vs Justin Concepcion", "JE-UST", "Justin Concepcion", 0, 89},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Match(tc.gh, tc.jira)
			if got < tc.min || got > tc.max {
				t.Errorf("Match(%q, %q) = %d, want %d..%d", tc.gh, tc.jira, got, tc.min, tc.max)
			}
		})
	}
}

func TestMatchIsSymmetric(t *testing.T) {
	// Symmetry matters because devs discover proposes both GH→Jira and the
	// reverse; an asymmetric scorer would surface different candidates
	// depending on call direction.
	pairs := [][2]string{
		{"jswinth-cd", "Jon Swinth"},
		{"Mathew Epstein", "mathewepstein"},
		{"alice", "Alice Williams"},
		{"jbrongust", "Jared Brongust"},
	}
	for _, p := range pairs {
		a, b := Match(p[0], p[1]), Match(p[1], p[0])
		if a != b {
			t.Errorf("Match(%q,%q)=%d but Match(%q,%q)=%d", p[0], p[1], a, p[1], p[0], b)
		}
	}
}

func TestProposeRanksAndCaps(t *testing.T) {
	candidates := map[string]string{
		"acct-jon":   "Jon Swinth",
		"acct-bob":   "Bob Smith",
		"acct-alice": "Alice Williams",
		"acct-jared": "Jared Brongust",
	}
	got := Propose("jswinth-cd", candidates, 2, 60)
	if len(got) != 1 {
		t.Fatalf("Propose: want 1 result above minScore=60, got %d (%v)", len(got), got)
	}
	if got[0].JiraAccountID != "acct-jon" {
		t.Errorf("top candidate = %q, want acct-jon", got[0].JiraAccountID)
	}
	if got[0].Score != 95 {
		t.Errorf("top score = %d, want 95", got[0].Score)
	}
}

func TestProposeIsDeterministicOnTies(t *testing.T) {
	candidates := map[string]string{
		"acct-b": "Alice Williams",
		"acct-a": "Alice Williams",
	}
	got := Propose("alice", candidates, 2, 40)
	if len(got) != 2 {
		t.Fatalf("want 2 candidates, got %d", len(got))
	}
	if got[0].JiraAccountID != "acct-a" || got[1].JiraAccountID != "acct-b" {
		t.Errorf("tie order = [%s, %s], want [acct-a, acct-b]", got[0].JiraAccountID, got[1].JiraAccountID)
	}
}

func TestProposeBelowMinReturnsNothing(t *testing.T) {
	got := Propose("zzz", map[string]string{"acct-1": "Alice"}, 3, 60)
	if len(got) != 0 {
		t.Errorf("want no candidates, got %v", got)
	}
}

func TestTokenize(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"jswinth-cd", []string{"jswinth", "cd"}},
		{"Jon Swinth", []string{"jon", "swinth"}},
		{"Nivethitha Shanmugam(UST", []string{"nivethitha", "shanmugam", "ust"}},
		{"  ", nil},
		{"a.b_c-d", []string{"a", "b", "c", "d"}},
	}
	for _, tc := range cases {
		got := tokenize(tc.in)
		if !equalStrings(got, tc.want) {
			t.Errorf("tokenize(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
