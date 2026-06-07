package analyze

import (
	"sort"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
)

// QA / cycle-time flow signals — DISPLAY-ONLY DIAGNOSTICS.
//
// Everything here is derived from the cached issue changelog at analyze time
// (no re-pull, no cache migration) and is NEVER fed into the composite score or
// Elo. It answers "how long do tickets take, how long do they sit waiting for
// QA / in QA, and how often does QA catch a defect" — not "who ranks higher".
//
// The status names below are ConsumerDirect's workflow, discovered from the
// real changelog (Selected for Development → In Progress → Code Review →
// Ready QA → In QA → Done). They live here as documented package vars rather
// than config because the dashboard is single-org today; promote to config if
// the tool is ever pointed at another org's workflow.
var (
	// qaActiveStatuses: QA is actively testing the ticket.
	qaActiveStatuses = map[string]bool{"In QA": true, "QA": true}
	// qaQueueStatuses: ticket is waiting to be picked up by QA.
	qaQueueStatuses = map[string]bool{"Ready QA": true}
	// reworkStatuses: dev-side statuses. A move from "In QA" into one of these
	// is QA bouncing the ticket back — i.e. QA caught a bug.
	reworkStatuses = map[string]bool{
		"In Progress":              true,
		"Code Review":              true,
		"Reviewed":                 true,
		"Selected for Development": true,
		"Reopened":                 true,
		"Work in progress":         true,
	}
)

// QAFlow is the display-only cycle-time + QA-effectiveness rollup for one
// window. Medians (not means) are used throughout — cycle time is heavily
// right-skewed, so a mean is dominated by a handful of long-lived tickets.
type QAFlow struct {
	MedianCycleHours   float64 `json:"median_cycle_hours"`    // FirstInProgress→DoneAt
	MedianReadyQAHours float64 `json:"median_ready_qa_hours"` // time a ticket sits in "Ready QA"
	MedianInQAHours    float64 `json:"median_in_qa_hours"`    // time a ticket spends in "In QA"
	BugsCaught         int     `json:"bugs_caught"`           // backward bounces out of "In QA" in window
	TicketsResolved    int     `json:"tickets_resolved"`      // resolved-in-window tickets (denominator context)
}

// statusTransitions returns an issue's status-field changelog entries in
// chronological order. Other field changes are ignored.
func statusTransitions(iss cache.JiraIssue) []cache.StatusTransition {
	var ts []cache.StatusTransition
	for _, t := range iss.Changelog {
		if t.Field == "" || t.Field == "status" {
			ts = append(ts, t)
		}
	}
	sort.SliceStable(ts, func(i, j int) bool { return ts[i].At.Before(ts[j].At) })
	return ts
}

// statusDurationHours sums the hours an issue spent in each To-status across
// its changelog. The final status runs until DoneAt (else Resolved); if the
// issue is still open with no terminal timestamp, the final interval is not
// counted (we can't know its true duration). Re-entries into a status are
// summed.
func statusDurationHours(iss cache.JiraIssue) map[string]float64 {
	ts := statusTransitions(iss)
	if len(ts) == 0 {
		return nil
	}
	out := map[string]float64{}
	for i, t := range ts {
		if t.To == "" {
			continue
		}
		var end time.Time
		switch {
		case i+1 < len(ts):
			end = ts[i+1].At
		case iss.DoneAt != nil:
			end = *iss.DoneAt
		case iss.Resolved != nil:
			end = *iss.Resolved
		default:
			continue // still open in its final status
		}
		if d := end.Sub(t.At).Hours(); d > 0 {
			out[t.To] += d
		}
	}
	return out
}

// workflowRank orders CD's workflow stages so a backward transition (real
// rework) can be told apart from forward progress and pre-dev backlog churn.
// Stages that aren't part of the dev→review→QA→done pipeline — Open, To Do,
// Selected for Development, Backlog, Blocked, Reopened, … — rank 0: moving among
// them is triage noise, not rework.
func workflowRank(status string) int {
	switch status {
	case "In Progress", "Work in progress":
		return 1
	case "Code Review", "Reviewed":
		return 2
	case "Ready QA":
		return 3
	case "In QA", "QA":
		return 4
	case "Done", "Closed", "Resolved":
		return 5
	default:
		return 0
	}
}

