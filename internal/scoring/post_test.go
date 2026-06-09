package scoring

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakePoster records calls and can be told to fail a specific step.
type fakePoster struct {
	sp       map[string]float64
	comments map[string][]string
	failSP   bool
	failCmt  bool
	scopeErr error
	spCalls  int
	cmtCalls int
}

func newFakePoster() *fakePoster {
	return &fakePoster{sp: map[string]float64{}, comments: map[string][]string{}}
}

func (f *fakePoster) SetStoryPoints(_ context.Context, key string, points float64) error {
	f.spCalls++
	if f.failSP {
		return errors.New("boom: set points")
	}
	f.sp[key] = points
	return nil
}

func (f *fakePoster) AddComment(_ context.Context, key string, lines []string) error {
	f.cmtCalls++
	if f.failCmt {
		return errors.New("boom: add comment")
	}
	f.comments[key] = lines
	return nil
}

func (f *fakePoster) VerifyWriteScope(context.Context) error { return f.scopeErr }

func TestBuildComment_Auto(t *testing.T) {
	rec := ScoreRecord{
		Points: 5, AutoPoints: 5, Source: SourceAuto,
		HardestAspect: "session-storage compliance edge case",
		Drivers:       []string{"d1", "d2", "d3", "d4"},
		SignalSummary: "3d cycle, +12/-3, 2 review rounds",
	}
	lines := BuildComment(rec)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "Story points: 5") {
		t.Errorf("missing points line:\n%s", joined)
	}
	if strings.Contains(joined, "override") {
		t.Errorf("auto record should not mention an override:\n%s", joined)
	}
	if !strings.Contains(joined, "1. d1") || !strings.Contains(joined, "3. d3") {
		t.Errorf("drivers not numbered:\n%s", joined)
	}
	if strings.Contains(joined, "4. d4") {
		t.Errorf("drivers should cap at 3:\n%s", joined)
	}
	if !strings.Contains(joined, "Hardest aspect: session-storage") {
		t.Errorf("missing hardest aspect:\n%s", joined)
	}
	if lines[len(lines)-1] == "" || !strings.Contains(lines[len(lines)-1], "Clauditor") {
		t.Errorf("missing Clauditor signature on last line:\n%s", joined)
	}
	if lines[len(lines)-2] != "---" {
		t.Errorf("signature should be preceded by a rule line:\n%s", joined)
	}
}

func TestBuildComment_DropsDriverEqualToHardestAspect(t *testing.T) {
	rec := ScoreRecord{
		Points: 5, AutoPoints: 5, Source: SourceAuto,
		HardestAspect: "Moderate-risk area: 1 hot file(s) touched",
		Drivers:       []string{"Moderate-risk area: 1 hot file(s) touched", "Cross-cutting change"},
	}
	joined := strings.Join(BuildComment(rec), "\n")
	// The hardest-aspect line appears once (as the hardest aspect), not also as a driver.
	if strings.Count(joined, "Moderate-risk area: 1 hot file(s) touched") != 1 {
		t.Errorf("driver duplicating the hardest aspect should be dropped:\n%s", joined)
	}
	if !strings.Contains(joined, "1. Cross-cutting change") {
		t.Errorf("the distinct driver should remain and renumber to 1:\n%s", joined)
	}
}

func TestBuildComment_Override(t *testing.T) {
	rec := ScoreRecord{Points: 8, AutoPoints: 3, Source: SourceHuman}
	joined := strings.Join(BuildComment(rec), "\n")
	if !strings.Contains(joined, "Story points: 8") || !strings.Contains(joined, "proposed 3") {
		t.Errorf("override should record both values:\n%s", joined)
	}
}

