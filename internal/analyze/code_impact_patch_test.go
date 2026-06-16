package analyze

import (
	"math"
	"testing"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

// defaultPatchConfig mirrors DefaultScoringConfig's code_impact tunables: the
// churn / bulk-import knobs OFF and bulk-data-dump dampening ON — the
// production default.
func defaultPatchConfig() config.CodeImpactConfig {
	return config.CodeImpactConfig{
		Alpha:              1,
		Beta:               0.5,
		Gamma:              2,
		ChurnFloor:         0.5,
		ChurnFullAt:        4,
		BulkImportMinLOC:   5000,
		BulkImportAddRatio: 0.95,
		BulkImportMinFiles: 20,
		BulkImportWeight:   0.25,
		// Bulk-data-dump dampening ON (production default); these floors keep
		// small / non-data test PRs from ever being flagged as dumps.
		DumpWeight:       0,
		DumpDominance:    0.9,
		DumpMinFiles:     50,
		LOCCapPercentile: 99,
	}
}

func mergedAt(month string) *time.Time {
	m := cache.MustParseMonth(month)
	t := m.Start().AddDate(0, 0, 5)
	return &t
}

func TestChurnWeightRamp(t *testing.T) {
	ci := defaultPatchConfig() // floor 0.5, full-at 4
	cases := []struct {
		touches int
		want    float64
	}{
		{0, 0.5}, // never-touched falls to the floor
		{1, 0.5}, // touched once = boilerplate floor
		{2, 0.5 + (1.0/3.0)*0.5},
		{3, 0.5 + (2.0/3.0)*0.5},
		{4, 1.0}, // reaches full weight at ChurnFullAt
		{9, 1.0}, // saturates above
	}
	for _, tc := range cases {
		if got := churnWeight(tc.touches, ci); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("churnWeight(%d) = %v, want %v", tc.touches, got, tc.want)
		}
	}
}

func TestChurnWeightFullAtOneEdge(t *testing.T) {
	ci := defaultPatchConfig()
	ci.ChurnFullAt = 1
	if got := churnWeight(1, ci); got != 1.0 {
		t.Errorf("ChurnFullAt=1, touches=1 should be 1.0, got %v", got)
	}
	if got := churnWeight(0, ci); got != ci.ChurnFloor {
		t.Errorf("ChurnFullAt=1, touches=0 should be floor, got %v", got)
	}
}

func TestIsBulkImport(t *testing.T) {
	ci := defaultPatchConfig() // minLOC 5000, addRatio 0.95, minFiles 20

	added := func(n int) []cache.FileChange {
		out := make([]cache.FileChange, n)
		for i := range out {
			out[i] = cache.FileChange{Status: "added", Additions: 300}
		}
		return out
	}

	qualifying := cache.GitHubPR{Additions: 8000, Deletions: 100, FileChanges: added(25)}
	if !isBulkImport(qualifying, ci) {
		t.Error("a big all-additions many-added-files PR should qualify")
	}

	tooSmall := cache.GitHubPR{Additions: 1000, Deletions: 0, FileChanges: added(25)}
	if isBulkImport(tooSmall, ci) {
		t.Error("below BulkImportMinLOC must not qualify")
	}

	tooMuchDelete := cache.GitHubPR{Additions: 6000, Deletions: 4000, FileChanges: added(25)}
	if isBulkImport(tooMuchDelete, ci) {
		t.Error("low additions ratio must not qualify (real refactor, not a dump)")
	}

	tooFewFiles := cache.GitHubPR{Additions: 8000, Deletions: 0, FileChanges: added(5)}
	if isBulkImport(tooFewFiles, ci) {
		t.Error("few added-status files must not qualify")
	}
}

func TestEffectivePRLOCKnobsOff(t *testing.T) {
	ci := defaultPatchConfig() // both knobs off
	pr := cache.GitHubPR{
		Additions: 8000, Deletions: 100,
		FileChanges: []cache.FileChange{{Path: "a.go", Status: "added", Additions: 8000, Deletions: 100}},
	}
	if got := effectivePRLOC(pr, nil, ci); got != 8100 {
		t.Errorf("knobs off should pass raw LOC through, got %v want 8100", got)
	}
}

func TestEffectivePRLOCBulkOnly(t *testing.T) {
	ci := defaultPatchConfig()
	ci.BulkImportDampening = true
	added := make([]cache.FileChange, 25)
	for i := range added {
		added[i] = cache.FileChange{Status: "added", Additions: 320}
	}
	pr := cache.GitHubPR{Additions: 8000, Deletions: 0, FileChanges: added}
	// Qualifies → 0.25 * 8000 (churn off, so PR-level raw LOC is damped directly).
	if got := effectivePRLOC(pr, nil, ci); math.Abs(got-2000) > 1e-9 {
		t.Errorf("bulk-import dampening = %v, want 2000", got)
	}
}

