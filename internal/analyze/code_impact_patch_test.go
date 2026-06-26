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
		DumpWeight:         0,
		DumpDominance:      0.9,
		DumpMinFiles:       50,
		DataFileLOCCeiling: 2000,
		// Boilerplate-family dampening ON (production default).
		FamilyMinSize:    3,
		FamilyMaxSizeCV:  0.25,
		FamilyWeight:     0.15,
		FamilyMaskMaxLen: 4,
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
	if got := effectivePRLOC(pr, nil, nil, ci, testNorm()); got != 8100 {
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
	if got := effectivePRLOC(pr, nil, nil, ci, testNorm()); math.Abs(got-2000) > 1e-9 {
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
	if got := effectivePRLOC(pr, churn, nil, ci, testNorm()); math.Abs(got-200) > 1e-9 {
		t.Errorf("churn-weighted LOC = %v, want 200", got)
	}
}

func TestEffectivePRLOCChurnNoFileDetailFallsBack(t *testing.T) {
	ci := defaultPatchConfig()
	ci.ChurnWeighting = true
	// No FileChanges → can't churn-weight → raw LOC passes through.
	pr := cache.GitHubPR{Additions: 300, Deletions: 50}
	if got := effectivePRLOC(pr, map[string]int{}, nil, ci, testNorm()); got != 350 {
		t.Errorf("no FileChange detail should fall back to raw LOC, got %v want 350", got)
	}
}

func TestFamilyStem(t *testing.T) {
	cases := []struct {
		path     string
		wantStem string
		wantOK   bool
	}{
		// Short product/env codes masked; long tokens kept; dir preserved.
		{"build/bash/app-install-cbm-dev.sh", "build/bash|#-install-#-#.sh", true},
		{"build/bash/app-install-cs-prod.sh", "build/bash|#-install-#-#.sh", true},
		// Same basename, different dir → different family.
		{"other/app-install-cbm-dev.sh", "other|#-install-#-#.sh", true},
		// camelCase source is a single long token → no maskable code → not grouped.
		{"src/UserController.java", "", false},
		// A name with only long tokens → not a copy-variant.
		{"infra/variables.tf", "", false},
	}
	for _, tc := range cases {
		gotStem, gotOK := familyStem(tc.path, 4)
		if gotOK != tc.wantOK || (gotOK && gotStem != tc.wantStem) {
			t.Errorf("familyStem(%q) = (%q,%v), want (%q,%v)", tc.path, gotStem, gotOK, tc.wantStem, tc.wantOK)
		}
	}
}

func TestBuildFamilyIndex(t *testing.T) {
	ci := defaultPatchConfig()
	added := func(path string, loc int) cache.FileChange {
		return cache.FileChange{Path: path, Status: "added", Additions: loc}
	}
	data := &Loaded{PRs: []cache.GitHubPR{{
		Merged: mergedAt("2026-02"),
		FileChanges: []cache.FileChange{
			// Tight copy-family of 3 (CV≈0): two redundant copies dampened.
			added("infra/deploy-cbm-dev.sh", 100),
			added("infra/deploy-cs-dev.sh", 100),
			added("infra/deploy-cp-dev.sh", 100),
			// Same-named-but-divergent family (CV high): NOT boilerplate, untouched.
			added("mod/role-aaa-x.tf", 10),
			added("mod/role-bbb-x.tf", 500),
			added("mod/role-ccc-x.tf", 30),
			// A lone file with a maskable token but no family (<MinSize).
			added("one/setup-q1-dev.sh", 80),
		},
	}}}
	fam := buildFamilyIndex(data, ci)
	// Exactly two redundant copies flagged (one representative survives per family).
	if len(fam) != 2 {
		t.Fatalf("family index size = %d, want 2; got %v", len(fam), fam)
	}
	for _, p := range []string{"infra/deploy-cs-dev.sh", "infra/deploy-cp-dev.sh"} {
		if w := familyWeight(fam, p); w != ci.FamilyWeight {
			t.Errorf("redundant copy %q weight = %v, want %v", p, w, ci.FamilyWeight)
		}
	}
	// Representative (largest/first of the tight family) keeps full weight.
	if w := familyWeight(fam, "infra/deploy-cbm-dev.sh"); w != 1.0 {
		t.Errorf("representative weight = %v, want 1.0", w)
	}
	// Divergent-size family is spared by the CV gate.
	for _, p := range []string{"mod/role-aaa-x.tf", "mod/role-bbb-x.tf", "mod/role-ccc-x.tf"} {
		if w := familyWeight(fam, p); w != 1.0 {
			t.Errorf("divergent same-named file %q weight = %v, want 1.0 (CV gate)", p, w)
		}
	}
}

func TestEffectivePRLOCFamilyDampening(t *testing.T) {
	ci := defaultPatchConfig() // boilerplate dampening on, churn off
	pr := cache.GitHubPR{
		Additions: 300, Deletions: 0,
		FileChanges: []cache.FileChange{
			{Path: "infra/deploy-cbm-dev.sh", Status: "added", Additions: 100}, // representative
			{Path: "infra/deploy-cs-dev.sh", Status: "added", Additions: 100},  // redundant
			{Path: "infra/deploy-cp-dev.sh", Status: "added", Additions: 100},  // redundant
		},
	}
	family := map[string]float64{
		"infra/deploy-cs-dev.sh": ci.FamilyWeight,
		"infra/deploy-cp-dev.sh": ci.FamilyWeight,
	}
	// 100 (full) + 0.15*100 + 0.15*100 = 130.
	if got := effectivePRLOC(pr, nil, family, ci, testNorm()); math.Abs(got-130) > 1e-9 {
		t.Errorf("family-dampened LOC = %v, want 130", got)
	}
	// Nil family → no dampening, raw 300.
	if got := effectivePRLOC(pr, nil, nil, ci, testNorm()); math.Abs(got-300) > 1e-9 {
		t.Errorf("nil family LOC = %v, want 300", got)
	}
}

func TestEffectivePRLOCExcludesGeneratedLOC(t *testing.T) {
	ci := defaultPatchConfig() // churn/family off → PR-level path
	norm := config.NormalizeConfig{GeneratedFilePatterns: []string{"*/generated/*", "*.lock"}}
	pr := cache.GitHubPR{
		Additions: 10100, Deletions: 0,
		FileChanges: []cache.FileChange{
			{Path: "src/model/generated/Stub.java", Status: "added", Additions: 10000}, // generated → excluded
			{Path: "src/Service.java", Status: "added", Additions: 100},                // real
		},
	}
	// Generated 10000 fully excluded → only the 100 real lines count.
	if got := effectivePRLOC(pr, nil, nil, ci, norm); math.Abs(got-100) > 1e-9 {
		t.Errorf("generated-excluded LOC = %v, want 100", got)
	}
	// No patterns → nothing excluded → raw 10100 (the pre-change behavior).
	if got := effectivePRLOC(pr, nil, nil, ci, config.NormalizeConfig{}); got != 10100 {
		t.Errorf("empty norm should pass raw LOC, got %v want 10100", got)
	}
}

func TestRollupMonthlyExcludesGeneratedLOC(t *testing.T) {
	ci := defaultPatchConfig()
	norm := config.NormalizeConfig{GeneratedFilePatterns: []string{"*/generated/*"}}
	data := &Loaded{PRs: []cache.GitHubPR{{
		Number: 1, Merged: mergedAt("2026-02"),
		Additions: 10050, Deletions: 0,
		Files: []string{"src/model/generated/Stub.java", "src/Service.java"},
		FileChanges: []cache.FileChange{
			{Path: "src/model/generated/Stub.java", Status: "added", Additions: 10000},
			{Path: "src/Service.java", Status: "added", Additions: 50},
		},
	}}}
	start := cache.MustParseMonth("2026-02")
	rows := rollupMonthly(data, start, start, ci, norm)
	// L netted to 50 (generated 10000 removed); F=2 raw files; P=1.
	want := math.Sqrt(1*2 + 0.5*50 + 2*1)
	if math.Abs(rows[0].CodeImpact-want) > 1e-9 {
		t.Errorf("monthly code_impact = %v, want %v (generated LOC excluded)", rows[0].CodeImpact, want)
	}
	// Display LOCAdded stays raw — only the formula input is netted.
	if rows[0].LOCAdded != 10050 {
		t.Errorf("display LOCAdded should stay raw 10050, got %d", rows[0].LOCAdded)
	}
}

func TestEffectivePRLOCExcludesOversizedDataFile(t *testing.T) {
	ci := defaultPatchConfig() // DataFileLOCCeiling 2000, dump dampening on
	pr := cache.GitHubPR{
		Additions: 30150, Deletions: 0,
		FileChanges: []cache.FileChange{
			{Path: "src/test/resources/3b/report.json", Status: "added", Additions: 30000}, // dumped report > ceiling → excluded
			{Path: "config/app.json", Status: "added", Additions: 50},                       // small config json → counted
			{Path: "src/Service.go", Status: "added", Additions: 100},                       // real code → counted
		},
	}
	// 30000 (oversized json) excluded; 50 + 100 = 150 remain.
	if got := effectivePRLOC(pr, nil, nil, ci, config.NormalizeConfig{}); math.Abs(got-150) > 1e-9 {
		t.Errorf("oversized-data-excluded LOC = %v, want 150", got)
	}
	// Disabling the ceiling (set huge) counts the dump again.
	hi := ci
	hi.DataFileLOCCeiling = 1 << 30
	if got := effectivePRLOC(pr, nil, nil, hi, config.NormalizeConfig{}); math.Abs(got-30150) > 1e-9 {
		t.Errorf("ceiling disabled should count raw LOC, got %v want 30150", got)
	}
}

func TestSizeCeilingRequiresDataExtensionAndSize(t *testing.T) {
	ci := defaultPatchConfig() // ceiling 2000
	// A large hand-authored CODE file: over the line minimum but NOT a data
	// extension → never excluded (size alone must not trigger exclusion).
	bigCode := cache.GitHubPR{Additions: 5000, FileChanges: []cache.FileChange{
		{Path: "internal/service/handler.go", Status: "added", Additions: 5000}}}
	if got := effectivePRLOC(bigCode, nil, nil, ci, config.NormalizeConfig{}); math.Abs(got-5000) > 1e-9 {
		t.Errorf("large code file must NOT be excluded by size, got %v want 5000", got)
	}
	// XML obeys the same size rule as any data format — count-based dumping
	// doesn't apply to it, but a single oversized XML is a dump/localization
	// artifact and IS excluded; a small XML config is real work and is kept.
	smallXML := cache.GitHubPR{Additions: 300, FileChanges: []cache.FileChange{
		{Path: "src/main/webapp/WEB-INF/web.xml", Status: "added", Additions: 300}}}
	if got := effectivePRLOC(smallXML, nil, nil, ci, config.NormalizeConfig{}); math.Abs(got-300) > 1e-9 {
		t.Errorf("small XML config should count as real work, got %v want 300", got)
	}
	bigXML := cache.GitHubPR{Additions: 9000, FileChanges: []cache.FileChange{
		{Path: "res/values/strings.xml", Status: "added", Additions: 9000}}}
	if got := effectivePRLOC(bigXML, nil, nil, ci, config.NormalizeConfig{}); math.Abs(got-0) > 1e-9 {
		t.Errorf("oversized XML dump should be excluded by size, got %v want 0", got)
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
	if got := effectiveLOCInWindow(data, start, end, nil, nil, ci, testNorm(), nil); got != 320 {
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
	if got := effectivePRLOC(pr, nil, nil, ci, testNorm()); math.Abs(got-want) > 1e-9 {
		t.Errorf("deletion-weighted PR LOC = %v, want %v", got, want)
	}
	// Gate off → the same PR counts every deleted line 1:1.
	off := ci
	off.DeletionWeighting = false
	if got := effectivePRLOC(pr, nil, nil, off, testNorm()); got != 222669 {
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
	if got := effectivePRLOC(dumpPR, nil, nil, ci, testNorm()); got != 0 {
		t.Errorf("a detected dump at DumpWeight=0 should contribute 0 LOC, got %v", got)
	}

	// Disable the knob → the same PR counts at full PR-level LOC.
	off := ci
	off.DisableDumpDampening = true
	if got := effectivePRLOC(dumpPR, nil, nil, off, testNorm()); got != 2_800_000 {
		t.Errorf("with dump dampening off, raw PR LOC should pass through, got %v", got)
	}

	// A normal source PR is untouched.
	src := cache.GitHubPR{Additions: 800, Deletions: 200,
		FileChanges: []cache.FileChange{{Path: "main.go", Additions: 800, Deletions: 200}}}
	if got := effectivePRLOC(src, nil, nil, ci, testNorm()); got != 1000 {
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
	if got := effectiveUniqueFilesInWindow(data, start, end, norm, ci, nil, nil); math.Abs(got-2.0) > 1e-9 {
		t.Errorf("dump data files should be dampened out of the file count, got %v (want 2.0)", got)
	}
}
