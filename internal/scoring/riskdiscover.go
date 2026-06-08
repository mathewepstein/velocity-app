package scoring

import (
	"sort"
	"strings"

	"github.com/mathewepstein/velocity/internal/analyze"
)

// RiskDiscoverOpts tunes the domain-risk discovery sweep.
type RiskDiscoverOpts struct {
	MinTickets   int  // minimum tickets touching a dir for it to be ranked (small-sample floor)
	Depth        int  // in-repo path depth at which directories are grouped
	Top          int  // keep only the top-N ranked dirs (0 = all)
	SeedKeywords bool  // also surface dirs whose path contains a generic sensitive term
}

// RiskCandidate is one proposed domain-risk directory with the evidence behind
// the proposal.
type RiskCandidate struct {
	Glob        string  // paste-ready glob, e.g. "**/auth-microservice/**"
	Tier        string  // proposed tier: high | medium
	Tickets     int     // distinct tickets touching the dir
	CycleDays   float64 // median active-cycle-days for those tickets
	CycleRatio  float64 // cycleDays / corpus baseline
	ReworkRate  float64 // share of those tickets with rework > 0
	ReworkRatio float64 // reworkRate / corpus baseline
	Outcome     float64 // blended outcome score (the rank key); 0 for migration-detected dirs
	Migration   bool    // surfaced by the structural migration detector, not outcome
	Seed        bool    // surfaced by --seed-keywords (suggestion only)
}

// RiskDiscoverResult is the full proposal: ranked candidates plus the corpus
// baselines they were measured against.
type RiskDiscoverResult struct {
	Candidates    []RiskCandidate
	BaselineCycle float64 // corpus median active-cycle-days
	BaselineRework float64 // corpus mean rework rate
	TicketsScanned int
}

// outcome-score thresholds: a dir whose tickets cost this many times the corpus
// baseline (blended cycle + rework) is proposed high; the lower bound is medium.
const (
	riskHighRatio = 2.0
	riskMedRatio  = 1.4
)

// migrationSegments are path conventions that mark DDL/DML directories. Any dir
// whose path contains one is risk-elevating by construction — proposed high
// regardless of its outcome stats.
var migrationSegments = []string{"db/changelog", "migrations", "liquibase", "flyway"}

// seedTerms are generic sensitive domains used only by --seed-keywords. They
// carry no org-specific opinion; they're a starter scan the user opts into.
var seedTerms = []string{"auth", "billing", "credit", "payment", "password", "token", "privacy", "signup", "secret"}

// dirAccum accumulates per-directory outcome evidence during the sweep.
type dirAccum struct {
	tickets   map[string]struct{}
	cycleDays []float64
	reworked  int
	seed      bool
	migration bool
}

// RiskDiscover sweeps the corpus and proposes a [storypoints.risk] block from
// outcome-correlation (dirs whose tickets historically cost more than baseline)
// blended with a structural migration-directory detector. It proposes, never
// writes — the caller prints the block + evidence for the user to curate.
func RiskDiscover(e *Extractor, opts RiskDiscoverOpts) RiskDiscoverResult {
	if opts.MinTickets <= 0 {
		opts.MinTickets = 5
	}
	if opts.Depth <= 0 {
		opts.Depth = 3
	}

	dirs := map[string]*dirAccum{}
	var corpusCycle []float64
	corpusTickets, corpusReworked := 0, 0

	for _, key := range e.Keys() {
		ev, ok := e.Extract(key)
		if !ok || len(ev.PRs) == 0 {
			continue
		}
		cd := cycleDays(ev)
		reworked := ev.ReworkCount > 0
		corpusTickets++
		if cd > 0 {
			corpusCycle = append(corpusCycle, cd)
		}
		if reworked {
			corpusReworked++
		}

		// Distinct risk-relevant dirs this ticket touched (code only — test and
		// resource paths excluded, mirroring the hot-file signal).
		touched := map[string]bool{} // dir -> isMigration
		ku := strings.ToUpper(strings.TrimSpace(key))
		for _, pr := range e.prsByKey[ku] {
			for f := range distinctPaths(pr) {
				if analyze.IsGeneratedPath(f, e.norm) || isRiskExcludedFile(f) || analyze.MatchesNoisePath(f, e.norm) {
					continue
				}
				// Migration paths group at the migration segment (so db/changelog
				// isn't truncated away by the depth cap); everything else groups at
				// the configured depth.
				if md := migrationDirOf(f); md != "" {
					touched[md] = true
				} else if d := dirAtDepth(f, opts.Depth); d != "" {
					touched[d] = touched[d] // keep false unless already migration
				}
			}
		}
		for d, isMig := range touched {
			acc := dirs[d]
			if acc == nil {
				acc = &dirAccum{tickets: map[string]struct{}{}}
				acc.migration = isMig
				acc.seed = opts.SeedKeywords && containsSeedTerm(d)
				dirs[d] = acc
			}
			acc.tickets[key] = struct{}{}
			if cd > 0 {
				acc.cycleDays = append(acc.cycleDays, cd)
			}
			if reworked {
				acc.reworked++
			}
		}
	}

	baseCycle := medianOf(corpusCycle)
	baseRework := 0.0
	if corpusTickets > 0 {
		baseRework = float64(corpusReworked) / float64(corpusTickets)
	}

	var cands []RiskCandidate
	for d, acc := range dirs {
		n := len(acc.tickets)
		// Only migration dirs bypass the small-sample floor (they're structural,
		// not statistical). Outcome-ranked AND seed-named dirs must clear it — a
		// 1-ticket dir with an extreme ratio is noise regardless of its name.
		if n < opts.MinTickets && !acc.migration {
			continue
		}
		cyc := medianOf(acc.cycleDays)
		rwk := 0.0
		if n > 0 {
			rwk = float64(acc.reworked) / float64(n)
		}
		cycRatio := ratio(cyc, baseCycle)
		rwkRatio := ratio(rwk, baseRework)
		outcome := (cycRatio + rwkRatio) / 2

		// --seed-keywords is purely ADDITIVE: it never changes the tier a dir
		// earns by outcome/migration. It only surfaces a sensitive-named dir that
		// would otherwise fall through, as a `medium` suggestion. So a dir that
		// qualifies by outcome keeps its outcome tier and is not marked a seed
		// suggestion; only a fall-through seed dir is.
		tier := ""
		seedSuggestion := false
		switch {
		case acc.migration:
			tier = "high"
		case outcome >= riskHighRatio:
			tier = "high"
		case outcome >= riskMedRatio:
			tier = "medium"
		case acc.seed:
			tier = "medium"
			seedSuggestion = true
		default:
			continue
		}

		cands = append(cands, RiskCandidate{
			Glob:        "**/" + d + "/**",
			Tier:        tier,
			Tickets:     n,
			CycleDays:   cyc,
			CycleRatio:  cycRatio,
			ReworkRate:  rwk,
			ReworkRatio: rwkRatio,
			Outcome:     outcome,
			Migration:   acc.migration,
			Seed:        seedSuggestion,
		})
	}

	// Rank: high before medium, migration dirs first within high, then by outcome.
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if (a.Tier == "high") != (b.Tier == "high") {
			return a.Tier == "high"
		}
		if a.Migration != b.Migration {
			return a.Migration
		}
		return a.Outcome > b.Outcome
	})
	if opts.Top > 0 && len(cands) > opts.Top {
		cands = cands[:opts.Top]
	}

	return RiskDiscoverResult{
		Candidates:     cands,
		BaselineCycle:  baseCycle,
		BaselineRework: baseRework,
		TicketsScanned: corpusTickets,
	}
}

