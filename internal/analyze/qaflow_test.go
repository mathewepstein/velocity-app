package analyze

import (
	"testing"

	"github.com/mathewepstein/velocity/internal/cache"
)

func st(at string, from, to string) cache.StatusTransition {
	return cache.StatusTransition{At: mustTime(at), From: from, To: to, Field: "status"}
}

// TestStatusDurationHours sums time spent in each status, with the final
// status running to DoneAt and re-entries accumulating.
func TestStatusDurationHours(t *testing.T) {
	done := mustTime("2026-01-04T00:00:00Z")
	iss := cache.JiraIssue{
		DoneAt: &done,
		Changelog: []cache.StatusTransition{
			st("2026-01-01T00:00:00Z", "Selected for Development", "In Progress"), // 24h In Progress
			st("2026-01-02T00:00:00Z", "In Progress", "Ready QA"),                 // 12h Ready QA
			st("2026-01-02T12:00:00Z", "Ready QA", "In QA"),                       // 12h In QA
			st("2026-01-03T00:00:00Z", "In QA", "In Progress"),                    // bounce; 12h In Progress (re-entry)
			st("2026-01-03T12:00:00Z", "In Progress", "In QA"),                    // 12h In QA (re-entry) → to DoneAt
		},
	}
	dur := statusDurationHours(iss)
	if got := dur["In QA"]; got != 24 {
		t.Errorf("In QA hours = %v, want 24 (12 + 12 re-entry)", got)
	}
	if got := dur["Ready QA"]; got != 12 {
		t.Errorf("Ready QA hours = %v, want 12", got)
	}
	if got := dur["In Progress"]; got != 36 {
		t.Errorf("In Progress hours = %v, want 36 (24 + 12)", got)
	}
}

// TestStatusDurationHoursOpenIssueDropsFinalInterval: an issue still open with
// no DoneAt/Resolved doesn't count its final (unbounded) status.
func TestStatusDurationHoursOpenIssue(t *testing.T) {
	iss := cache.JiraIssue{
		Changelog: []cache.StatusTransition{
			st("2026-01-01T00:00:00Z", "Open", "In Progress"),  // 24h
			st("2026-01-02T00:00:00Z", "In Progress", "In QA"), // open-ended → not counted
		},
	}
	dur := statusDurationHours(iss)
	if _, ok := dur["In QA"]; ok {
		t.Errorf("open final status should not be counted, got %v", dur["In QA"])
	}
	if got := dur["In Progress"]; got != 24 {
		t.Errorf("In Progress = %v, want 24", got)
	}
}

func TestDeriveQAFlow(t *testing.T) {
	mk := func(key, done string, cl []cache.StatusTransition) cache.JiraIssue {
		d := mustTime(done)
		return cache.JiraIssue{Key: key, DoneAt: &d, Changelog: cl}
	}
	data := &Loaded{Issues: []cache.JiraIssue{
		// In-window, passed QA cleanly. Active dev = 24h (In Progress).
		mk("CD-1", "2026-02-10T00:00:00Z", []cache.StatusTransition{
			st("2026-02-06T00:00:00Z", "Selected for Development", "In Progress"), // 24h In Progress
			st("2026-02-07T00:00:00Z", "In Progress", "Ready QA"),                 // 24h Ready QA
			st("2026-02-08T00:00:00Z", "Ready QA", "In QA"),                       // 48h In QA → DoneAt
			st("2026-02-10T00:00:00Z", "In QA", "Done"),
		}),
		// In-window, QA caught a bug (In QA → In Progress). Active dev = 60h (48 + 12 re-entry).
		mk("CD-2", "2026-02-20T00:00:00Z", []cache.StatusTransition{
			st("2026-02-16T00:00:00Z", "Selected for Development", "In Progress"), // 48h In Progress
			st("2026-02-18T00:00:00Z", "In Progress", "In QA"),
			st("2026-02-19T00:00:00Z", "In QA", "In Progress"), // bounce in window; +12h In Progress
			st("2026-02-19T12:00:00Z", "In Progress", "In QA"),
			st("2026-02-20T00:00:00Z", "In QA", "Done"),
		}),
		// Out of window — must not count.
		mk("CD-3", "2026-09-01T00:00:00Z", []cache.StatusTransition{
			st("2026-08-31T00:00:00Z", "In QA", "Code Review"), // bounce out of window
		}),
	}}

	qf := deriveQAFlow(data, cache.MustParseMonth("2026-01"), cache.MustParseMonth("2026-04"))

	if qf.TicketsResolved != 2 {
		t.Errorf("TicketsResolved = %d, want 2 (CD-3 out of window)", qf.TicketsResolved)
	}
	if qf.BugsCaught != 1 {
		t.Errorf("BugsCaught = %d, want 1 (CD-2's In QA→In Progress; CD-3 out of window)", qf.BugsCaught)
	}
	if qf.MedianCycleHours != 42 {
		t.Errorf("MedianCycleHours = %v, want 42 (median active dev of 24,60)", qf.MedianCycleHours)
	}
	if qf.MedianReadyQAHours <= 0 {
		t.Errorf("MedianReadyQAHours = %v, want > 0 (CD-1 sat in Ready QA)", qf.MedianReadyQAHours)
	}
	if qf.MedianInQAHours <= 0 {
		t.Errorf("MedianInQAHours = %v, want > 0", qf.MedianInQAHours)
	}
}

