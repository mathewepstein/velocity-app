package pull

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// fakeClock returns a controllable time source for deterministic pacing tests.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func ghHeaders(remaining int, reset time.Time) http.Header {
	h := http.Header{}
	h.Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
	h.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
	return h
}

func TestGovernor_FirstCallNoWait(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	g := NewRateGovernor(GovernorConfig{MinInterval: time.Second, Clock: clk.now})
	if d := g.computeLocked(clk.now()); d != 0 {
		t.Fatalf("first call sleep = %v, want 0", d)
	}
}

func TestGovernor_MinIntervalGovernsWithoutHeaders(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	g := NewRateGovernor(GovernorConfig{MinInterval: 2 * time.Second, Clock: clk.now})

	// Simulate a first call going out now.
	g.lastCall = clk.now()
	// No time elapsed → must wait the full minInterval.
	if d := g.computeLocked(clk.now()); d != 2*time.Second {
		t.Fatalf("sleep = %v, want 2s (minInterval, no elapsed)", d)
	}
	// Half the interval elapsed → wait the remainder.
	clk.advance(time.Second)
	if d := g.computeLocked(clk.now()); d != time.Second {
		t.Fatalf("sleep = %v, want 1s remaining", d)
	}
	// Full interval elapsed → no wait.
	clk.advance(time.Second)
	if d := g.computeLocked(clk.now()); d != 0 {
		t.Fatalf("sleep = %v, want 0 once interval elapsed", d)
	}
}

func TestGovernor_HeaderIntervalSpreadsBudget(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	g := NewRateGovernor(GovernorConfig{Floor: 500, Clock: clk.now})

	// 1500 remaining, floor 500 → 1000 usable. Reset 1000s out → 1s interval.
	g.observeHeaders(ghHeaders(1500, clk.now().Add(1000*time.Second)))
	g.lastCall = clk.now()
	if d := g.computeLocked(clk.now()); d != time.Second {
		t.Fatalf("interval = %v, want 1s ((reset-now)/(remaining-floor))", d)
	}
}

func TestGovernor_AtFloorWaitsForReset(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	g := NewRateGovernor(GovernorConfig{Floor: 500, Clock: clk.now})

	// remaining == floor → budget clamps to 1 → interval ≈ whole window.
	g.observeHeaders(ghHeaders(500, clk.now().Add(300*time.Second)))
	g.lastCall = clk.now()
	if d := g.computeLocked(clk.now()); d != 300*time.Second {
		t.Fatalf("sleep = %v, want ~full window (300s) at floor", d)
	}
}

func TestGovernor_RetryAfterIsHardWall(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	g := NewRateGovernor(GovernorConfig{MinInterval: time.Second, Clock: clk.now})
	g.lastCall = clk.now()

	g.observeRetryAfter(30 * time.Second)
	// Even though minInterval is only 1s, the Retry-After wall dominates.
	if d := g.computeLocked(clk.now()); d != 30*time.Second {
		t.Fatalf("sleep = %v, want 30s (Retry-After wall)", d)
	}
	// After the wall passes, normal pacing resumes.
	clk.advance(31 * time.Second)
	if d := g.computeLocked(clk.now()); d != 0 {
		t.Fatalf("sleep = %v, want 0 after wall passes (interval long elapsed)", d)
	}
}

func TestGovernor_ExpiredResetIgnored(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	g := NewRateGovernor(GovernorConfig{MinInterval: time.Second, Clock: clk.now})
	g.lastCall = clk.now()

	// A reset already in the past must not produce a negative/huge interval;
	// minInterval governs instead.
	g.observeHeaders(ghHeaders(10, clk.now().Add(-100*time.Second)))
	if d := g.computeLocked(clk.now()); d != time.Second {
		t.Fatalf("sleep = %v, want minInterval 1s when reset is stale", d)
	}
}

func TestGovernor_WaitRespectsContextCancel(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	g := NewRateGovernor(GovernorConfig{MinInterval: time.Hour, Clock: clk.now})
	g.lastCall = clk.now() // forces a long wait

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.Wait(ctx); err == nil {
		t.Fatal("Wait returned nil on a cancelled context, want ctx error")
	}
}

func TestGovernor_MalformedHeadersIgnored(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	g := NewRateGovernor(GovernorConfig{MinInterval: time.Second, Clock: clk.now})
	g.lastCall = clk.now()

	h := http.Header{}
	h.Set("X-RateLimit-Remaining", "not-a-number")
	h.Set("X-RateLimit-Reset", "garbage")
	g.observeHeaders(h)
	if g.haveLimit {
		t.Fatal("malformed headers should not set haveLimit")
	}
	if d := g.computeLocked(clk.now()); d != time.Second {
		t.Fatalf("sleep = %v, want minInterval (headers ignored)", d)
	}
}