// dirAtDepth returns the first depth path segments of an in-repo file path
// (joined), or "" for a file shallower than two segments (no useful dir).
func dirAtDepth(p string, depth int) string {
	p = strings.TrimPrefix(p, "./")
	segs := strings.Split(p, "/")
	if len(segs) <= 1 {
		return "" // root-level file
	}
	// Drop the filename; cap the remaining dir segments at depth.
	segs = segs[:len(segs)-1]
	if len(segs) > depth {
		segs = segs[:depth]
	}
	return strings.Join(segs, "/")
}

// migrationDirOf returns the directory prefix of path up to and including a
// migration-convention segment (e.g. "src/main/resources/db/changelog" for a
// changeset file beneath it), or "" if path is not a migration path. Grouping
// at the segment keeps the migration dir intact regardless of the depth cap.
//
// Matching is on whole path SEGMENTS, not raw substrings: "migrations" matches
// a `.../migrations/...` directory but NOT a Java class dir like
// `.../CustomerMigrations/...`. A `.sql` file beneath a `resources/` segment is
// also treated as DDL, grouped at its immediate parent dir.
func migrationDirOf(path string) string {
	p := strings.TrimPrefix(path, "./")
	segs := strings.Split(p, "/")
	if len(segs) <= 1 {
		return ""
	}
	lower := make([]string, len(segs))
	for i, s := range segs {
		lower[i] = strings.ToLower(s)
	}
	for _, pat := range migrationSegments {
		patSegs := strings.Split(pat, "/")
		if end := segSeqIndex(lower, patSegs); end >= 0 {
			return strings.Join(segs[:end+1], "/")
		}
	}
	if strings.HasSuffix(lower[len(lower)-1], ".sql") && containsSeg(lower, "resources") {
		return strings.Join(segs[:len(segs)-1], "/") // immediate parent dir
	}
	return ""
}

// segSeqIndex returns the index of the LAST segment of the first occurrence of
// pat as a consecutive run within segs, or -1 if pat does not appear.
func segSeqIndex(segs, pat []string) int {
	for i := 0; i+len(pat) <= len(segs); i++ {
		match := true
		for j := range pat {
			if segs[i+j] != pat[j] {
				match = false
				break
			}
		}
		if match {
			return i + len(pat) - 1
		}
	}
	return -1
}

// containsSeg reports whether segs contains the exact segment s.
func containsSeg(segs []string, s string) bool {
	for _, seg := range segs {
		if seg == s {
			return true
		}
	}
	return false
}

// containsSeedTerm reports whether a dir path has a generic sensitive term as a
// whole delimited token. Tokenizing on path + word delimiters (/ - _ .) is what
// distinguishes a genuine sensitive dir from company branding: "auth-microservice"
// → {auth, microservice} matches "auth", but "fusionauth" and "smartcredit" stay
// whole and do NOT match "auth"/"credit". Substring matching (the old behaviour)
// flagged nearly every CD path and is why --seed-keywords was unusable.
func containsSeedTerm(dir string) bool {
	for _, tok := range tokenizePath(dir) {
		for _, t := range seedTerms {
			if tok == t {
				return true
			}
		}
	}
	return false
}

// tokenizePath splits a path into lowercased tokens on path and word delimiters.
func tokenizePath(p string) []string {
	return strings.FieldsFunc(strings.ToLower(p), func(r rune) bool {
		return r == '/' || r == '-' || r == '_' || r == '.'
	})
}

// ratio guards against a zero baseline (returns 0 so an undefined ratio never
// ranks a dir top on a divide-by-zero).
func ratio(v, base float64) float64 {
	if base <= 0 {
		return 0
	}
	return v / base
}

// medianOf returns the median of xs (0 for empty).
func medianOf(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}
