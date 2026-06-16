// velocity-rating-audit is the read-only validation harness for the
// rating/composite overhaul (see _control-center roadmap, Phase 0). It does
// NOT touch live ratings.json — it runs analyze.Run with NoPersist so the
// whole Elo + composite computation happens in memory against the read-only
// cache.
//
// Two modes:
//
//	velocity-rating-audit snapshot <out.json>
//	    Compute the current leaderboard read-only and write a per-dev snapshot
//	    (composite rank/score, Elo rank/rating, periods played, first Elo
//	    period, Spearman ρ). Run once on the baseline code, then again after a
//	    change, to capture before/after.
//
//	velocity-rating-audit diff <baseline.json> <candidate.json> [--expect e.json]
//	    Join two snapshots by dev, print the rank/score/rating movement table
//	    sorted by composite-rank movement, report Δρ (DIAGNOSTIC ONLY — ρ
//	    measures Elo↔composite self-agreement, not accuracy, and is EXPECTED to
//	    fall when Elo is intentionally decoupled), and evaluate the locked
//	    directional expectations as PASS/FAIL.
//
// Why directional expectations and not ρ as the gate: there is no external
// ground-truth dev ranking in the system (CD doesn't even populate Jira SP),
// and ρ is partly tautological because Elo's `actual` is derived from the
// composite today. Evidence-backed per-dev directional checks are the honest
// gate given no labeled ground truth.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/mathewepstein/velocity/internal/analyze"
	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "snapshot":
		if len(os.Args) < 3 {
			usage()
		}
		integration := false
		for _, a := range os.Args[3:] {
			if a == "--integration" {
				integration = true
			}
		}
		err = snapshotCmd(os.Args[2], integration)
	case "diff":
		if len(os.Args) < 4 {
			usage()
		}
		expect := ""
		args := os.Args[4:]
		for i := 0; i < len(args); i++ {
			if args[i] == "--expect" && i+1 < len(args) {
				expect = args[i+1]
				i++
			}
		}
		err = diffCmd(os.Args[2], os.Args[3], expect)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  velocity-rating-audit snapshot <out.json> [--integration]")
	fmt.Fprintln(os.Stderr, "  velocity-rating-audit diff <baseline.json> <candidate.json> [--expect expectations.json]")
	os.Exit(2)
}

// --- snapshot ---------------------------------------------------------------

type devRow struct {
	Dev            string  `json:"dev"`
	CompositeScore float64 `json:"composite_score"`
	CompositeRank  int     `json:"composite_rank"`
	EloRating      float64 `json:"elo_rating"`
	EloRank        int     `json:"elo_rank"`
	PeriodsPlayed  int     `json:"periods_played"`
	FirstPeriod    string  `json:"first_period"` // YYYY-MM-DD of first Elo period, "" if never played
	Provisional    bool    `json:"provisional"`
}

type snapshot struct {
	Label       string   `json:"label"`
	GeneratedAt string   `json:"generated_at"`
	SpearmanRho float64  `json:"spearman_rho_elo_composite"`
	Devs        []devRow `json:"devs"`
}

func snapshotCmd(out string, enableIntegration bool) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	store, err := cache.OpenStore()
	if err != nil {
		return fmt.Errorf("open cache: %w", err)
	}
	defer store.Close()

	// In-memory profile override so the OFF→ON comparison needs no config edit
	// and never persists (NoPersist below). The composite and Elo paths share
	// the same weighter built from this profile, so both axes reflect the flag.
	profile := cfg.ActiveProfile()
	if enableIntegration {
		profile.Scoring.Integration.Enabled = true
	}

	// Rebuild + NoPersist: walk every period in memory from a clean slate,
	// never writing ratings.json.
	res, err := analyze.Run(analyze.Options{
		Profile:   profile,
		Now:       time.Now(),
		Rebuild:   true,
		NoPersist: true,
		Store:     store,
	})
	if err != nil {
		return fmt.Errorf("analyze run: %w", err)
	}

	snap := snapshot{
		Label:       out,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		SpearmanRho: res.Meta.EloCompositeSpearman,
	}
	// Elo ranks: rated, played devs ordered by current rating desc.
	type er struct {
		name string
		cur  float64
	}
	var rated []er
	for _, d := range res.Devs {
		if d.Dev.DisplayName == "unknown" || d.Score == nil {
			continue
		}
		if d.Rating != nil && d.Rating.PeriodsPlayed > 0 {
			rated = append(rated, er{d.Dev.DisplayName, d.Rating.Current})
		}
	}
	sort.SliceStable(rated, func(i, j int) bool { return rated[i].cur > rated[j].cur })
	eloRank := make(map[string]int, len(rated))
	for i, r := range rated {
		eloRank[r.name] = i + 1
	}

	for _, d := range res.Devs {
		if d.Dev.DisplayName == "unknown" || d.Score == nil {
			continue
		}
		row := devRow{
			Dev:            d.Dev.DisplayName,
			CompositeScore: d.Score.Total,
			CompositeRank:  d.Score.Rank,
		}
		if d.Rating != nil {
			row.EloRating = d.Rating.Current
			row.PeriodsPlayed = d.Rating.PeriodsPlayed
			row.Provisional = d.Rating.Provisional
			row.EloRank = eloRank[d.Dev.DisplayName]
			if len(d.Rating.HistoryDates) > 0 {
				row.FirstPeriod = d.Rating.HistoryDates[0]
			}
		}
		snap.Devs = append(snap.Devs, row)
	}
	sort.SliceStable(snap.Devs, func(i, j int) bool { return snap.Devs[i].CompositeRank < snap.Devs[j].CompositeRank })

	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, b, 0o600); err != nil {
		return err
	}
	fmt.Printf("wrote %s — %d devs, ρ(elo↔composite)=%.4f\n", out, len(snap.Devs), snap.SpearmanRho)
	return nil
}