func TestEffectivePRLOCChurnOnly(t *testing.T) {
	ci := defaultPatchConfig()
	ci.ChurnWeighting = true
	pr := cache.GitHubPR{
		Additions: 300, Deletions: 0,
		FileChanges: []cache.FileChange{
			{Path: "boiler.go", Additions: 200}, // touched once → floor 0.5
			{Path: "hot.go", Additions: 100},    // touched a lot → 1.0
		},
	}
	churn := map[string]int{"boiler.go": 1, "hot.go": 9}
	// 0.5*200 + 1.0*100 = 200
	if got := effectivePRLOC(pr, churn, ci); math.Abs(got-200) > 1e-9 {
		t.Errorf("churn-weighted LOC = %v, want 200", got)
	}
}

func TestEffectivePRLOCChurnNoFileDetailFallsBack(t *testing.T) {
	ci := defaultPatchConfig()
	ci.ChurnWeighting = true
	// No FileChanges → can't churn-weight → raw LOC passes through.
	pr := cache.GitHubPR{Additions: 300, Deletions: 50}
	if got := effectivePRLOC(pr, map[string]int{}, ci); got != 350 {
		t.Errorf("no FileChange detail should fall back to raw LOC, got %v want 350", got)
	}
}

func TestEffectiveLOCInWindowDefaultEqualsRaw(t *testing.T) {
	ci := defaultPatchConfig() // knobs off
	data := &Loaded{PRs: []cache.GitHubPR{
		{Number: 1, Merged: mergedAt("2026-02"), Additions: 100, Deletions: 20},
		{Number: 2, Merged: mergedAt("2026-03"), Additions: 200, Deletions: 0},
		{Number: 3, Merged: mergedAt("2026-09"), Additions: 999, Deletions: 0}, // out of window
		{Number: 4, Merged: nil, Additions: 500, Deletions: 0},                 // unmerged
	}}
	start := cache.MustParseMonth("2026-01")
	end := cache.MustParseMonth("2026-04")
	// Only PRs 1 + 2 count: (120) + (200) = 320.
	if got := effectiveLOCInWindow(data, start, end, nil, ci, nil); got != 320 {
		t.Errorf("default effectiveLOCInWindow = %v, want 320 (raw merged-in-window sum)", got)
	}
}

func TestBuildChurnIndexCountsDistinctPRsPerPath(t *testing.T) {
	data := &Loaded{PRs: []cache.GitHubPR{
		{
			Number: 1, Merged: mergedAt("2026-01"),
			FileChanges: []cache.FileChange{{Path: "a.go"}, {Path: "b.go"}, {Path: "a.go"}}, // dup within PR
		},
		{
			Number: 2, Merged: mergedAt("2026-02"),
			FileChanges: []cache.FileChange{{Path: "a.go"}},
		},
		{
			Number: 3, Merged: nil, // unmerged → ignored
			FileChanges: []cache.FileChange{{Path: "a.go"}},
		},
		{
			Number: 4, Merged: mergedAt("2026-03"),
			Files: []string{"c.go"}, // no FileChanges → fall back to Files
		},
	}}
	idx := buildChurnIndex(data)
	if idx["a.go"] != 2 {
		t.Errorf("a.go churn = %d, want 2 (PR1 dedup'd, PR2; PR3 unmerged)", idx["a.go"])
	}
	if idx["b.go"] != 1 {
		t.Errorf("b.go churn = %d, want 1", idx["b.go"])
	}
	if idx["c.go"] != 1 {
		t.Errorf("c.go churn = %d, want 1 (Files fallback)", idx["c.go"])
	}
}

// --- Deletion-weighting -------------------------------------------------

func TestWeightedLOCGateOffEqualsRaw(t *testing.T) {
	ci := defaultPatchConfig() // DeletionWeighting off
	ci.DeletionWeight = 0.25   // set but ignored while the gate is off
	if got := weightedLOC(100, 900, ci); got != 1000 {
		t.Errorf("gate off must sum additions+deletions, got %v want 1000", got)
	}
}

func TestWeightedLOCGateOn(t *testing.T) {
	ci := defaultPatchConfig()
	ci.DeletionWeighting = true
	ci.DeletionWeight = 0.25
	// 100 additions + 0.25*900 deletions = 325.
	if got := weightedLOC(100, 900, ci); math.Abs(got-325) > 1e-9 {
		t.Errorf("deletion-weighted LOC = %v, want 325", got)
	}
}

