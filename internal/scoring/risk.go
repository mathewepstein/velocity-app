package scoring

import (
	"github.com/mathewepstein/velocity/internal/analyze"
	"github.com/mathewepstein/velocity/internal/config"
)

// riskTierRank orders the coarse risk labels so they can be max'd numerically.
// Unknown labels rank as low.
func riskTierRank(tier string) int {
	switch tier {
	case "high":
		return 2
	case "medium":
		return 1
	default:
		return 0
	}
}

// maxTier returns the higher-severity of two risk labels (high > medium > low).
// This is how the churn-derived tier and the domain-config tier compose: domain
// risk only ever elevates, never lowers, the churn baseline.
func maxTier(a, b string) string {
	if riskTierRank(a) >= riskTierRank(b) {
		return normalizeTier(a)
	}
	return normalizeTier(b)
}

// normalizeTier collapses any unrecognized/empty label to "low".
func normalizeTier(t string) string {
	switch t {
	case "high", "medium":
		return t
	default:
		return "low"
	}
}

// domainRiskTier scans paths against the configured domain-risk globs and
// returns the highest tier any path matches, plus the glob that drove it (for
// the explainability driver line). A path matching a High glob wins outright;
// otherwise the first Medium match sets the tier. No match → ("low", "").
//
// Matching uses doublestar so `**/auth-microservice/**` spans path segments.
// Paths are matched as-is (leading "./" trimmed) and a bare segment glob like
// `auth-microservice` also matches via a substring fallback so users can list a
// directory name without remembering the `**/…/**` wrapping.
func domainRiskTier(paths []string, cfg config.RiskConfig) (tier, matched string) {
	if cfg.Empty() {
		return "low", ""
	}
	// High wins, so check it first across all paths.
	if g := firstMatch(paths, cfg.High); g != "" {
		return "high", g
	}
	if g := firstMatch(paths, cfg.Medium); g != "" {
		return "medium", g
	}
	return "low", ""
}

// firstMatch returns the first glob in globs that any path matches, or "".
// Glob matching is shared with the noise-path list via analyze.MatchGlob.
func firstMatch(paths, globs []string) string {
	for _, g := range globs {
		for _, p := range paths {
			if analyze.MatchGlob(g, p) {
				return g
			}
		}
	}
	return ""
}
