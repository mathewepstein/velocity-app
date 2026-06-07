package progress

import (
	"strings"
	"testing"
	"time"
)

// barWithBuf returns a Bar that renders into a buffer as if it were a TTY, with
// an injectable clock — so rendering can be asserted deterministically.
func barWithBuf(tty bool, clk func() time.Time) (*Bar, *strings.Builder) {
	var b strings.Builder
	return &Bar{w: &b, tty: tty, clock: clk}, &b
}

func TestNopIsSilent(t *testing.T) {
	r := Nop()
	r.EnterCell("x")
	r.Page(1, 2)
	r.Detail(1, 2)
	r.Bisect("w")
	r.Wait(time.Second, "r")
	r.Clear() // must not panic; nothing to assert
}

func TestBarTTYRendersInPlaceAndClears(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	bar, buf := barWithBuf(true, func() time.Time { return now })

	bar.EnterCell("jira-detail CD/2026-05")
	bar.Detail(3, 100)
	out := buf.String()
	if !strings.Contains(out, "jira-detail CD/2026-05") || !strings.Contains(out, "3/100") {
		t.Fatalf("status line missing cell/detail: %q", out)
	}
	if !strings.HasPrefix(out, "\r") {
		t.Fatalf("TTY render should use carriage return, got %q", out)
	}

	buf.Reset()
	bar.Clear()
	cleared := buf.String()
	if !strings.HasPrefix(cleared, "\r") || strings.TrimSpace(cleared) != "" {
		t.Fatalf("Clear should blank the line, got %q", cleared)
	}
}

func TestBarWaitMessage(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	bar, buf := barWithBuf(true, func() time.Time { return now })
	bar.EnterCell("c")
	buf.Reset()
	bar.Wait(12*time.Second, "rate limit")
	if !strings.Contains(buf.String(), "waiting 12s (rate limit)") {
		t.Fatalf("wait line = %q", buf.String())
	}
}

func TestBarNonTTYThrottles(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	clk := func() time.Time { return now }
	bar, buf := barWithBuf(false, clk)

	bar.EnterCell("c") // first flush at t0
	first := buf.String()
	if !strings.Contains(first, "c") || !strings.HasSuffix(first, "\n") {
		t.Fatalf("non-TTY should print newline lines, got %q", first)
	}
	buf.Reset()
	bar.Detail(1, 10) // same instant → throttled, no output
	if buf.String() != "" {
		t.Fatalf("non-TTY should throttle within the interval, got %q", buf.String())
	}
	now = now.Add(nonTTYInterval + time.Second)
	bar.Detail(2, 10) // past the interval → flushes
	if !strings.Contains(buf.String(), "2/10") {
		t.Fatalf("non-TTY should flush after the interval, got %q", buf.String())
	}
}

func TestLanesComposeOnOneLine(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	bar, buf := barWithBuf(true, func() time.Time { return now })

	a := bar.Lane()
	g := bar.Lane()
	a.EnterCell("jira-detail CD/2026-05")
	a.Detail(3, 100)
	g.EnterCell("pr-comments cd/2026-05")
	g.Detail(1, 40)

	out := buf.String()
	if !strings.Contains(out, "jira-detail CD/2026-05 · 3/100") ||
		!strings.Contains(out, "pr-comments cd/2026-05 · 1/40") ||
		!strings.Contains(out, " | ") {
		t.Fatalf("composed line missing a lane: %q", out)
	}

	// Retiring a lane drops its segment from subsequent renders.
	a.EnterCell("")
	buf.Reset()
	g.Detail(2, 40)
	out = buf.String()
	if strings.Contains(out, "jira-detail") {
		t.Fatalf("retired lane still rendering: %q", out)
	}
	if !strings.Contains(out, "2/40") {
		t.Fatalf("surviving lane missing: %q", out)
	}
}

func TestSingleLaneFormatUnchanged(t *testing.T) {
	// The Bar's own reporter must keep the pre-lane single-line format:
	// "label · detail · elapsed <spinner>".
	now := time.Unix(1_000_000, 0)
	bar, buf := barWithBuf(true, func() time.Time { return now })
	bar.EnterCell("github/org 2024-03")
	buf.Reset()
	bar.Page(2, 220)
	out := strings.TrimPrefix(buf.String(), "\r")
	if !strings.HasPrefix(out, "github/org 2024-03 · page 2 · 220 so far · 0s") {
		t.Fatalf("single-lane format changed: %q", out)
	}
}

func TestBarPrintfClearsAtomically(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	bar, buf := barWithBuf(true, func() time.Time { return now })
	bar.EnterCell("cell")
	buf.Reset()
	bar.Printf("[perm] done\n")
	out := buf.String()
	// Clear (CR + spaces + CR) must precede the permanent line.
	if !strings.HasPrefix(out, "\r") || !strings.HasSuffix(out, "[perm] done\n") {
		t.Fatalf("Printf should clear then print: %q", out)
	}
}

func TestPrintfHelperFallsBackForNop(t *testing.T) {
	var b strings.Builder
	Printf(Nop(), &b, "line %d\n", 7)
	if b.String() != "line 7\n" {
		t.Fatalf("fallback printf = %q", b.String())
	}
}

func TestElapsedFormatting(t *testing.T) {
	cases := map[time.Duration]string{
		45 * time.Second:               "45s",
		3*time.Minute + 12*time.Second: "3m12s",
		1*time.Hour + 4*time.Minute:    "1h04m",
	}
	for d, want := range cases {
		if got := elapsed(d); got != want {
			t.Errorf("elapsed(%v) = %q, want %q", d, got, want)
		}
	}
}