func TestPostScores_DryRunWritesNothing(t *testing.T) {
	ss := tmpStore(t)
	if _, err := ss.SaveAuto(autoRec("CD-1", "h1", 5)); err != nil {
		t.Fatal(err)
	}
	f := newFakePoster()
	rep, err := PostScores(context.Background(), ss, f, PostOptions{Tickets: []string{"CD-1"}, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Previewed != 1 || rep.Posted != 0 {
		t.Errorf("want 1 previewed/0 posted, got %+v", rep)
	}
	if f.spCalls != 0 || f.cmtCalls != 0 {
		t.Errorf("dry run must not call the poster: sp=%d cmt=%d", f.spCalls, f.cmtCalls)
	}
	if rep.Results[0].Action != ActionPreview || len(rep.Results[0].Comment) == 0 {
		t.Errorf("preview should carry the comment: %+v", rep.Results[0])
	}
	got, _, _ := ss.Get("CD-1", "")
	if got.PostedToJira {
		t.Error("dry run must not mark the row posted")
	}
}

func TestPostScores_LiveWritesAndMarks(t *testing.T) {
	ss := tmpStore(t)
	if _, err := ss.SaveAuto(autoRec("CD-1", "h1", 5)); err != nil {
		t.Fatal(err)
	}
	f := newFakePoster()
	rep, err := PostScores(context.Background(), ss, f, PostOptions{Tickets: []string{"CD-1"}, DryRun: false})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Posted != 1 {
		t.Errorf("want 1 posted, got %+v", rep)
	}
	if f.sp["CD-1"] != 5 {
		t.Errorf("story points not written: %v", f.sp)
	}
	if len(f.comments["CD-1"]) == 0 {
		t.Error("comment not posted")
	}
	got, _, _ := ss.Get("CD-1", "")
	if !got.PostedToJira || got.JiraPostedAt == nil {
		t.Errorf("row not marked posted: %+v", got)
	}
}

func TestPostScores_Idempotent(t *testing.T) {
	ss := tmpStore(t)
	if _, err := ss.SaveAuto(autoRec("CD-1", "h1", 5)); err != nil {
		t.Fatal(err)
	}
	f := newFakePoster()
	// First live post.
	if _, err := PostScores(context.Background(), ss, f, PostOptions{Tickets: []string{"CD-1"}, DryRun: false}); err != nil {
		t.Fatal(err)
	}
	// Second post should be a no-op skip.
	rep, err := PostScores(context.Background(), ss, f, PostOptions{Tickets: []string{"CD-1"}, DryRun: false})
	if err != nil {
		t.Fatal(err)
	}
	if rep.AlreadyPosted != 1 || rep.Posted != 0 {
		t.Errorf("want already-posted skip, got %+v", rep)
	}
	if f.spCalls != 1 || f.cmtCalls != 1 {
		t.Errorf("idempotent re-run must not call the poster again: sp=%d cmt=%d", f.spCalls, f.cmtCalls)
	}
}

func TestPostScores_NoRow(t *testing.T) {
	ss := tmpStore(t)
	f := newFakePoster()
	rep, err := PostScores(context.Background(), ss, f, PostOptions{Tickets: []string{"CD-404"}, DryRun: false})
	if err != nil {
		t.Fatal(err)
	}
	if rep.NoRow != 1 || rep.Results[0].Action != ActionNoRow {
		t.Errorf("want no_score, got %+v", rep)
	}
	if f.spCalls != 0 {
		t.Error("must not write for a ticket with no score row")
	}
}

func TestPostScores_CommentFailureLeavesRowUnposted(t *testing.T) {
	ss := tmpStore(t)
	if _, err := ss.SaveAuto(autoRec("CD-1", "h1", 5)); err != nil {
		t.Fatal(err)
	}
	f := newFakePoster()
	f.failCmt = true
	rep, err := PostScores(context.Background(), ss, f, PostOptions{Tickets: []string{"CD-1"}, DryRun: false})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Errors != 1 || rep.Results[0].Action != ActionError {
		t.Errorf("want one error, got %+v", rep)
	}
	got, _, _ := ss.Get("CD-1", "")
	if got.PostedToJira {
		t.Error("a comment failure must not mark the row posted (so a re-run retries)")
	}
}

func TestPostScores_LiveRequiresPoster(t *testing.T) {
	ss := tmpStore(t)
	if _, err := PostScores(context.Background(), ss, nil, PostOptions{Tickets: []string{"CD-1"}, DryRun: false}); err == nil {
		t.Error("a live run with no poster must error")
	}
}
