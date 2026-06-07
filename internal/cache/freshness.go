package cache

import "time"

// GraceDays is the tail of the next month during which we still re-pull the
// "last closed" month to catch late resolutions (tickets closed on day 31 may
// not appear in a pull done on day 31 if the Jira index is mid-update).
const GraceDays = 7

// NeedsPull reports whether (source, scope, month) needs to be pulled now.
//
// Rules (highest priority first):
//  1. force → always pull.
//  2. No manifest entry → pull (nothing cached).
//  3. month is the current month → pull (active data).
//  4. the cached data is partial — captured while the month was still in
//     progress (pulled during or before the month itself) → pull. Freshness is
//     anchored to when we actually pulled, not to the calendar: a month first
//     pulled mid-month and not re-captured inside the next month's grace window
//     would otherwise freeze incomplete forever (e.g. a cron that only runs
//     after the 7th).
//  5. month is the month immediately before current AND now.Day() ≤ GraceDays
//     → pull (grace window for late resolutions).
//  6. Otherwise → frozen; do not pull.
//
// now is injected so the freshness decision is deterministic in tests.
func NeedsPull(manifest *Manifest, source Source, scope string, month Month, now time.Time, force bool) bool {
	if force {
		return true
	}
	if manifest == nil {
		return true
	}
	entry, ok := manifest.Entry(source, scope, month)
	if !ok {
		return true
	}

	current := CurrentMonth(now)
	if month.Equal(current) {
		return true
	}

	// Partial-capture check: pulledMonth ≤ month means we pulled it before it
	// closed, so the data is incomplete. Re-pull until captured after close.
	pulledMonth := CurrentMonth(entry.PulledAt)
	if !month.Before(pulledMonth) {
		return true
	}

	lastClosed := current.Add(-1)
	if month.Equal(lastClosed) && now.UTC().Day() <= GraceDays {
		return true
	}

	return false
}
