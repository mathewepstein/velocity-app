package analyze

import (
	"path"
	"strings"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

// Phase 6.2 — silent anti-gaming normalization.
//
// Two layers, both invisible to the UI:
//
//   1. Generated-file fractional counting (effectiveUniqueFilesInWindow).
//      Files matching a configured pattern (lockfiles, dist/, *.pb.go, etc.)
//      count as cfg.GeneratedFileWeight (default 0.25) toward the F input of
//      code_impact, instead of 1.0. The raw cardinality remains on
//      Totals.UniqueFilesTouched for display.
//
//   2. effortMultiplier — clipped product of two signals applied to the
//      gameable inputs (commits / loc_changed / code_impact) before z-scoring:
//        - spam: commits / unique_files_touched > SpamThreshold suggests a
//          dev is bisecting the same handful of files. Penalty scales with
//          how far above the threshold the ratio sits.
//        - stuff: loc / unique_files_touched landing above the team's p90
//          suggests dependency-dump-style LOC stuffing. Penalty scales with
//          the overflow ratio.
//      Floor at MultiplierFloor (default 0.5) so the layer can never zero
//      a dev's contribution.

// effectiveUniqueFilesInWindow returns the gen-file-weighted cardinality of
// the union of file paths across every merged PR in [start, end]. Used as
// the F input to code_impact at window scope. Mirrors uniqueFilesInWindow's
// PR walk so the two stay in lock-step.
func effectiveUniqueFilesInWindow(data *Loaded, start, end cache.Month, cfg config.NormalizeConfig, ci config.CodeImpactConfig) float64 {
	// Per-path weight: a file in a detected bulk-data dump contributes
	// ci.DumpWeight, a generated-pattern file contributes GeneratedFileWeight,
	// everything else 1.0. A path seen in multiple PRs keeps its least-dampened
	// weight, so real work on a file isn't erased by an unrelated dump touching it.
	weights := map[string]float64{}
	for _, p := range data.PRs {
		if p.Merged == nil || !monthInRange(monthKey(*p.Merged), start, end) {
			continue
		}
		dump := !ci.DisableDumpDampening && isBulkDataDump(fileChangePaths(p), ci)
		for _, f := range p.Files {
			w := genFileWeight(f, cfg)
			if dump && isDumpDataExt(extLower(f)) {
				w = ci.DumpWeight
			}
			if cur, ok := weights[f]; !ok || w > cur {
				weights[f] = w
			}
		}
	}
	var n float64
	for _, w := range weights {
		n += w
	}
	return n
}

// IsGeneratedPath reports whether p matches any of cfg's generated-file
// patterns (lockfiles, dist/, *.pb.go, snapshots, etc.). Exported so the
// scoring engine can exclude generated output from net-LOC the same way the
// code_impact normalizer does, without duplicating the matcher.
func IsGeneratedPath(p string, cfg config.NormalizeConfig) bool {
	return matchesGeneratedPattern(p, cfg.GeneratedFilePatterns)
}

// genFileWeight is the per-file generated-pattern weight: GeneratedFileWeight
// for a path matching a configured pattern, else 1.0.
func genFileWeight(path string, cfg config.NormalizeConfig) float64 {
	if matchesGeneratedPattern(path, cfg.GeneratedFilePatterns) {
		return cfg.GeneratedFileWeight
	}
	return 1.0
}

// effectiveFilesCount applies the generated-file fractional weight to a
// file set, returning the weighted cardinality. Each file contributes 1.0
// unless its path matches a configured generated-file pattern, in which
// case it contributes cfg.GeneratedFileWeight.
func effectiveFilesCount(files map[string]struct{}, cfg config.NormalizeConfig) float64 {
	weight := cfg.GeneratedFileWeight
	var n float64
	for f := range files {
		if matchesGeneratedPattern(f, cfg.GeneratedFilePatterns) {
			n += weight
			continue
		}
		n += 1.0
	}
	return n
}

// matchesGeneratedPattern reports whether p matches any configured pattern.
// Glob semantics piggyback on path.Match — `*` doesn't cross `/`, plus
// `[abc]` character classes work. For ergonomic paths-anywhere matching
// we additionally:
//   - test the basename so `*.lock` and `package-lock.json` match
//     `web/yarn.lock` / `web/package-lock.json`
//   - treat `*/seg/*` patterns as "contains /seg/ anywhere or starts with seg/"
//     so `*/vendor/*` flags `vendor/foo.go`
func matchesGeneratedPattern(p string, patterns []string) bool {
	pl := strings.ToLower(p)
	base := path.Base(pl)
	for _, raw := range patterns {
		pat := strings.ToLower(strings.TrimSpace(raw))
		if pat == "" {
			continue
		}
		if ok, _ := path.Match(pat, base); ok {
			return true
		}
		if ok, _ := path.Match(pat, pl); ok {
			return true
		}
		if seg := segmentPattern(pat); seg != "" {
			if strings.Contains(pl, "/"+seg+"/") || strings.HasPrefix(pl, seg+"/") {
				return true
			}
		}
	}
	return false
}

// segmentPattern extracts the meat from `*/seg/*` or `**/seg/*` style
// patterns; returns "" if pat doesn't have the leading-and-trailing
// path-segment shape (in which case path.Match alone is sufficient).
func segmentPattern(pat string) string {
	if strings.HasPrefix(pat, "*/") && strings.HasSuffix(pat, "/*") && len(pat) > 4 {
		return pat[2 : len(pat)-2]
	}
	return ""
}

// effortMultiplier returns the combined spam+stuff dampening factor in
// [MultiplierFloor, 1.0]. Inputs come from this dev's window Totals plus
// the team's per-dev LOC/file ratio distribution (used to identify
// top-decile stuffers).
func effortMultiplier(t Totals, teamLOCPerFile []float64, cfg config.NormalizeConfig) float64 {
	floor := cfg.MultiplierFloor
	if floor <= 0 {
		floor = 0.5
	}
	m := spamMultiplier(t, cfg, floor) * stuffMultiplier(t, teamLOCPerFile, cfg, floor)
	return clampUnit(m, floor)
}

// AuditEffortMultiplier exposes the silent dampening function for
// calibration tooling. Returns the combined multiplier together with the
// two parts so callers can attribute which signal drove the dampening.
// Internal-package only — never call this from production scoring; that
// path already applies the multiplier transparently inside
// computeContributorScores.
func AuditEffortMultiplier(t Totals, teamLOCPerFile []float64, cfg config.NormalizeConfig) (multiplier, spam, stuff float64) {
	floor := cfg.MultiplierFloor
	if floor <= 0 {
		floor = 0.5
	}
	spam = spamMultiplier(t, cfg, floor)
	stuff = stuffMultiplier(t, teamLOCPerFile, cfg, floor)
	multiplier = clampUnit(spam*stuff, floor)
	return
}

// AuditTeamLOCPerFile re-exposes teamLOCPerFileDistribution for calibration
// tooling. Same shape, same exclusions as the scoring path.
func AuditTeamLOCPerFile(devs []DevWindowMetrics) []float64 {
	return teamLOCPerFileDistribution(devs)
}

func spamMultiplier(t Totals, cfg config.NormalizeConfig, floor float64) float64 {
	if t.UniqueFilesTouched == 0 || t.Commits == 0 {
		return 1
	}
	threshold := cfg.SpamThreshold
	if threshold <= 0 {
		threshold = 1.5
	}
	ratio := float64(t.Commits) / float64(t.UniqueFilesTouched)
	if ratio <= threshold {
		return 1
	}
	penalty := cfg.SpamPenalty
	if penalty <= 0 {
		penalty = 0.25
	}
	return clampUnit(1-penalty*(ratio-threshold), floor)
}

func stuffMultiplier(t Totals, teamLOCPerFile []float64, cfg config.NormalizeConfig, floor float64) float64 {
	if t.UniqueFilesTouched == 0 || len(teamLOCPerFile) == 0 {
		return 1
	}
	p90 := percentile(teamLOCPerFile, 90)
	if p90 <= 0 {
		return 1
	}
	devRatio := float64(t.LOCAdded+t.LOCDeleted) / float64(t.UniqueFilesTouched)
	if devRatio <= p90 {
		return 1
	}
	penalty := cfg.StuffPenalty
	if penalty <= 0 {
		penalty = 0.25
	}
	overflow := (devRatio - p90) / p90
	return clampUnit(1-penalty*overflow, floor)
}

// teamLOCPerFileDistribution collects each scoreable dev's
// (loc_changed / unique_files_touched) ratio for the window. Devs with no
// merged-PR activity (UniqueFilesTouched == 0) are excluded so they don't
// crowd the lower tail with zeros.
func teamLOCPerFileDistribution(devs []DevWindowMetrics) []float64 {
	out := make([]float64, 0, len(devs))
	for _, d := range devs {
		if d.Dev.DisplayName == "unknown" {
			continue
		}
		if d.Totals.UniqueFilesTouched == 0 {
			continue
		}
		out = append(out, float64(d.Totals.LOCAdded+d.Totals.LOCDeleted)/float64(d.Totals.UniqueFilesTouched))
	}
	return out
}

func clampUnit(v, floor float64) float64 {
	if v < floor {
		return floor
	}
	if v > 1 {
		return 1
	}
	return v
}