// --- diff -------------------------------------------------------------------

type expectation struct {
	Dev        string  `json:"dev"`
	Check      string  `json:"check"` // first_period_on_or_after | composite_rank_dir | elo_rating_dir | periods_played_dir
	Value      string  `json:"value"` // direction ("up"/"down"/"stable") or a YYYY-MM-DD date
	MaxAbsDrop float64 `json:"max_abs_drop,omitempty"`
	Note       string  `json:"note,omitempty"`
}

type expectFile struct {
	Expectations           []expectation `json:"expectations"`
	MaxUnexplainedRankMove int           `json:"max_unexplained_rank_move"`
}

func loadSnapshot(path string) (snapshot, error) {
	var s snapshot
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(b, &s)
	return s, err
}

func diffCmd(basePath, candPath, expectPath string) error {
	base, err := loadSnapshot(basePath)
	if err != nil {
		return fmt.Errorf("read baseline: %w", err)
	}
	cand, err := loadSnapshot(candPath)
	if err != nil {
		return fmt.Errorf("read candidate: %w", err)
	}
	bm := map[string]devRow{}
	for _, d := range base.Devs {
		bm[d.Dev] = d
	}
	cm := map[string]devRow{}
	for _, d := range cand.Devs {
		cm[d.Dev] = d
	}

	// Union of devs, sorted by absolute composite-rank movement.
	names := map[string]struct{}{}
	for n := range bm {
		names[n] = struct{}{}
	}
	for n := range cm {
		names[n] = struct{}{}
	}
	type movement struct {
		name      string
		b, c      devRow
		inB, inC  bool
		rankDelta int
	}
	var moves []movement
	for n := range names {
		b, inB := bm[n]
		c, inC := cm[n]
		rd := 0
		if inB && inC {
			rd = c.CompositeRank - b.CompositeRank
		}
		moves = append(moves, movement{n, b, c, inB, inC, rd})
	}
	sort.SliceStable(moves, func(i, j int) bool {
		ai, aj := abs(moves[i].rankDelta), abs(moves[j].rankDelta)
		if ai != aj {
			return ai > aj
		}
		return moves[i].name < moves[j].name
	})

	w := os.Stdout
	fmt.Fprintf(w, "# Rating audit diff: %s  →  %s\n", basePath, candPath)
	fmt.Fprintf(w, "# ρ(elo↔composite): %.4f → %.4f (Δ %+.4f)  [DIAGNOSTIC ONLY — self-agreement, expected to fall when Elo decouples]\n#\n", base.SpearmanRho, cand.SpearmanRho, cand.SpearmanRho-base.SpearmanRho)
	fmt.Fprintf(w, "%-22s %12s %14s %12s %10s %s\n", "dev", "compRank", "compScore", "elo", "periods", "firstPeriod")
	for _, m := range moves {
		if !m.inB {
			fmt.Fprintf(w, "%-22s %12s  (new in candidate)\n", trunc(m.name, 21), "—")
			continue
		}
		if !m.inC {
			fmt.Fprintf(w, "%-22s %12s  (dropped from candidate)\n", trunc(m.name, 21), "—")
			continue
		}
		fmt.Fprintf(w, "%-22s %5d→%-3d(%+d) %6.2f→%-6.2f %5.0f→%-5.0f %4d→%-3d %s→%s\n",
			trunc(m.name, 21),
			m.b.CompositeRank, m.c.CompositeRank, m.rankDelta,
			m.b.CompositeScore, m.c.CompositeScore,
			m.b.EloRating, m.c.EloRating,
			m.b.PeriodsPlayed, m.c.PeriodsPlayed,
			dash(m.b.FirstPeriod), dash(m.c.FirstPeriod))
	}

	if expectPath == "" {
		fmt.Fprintf(w, "\n(no --expect file; movement table only)\n")
		return nil
	}

	eb, err := os.ReadFile(expectPath)
	if err != nil {
		return fmt.Errorf("read expectations: %w", err)
	}
	var ef expectFile
	if err := json.Unmarshal(eb, &ef); err != nil {
		return fmt.Errorf("parse expectations: %w", err)
	}

	fmt.Fprintf(w, "\n## Directional expectations\n")
	named := map[string]struct{}{}
	pass, fail := 0, 0
	for _, e := range ef.Expectations {
		named[e.Dev] = struct{}{}
		c, okc := cm[e.Dev]
		b, okb := bm[e.Dev]
		ok, detail := evalExpectation(e, b, okb, c, okc)
		status := "FAIL"
		if ok {
			status = "PASS"
			pass++
		} else {
			fail++
		}
		fmt.Fprintf(w, "  [%s] %-16s %-26s %s\n", status, trunc(e.Dev, 15), e.Check, detail)
		if e.Note != "" {
			fmt.Fprintf(w, "         note: %s\n", e.Note)
		}
	}

	// Unexplained large movers: anyone moving more than the threshold who is
	// not named in an expectation needs a mechanism explanation.
	if ef.MaxUnexplainedRankMove > 0 {
		fmt.Fprintf(w, "\n## Unexplained movers (|Δrank| > %d, not named in expectations)\n", ef.MaxUnexplainedRankMove)
		any := false
		for _, m := range moves {
			if !m.inB || !m.inC {
				continue
			}
			if abs(m.rankDelta) > ef.MaxUnexplainedRankMove {
				if _, ok := named[m.name]; !ok {
					fmt.Fprintf(w, "  WARN  %-22s compRank %d→%d (%+d)\n", trunc(m.name, 21), m.b.CompositeRank, m.c.CompositeRank, m.rankDelta)
					any = true
				}
			}
		}
		if !any {
			fmt.Fprintf(w, "  none\n")
		}
	}

	fmt.Fprintf(w, "\n%d passed, %d failed\n", pass, fail)
	if fail > 0 {
		os.Exit(1)
	}
	return nil
}

