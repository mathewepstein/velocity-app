package analyze

import (
	"math"
	"strings"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

// dumpDataExtensions drives the COUNT-based multi-file bulk-dump rule: a PR
// dominated by many files of one of these formats is a dump. These are formats
// where "lots of files of this type" is itself a reliable dump signal — you
// don't hand-author 50 parquet/ndjson files. Deliberately excludes source code
// and ambiguous source-adjacent formats (.sql migrations, .yaml config, .svg).
//
// `.xml` is deliberately NOT here: many small XML files in one PR is commonly
// authored work (Android layouts, per-locale resource files, Spring configs),
// not a dump — so the file-COUNT heuristic is unsafe for XML. XML is instead
// caught by the per-file SIZE heuristic (isOversizedDataExt): a single very
// large XML file is a dump/generated/localization artifact in any ecosystem.
// This split keeps the rule org-agnostic — no judgement about whether a given
// org's XML is "mostly config" or "mostly data"; size decides per file.
var dumpDataExtensions = map[string]struct{}{
	"json": {}, "csv": {}, "tsv": {}, "ndjson": {}, "geojson": {},
	"parquet": {}, "snap": {},
}

// isOversizedDataExt reports whether ext is a serialized-data format subject to
// the per-file size ceiling. Superset of dumpDataExtensions plus xml: a single
// huge file of any of these is a dump regardless of ecosystem, whereas xml is
// excluded from the count-based rule above (many small XML files can be authored).
func isOversizedDataExt(ext string) bool {
	if _, ok := dumpDataExtensions[ext]; ok {
		return true
	}
	return ext == "xml"
}

// extLower returns a path's lowercased extension (no dot), or "".
func extLower(path string) string {
	base := path
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	if i := strings.LastIndexByte(base, '.'); i >= 0 {
		return strings.ToLower(base[i+1:])
	}
	return ""
}

// isDumpDataExt reports whether ext is a dampened serialized-data format.
func isDumpDataExt(ext string) bool {
	_, ok := dumpDataExtensions[ext]
	return ok
}

// isBulkDataDump reports whether a PR's file list looks like a bulk-data dump:
// large (≥ ci.DumpMinFiles files), dominated (≥ ci.DumpDominance) by a single
// extension, and that dominant extension is a serialized-data format. All three
// are required, so a big source-code PR or a small data PR is never flagged.
// Operates on the path list (p.Files / FileChanges paths), so it needs no
// per-file LOC and is unaffected by GitHub's per-file truncation of huge PRs.
func isBulkDataDump(paths []string, ci config.CodeImpactConfig) bool {
	if len(paths) < ci.DumpMinFiles || len(paths) == 0 {
		return false
	}
	counts := map[string]int{}
	topExt, topN := "", 0
	for _, p := range paths {
		e := extLower(p)
		counts[e]++
		if counts[e] > topN {
			topN, topExt = counts[e], e
		}
	}
	if float64(topN)/float64(len(paths)) < ci.DumpDominance {
		return false
	}
	return isDumpDataExt(topExt)
}

// Optional code_impact patches (dashboard-metrics-overhaul D4).
//
// Three opt-in knobs refine the L (LOC) input to code_impact so framework
// dumps, boilerplate, and bulk deletions stop registering as full substance.
// All default OFF in CodeImpactConfig; when off, effectiveLOCInWindow returns
// the raw merged-PR LOC sum, so code_impact is computed exactly as before.
//
//   0. Deletion-weighting — deleted lines count for DeletionWeight each instead
//      of 1:1 with additions (see weightedLOC), so a large dead-code removal no
//      longer reads as the same impact as writing the same volume.
//   1. Bulk-import dampening — a single PR that adds a large amount of code,
//      almost entirely additions, across many added-status files (a vendor /
//      boilerplate dump) has its LOC contribution scaled by BulkImportWeight.
//      A structural counterpart to the reactive p95 cap in applyCodeImpactCap.
//   2. Churn-weighting — each file's LOC is weighted by how many merged PRs
//      across the whole corpus touch that path. Add-once boilerplate (touched
//      once) counts ChurnFloor; a repeatedly-revisited file ramps to 1.0 at
//      ChurnFullAt touches. Requires FileChange data (backfill file-changes).

// buildChurnIndex counts, across every merged PR in the full corpus, how many
// distinct PRs touch each file path. Returns nil for a nil corpus. Callers
// build it only when churn-weighting is enabled. Falls back to the path-only
// Files list for PRs that predate FileChange backfill.
func buildChurnIndex(data *Loaded) map[string]int {
	if data == nil {
		return nil
	}
	idx := make(map[string]int)
	for _, p := range data.PRs {
		if p.Merged == nil {
			continue
		}
		// One increment per (PR, path): a PR re-listing a path can't inflate churn.
		seen := map[string]struct{}{}
		for _, fp := range fileChangePaths(p) {
			if _, ok := seen[fp]; ok {
				continue
			}
			seen[fp] = struct{}{}
			idx[fp]++
		}
	}
	return idx
}

// fileChangePaths returns the file paths a PR touched, preferring the richer
// FileChanges list and falling back to the path-only Files list when FileChange
// data was never backfilled for that record.
func fileChangePaths(p cache.GitHubPR) []string {
	if len(p.FileChanges) > 0 {
		out := make([]string, 0, len(p.FileChanges))
		for _, fc := range p.FileChanges {
			out = append(out, fc.Path)
		}
		return out
	}
	return p.Files
}

// isExcludedLOCFile reports whether a file's LOC is fully excluded from the
// code_impact LOC term. Two cases, both "nobody authored these lines":
//   - generated output: path matches a generated pattern (lockfiles, stubs,
//     dist/, …);
//   - a dumped data file: a serialized-data extension (json/csv/xml/… — see
//     isOversizedDataExt) AND more lines than ci.DataFileLOCCeiling.
//
// The data case requires BOTH conditions together: a large file is excluded
// only when it's also a recognized data format, so a big hand-authored *code*
// file (a 5k-line .go/.ts) is never excluded for its size alone, and a small
// data file (a 200-line config .json/.xml) is never excluded for its format
// alone. Only the intersection — large AND serialized-data — reads as a dump.
// The size case is gated by DisableDumpDampening (it's part of dump handling)
// and needs per-file LOC, so callers pass the FileChange.
func isExcludedLOCFile(fc cache.FileChange, ci config.CodeImpactConfig, norm config.NormalizeConfig) bool {
	if IsGeneratedPath(fc.Path, norm) {
		return true
	}
	if !ci.DisableDumpDampening && ci.DataFileLOCCeiling > 0 &&
		isOversizedDataExt(extLower(fc.Path)) && fc.Additions+fc.Deletions > ci.DataFileLOCCeiling {
		return true
	}
	return false
}

// excludedLOCParts returns the additions and deletions within a PR's
// FileChanges that are fully excluded from the code_impact LOC term (generated
// output + oversized dumped data files; see isExcludedLOCFile). Returns (0,0)
// when the PR has no FileChange detail, so its raw LOC passes through unchanged
// (same fallback as churn/family weighting).
func excludedLOCParts(p cache.GitHubPR, ci config.CodeImpactConfig, norm config.NormalizeConfig) (adds, dels int) {
	for _, fc := range p.FileChanges {
		if isExcludedLOCFile(fc, ci, norm) {
			adds += fc.Additions
			dels += fc.Deletions
		}
	}
	return adds, dels
}

// excludedWeightedLOC is the deletion-weighted LOC of a PR's excluded files —
// the amount netted out of the code_impact LOC term at PR / window scope.
func excludedWeightedLOC(p cache.GitHubPR, ci config.CodeImpactConfig, norm config.NormalizeConfig) float64 {
	a, d := excludedLOCParts(p, ci, norm)
	return weightedLOC(a, d, ci)
}

// excludedLOCInWindow sums the raw additions+deletions of excluded files across
// merged PRs in [start, end] — netted out of the window-totals code_impact LOC
// term (which uses unweighted add+del, so this matches).
func excludedLOCInWindow(data *Loaded, start, end cache.Month, ci config.CodeImpactConfig, norm config.NormalizeConfig) int {
	var total int
	for _, p := range data.PRs {
		if p.Merged == nil || !monthInRange(monthKey(*p.Merged), start, end) {
			continue
		}
		a, d := excludedLOCParts(p, ci, norm)
		total += a + d
	}
	return total
}

// effectiveLOCInWindow returns the L input to code_impact for the dev's merged
// PRs in [start, end]: excluded-file LOC (generated + dumped data) removed, then
// the optional churn / family / bulk-import dampening. With every knob off it
// equals the raw non-excluded LOC sum (same merged-PR-in-window walk as the rollup).
func effectiveLOCInWindow(data *Loaded, start, end cache.Month, churn map[string]int, family map[string]float64, ci config.CodeImpactConfig, norm config.NormalizeConfig, w prIntegrationWeight) float64 {
	var total float64
	for _, p := range data.PRs {
		if p.Merged == nil || !monthInRange(monthKey(*p.Merged), start, end) {
			continue
		}
		// Integration down-weight: a merge-up PR's LOC is mostly re-shipped
		// already-merged code, so it contributes its factor (else 1.0 / nil).
		total += w.weightFor(p) * effectivePRLOC(p, churn, family, ci, norm)
	}
	return total
}

// effectivePRLOC applies generated-output exclusion plus bulk-import, churn, and
// boilerplate-family dampening to a single merged PR's LOC. A PR with no
// FileChange detail can't be per-file adjusted, so its raw LOC passes through
// (still subject to the PR-level bulk-import damp).
func effectivePRLOC(p cache.GitHubPR, churn map[string]int, family map[string]float64, ci config.CodeImpactConfig, norm config.NormalizeConfig) float64 {
	rawLOC := weightedLOC(p.Additions, p.Deletions, ci)

	// Bulk-data-dump dampening: a fixture/export dump (one serialized-data
	// extension dominating a large PR) contributes its PR-level LOC at
	// DumpWeight. Applied at the PR level (not per-file) so GitHub's 3000-file
	// truncation of huge PRs doesn't undercount the dump. Short-circuits the
	// generated/churn/family/bulk-import handling below.
	if !ci.DisableDumpDampening && isBulkDataDump(fileChangePaths(p), ci) {
		return ci.DumpWeight * rawLOC
	}

	bulk := 1.0
	if ci.BulkImportDampening && isBulkImport(p, ci) {
		bulk = ci.BulkImportWeight
	}

	// Per-file reweighting (churn and/or family) needs FileChange detail. When
	// neither knob is active — or this PR has no per-file detail — damp the
	// PR-level LOC directly, netting out excluded-file LOC (zero when there's
	// no FileChange detail, so raw LOC passes through unchanged).
	familyOn := !ci.DisableBoilerplateDampening && len(family) > 0
	if (!ci.ChurnWeighting && !familyOn) || len(p.FileChanges) == 0 {
		nonExcluded := rawLOC - excludedWeightedLOC(p, ci, norm)
		if nonExcluded < 0 {
			nonExcluded = 0
		}
		return bulk * nonExcluded
	}

	var weighted float64
	for _, fc := range p.FileChanges {
		// Generated output and dumped data files are fully excluded from LOC.
		if isExcludedLOCFile(fc, ci, norm) {
			continue
		}
		loc := weightedLOC(fc.Additions, fc.Deletions, ci)
		w := 1.0
		if ci.ChurnWeighting {
			w *= churnWeight(churn[fc.Path], ci)
		}
		if familyOn {
			w *= familyWeight(family, fc.Path)
		}
		weighted += w * loc
	}
	return bulk * weighted
}

// weightedLOC returns a LOC magnitude with deleted lines scaled by the
// deletion-weighting knob. With DeletionWeighting off (default) it's the raw
// additions+deletions sum, byte-identical to the pre-knob metric; with it on,
// deletions count for DeletionWeight each, so a large dead-code removal no
// longer reads as equal to writing the same volume. Detection thresholds
// (isBulkImport / isBulkDataDump) deliberately keep using raw counts — only the
// LOC that flows into the formula is reweighted.
func weightedLOC(additions, deletions int, ci config.CodeImpactConfig) float64 {
	if !ci.DeletionWeighting {
		return float64(additions + deletions)
	}
	return float64(additions) + ci.DeletionWeight*float64(deletions)
}

// isBulkImport reports whether a PR looks like a boilerplate / vendor dump:
// large added LOC, almost entirely additions, spread across many added-status
// files. Requires FileChange detail to count added-status files.
func isBulkImport(p cache.GitHubPR, ci config.CodeImpactConfig) bool {
	total := p.Additions + p.Deletions
	if total < ci.BulkImportMinLOC || total == 0 {
		return false
	}
	if float64(p.Additions)/float64(total) < ci.BulkImportAddRatio {
		return false
	}
	added := 0
	for _, fc := range p.FileChanges {
		if fc.Status == "added" {
			added++
		}
	}
	return added >= ci.BulkImportMinFiles
}

// churnWeight maps a file's corpus-wide touch count to a LOC weight in
// [ChurnFloor, 1.0]: a file touched once gets ChurnFloor, ramping linearly to
// 1.0 at ChurnFullAt touches.
func churnWeight(touches int, ci config.CodeImpactConfig) float64 {
	full := ci.ChurnFullAt
	if full <= 1 {
		if touches >= 1 {
			return 1.0
		}
		return ci.ChurnFloor
	}
	if touches <= 1 {
		return ci.ChurnFloor
	}
	if touches >= full {
		return 1.0
	}
	frac := float64(touches-1) / float64(full-1)
	return ci.ChurnFloor + frac*(1.0-ci.ChurnFloor)
}

// buildFamilyIndex detects copy-paste file families across the whole merged
// corpus and returns the per-path LOC/file weight for the redundant copies
// (ci.FamilyWeight); paths not in any flagged family are absent (weight 1.0 via
// familyWeight). Returns nil when boilerplate dampening is off or nothing
// qualifies, so the per-file walk is skipped entirely.
//
// A "family" is the set of added files sharing a directory + a basename stem
// with short alnum tokens masked out (familyStem). A family qualifies as
// copy-paste only when it has ≥ ci.FamilyMinSize members AND their sizes
// cluster tightly (coefficient of variation ≤ ci.FamilyMaxSizeCV) — the
// dispersion gate is what separates forked copies (near-identical sizes) from
// genuinely-different same-named files (e.g. main.tf vs a 400-line variables.tf).
// The single largest member keeps full weight (the one real authored unit); the
// rest are the redundant copies. Purely structural: no repo/product knowledge.
func buildFamilyIndex(data *Loaded, ci config.CodeImpactConfig) map[string]float64 {
	if data == nil {
		return nil
	}
	// Per added path, the largest LOC it was ever added with (a path re-added in
	// a later PR can't inflate its family weight). Only added-status files: a
	// later edit to one copy isn't part of the fork's redundancy.
	pathLOC := map[string]float64{}
	for _, p := range data.PRs {
		if p.Merged == nil {
			continue
		}
		for _, fc := range p.FileChanges {
			if fc.Status != "added" {
				continue
			}
			loc := float64(fc.Additions + fc.Deletions)
			if cur, ok := pathLOC[fc.Path]; !ok || loc > cur {
				pathLOC[fc.Path] = loc
			}
		}
	}

	type member struct {
		path string
		loc  float64
	}
	families := map[string][]member{}
	for path, loc := range pathLOC {
		stem, ok := familyStem(path, ci.FamilyMaskMaxLen)
		if !ok {
			continue
		}
		families[stem] = append(families[stem], member{path, loc})
	}

	out := map[string]float64{}
	for _, members := range families {
		if len(members) < ci.FamilyMinSize {
			continue
		}
		// Size coefficient of variation (population stddev / mean).
		var sum float64
		for _, m := range members {
			sum += m.loc
		}
		mean := sum / float64(len(members))
		if mean <= 0 {
			continue
		}
		var sq float64
		for _, m := range members {
			d := m.loc - mean
			sq += d * d
		}
		cv := math.Sqrt(sq/float64(len(members))) / mean
		if cv > ci.FamilyMaxSizeCV {
			continue
		}
		// Keep the single largest member at full weight (the real authored unit);
		// damp every other copy. Tie-break by lexicographically-smallest path so
		// the representative is deterministic regardless of map-iteration order —
		// otherwise, when copies are split across authors (the common case), WHICH
		// dev keeps the full-weight copy would vary run-to-run and flake scores.
		best := 0
		for i := 1; i < len(members); i++ {
			if members[i].loc > members[best].loc ||
				(members[i].loc == members[best].loc && members[i].path < members[best].path) {
				best = i
			}
		}
		for i, m := range members {
			if i != best {
				out[m.path] = ci.FamilyWeight
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// familyStem returns a path's copy-paste family key — its directory plus the
// basename with short alnum tokens (the variable codes that distinguish copies,
// length 1..maskMaxLen) replaced by "#" — and whether the basename had any such
// token. A name with no maskable token can't be a fork variant, so ok is false
// and the path is never grouped. Tokens are split on -, _, and . so kebab/snake
// config and script names cluster while camelCase source names (single long
// tokens) do not.
func familyStem(path string, maskMaxLen int) (string, bool) {
	if maskMaxLen <= 0 {
		maskMaxLen = 4
	}
	dir, base := "", path
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		dir, base = path[:i], path[i+1:]
	}
	name, ext := base, ""
	if i := strings.LastIndexByte(base, '.'); i > 0 {
		name, ext = base[:i], base[i:]
	}
	masked := false
	tokens := strings.FieldsFunc(name, func(r rune) bool {
		return r == '-' || r == '_' || r == '.'
	})
	for i, t := range tokens {
		if len(t) <= maskMaxLen && isAlnum(t) {
			tokens[i] = "#"
			masked = true
		}
	}
	if !masked {
		return "", false
	}
	return dir + "|" + strings.Join(tokens, "-") + ext, true
}

// isAlnum reports whether s is non-empty and all ASCII letters/digits.
func isAlnum(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// familyWeight returns the boilerplate-family weight for a path: the dampened
// copy weight when the path is a redundant family member, else 1.0.
func familyWeight(family map[string]float64, path string) float64 {
	if w, ok := family[path]; ok {
		return w
	}
	return 1.0
}
