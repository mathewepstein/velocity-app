// Package progress renders a live, single-line status indicator for
// long-running pulls and backfills so they never look frozen — during search
// pagination, per-entity detail fetches, date-window bisection, and (most
// importantly) rate-limit / backoff sleeps.
//
// On a TTY the status line is re-rendered in place with a carriage return; when
// the output is piped or redirected (cron, logs) it falls back to throttled
// newline lines so the log stays readable. No emoji — plain text plus an ASCII
// spinner as the liveness cue.
//
// Output contract: the Reporter owns ONLY the transient status line. Callers
// that also print permanent (scrolling) lines to the same writer must call
// Clear() first, so the permanent line doesn't collide with the status line;
// the next status event re-renders. The no-op reporter (Nop) makes that
// contract free — Clear() does nothing, permanent output is unaffected.
// When several goroutines share one stream, Clear-then-print is racy — use
// the Printf helper (or Bar.Printf), which clears and prints under one lock.
//
// Concurrent phases can share one Bar via Lane(): each lane reports its own
// cell/detail state and the Bar composes every active lane into the single
// status line. A finished lane retires itself with EnterCell("").
package progress

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Reporter receives progress events. All methods must be safe to call on a nil
// underlying sink; use Nop() when no reporting is wanted.
type Reporter interface {
	// EnterCell starts a new unit of work (e.g. a source/scope/month cell) and
	// resets the elapsed timer. label anchors every subsequent status line.
	// An empty label retires the cell — the reporter contributes nothing to
	// the status line until the next EnterCell.
	EnterCell(label string)
	// Page reports pagination progress within the current cell.
	Page(n, cumulative int)
	// Detail reports per-entity hydration progress (done of total; total 0 = unknown).
	Detail(done, total int)
	// Bisect reports a search-window bisection.
	Bisect(window string)
	// Wait reports a deliberate pause — the key anti-"frozen" signal.
	Wait(d time.Duration, reason string)
	// Clear erases the transient status line. Call before printing a permanent
	// line to the same writer.
	Clear()
}

// permPrinter is the optional capability of writing a permanent line and
// clearing the transient status line under one lock (Bar and its lanes
// implement it). Printf routes through it when available.
type permPrinter interface {
	Printf(format string, args ...interface{})
}

// Printf writes a permanent (scrolling) line to w, routing through rep's
// atomic clear-and-print when rep supports it, and falling back to
// Clear-then-write otherwise. The atomic path is what makes permanent output
// safe when concurrent lanes share the stream.
func Printf(rep Reporter, w io.Writer, format string, args ...interface{}) {
	if pp, ok := rep.(permPrinter); ok {
		pp.Printf(format, args...)
		return
	}
	rep.Clear()
	fmt.Fprintf(w, format, args...)
}

// Nop returns a reporter that does nothing.
func Nop() Reporter { return nop{} }

type nop struct{}

func (nop) EnterCell(string)           {}
func (nop) Page(int, int)              {}
func (nop) Detail(int, int)            {}
func (nop) Bisect(string)              {}
func (nop) Wait(time.Duration, string) {}
func (nop) Clear()                     {}

var spinner = []byte{'|', '/', '-', '\\'}

// Bar renders the status line to a writer. Safe for concurrent use. The Bar
// itself is a Reporter (backed by a default lane); additional concurrent
// phases get their own lane via Lane().
type Bar struct {
	mu        sync.Mutex
	w         io.Writer
	tty       bool
	clock     func() time.Time
	lanes     []*lane
	def       *lane     // backs the Bar's own Reporter methods; lazy
	lastLen   int       // TTY: width of the last status line, for space-padding
	lastFlush time.Time // non-TTY: throttle clock
	spinIdx   int
}

// lane is one phase's contribution to the shared status line.
type lane struct {
	b      *Bar
	label  string
	detail string
	at     time.Time
}

// New builds a Bar writing to w, auto-detecting whether w is a terminal.
func New(w io.Writer) *Bar {
	return &Bar{w: w, tty: isTTY(w), clock: time.Now}
}

// Lane returns an additional reporter whose status renders jointly with the
// Bar's own (and any other lanes') on the single transient line — for
// concurrent phases sharing one stream. A finished lane should call
// EnterCell("") so it stops contributing to the line.
func (b *Bar) Lane() Reporter {
	b.mu.Lock()
	defer b.mu.Unlock()
	l := &lane{b: b}
	b.lanes = append(b.lanes, l)
	return l
}

// defaultLane lazily registers the lane behind the Bar's own Reporter methods,
// so a zero-value Bar (as tests construct) still works.
func (b *Bar) defaultLane() *lane {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.def == nil {
		b.def = &lane{b: b}
		b.lanes = append(b.lanes, b.def)
	}
	return b.def
}

