package analyze

import (
	"fmt"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
)

// CodeImpactRow is one mapped dev's current-window code_impact + composite under
// the active CodeImpactConfig — the read-only output of CohortCodeImpact, used
// to inspect / calibrate the code_impact metric without mutating persisted state.
type CodeImpactRow struct {
	Dev          string
	PrimaryLogin string
	RawLOC       float64 // LOCAdded+LOCDeleted in window
	EffectiveLOC float64 // post-dampening L input to code_impact
	CodeImpact   float64 // post-cap code_impact metric (the scored value)
	Composite    float64 // composite score total (0 when the dev is unscored)
	Rank         int      // composite rank, 1-indexed (0 when unscored)
	Scored       bool
}

// CohortForCurrentWindow recomputes the current-window mapped-dev cohort for
// opts.Profile using the exact dashboard scoring pipeline (buildDevWindows →
// applyCodeImpactCap → computeContributorScores) and returns the scored cohort
// alongside the resolved current window. It deliberately omits Run's Elo block,
// so it advances/saves nothing and is safe to call repeatedly; the returned
// devs have no rating attached.
//
// This is the in-process equivalent of metrics.json's `devs` array. The
// dashboard no longer serializes `devs` (live pages read /api/contributors), so
// audit/calibration tools recompute the cohort here instead of decoding the
// blob. Read-only: it loads the cache but writes nothing.
func CohortForCurrentWindow(opts Options) ([]DevWindowMetrics, WindowMetrics, error) {
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	if opts.Store == nil {
		opts.Store = cache.JSONStore{}
	}
	current := cache.CurrentMonth(opts.Now)

	data, err := LoadAll(opts.Profile, current, opts.Store)
	if err != nil {
		return nil, WindowMetrics{}, err
	}
	backfillStart, err := cache.ParseMonth(opts.Profile.Window.BackfillStart)
	if err != nil {
		return nil, WindowMetrics{}, fmt.Errorf("invalid backfill_start: %w", err)
	}

	length := opts.Profile.Window.DefaultLengthMonths
	if length < 1 {
		length = 4
	}
	ci := opts.Profile.Scoring.CodeImpact
	norm := opts.Profile.Scoring.Normalize

	// Resolve the window exactly as Run does — on the self-filtered data — so
	// the cohort's window bounds match the live dashboard.
	selfData := selfFilter(data, opts.Profile)
	curWin := currentWindow(selfData, current, length, ci)
	curStart, _ := cache.ParseMonth(curWin.Window.Start)
	curEnd, _ := cache.ParseMonth(curWin.Window.End)

	devs := buildDevWindows(data, opts.Profile.Devs, opts.Profile.Scoring.EffectiveExcludes(), curStart, curEnd, backfillStart, current, ci, norm)
	applyCodeImpactCap(devs, ci)
	devs = computeContributorScores(devs, opts.Profile.Scoring.Weights, norm)
	return devs, curWin, nil
}

// CohortCodeImpact computes the current-window per-dev code_impact and composite
// for opts.Profile via CohortForCurrentWindow (the exact dashboard pipeline minus
// Elo), so this is safe to call repeatedly. Vary opts.Profile.Scoring.CodeImpact
// between calls to compare configurations. Read-only: loads the cache, writes
// nothing.
func CohortCodeImpact(opts Options) ([]CodeImpactRow, error) {
	devs, _, err := CohortForCurrentWindow(opts)
	if err != nil {
		return nil, err
	}

	rows := make([]CodeImpactRow, 0, len(devs))
	for i := range devs {
		d := &devs[i]
		if d.Dev.DisplayName == "unknown" {
			continue
		}
		row := CodeImpactRow{
			Dev:          d.Dev.DisplayName,
			PrimaryLogin: d.PrimaryLogin,
			RawLOC:       float64(d.Totals.LOCAdded + d.Totals.LOCDeleted),
			EffectiveLOC: d.effectiveLOC,
			CodeImpact:   d.Totals.CodeImpact,
		}
		if d.Score != nil {
			row.Composite = d.Score.Total
			row.Rank = d.Score.Rank
			row.Scored = true
		}
		rows = append(rows, row)
	}
	return rows, nil
}
