package pull

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/mathewepstein/velocity/internal/progress"
)

// RateGovernor is the proactive pacing layer for the backfill jobs. It watches
// the rate-limit signal on every response (GitHub's X-RateLimit-Remaining /
// -Reset headers, and any API's Retry-After) and, before each call, computes
// how long to wait so the remaining budget is spread evenly across the reset
// window — riding just under the ceiling instead of bursting into it.
//
// It is the proactive complement to backoffClient, which stays underneath as
// the reactive safety net (it still retries 429/5xx with backoff). The
// governor's job is to make those reactive retries rare.
//
// Pacing model (per backfill-missing-data-plan B3):
//
//	interval = (resetAt - now) / max(remaining - floor, 1)   // header-derived
//	interval = max(interval, minInterval)                    // --qps ceiling
//	sleep    = max(0, interval - elapsedSinceLastCall)
//	          ↑ overridden by a hard Retry-After wall when one is in effect.
//
// GitHub: headers drive the interval; floor (= --min-remaining) leaves a
// reserve so a 5000/hr budget rides at ~90% utilization. Jira: no usable
// remaining-headers, so minInterval (the high baseline) governs and a 429's
// Retry-After is the hard backstop. One governor serves both — header parsing
// is best-effort and simply no-ops when the headers are absent.
//
// Safe for concurrent use: jira-detail and pr-comments phases each hold their
// own governor, but Wait/Observe on a single governor are mutex-guarded.
type RateGovernor struct {
	mu       sync.Mutex
	floor    int
	minIval  time.Duration
	clock    func() time.Time
	reporter progress.Reporter
	lastCall time.Time

	// Observed rate state from the most recent response.
	haveLimit  bool
	remaining  int
	reset      time.Time
	retryUntil time.Time // hard wall from a Retry-After; zero when none
}

// GovernorConfig configures a RateGovernor.
type GovernorConfig struct {
	// Floor keeps this many requests in reserve — pacing slows to a crawl as
	// remaining approaches it, so a burst never drains the budget to zero.
	Floor int
	// MinInterval is the fastest the governor will ever go (the --qps ceiling).
	// It governs entirely when no remaining-headers are available (Jira).
	MinInterval time.Duration
	// Clock is injectable for tests; nil → time.Now.
	Clock func() time.Time
	// Reporter surfaces substantial pacing pauses as a "waiting" status. Nil → no-op.
	Reporter progress.Reporter
}

// NewRateGovernor builds a governor. A zero/negative Floor is treated as 0.
func NewRateGovernor(cfg GovernorConfig) *RateGovernor {
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	floor := cfg.Floor
	if floor < 0 {
		floor = 0
	}
	rep := cfg.Reporter
	if rep == nil {
		rep = progress.Nop()
	}
	return &RateGovernor{floor: floor, minIval: cfg.MinInterval, clock: clock, reporter: rep}
}

// Wait blocks until it's polite to issue the next request, then records that a
// call is about to go out. It returns ctx.Err() if the context is cancelled
// during the sleep. Safe to use directly as the backfill runner's Pace hook.
func (g *RateGovernor) Wait(ctx context.Context) error {
	g.mu.Lock()
	now := g.clock()
	sleep := g.computeLocked(now)
	throttled := g.retryUntil.After(now)
	// Record the projected send time so the next Wait measures elapsed from
	// when this call actually goes out, not from now.
	g.lastCall = now.Add(sleep)
	rep := g.reporter
	g.mu.Unlock()

	if sleep <= 0 {
		return nil
	}
	// Only surface substantial pauses — sub-second pacing isn't worth a
	// status flicker, but a multi-second rate-limit wait must look alive.
	if rep != nil && sleep >= time.Second {
		reason := "pacing"
		if throttled {
			reason = "rate limit"
		}
		rep.Wait(sleep, reason)
	}
	return sleepOrCancel(ctx, sleep)
}

// computeLocked derives the sleep duration. Caller holds the mutex.
func (g *RateGovernor) computeLocked(now time.Time) time.Duration {
	interval := g.minIval
	if g.haveLimit && g.reset.After(now) {
		budget := g.remaining - g.floor
		if budget < 1 {
			budget = 1 // at/under floor → one call per window ≈ wait for reset
		}
		if hi := g.reset.Sub(now) / time.Duration(budget); hi > interval {
			interval = hi
		}
	}

	elapsed := now.Sub(g.lastCall)
	if g.lastCall.IsZero() {
		elapsed = interval // first call: no artificial wait
	}
	sleep := interval - elapsed
	if sleep < 0 {
		sleep = 0
	}

	// A hard Retry-After wall overrides the smooth pacing.
	if g.retryUntil.After(now.Add(sleep)) {
		sleep = g.retryUntil.Sub(now)
	}
	return sleep
}

// observeHeaders ingests rate-limit headers from a response. GitHub sends
// X-RateLimit-Remaining (an integer) and X-RateLimit-Reset (unix seconds).
// Absent or unparseable headers are ignored, leaving the prior state intact.
func (g *RateGovernor) observeHeaders(h http.Header) {
	rem := h.Get("X-RateLimit-Remaining")
	rst := h.Get("X-RateLimit-Reset")
	if rem == "" || rst == "" {
		return
	}
	remaining, err1 := strconv.Atoi(rem)
	resetUnix, err2 := strconv.ParseInt(rst, 10, 64)
	if err1 != nil || err2 != nil {
		return
	}
	g.mu.Lock()
	g.haveLimit = true
	g.remaining = remaining
	g.reset = time.Unix(resetUnix, 0)
	g.mu.Unlock()
}

// observeRetryAfter records a hard backoff wall from a 429 / secondary-limit
// Retry-After. The next Wait yields at least until the wall passes.
func (g *RateGovernor) observeRetryAfter(d time.Duration) {
	if d <= 0 {
		return
	}
	g.mu.Lock()
	g.retryUntil = g.clock().Add(d)
	g.mu.Unlock()
}