// evalExpectation returns (pass, human-readable detail).
func evalExpectation(e expectation, b devRow, okb bool, c devRow, okc bool) (bool, string) {
	switch e.Check {
	case "first_period_on_or_after":
		if !okc {
			return false, "candidate missing dev"
		}
		if c.FirstPeriod == "" {
			return false, "candidate has no Elo period (never played)"
		}
		ok := c.FirstPeriod >= e.Value
		return ok, fmt.Sprintf("first_period=%s, want ≥ %s", c.FirstPeriod, e.Value)
	case "composite_rank_dir":
		if !okb || !okc {
			return false, "missing in one snapshot"
		}
		d := c.CompositeRank - b.CompositeRank // + = worse rank (down), - = better (up)
		return dirOK(e.Value, float64(-d)), fmt.Sprintf("compRank %d→%d (%+d), want %s", b.CompositeRank, c.CompositeRank, d, e.Value)
	case "elo_rating_dir":
		if !okb || !okc {
			return false, "missing in one snapshot"
		}
		d := c.EloRating - b.EloRating
		dirok := dirOK(e.Value, d)
		detail := fmt.Sprintf("elo %.0f→%.0f (%+.0f), want %s", b.EloRating, c.EloRating, d, e.Value)
		if dirok && e.MaxAbsDrop > 0 && d < 0 && -d > e.MaxAbsDrop {
			return false, detail + fmt.Sprintf(" but drop %.0f exceeds max_abs_drop %.0f (not gentle)", -d, e.MaxAbsDrop)
		}
		return dirok, detail
	case "periods_played_dir":
		if !okb || !okc {
			return false, "missing in one snapshot"
		}
		d := c.PeriodsPlayed - b.PeriodsPlayed
		return dirOK(e.Value, float64(d)), fmt.Sprintf("periods %d→%d (%+d), want %s", b.PeriodsPlayed, c.PeriodsPlayed, d, e.Value)
	default:
		return false, "unknown check: " + e.Check
	}
}

// dirOK reports whether signed delta matches direction ("up">0, "down"<0,
// "stable" within a small tolerance).
func dirOK(dir string, delta float64) bool {
	switch dir {
	case "up":
		return delta > 0
	case "down":
		return delta < 0
	case "stable":
		return delta > -1.5 && delta < 1.5
	}
	return false
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
