package backfill

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
)

// rec is a tiny stand-in for a cache record. Done==false is the unfetched
// sentinel (NeedsWork true); Fetch flips it true.
type rec struct {
	ID   int  `json:"id"`
	Done bool `json:"done"`
}

const testSource = cache.SourceGitHubPRs

func seed(t *testing.T, scope, month string, recs []rec) {
	t.Helper()
	mo, err := cache.ParseMonth(month)
	if err != nil {
		t.Fatalf("parse month: %v", err)
	}
	if err := cache.WriteMonth(testSource, scope, mo, recs); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	m, err := cache.LoadManifest()
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	m.Update(testSource, scope, mo, len(recs), time.Unix(0, 0).UTC())
	if err := m.Save(); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
}

func read(t *testing.T, scope, month string) []rec {
	t.Helper()
	mo, _ := cache.ParseMonth(month)
	got, err := cache.ReadMonth[rec](testSource, scope, mo)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	return got
}

func needsWork(r *rec) bool { return !r.Done }

// readRec / writeRec are the phase cell-I/O hooks for the test's stand-in rec
// type. The Store interface only exposes the four real record sources, so these
// route through the generic free funcs (ignoring the passed store) — exactly
// how a phase over a non-Store-typed record would.
func readRec(_ cache.Store, scope string, m cache.Month) ([]rec, error) {
	return cache.ReadMonth[rec](testSource, scope, m)
}
func writeRec(_ cache.Store, scope string, m cache.Month, recs []rec) error {
	return cache.WriteMonth(testSource, scope, m, recs)
}

// runRec binds the rec I/O hooks onto ph and runs it against the JSON store
// (which the hooks ignore — they hit testSource directly).
func runRec(ph Phase[rec], mf *cache.Manifest, o Options) (Stats, error) {
	ph.Read = readRec
	ph.Write = writeRec
	return Run(context.Background(), ph, mf, cache.JSONStore{}, o)
}

func loadManifest(t *testing.T) *cache.Manifest {
	t.Helper()
	m, err := cache.LoadManifest()
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	return m
}

func TestRun_HydratesAllCandidates(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	seed(t, "repo", "2026-01", []rec{{ID: 1}, {ID: 2}, {ID: 3}})

	ph := Phase[rec]{
		Name:      "test",
		Source:    testSource,
		NeedsWork: needsWork,
		Fetch: func(_ context.Context, r *rec) (Outcome, error) {
			r.Done = true
			return Hydrated, nil
		},
	}
	st, err := runRec(ph, loadManifest(t), Options{Out: io.Discard})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if st.Hydrated != 3 || st.Candidates != 3 {
		t.Fatalf("stats = %+v, want 3 hydrated / 3 candidates", st)
	}
	for _, r := range read(t, "repo", "2026-01") {
		if !r.Done {
			t.Fatalf("rec %d not hydrated on disk", r.ID)
		}
	}
}

func TestRun_AlreadyDoneSkipped(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	seed(t, "repo", "2026-01", []rec{{ID: 1, Done: true}, {ID: 2}})

	calls := 0
	ph := Phase[rec]{
		Name: "test", Source: testSource, NeedsWork: needsWork,
		Fetch: func(_ context.Context, r *rec) (Outcome, error) {
			calls++
			r.Done = true
			return Hydrated, nil
		},
	}
	st, err := runRec(ph, loadManifest(t), Options{Out: io.Discard})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if calls != 1 {
		t.Fatalf("Fetch called %d times, want 1 (the one unfetched rec)", calls)
	}
	if st.AlreadyDone != 1 || st.Hydrated != 1 {
		t.Fatalf("stats = %+v, want 1 already-done / 1 hydrated", st)
	}
}

func TestRun_PermSkipNotRetried(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	seed(t, "repo", "2026-01", []rec{{ID: 1}, {ID: 2}})

	ph := Phase[rec]{
		Name: "test", Source: testSource, NeedsWork: needsWork,
		Fetch: func(_ context.Context, r *rec) (Outcome, error) {
			if r.ID == 1 {
				r.Done = true // sentinel set so NeedsWork is false next run
				return PermSkip, nil
			}
			r.Done = true
			return Hydrated, nil
		},
	}
	st, err := runRec(ph, loadManifest(t), Options{Out: io.Discard})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if st.SkippedPerm != 1 || st.Hydrated != 1 {
		t.Fatalf("stats = %+v, want 1 perm / 1 hydrated", st)
	}
	// Second run: both records have Done=true, so nothing should be fetched.
	calls := 0
	ph.Fetch = func(_ context.Context, r *rec) (Outcome, error) { calls++; return Hydrated, nil }
	if _, err := runRec(ph, loadManifest(t), Options{Out: io.Discard}); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if calls != 0 {
		t.Fatalf("rerun fetched %d records, want 0 (all resolved)", calls)
	}
}

