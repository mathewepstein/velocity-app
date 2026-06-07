// Package backfill is the shared scaffolding for lazy detail-hydration jobs
// that walk the month-partitioned cache and fill in per-record fields the base
// `velocity refresh` pull doesn't fetch (PR file lists, PR review comments,
// Jira changelog/comments/description).
//
// It was extracted from cmd/velocity-backfill-files, whose design priorities it
// preserves verbatim:
//
//  1. Resume gracefully. SIGINT, kill, network drop — none lose more than one
//     checkpoint interval. Hydrated records persist via atomic per-month
//     rewrite every CheckpointEvery records; the next run resumes because
//     iteration is "records where NeedsWork is still true".
//
//  2. Distinguish transient from permanent failures. Permanent errors (the
//     phase's Fetch returns PermSkip after setting the record's sentinel) are
//     never retried; transient errors stop the run cleanly with a checkpoint so
//     a rerun continues from here.
//
//  3. Stay polite to the API. A proactive inter-call Sleep plus an optional
//     RateGuard hook (polled every RateCheckEvery records) keep the job under
//     the rate ceiling; the reactive backoff in internal/pull stays underneath
//     as the safety net.
//
// The runner is generic over the cache record type T (cache.GitHubPR,
// cache.JiraIssue, …) and deliberately does NOT import internal/pull, so each
// phase wires its own API client into the NeedsWork/Fetch closures and there is
// no import cycle.
package backfill

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/progress"
)

// Outcome classifies what a phase's Fetch did to one record.
type Outcome int

const (
	// Hydrated means Fetch fetched data and mutated the record in place.
	Hydrated Outcome = iota
	// PermSkip means Fetch hit a permanent error and set the record's
	// nil-vs-empty sentinel so NeedsWork returns false on the next run.
	PermSkip
	// AlreadyDone means there was nothing to do. NeedsWork normally filters
	// these out before Fetch is ever called; it exists for completeness.
	AlreadyDone
)

// Phase describes one backfill phase over a single cache source: how to spot a
// candidate record and how to hydrate it. T is the cache record shape.
type Phase[T any] struct {
	// Name labels the phase in progress output.
	Name string
	// Source selects which manifest cells (and on-disk partitions) to iterate.
	Source cache.Source
	// NeedsWork reports whether rec is a hydration candidate. Records that
	// return false are counted as already-done and skipped without a Fetch.
	NeedsWork func(rec *T) bool
	// Fetch hydrates rec in place. On success it mutates rec and returns
	// Hydrated. On a permanent error it must set rec's sentinel (so NeedsWork
	// returns false next run) and return PermSkip with a nil error. On a
	// transient failure it returns a non-nil error, which stops the run after
	// persisting a checkpoint so the next run resumes here. Surfacing
	// context.Canceled / context.DeadlineExceeded (e.g. on SIGINT) is treated
	// as a clean stop, not a failure.
	Fetch func(ctx context.Context, rec *T) (Outcome, error)
	// Read / Write are the store-typed cell I/O for this phase's record type.
	// The runner is generic over T and Go has no generic interface methods, so
	// each phase binds the concrete cache.Store accessor for its source (e.g.
	// ReadGitHubPRs / WriteGitHubPRs). Read must return a wrapped fs.ErrNotExist
	// on a never-pulled cell so the runner skips it.
	Read  func(s cache.Store, scope string, m cache.Month) ([]T, error)
	Write func(s cache.Store, scope string, m cache.Month, recs []T) error
}

// Options tune the runner's pacing and checkpoint cadence.
type Options struct {
	// DryRun counts candidates without calling Fetch.
	DryRun bool
	// Limit stops the run after this many records are processed
	// (Hydrated + PermSkip). 0 means no limit.
	Limit int
	// CheckpointEvery persists the current month partition after every N
	// hydrated records. Lower = less work lost on a crash, more disk churn.
	// Values < 1 are treated as 1.
	CheckpointEvery int
	// Sleep is a fixed proactive inter-call spacing applied after each Fetch.
	// Superseded by Pace when an adaptive governor is wired in; leave at 0 then.
	Sleep time.Duration
	// Pace, when set, is called immediately before each Fetch to block until
	// it's polite to issue the next request (the rate governor's Wait). It is
	// the adaptive replacement for the fixed Sleep. Returning an error stops
	// the run after a checkpoint.
	Pace func(context.Context) error
	// RateCheckEvery invokes RateGuard every N processed records. 0 disables.
	RateCheckEvery int
	// RateGuard, if set, is called every RateCheckEvery processed records to
	// proactively pause before an API rate ceiling is hit. Returning an error
	// stops the run (after a checkpoint).
	RateGuard func(context.Context) error
	// Out is the permanent (scrolling) output sink. Defaults to os.Stdout.
	Out io.Writer
	// Reporter renders the transient live status line (cell / per-record /
	// waiting). Defaults to a no-op. When set it must write to the same stream
	// as Out — the runner clears it before every permanent line.
	Reporter progress.Reporter
	// Scope, when non-empty, restricts the run to that one cache scope (repo or
	// Jira project key). Empty processes every scope.
	Scope string
	// Since, when non-empty ("YYYY-MM"), skips cells for months earlier than it.
	// Useful for targeted re-runs over recent activity.
	Since string
}

func (o Options) checkpointEvery() int {
	if o.CheckpointEvery < 1 {
		return 1
	}
	return o.CheckpointEvery
}

func (o Options) out() io.Writer {
	if o.Out == nil {
		return os.Stdout
	}
	return o.Out
}

func (o Options) reporter() progress.Reporter {
	if o.Reporter == nil {
		return progress.Nop()
	}
	return o.Reporter
}