// ReworkCount counts genuine rework on an issue: transitions that bounce
// BACKWARD from a review-or-later stage (Code Review, Ready QA, In QA) back into
// active development or review (In Progress / Code Review). It deliberately
// ignores pre-dev backlog churn (Open→Selected→Blocked→Selected→Open means
// nothing for effort) and all forward progress — only a return to dev/review
// after reaching review or QA signals that work had to be redone. This replaces
// the naive status-re-entry tally, which conflated process churn with rework.
func ReworkCount(iss cache.JiraIssue) int {
	n := 0
	for _, t := range statusTransitions(iss) {
		from, to := workflowRank(t.From), workflowRank(t.To)
		if from >= 2 && to >= 1 && to <= 2 && to < from {
			n++
		}
	}
	return n
}

// QueueHours returns the hours an issue spent in QA-queue / waiting statuses
// (currently "Ready QA") across its changelog. Exported for the story-points
// band engine, which subtracts queue time from In-Progress→Done cycle time so a
// long QA-queue wait — dead time, not thinking effort — doesn't inflate the
// deterministic band. Returns 0 when the changelog has no queue time.
func QueueHours(iss cache.JiraIssue) float64 {
	return sumStatuses(statusDurationHours(iss), qaQueueStatuses)
}

// sumStatuses totals the durations for the statuses in set.
func sumStatuses(dur map[string]float64, set map[string]bool) float64 {
	var s float64
	for status, h := range dur {
		if set[status] {
			s += h
		}
	}
	return s
}

// issueDoneAt returns the issue's terminal timestamp for window bucketing:
// DoneAt (last status transition / resolution) if present, else Resolved.
func issueDoneAt(iss cache.JiraIssue) *time.Time {
	if iss.DoneAt != nil {
		return iss.DoneAt
	}
	return iss.Resolved
}

// deriveQAFlow rolls up the team-wide, display-only QA/cycle signals over
// [start, end]. Medians are taken over tickets that resolved in the window;
// BugsCaught counts QA bounce events (In QA → rework status) whose transition
// timestamp falls in the window.
func deriveQAFlow(data *Loaded, start, end cache.Month) QAFlow {
	var cycle, readyQA, inQA []float64
	resolved := 0
	for _, iss := range data.Issues {
		done := issueDoneAt(iss)
		if done == nil || !monthInRange(monthKey(*done), start, end) {
			continue
		}
		resolved++
		if iss.CycleHours > 0 {
			cycle = append(cycle, iss.CycleHours)
		}
		dur := statusDurationHours(iss)
		if h := sumStatuses(dur, qaQueueStatuses); h > 0 {
			readyQA = append(readyQA, h)
		}
		if h := sumStatuses(dur, qaActiveStatuses); h > 0 {
			inQA = append(inQA, h)
		}
	}

	bugs := 0
	for _, iss := range data.Issues {
		for _, t := range iss.Changelog {
			if t.Field != "" && t.Field != "status" {
				continue
			}
			if qaActiveStatuses[t.From] && reworkStatuses[t.To] && monthInRange(monthKey(t.At), start, end) {
				bugs++
			}
		}
	}

	return QAFlow{
		MedianCycleHours:   percentile(cycle, 50),
		MedianReadyQAHours: percentile(readyQA, 50),
		MedianInQAHours:    percentile(inQA, 50),
		BugsCaught:         bugs,
		TicketsResolved:    resolved,
	}
}

// medianCycleHoursInWindow is the per-dev cycle-time median over the dev's
// tickets that resolved in [start, end]. Display-only.
func medianCycleHoursInWindow(data *Loaded, start, end cache.Month) float64 {
	var cyc []float64
	for _, iss := range data.Issues {
		done := issueDoneAt(iss)
		if done == nil || !monthInRange(monthKey(*done), start, end) {
			continue
		}
		if iss.CycleHours > 0 {
			cyc = append(cyc, iss.CycleHours)
		}
	}
	return percentile(cyc, 50)
}