// nonTTYInterval throttles newline output when piped/redirected.
const nonTTYInterval = 2 * time.Second

func (b *Bar) EnterCell(label string)         { b.enterCell(b.defaultLane(), label) }
func (b *Bar) Page(n, cumulative int)         { b.setDetail(b.defaultLane(), pageDetail(n, cumulative)) }
func (b *Bar) Detail(done, total int)         { b.setDetail(b.defaultLane(), detailDetail(done, total)) }
func (b *Bar) Bisect(window string)           { b.setDetail(b.defaultLane(), "bisecting "+window) }
func (b *Bar) Wait(d time.Duration, r string) { b.setDetail(b.defaultLane(), waitDetail(d, r)) }

func (l *lane) EnterCell(label string)         { l.b.enterCell(l, label) }
func (l *lane) Page(n, cumulative int)         { l.b.setDetail(l, pageDetail(n, cumulative)) }
func (l *lane) Detail(done, total int)         { l.b.setDetail(l, detailDetail(done, total)) }
func (l *lane) Bisect(window string)           { l.b.setDetail(l, "bisecting "+window) }
func (l *lane) Wait(d time.Duration, r string) { l.b.setDetail(l, waitDetail(d, r)) }
func (l *lane) Clear()                         { l.b.Clear() }

// Printf writes a permanent line through the parent Bar (atomic with the
// status-line clear).
func (l *lane) Printf(format string, args ...interface{}) { l.b.Printf(format, args...) }

func pageDetail(n, cumulative int) string {
	return fmt.Sprintf("page %d · %d so far", n, cumulative)
}

func detailDetail(done, total int) string {
	if total > 0 {
		return fmt.Sprintf("%d/%d", done, total)
	}
	return fmt.Sprintf("%d", done)
}

func waitDetail(d time.Duration, reason string) string {
	return fmt.Sprintf("waiting %s (%s)", d.Round(time.Second), reason)
}

func (b *Bar) enterCell(l *lane, label string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	l.label = label
	l.detail = ""
	if label == "" {
		l.at = time.Time{} // retired: contributes nothing until the next cell
	} else {
		l.at = b.clock()
	}
	b.lastFlush = time.Time{}
	b.renderLocked()
}

func (b *Bar) setDetail(l *lane, detail string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	l.detail = detail
	b.renderLocked()
}

func (b *Bar) Clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.clearLocked()
}

// Printf writes a permanent (scrolling) line to the Bar's stream, clearing the
// transient status line under the same lock so concurrent lanes can't squeeze
// a render between the clear and the write.
func (b *Bar) Printf(format string, args ...interface{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.clearLocked()
	fmt.Fprintf(b.w, format, args...)
}

func (b *Bar) clearLocked() {
	if b.tty && b.lastLen > 0 {
		fmt.Fprintf(b.w, "\r%s\r", strings.Repeat(" ", b.lastLen))
		b.lastLen = 0
	}
}

// renderLocked composes every active lane into the single status line and
// emits it. Callers hold b.mu.
func (b *Bar) renderLocked() {
	segs := make([]string, 0, len(b.lanes))
	for _, l := range b.lanes {
		if l.label == "" && l.detail == "" {
			continue
		}
		parts := make([]string, 0, 3)
		if l.label != "" {
			parts = append(parts, l.label)
		}
		if l.detail != "" {
			parts = append(parts, l.detail)
		}
		if !l.at.IsZero() {
			parts = append(parts, elapsed(b.clock().Sub(l.at)))
		}
		segs = append(segs, strings.Join(parts, " · "))
	}
	line := strings.Join(segs, " | ")
	if line == "" {
		return
	}

	if b.tty {
		sp := spinner[b.spinIdx%len(spinner)]
		b.spinIdx++
		rendered := line + " " + string(sp)
		pad := ""
		if d := b.lastLen - len(rendered); d > 0 {
			pad = strings.Repeat(" ", d)
		}
		fmt.Fprintf(b.w, "\r%s%s", rendered, pad)
		b.lastLen = len(rendered)
		return
	}

	// Non-TTY: throttle to one line per interval so logs stay readable.
	now := b.clock()
	if !b.lastFlush.IsZero() && now.Sub(b.lastFlush) < nonTTYInterval {
		return
	}
	b.lastFlush = now
	fmt.Fprintf(b.w, "%s\n", line)
}

// elapsed formats a duration as a compact "3m12s" / "45s" / "1h04m".
func elapsed(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// isTTY reports whether w is a character device (terminal). Uses only stdlib:
// a piped/redirected file's mode lacks ModeCharDevice.
func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}