func TestRun_TransientStopsAndCheckpoints(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	seed(t, "repo", "2026-01", []rec{{ID: 1}, {ID: 2}, {ID: 3}})

	boom := errors.New("network blip")
	ph := Phase[rec]{
		Name: "test", Source: testSource, NeedsWork: needsWork,
		Fetch: func(_ context.Context, r *rec) (Outcome, error) {
			if r.ID == 2 {
				return AlreadyDone, boom
			}
			r.Done = true
			return Hydrated, nil
		},
	}
	// CheckpointEvery 1 so rec 1 is flushed before rec 2 fails.
	st, err := runRec(ph, loadManifest(t), Options{Out: io.Discard, CheckpointEvery: 1})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if st.Hydrated != 1 {
		t.Fatalf("stats = %+v, want 1 hydrated before stop", st)
	}
	got := read(t, "repo", "2026-01")
	if !got[0].Done {
		t.Fatalf("rec 1 not checkpointed to disk before the transient stop")
	}
	if got[1].Done || got[2].Done {
		t.Fatalf("recs past the failure should remain unfetched, got %+v", got)
	}
}

func TestRun_LimitStops(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	seed(t, "repo", "2026-01", []rec{{ID: 1}, {ID: 2}, {ID: 3}, {ID: 4}})

	ph := Phase[rec]{
		Name: "test", Source: testSource, NeedsWork: needsWork,
		Fetch: func(_ context.Context, r *rec) (Outcome, error) { r.Done = true; return Hydrated, nil },
	}
	st, err := runRec(ph, loadManifest(t), Options{Out: io.Discard, Limit: 2, CheckpointEvery: 1})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if st.Hydrated != 2 {
		t.Fatalf("stats = %+v, want exactly 2 hydrated under --limit 2", st)
	}
}

func TestRun_DryRunDoesNotFetch(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	seed(t, "repo", "2026-01", []rec{{ID: 1}, {ID: 2}})

	calls := 0
	ph := Phase[rec]{
		Name: "test", Source: testSource, NeedsWork: needsWork,
		Fetch: func(_ context.Context, r *rec) (Outcome, error) { calls++; return Hydrated, nil },
	}
	st, err := runRec(ph, loadManifest(t), Options{Out: io.Discard, DryRun: true})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if calls != 0 {
		t.Fatalf("dry-run fetched %d records, want 0", calls)
	}
	if st.Candidates != 2 {
		t.Fatalf("stats = %+v, want 2 candidates counted", st)
	}
	for _, r := range read(t, "repo", "2026-01") {
		if r.Done {
			t.Fatalf("dry-run mutated rec %d on disk", r.ID)
		}
	}
}

func TestRun_RateGuardInvoked(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	seed(t, "repo", "2026-01", []rec{{ID: 1}, {ID: 2}, {ID: 3}, {ID: 4}})

	guardCalls := 0
	ph := Phase[rec]{
		Name: "test", Source: testSource, NeedsWork: needsWork,
		Fetch: func(_ context.Context, r *rec) (Outcome, error) { r.Done = true; return Hydrated, nil },
	}
	_, err := runRec(ph, loadManifest(t), Options{
		Out:             io.Discard,
		CheckpointEvery: 1,
		RateCheckEvery:  2,
		RateGuard:       func(context.Context) error { guardCalls++; return nil },
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if guardCalls != 2 { // fires at processed==2 and processed==4
		t.Fatalf("RateGuard called %d times, want 2", guardCalls)
	}
}

func TestRun_NoCellsIsNoError(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ph := Phase[rec]{Name: "test", Source: testSource, NeedsWork: needsWork,
		Fetch: func(_ context.Context, r *rec) (Outcome, error) { return Hydrated, nil }}
	st, err := runRec(ph, cache.NewManifest(), Options{Out: io.Discard})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if st.Months != 0 {
		t.Fatalf("stats = %+v, want 0 months", st)
	}
}