// Stats is the run summary.
type Stats struct {
	Months      int
	Scopes      int
	Candidates  int
	Hydrated    int
	SkippedPerm int
	AlreadyDone int
}

// Run walks every (scope, month) cell for ph.Source in manifest, oldest first,
// and hydrates each candidate record. It returns partial Stats even on error so
// callers can report what was accomplished before the stop.
func Run[T any](ctx context.Context, ph Phase[T], manifest *cache.Manifest, store cache.Store, o Options) (Stats, error) {
	var st Stats
	cells := sourceCells(manifest, ph.Source, o.Scope, o.Since)
	st.Months = len(cells)
	st.Scopes = countScopes(cells)
	out := o.out()
	rep := o.reporter()
	defer rep.Clear()

	// printf writes a permanent (scrolling) line, clearing the transient status
	// line first so the two never collide on the shared stream. When the
	// reporter is a progress.Bar (or one of its lanes) the clear+print happens
	// under one lock, which keeps permanent output intact when concurrent
	// phases share the stream.
	printf := func(format string, args ...interface{}) {
		progress.Printf(rep, out, format, args...)
	}

	if len(cells) == 0 {
		printf("no %s months in manifest — nothing to backfill\n", ph.Source)
		return st, nil
	}
	printf("[%s] scanning %d months across %d scopes\n", ph.Name, st.Months, st.Scopes)

	for _, cell := range cells {
		if err := ctx.Err(); err != nil {
			printf("shutdown signal received; exiting cleanly\n")
			return st, nil
		}
		mo, err := cache.ParseMonth(cell.Month)
		if err != nil {
			printf("[skip] bad month label %q in manifest: %v\n", cell.Month, err)
			continue
		}
		rep.EnterCell(fmt.Sprintf("%s %s/%s", ph.Name, cell.Scope, cell.Month))
		recs, err := ph.Read(store, cell.Scope, mo)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return st, fmt.Errorf("read %s/%s: %w", cell.Scope, mo, err)
		}

		monthHydrated := 0
		monthCandidates := 0
		dirty := false
		write := func() error {
			if !dirty {
				return nil
			}
			if err := ph.Write(store, cell.Scope, mo, recs); err != nil {
				return fmt.Errorf("write %s/%s: %w", cell.Scope, mo, err)
			}
			dirty = false
			return nil
		}

		for i := range recs {
			rec := &recs[i]
			if !ph.NeedsWork(rec) {
				st.AlreadyDone++
				continue
			}
			monthCandidates++
			st.Candidates++

			if o.DryRun {
				continue
			}

			rep.Detail(i+1, len(recs))

			processed := st.Hydrated + st.SkippedPerm
			if o.Limit > 0 && processed >= o.Limit {
				printf("[limit] reached --limit %d; stopping\n", o.Limit)
				if err := write(); err != nil {
					return st, err
				}
				return st, nil
			}

			if o.Pace != nil {
				if err := o.Pace(ctx); err != nil {
					if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						_ = write()
						return st, nil
					}
					_ = write()
					return st, err
				}
			}

			outcome, ferr := ph.Fetch(ctx, rec)
			switch {
			case ferr == nil && outcome == Hydrated:
				monthHydrated++
				st.Hydrated++
				dirty = true
			case ferr == nil && outcome == PermSkip:
				st.SkippedPerm++
				dirty = true
			case ferr == nil:
				// AlreadyDone returned from Fetch (unusual). Nothing to persist.
			case errors.Is(ferr, context.Canceled), errors.Is(ferr, context.DeadlineExceeded):
				printf("context canceled; persisting progress and exiting\n")
				if err := write(); err != nil {
					return st, err
				}
				return st, nil
			default:
				printf("[transient] %s: %v\n", ph.Name, ferr)
				printf("persisting checkpoint and exiting; rerun to resume\n")
				if err := write(); err != nil {
					return st, err
				}
				return st, ferr
			}

			if monthHydrated > 0 && monthHydrated%o.checkpointEvery() == 0 {
				if err := write(); err != nil {
					return st, err
				}
			}

			if o.Sleep > 0 {
				select {
				case <-ctx.Done():
					if err := write(); err != nil {
						return st, err
					}
					return st, nil
				case <-time.After(o.Sleep):
				}
			}

			if o.RateGuard != nil && o.RateCheckEvery > 0 {
				if processed := st.Hydrated + st.SkippedPerm; processed%o.RateCheckEvery == 0 {
					if err := o.RateGuard(ctx); err != nil {
						_ = write()
						return st, err
					}
				}
			}
		}

		if err := write(); err != nil {
			return st, err
		}
		if monthCandidates > 0 {
			printf("[%s/%s] %d candidates, %d hydrated this run\n", cell.Scope, mo, monthCandidates, monthHydrated)
		}
	}

	if o.DryRun {
		printf("\n[%s dry-run] %d records need hydration\n", ph.Name, st.Candidates)
	}
	return st, nil
}

// sourceCells returns every manifest cell for source, sorted oldest → newest
// within each scope so progress reads naturally and the lazy hydration finishes
// the back catalog before recent months.
func sourceCells(m *cache.Manifest, source cache.Source, scope, since string) []cache.ManifestEntry {
	out := make([]cache.ManifestEntry, 0)
	for _, e := range m.Entries {
		if e.Source != source {
			continue
		}
		if scope != "" && e.Scope != scope {
			continue
		}
		if since != "" && e.Month < since {
			continue // "YYYY-MM" sorts lexicographically == chronologically
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		return out[i].Month < out[j].Month
	})
	return out
}

func countScopes(cells []cache.ManifestEntry) int {
	seen := map[string]struct{}{}
	for _, c := range cells {
		seen[c.Scope] = struct{}{}
	}
	return len(seen)
}