// TestQAFlowEmptyIsZero: no changelog / no resolved tickets → all-zero, no panic.
func TestQAFlowEmptyIsZero(t *testing.T) {
	qf := deriveQAFlow(&Loaded{}, cache.MustParseMonth("2026-01"), cache.MustParseMonth("2026-04"))
	if qf.TicketsResolved != 0 || qf.BugsCaught != 0 || qf.MedianCycleHours != 0 {
		t.Errorf("empty QAFlow should be zero-valued, got %+v", qf)
	}
}

// guard against accidentally feeding QA signals into scoring: the metric set
// must not contain a cycle-time / QA key.
func TestQASignalsAreNotScoredMetrics(t *testing.T) {
	for _, m := range allMetrics {
		switch m {
		case "median_cycle_hours", "qa_flow", "bugs_caught", "in_qa_hours", "ready_qa_hours":
			t.Errorf("QA/cycle signal %q leaked into allMetrics — must stay display-only", m)
		}
	}
}

func TestReworkCount(t *testing.T) {
	// Pure backlog/triage churn → no rework (matters nothing for effort).
	backlog := cache.JiraIssue{Changelog: []cache.StatusTransition{
		st("2026-01-01T00:00:00Z", "Open", "Selected for Development"),
		st("2026-01-02T00:00:00Z", "Selected for Development", "Blocked"),
		st("2026-01-03T00:00:00Z", "Blocked", "Selected for Development"),
		st("2026-01-04T00:00:00Z", "Selected for Development", "Open"),
	}}
	if got := ReworkCount(backlog); got != 0 {
		t.Errorf("backlog churn ReworkCount = %d, want 0", got)
	}

	// Genuine rework: bounces backward from review/QA into dev.
	rework := cache.JiraIssue{Changelog: []cache.StatusTransition{
		st("2026-01-01T00:00:00Z", "In Progress", "Code Review"),
		st("2026-01-02T00:00:00Z", "Code Review", "In Progress"), // review bounce ✓
		st("2026-01-03T00:00:00Z", "In Progress", "Code Review"),
		st("2026-01-04T00:00:00Z", "Code Review", "Ready QA"),
		st("2026-01-05T00:00:00Z", "Ready QA", "Code Review"), // QA-queue bounce ✓
		st("2026-01-06T00:00:00Z", "Code Review", "In QA"),
		st("2026-01-07T00:00:00Z", "In QA", "In Progress"), // QA bounce ✓
		st("2026-01-08T00:00:00Z", "In Progress", "Done"),
	}}
	if got := ReworkCount(rework); got != 3 {
		t.Errorf("rework ReworkCount = %d, want 3", got)
	}
}
