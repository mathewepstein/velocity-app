package analyze

import (
	"fmt"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
)

// TeamFlowForWindow computes the macro team-flow view on demand for an
// arbitrary window [from, to] — the backend behind the Step 2 /api/team/flow
// endpoint.
//
// The Monthly series is always the full history (backfill_start … current), so
// the frontend keeps windowing the flow chart client-side exactly as before;
// from/to scope only the Claude-attribution cut, which is the genuinely
// window-relative part the precomputed blob froze to the current window. Called
// with the window Run used, it reproduces metrics.json's team_flow byte-for-byte
// (the parity gate).
//
// TeamFlow carries no identifying fields (counts, months, cycle hours), so —
// unlike /api/contributors — there is no incognito scrub at the boundary.
func TeamFlowForWindow(opts Options, from, to cache.Month) (TeamFlow, error) {
	if opts.Store == nil {
		opts.Store = cache.JSONStore{}
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	if to.Before(from) {
		return TeamFlow{}, fmt.Errorf("window end %s precedes start %s", to, from)
	}
	current := cache.CurrentMonth(opts.Now)

	data, err := LoadAll(opts.Profile, current, opts.Store)
	if err != nil {
		return TeamFlow{}, err
	}
	backfillStart, err := cache.ParseMonth(opts.Profile.Window.BackfillStart)
	if err != nil {
		return TeamFlow{}, fmt.Errorf("invalid backfill_start: %w", err)
	}

	return deriveTeamFlow(data, backfillStart, current, from, to), nil
}
