package analyze

import (
	"strings"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

// dumpDataExtensions is the CONSERVATIVE set of serialized-data formats that
// can qualify a PR as a bulk-data dump. Deliberately excludes source code and
// the ambiguous-but-often-legitimate formats (.sql migrations, .yaml config,
// .svg assets), so a PR dominated by one of those is never flagged.
var dumpDataExtensions = map[string]struct{}{
	"json": {}, "csv": {}, "tsv": {}, "ndjson": {}, "geojson": {},
	"xml": {}, "parquet": {}, "snap": {},
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
// Two opt-in knobs refine the L (LOC) input to code_impact so framework dumps
// and boilerplate stop registering as substance. Both default OFF in
// CodeImpactConfig; when off, effectiveLOCInWindow returns the raw merged-PR
// LOC sum, so code_impact is computed exactly as before.
//
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

// effectiveLOCInWindow returns the L input to code_impact for the dev's merged
// PRs in [start, end], after the optional churn-weighting + bulk-import knobs.
// When both knobs are off it returns the raw LOCAdded+LOCDeleted sum, matching
// the pre-patch metric exactly (same merged-PR-in-window walk as the rollup).
func effectiveLOCInWindow(data *Loaded, start, end cache.Month, churn map[string]int, ci config.CodeImpactConfig, w prIntegrationWeight) float64 {
	var total float64
	for _, p := range data.PRs {
		if p.Merged == nil || !monthInRange(monthKey(*p.Merged), start, end) {
			continue
		}
		// Integration down-weight: a merge-up PR's LOC is mostly re-shipped
		// already-merged code, so it contributes its factor (else 1.0 / nil).
		total += w.weightFor(p) * effectivePRLOC(p, churn, ci)
	}
	return total
}

// effectivePRLOC applies bulk-import dampening and churn-weighting to a single
// merged PR's LOC. A PR with no FileChange detail can't be churn-weighted, so
// its raw LOC passes through (still subject to the PR-level bulk-import damp).
func effectivePRLOC(p cache.GitHubPR, churn map[string]int, ci config.CodeImpactConfig) float64 {
	rawLOC := float64(p.Additions + p.Deletions)

	// Bulk-data-dump dampening: a fixture/export dump (one serialized-data
	// extension dominating a large PR) contributes its PR-level LOC at
	// DumpWeight. Applied at the PR level (not per-file) so GitHub's 3000-file
	// truncation of huge PRs doesn't undercount the dump. Short-circuits the
	// churn/bulk-import knobs below (which stay off by default).
	if !ci.DisableDumpDampening && isBulkDataDump(fileChangePaths(p), ci) {
		return ci.DumpWeight * rawLOC
	}

	bulk := 1.0
	if ci.BulkImportDampening && isBulkImport(p, ci) {
		bulk = ci.BulkImportWeight
	}

	if !ci.ChurnWeighting || len(p.FileChanges) == 0 {
		// Churn off (or no per-file detail): damp the PR-level LOC directly.
		return bulk * rawLOC
	}

	var weighted float64
	for _, fc := range p.FileChanges {
		loc := float64(fc.Additions + fc.Deletions)
		weighted += churnWeight(churn[fc.Path], ci) * loc
	}
	return bulk * weighted
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
