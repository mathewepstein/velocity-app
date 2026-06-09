package scoring

import (
	"path/filepath"
	"testing"
	"time"
)

func tmpStore(t *testing.T) *ScoreStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "velocity.db")
	ss, err := OpenScoreStore(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { ss.Close() })
	return ss
}

func autoRec(ticket, hash string, points int) ScoreRecord {
	return ScoreRecord{
		Ticket: ticket, Scorer: ScorerID, Points: points, Source: SourceAuto,
		AutoPoints: points, Band: "5", Confidence: "medium", NeedsInsight: false,
		Drivers: []string{"d1"}, EvidenceHash: hash, ScoredAt: time.Unix(1000, 0).UTC(),
	}
}

func TestScoreStore_DateRoundTrip(t *testing.T) {
	ss := tmpStore(t)
	created := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	resolved := time.Date(2026, 4, 12, 17, 0, 0, 0, time.UTC)
	rec := autoRec("CD-1", "h1", 5)
	rec.CreatedAt = created
	rec.ResolvedAt = &resolved
	if _, err := ss.SaveAuto(rec); err != nil {
		t.Fatal(err)
	}
	got, _, err := ss.Get("CD-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if !got.CreatedAt.Equal(created) {
		t.Errorf("created_at = %v, want %v", got.CreatedAt, created)
	}
	if got.ResolvedAt == nil || !got.ResolvedAt.Equal(resolved) {
		t.Errorf("resolved_at = %v, want %v", got.ResolvedAt, resolved)
	}
}

func TestScoreStore_OpenTicketHasNoResolvedDate(t *testing.T) {
	ss := tmpStore(t)
	rec := autoRec("CD-1", "h1", 5)
	rec.CreatedAt = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	rec.ResolvedAt = nil // open ticket
	if _, err := ss.SaveAuto(rec); err != nil {
		t.Fatal(err)
	}
	got, _, _ := ss.Get("CD-1", "")
	if got.ResolvedAt != nil {
		t.Errorf("open ticket should have nil resolved_at, got %v", got.ResolvedAt)
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at should round-trip for an open ticket")
	}
}

func TestScoreStore_RegenBackfillsDatesOnUnchangedEvidence(t *testing.T) {
	ss := tmpStore(t)
	// A row scored before the date columns existed: same evidence hash, no dates.
	if _, err := ss.SaveAuto(autoRec("CD-1", "h1", 5)); err != nil {
		t.Fatal(err)
	}
	// Re-score with identical evidence but now-known dates → skip the re-score,
	// but patch the dates (Phase 6 backfill).
	created := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	withDates := autoRec("CD-1", "h1", 5)
	withDates.CreatedAt = created
	outcome, err := ss.SaveAuto(withDates)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != OutcomeSkipped {
		t.Errorf("outcome = %q, want skipped (evidence unchanged)", outcome)
	}
	got, _, _ := ss.Get("CD-1", "")
	if !got.CreatedAt.Equal(created) {
		t.Errorf("dates not backfilled on unchanged-evidence regen: created_at = %v", got.CreatedAt)
	}
}

func TestScoreStore_InsertGet(t *testing.T) {
	ss := tmpStore(t)
	if _, err := ss.SaveAuto(autoRec("CD-1", "h1", 5)); err != nil {
		t.Fatal(err)
	}
	got, ok, err := ss.Get("CD-1", "")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Points != 5 || got.Source != SourceAuto || len(got.Drivers) != 1 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestScoreStore_Idempotent(t *testing.T) {
	ss := tmpStore(t)
	if o, _ := ss.SaveAuto(autoRec("CD-1", "h1", 5)); o != OutcomeInserted {
		t.Errorf("first = %s, want inserted", o)
	}
	if o, _ := ss.SaveAuto(autoRec("CD-1", "h1", 5)); o != OutcomeSkipped {
		t.Errorf("same hash = %s, want skipped", o)
	}
	if o, _ := ss.SaveAuto(autoRec("CD-1", "h2", 8)); o != OutcomeUpdated {
		t.Errorf("new hash = %s, want updated", o)
	}
	got, _, _ := ss.Get("CD-1", "")
	if got.Points != 8 {
		t.Errorf("after update points = %d, want 8", got.Points)
	}
}

func TestScoreStore_PreservesHumanOverride(t *testing.T) {
	ss := tmpStore(t)
	ss.SaveAuto(autoRec("CD-1", "h1", 8))
	if err := ss.SetHumanOverride("CD-1", "", 5, 8, time.Unix(2000, 0).UTC()); err != nil {
		t.Fatal(err)
	}

	// Re-running the generator with new evidence must NOT clobber the override.
	o, err := ss.SaveAuto(autoRec("CD-1", "h2", 13))
	if err != nil {
		t.Fatal(err)
	}
	if o != OutcomePreserved {
		t.Errorf("outcome = %s, want preserved", o)
	}
	got, _, _ := ss.Get("CD-1", "")
	if got.Points != 5 || got.Source != SourceHuman {
		t.Errorf("override lost: points=%d source=%s", got.Points, got.Source)
	}
	if got.AutoPoints != 13 {
		t.Errorf("auto_points should refresh to current band 13, got %d", got.AutoPoints)
	}
}

func TestScoreStore_PreservesPostedStateAcrossRecompute(t *testing.T) {
	ss := tmpStore(t)
	ss.SaveAuto(autoRec("CD-1", "h1", 5))
	if err := ss.MarkPosted("CD-1", "", time.Unix(3000, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	// New evidence → auto row updates, but posted state must survive.
	ss.SaveAuto(autoRec("CD-1", "h2", 8))
	got, _, _ := ss.Get("CD-1", "")
	if !got.PostedToJira || got.JiraPostedAt == nil {
		t.Errorf("posted state lost across recompute: %+v", got)
	}
}

func TestScoreStore_ListNeedsInsight(t *testing.T) {
	ss := tmpStore(t)
	r1 := autoRec("CD-1", "h1", 2)
	r2 := autoRec("CD-2", "h2", 8)
	r2.NeedsInsight = true
	ss.SaveAuto(r1)
	ss.SaveAuto(r2)

	all, _ := ss.List(ScoreFilter{})
	if len(all) != 2 {
		t.Fatalf("list all = %d, want 2", len(all))
	}
	// needs-insight rows sort first.
	if all[0].Ticket != "CD-2" {
		t.Errorf("needs-insight should sort first, got %s", all[0].Ticket)
	}
	flagged, _ := ss.List(ScoreFilter{NeedsInsightOnly: true})
	if len(flagged) != 1 || flagged[0].Ticket != "CD-2" {
		t.Errorf("needs-insight filter wrong: %+v", flagged)
	}
}