func TestEffectivePRLOCDeletionWeighted(t *testing.T) {
	ci := defaultPatchConfig()
	ci.DeletionWeighting = true
	ci.DeletionWeight = 0.25
	// A pure dead-code-removal PR (the JE-CD CD-4992 shape): tiny additions,
	// huge deletions, diverse extensions so neither dump nor bulk-import fires.
	pr := cache.GitHubPR{Additions: 63, Deletions: 222606}
	want := 63.0 + 0.25*222606.0
	if got := effectivePRLOC(pr, nil, ci); math.Abs(got-want) > 1e-9 {
		t.Errorf("deletion-weighted PR LOC = %v, want %v", got, want)
	}
	// Gate off → the same PR counts every deleted line 1:1.
	off := ci
	off.DeletionWeighting = false
	if got := effectivePRLOC(pr, nil, off); got != 222669 {
		t.Errorf("gate off should pass full raw LOC, got %v want 222669", got)
	}
}

// --- Bulk-data-dump dampening (dashboard-overhaul code_impact rework) ---

// filesWithExt builds n file paths all carrying the given extension.
func filesWithExt(n int, ext string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "data/f" + string(rune('a'+i%26)) + "." + ext
	}
	return out
}

func TestIsBulkDataDump(t *testing.T) {
	ci := defaultPatchConfig() // dominance 0.9, min-files 50

	dump := isBulkDataDump(filesWithExt(100, "json"), ci)
	if !dump {
		t.Error("100 json files (≥50, 100%% one data ext) should be a dump")
	}

	// A large source-code PR dominated by .go is NOT a dump (not a data ext).
	if isBulkDataDump(filesWithExt(100, "go"), ci) {
		t.Error("source-code PR (.go) must never be flagged as a data dump")
	}
	// .sql is deliberately excluded (migrations are legitimate work).
	if isBulkDataDump(filesWithExt(100, "sql"), ci) {
		t.Error("sql-dominated PR must not be flagged (excluded ext)")
	}
	// Below the file-count floor.
	if isBulkDataDump(filesWithExt(20, "json"), ci) {
		t.Error("20 json files is below the dump file floor")
	}
	// Mixed: 60 json + 50 go = 54%% dominant, below the 0.9 dominance gate.
	mixed := append(filesWithExt(60, "json"), filesWithExt(50, "go")...)
	if isBulkDataDump(mixed, ci) {
		t.Error("a PR only 54%% one extension must not be flagged")
	}
}

func TestEffectivePRLOCDumpDampened(t *testing.T) {
	ci := defaultPatchConfig() // DumpWeight 0

	fcs := make([]cache.FileChange, 100)
	for i := range fcs {
		fcs[i] = cache.FileChange{Path: "fixtures/f.json", Status: "added", Additions: 5000, Deletions: 2000}
	}
	// PR-level LOC is what production dampens (truncation-correct), not the
	// per-file sum.
	dumpPR := cache.GitHubPR{Additions: 2_000_000, Deletions: 800_000, FileChanges: fcs}
	if got := effectivePRLOC(dumpPR, nil, ci); got != 0 {
		t.Errorf("a detected dump at DumpWeight=0 should contribute 0 LOC, got %v", got)
	}

	// Disable the knob → the same PR counts at full PR-level LOC.
	off := ci
	off.DisableDumpDampening = true
	if got := effectivePRLOC(dumpPR, nil, off); got != 2_800_000 {
		t.Errorf("with dump dampening off, raw PR LOC should pass through, got %v", got)
	}

	// A normal source PR is untouched.
	src := cache.GitHubPR{Additions: 800, Deletions: 200,
		FileChanges: []cache.FileChange{{Path: "main.go", Additions: 800, Deletions: 200}}}
	if got := effectivePRLOC(src, nil, ci); got != 1000 {
		t.Errorf("normal source PR should pass through at raw LOC, got %v", got)
	}
}

func TestEffectiveUniqueFilesDumpDampened(t *testing.T) {
	ci := defaultPatchConfig() // DumpWeight 0
	norm := config.NormalizeConfig{GeneratedFileWeight: 0.25}

	dumpFiles := filesWithExt(100, "json")
	data := &Loaded{PRs: []cache.GitHubPR{
		{Merged: mergedAt("2026-03"), Files: dumpFiles},
		{Merged: mergedAt("2026-03"), Files: []string{"main.go", "util.go"}},
	}}
	start := cache.MustParseMonth("2026-01")
	end := cache.MustParseMonth("2026-04")
	// Dump json files → 0 weight; the two .go files → 1.0 each = 2.0.
	if got := effectiveUniqueFilesInWindow(data, start, end, norm, ci, nil); math.Abs(got-2.0) > 1e-9 {
		t.Errorf("dump data files should be dampened out of the file count, got %v (want 2.0)", got)
	}
}
