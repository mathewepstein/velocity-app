package analyze

import (
	"testing"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

func TestDevKey(t *testing.T) {
	if got := devKey(config.DevIdentity{JiraAccountID: "acct-x", GitHubLogins: []string{"alice"}}); got != "acct-x" {
		t.Errorf("Jira accountId should win: got %q, want acct-x", got)
	}
	if got := devKey(config.DevIdentity{GitHubLogins: []string{"alice", "bob"}}); got != "gh:alice" {
		t.Errorf("GH-only dev should key off first login: got %q, want gh:alice", got)
	}
	if got := devKey(config.DevIdentity{}); got != "" {
		t.Errorf("empty identity should produce empty key, got %q", got)
	}
}

func TestApplyEloPeriodSkipsIdleDevs(t *testing.T) {
	// Alice has activity in the period; Bob has none. Only Alice should gain
	// a history entry.
	period := "2024-P19" // 2024-05-06 .. 2024-05-19
	data := &Loaded{
		PRs: []cache.GitHubPR{
			{Number: 1, Author: "alice", Created: mustTime("2024-05-10T00:00:00Z"),
				Merged: ptrTime(mustTime("2024-05-12T00:00:00Z")), Additions: 10},
		},
	}
	devs := []config.DevIdentity{
		{GitHubLogins: []string{"alice"}, DisplayName: "Alice"},
		{GitHubLogins: []string{"bob"}, DisplayName: "Bob"},
	}
	state := map[string]cache.DevRatingState{}
	scoring := config.ScoringConfig{
		KTiers:  []config.KTier{{MinPeriods: 0, K: 32}, {MinPeriods: 6, K: 16}},
		Weights: map[string]float64{"prs_merged": 3.0},
	}
	if err := applyEloPeriod(state, devs, data, period, scoring); err != nil {
		t.Fatalf("applyEloPeriod: %v", err)
	}
	if _, ok := state["gh:bob"]; ok {
		t.Errorf("idle Bob should not appear in state")
	}
	alice, ok := state["gh:alice"]
	if !ok {
		t.Fatalf("Alice missing from state")
	}
	if alice.PeriodsPlayed != 1 {
		t.Errorf("Alice PeriodsPlayed = %d, want 1", alice.PeriodsPlayed)
	}
	if len(alice.History) != 1 || alice.History[0].Period != period {
		t.Errorf("Alice history wrong: %+v", alice.History)
	}
}

func TestApplyEloPeriodSoloDevDrawsAtHalf(t *testing.T) {
	// Solo active dev: actual=0.5 (min-max collapses to 0.5), expected=0.5
	// (team-of-one mean is their own R), delta=0. The dev counts a period
	// played but their rating doesn't move.
	period := "2024-P19"
	data := &Loaded{
		PRs: []cache.GitHubPR{
			{Number: 1, Author: "alice", Created: mustTime("2024-05-10T00:00:00Z"),
				Merged: ptrTime(mustTime("2024-05-12T00:00:00Z"))},
		},
	}
	devs := []config.DevIdentity{
		{GitHubLogins: []string{"alice"}, DisplayName: "Alice"},
	}
	state := map[string]cache.DevRatingState{}
	scoring := config.ScoringConfig{KTiers: []config.KTier{{MinPeriods: 0, K: 32}, {MinPeriods: 6, K: 16}}, Weights: map[string]float64{"prs_merged": 3.0}}
	if err := applyEloPeriod(state, devs, data, period, scoring); err != nil {
		t.Fatalf("applyEloPeriod: %v", err)
	}
	alice := state["gh:alice"]
	if alice.Current != eloStartingRating {
		t.Errorf("solo dev rating = %v, want %v (drew the period)", alice.Current, eloStartingRating)
	}
	if alice.History[0].Delta != 0 {
		t.Errorf("solo dev delta = %v, want 0", alice.History[0].Delta)
	}
	if alice.History[0].Score != 0.5 {
		t.Errorf("solo dev normalized score = %v, want 0.5", alice.History[0].Score)
	}
}

func TestApplyEloPeriodRewardsTopAndPenalizesBottom(t *testing.T) {
	// Two devs, period 19. Alice ships 10 merged PRs, Bob ships 1. With
	// only prs_merged weighted, Alice should gain, Bob should lose.
	period := "2024-P19"
	pr := func(num int, author string, addn int) cache.GitHubPR {
		return cache.GitHubPR{
			Number: num, Author: author,
			Created: mustTime("2024-05-08T00:00:00Z"),
			Merged:  ptrTime(mustTime("2024-05-10T00:00:00Z")),
			Additions: addn,
		}
	}
	data := &Loaded{}
	for i := 0; i < 10; i++ {
		data.PRs = append(data.PRs, pr(100+i, "alice", 50))
	}
	data.PRs = append(data.PRs, pr(200, "bob", 50))

	devs := []config.DevIdentity{
		{GitHubLogins: []string{"alice"}, DisplayName: "Alice"},
		{GitHubLogins: []string{"bob"}, DisplayName: "Bob"},
	}
	state := map[string]cache.DevRatingState{}
	scoring := config.ScoringConfig{KTiers: []config.KTier{{MinPeriods: 0, K: 32}, {MinPeriods: 6, K: 16}}, Weights: map[string]float64{"prs_merged": 3.0}}
	if err := applyEloPeriod(state, devs, data, period, scoring); err != nil {
		t.Fatalf("applyEloPeriod: %v", err)
	}
	alice := state["gh:alice"]
	bob := state["gh:bob"]
	if alice.Current <= eloStartingRating {
		t.Errorf("Alice rating = %v, want > %v", alice.Current, eloStartingRating)
	}
	if bob.Current >= eloStartingRating {
		t.Errorf("Bob rating = %v, want < %v", bob.Current, eloStartingRating)
	}
	// Zero-sum across the active pair (K equal, expected = 0.5 each since
	// team mean = each dev's R = 1000 starting).
	deltaSum := alice.History[0].Delta + bob.History[0].Delta
	if deltaSum < -1e-6 || deltaSum > 1e-6 {
		t.Errorf("delta sum = %v, want ~0 for equal-K equal-mean pair", deltaSum)
	}
}

func TestAdvanceRatingsSkipsAlreadyAppliedPeriods(t *testing.T) {
	// rt records P19 as already played. AdvanceRatings should only walk P21+.
	rt := &cache.Ratings{
		Version:    cache.CurrentRatingsVersion,
		LastPeriod: "2024-P19",
		Devs: map[string]cache.DevRatingState{
			"gh:alice": {Current: 1016, PeriodsPlayed: 1, History: []cache.EloPoint{{Period: "2024-P19", Rating: 1016, Delta: 16, Score: 1}}},
		},
	}
	data := &Loaded{
		PRs: []cache.GitHubPR{
			{Number: 1, Author: "alice", Created: mustTime("2024-05-20T00:00:00Z"),
				Merged: ptrTime(mustTime("2024-05-22T00:00:00Z"))},
		},
	}
	devs := []config.DevIdentity{{GitHubLogins: []string{"alice"}, DisplayName: "Alice"}}
	scoring := config.ScoringConfig{KTiers: []config.KTier{{MinPeriods: 0, K: 32}, {MinPeriods: 6, K: 16}}, Weights: map[string]float64{"prs_merged": 3.0}}
	// "Now" past P21 end (2024-06-02).
	now := time.Date(2024, 6, 10, 0, 0, 0, 0, time.UTC)
	last, err := advanceRatings(rt, devs, data, cache.MustParseMonth("2024-05"), cache.MustParseMonth("2024-06"), scoring, now)
	if err != nil {
		t.Fatalf("advanceRatings: %v", err)
	}
	if last != "2024-P21" {
		t.Errorf("last applied = %q, want 2024-P21", last)
	}
	alice := rt.Devs["gh:alice"]
	if alice.PeriodsPlayed != 2 {
		t.Errorf("Alice PeriodsPlayed = %d, want 2 (P19 preserved + P21 new)", alice.PeriodsPlayed)
	}
	// Original P19 entry must survive.
	if alice.History[0].Period != "2024-P19" {
		t.Errorf("first history entry overwritten: %+v", alice.History[0])
	}
}

// idleDecayScoring returns a ScoringConfig that exercises the decay path
// with the Phase 7.3 defaults — short grace window so test fixtures can
// trigger decay without 30+ periods of setup.
func idleDecayScoring() config.ScoringConfig {
	return config.ScoringConfig{
		KTiers:         []config.KTier{{MinPeriods: 0, K: 32}, {MinPeriods: 6, K: 16}},
		Weights:        map[string]float64{"prs_merged": 3.0},
		IdleDecayAfter: 3,
		IdleDecayDelta: 8.0,
	}
}

func TestIdleDecayBeginsAfterN(t *testing.T) {
	// Alice has state (167 periods played, IdleStreak=3 — at the threshold,
	// no decay yet). Bob is active this period. Expect: Alice IdleStreak
	// ticks to 4 and one decay history entry is appended.
	period := "2024-P19"
	data := &Loaded{
		PRs: []cache.GitHubPR{
			{Number: 1, Author: "bob", Created: mustTime("2024-05-10T00:00:00Z"),
				Merged: ptrTime(mustTime("2024-05-12T00:00:00Z"))},
		},
	}
	devs := []config.DevIdentity{
		{GitHubLogins: []string{"alice"}, DisplayName: "Alice"},
		{GitHubLogins: []string{"bob"}, DisplayName: "Bob"},
	}
	state := map[string]cache.DevRatingState{
		"gh:alice": {Current: 1100, PeriodsPlayed: 10, IdleStreak: 3},
	}
	if err := applyEloPeriod(state, devs, data, period, idleDecayScoring()); err != nil {
		t.Fatalf("applyEloPeriod: %v", err)
	}
	alice := state["gh:alice"]
	if alice.IdleStreak != 4 {
		t.Errorf("Alice IdleStreak = %d, want 4", alice.IdleStreak)
	}
	if alice.PeriodsPlayed != 10 {
		t.Errorf("Alice PeriodsPlayed = %d, want 10 (no change on decay)", alice.PeriodsPlayed)
	}
	if len(alice.History) != 1 || alice.History[0].Kind != "decay" {
		t.Errorf("Alice history wrong: %+v", alice.History)
	}
}

func TestIdleDecayDoesNotFireDuringGracePeriod(t *testing.T) {
	// IdleStreak=2 → bump to 3, still ≤ IdleDecayAfter=3, no decay.
	period := "2024-P19"
	data := &Loaded{
		PRs: []cache.GitHubPR{
			{Number: 1, Author: "bob", Created: mustTime("2024-05-10T00:00:00Z"),
				Merged: ptrTime(mustTime("2024-05-12T00:00:00Z"))},
		},
	}
	devs := []config.DevIdentity{
		{GitHubLogins: []string{"alice"}, DisplayName: "Alice"},
		{GitHubLogins: []string{"bob"}, DisplayName: "Bob"},
	}
	state := map[string]cache.DevRatingState{
		"gh:alice": {Current: 1100, PeriodsPlayed: 10, IdleStreak: 2},
	}
	if err := applyEloPeriod(state, devs, data, period, idleDecayScoring()); err != nil {
		t.Fatalf("applyEloPeriod: %v", err)
	}
	alice := state["gh:alice"]
	if alice.IdleStreak != 3 {
		t.Errorf("Alice IdleStreak = %d, want 3", alice.IdleStreak)
	}
	if alice.Current != 1100 {
		t.Errorf("Alice Current = %v, want 1100 (still in grace window)", alice.Current)
	}
	if len(alice.History) != 0 {
		t.Errorf("Alice history should be empty during grace, got %+v", alice.History)
	}
}

func TestIdleDecayPullsTowardTeamMean(t *testing.T) {
	// Two active devs at R=1000 each → teamMean=1000.
	// Alice sits at 1100 with IdleStreak=5 (already past threshold) → decay
	// pulls -8. Carol sits at 900 with same streak → decay pulls +8.
	period := "2024-P19"
	data := &Loaded{
		PRs: []cache.GitHubPR{
			{Number: 1, Author: "bob", Created: mustTime("2024-05-10T00:00:00Z"),
				Merged: ptrTime(mustTime("2024-05-12T00:00:00Z"))},
			{Number: 2, Author: "dave", Created: mustTime("2024-05-11T00:00:00Z"),
				Merged: ptrTime(mustTime("2024-05-13T00:00:00Z"))},
		},
	}
	devs := []config.DevIdentity{
		{GitHubLogins: []string{"alice"}, DisplayName: "Alice"},
		{GitHubLogins: []string{"bob"}, DisplayName: "Bob"},
		{GitHubLogins: []string{"carol"}, DisplayName: "Carol"},
		{GitHubLogins: []string{"dave"}, DisplayName: "Dave"},
	}
	state := map[string]cache.DevRatingState{
		"gh:alice": {Current: 1100, PeriodsPlayed: 10, IdleStreak: 5},
		"gh:carol": {Current: 900, PeriodsPlayed: 10, IdleStreak: 5},
	}
	if err := applyEloPeriod(state, devs, data, period, idleDecayScoring()); err != nil {
		t.Fatalf("applyEloPeriod: %v", err)
	}
	alice := state["gh:alice"]
	carol := state["gh:carol"]
	if alice.Current != 1092 {
		t.Errorf("Alice Current = %v, want 1092 (pulled down by 8)", alice.Current)
	}
	if alice.History[0].Delta != -8 {
		t.Errorf("Alice decay Delta = %v, want -8", alice.History[0].Delta)
	}
	if carol.Current != 908 {
		t.Errorf("Carol Current = %v, want 908 (pulled up by 8)", carol.Current)
	}
	if carol.History[0].Delta != 8 {
		t.Errorf("Carol decay Delta = %v, want +8", carol.History[0].Delta)
	}
}

func TestIdleDecayClampsAtTeamMean(t *testing.T) {
	// Bob is active at R=1000 → teamMean=1000. Alice sits at 1003 with
	// IdleStreak=5: decay would normally pull -8, but |gap|=3 so it clamps
	// to -3, snapping Alice exactly to the mean (1000), never past.
	period := "2024-P19"
	data := &Loaded{
		PRs: []cache.GitHubPR{
			{Number: 1, Author: "bob", Created: mustTime("2024-05-10T00:00:00Z"),
				Merged: ptrTime(mustTime("2024-05-12T00:00:00Z"))},
		},
	}
	devs := []config.DevIdentity{
		{GitHubLogins: []string{"alice"}, DisplayName: "Alice"},
		{GitHubLogins: []string{"bob"}, DisplayName: "Bob"},
	}
	state := map[string]cache.DevRatingState{
		"gh:alice": {Current: 1003, PeriodsPlayed: 10, IdleStreak: 5},
	}
	if err := applyEloPeriod(state, devs, data, period, idleDecayScoring()); err != nil {
		t.Fatalf("applyEloPeriod: %v", err)
	}
	alice := state["gh:alice"]
	if alice.Current != 1000 {
		t.Errorf("Alice Current = %v, want 1000 (clamped at teamMean)", alice.Current)
	}
	if alice.History[0].Delta != -3 {
		t.Errorf("Alice decay Delta = %v, want -3 (clamped)", alice.History[0].Delta)
	}
}

func TestIdleDecayActivityResetsStreak(t *testing.T) {
	// Alice with IdleStreak=5 (past threshold) plays this period → streak
	// resets to 0, no decay entry, normal active entry appended.
	period := "2024-P19"
	data := &Loaded{
		PRs: []cache.GitHubPR{
			{Number: 1, Author: "alice", Created: mustTime("2024-05-10T00:00:00Z"),
				Merged: ptrTime(mustTime("2024-05-12T00:00:00Z"))},
		},
	}
	devs := []config.DevIdentity{
		{GitHubLogins: []string{"alice"}, DisplayName: "Alice"},
	}
	state := map[string]cache.DevRatingState{
		"gh:alice": {Current: 1100, PeriodsPlayed: 10, IdleStreak: 5},
	}
	if err := applyEloPeriod(state, devs, data, period, idleDecayScoring()); err != nil {
		t.Fatalf("applyEloPeriod: %v", err)
	}
	alice := state["gh:alice"]
	if alice.IdleStreak != 0 {
		t.Errorf("Alice IdleStreak after activity = %d, want 0", alice.IdleStreak)
	}
	if alice.PeriodsPlayed != 11 {
		t.Errorf("Alice PeriodsPlayed = %d, want 11", alice.PeriodsPlayed)
	}
	if len(alice.History) != 1 || alice.History[0].Kind != "active" {
		t.Errorf("Alice history should have single active entry, got %+v", alice.History)
	}
}

func TestIdleDecaySkipsOrphans(t *testing.T) {
	// Charlie is in state but not in the configured devs list (removed from
	// config). Decay should not fire on him.
	period := "2024-P19"
	data := &Loaded{
		PRs: []cache.GitHubPR{
			{Number: 1, Author: "bob", Created: mustTime("2024-05-10T00:00:00Z"),
				Merged: ptrTime(mustTime("2024-05-12T00:00:00Z"))},
		},
	}
	devs := []config.DevIdentity{
		{GitHubLogins: []string{"bob"}, DisplayName: "Bob"},
	}
	state := map[string]cache.DevRatingState{
		"gh:charlie": {Current: 1100, PeriodsPlayed: 10, IdleStreak: 5},
	}
	if err := applyEloPeriod(state, devs, data, period, idleDecayScoring()); err != nil {
		t.Fatalf("applyEloPeriod: %v", err)
	}
	charlie := state["gh:charlie"]
	if charlie.Current != 1100 {
		t.Errorf("orphan Charlie Current = %v, want 1100 (unchanged)", charlie.Current)
	}
	if charlie.IdleStreak != 5 {
		t.Errorf("orphan Charlie IdleStreak = %d, want 5 (unchanged)", charlie.IdleStreak)
	}
}

func TestAttachRatingsLeavesUnknownAlone(t *testing.T) {
	devs := []DevWindowMetrics{
		{Dev: config.DevIdentity{DisplayName: "Alice", GitHubLogins: []string{"alice"}}},
		{Dev: config.DevIdentity{DisplayName: "unknown"}},
	}
	state := map[string]cache.DevRatingState{
		"gh:alice": {Current: 1042, PeriodsPlayed: 3, History: []cache.EloPoint{
			{Period: "2024-P19", Rating: 1016, Delta: 16},
			{Period: "2024-P21", Rating: 1030, Delta: 14},
			{Period: "2024-P23", Rating: 1042, Delta: 12},
		}},
	}
	got := attachRatings(devs, state, config.ScoringConfig{ProvisionalUntilPeriods: 12})
	if got[0].Rating == nil {
		t.Fatal("Alice rating should be attached")
	}
	if got[0].Rating.Current != 1042 || got[0].Rating.DeltaPeriod != 12 || got[0].Rating.PeriodsPlayed != 3 {
		t.Errorf("Alice rating wrong: %+v", got[0].Rating)
	}
	if !got[0].Rating.Provisional {
		t.Errorf("Alice (3 periods) should be Provisional under threshold=12, got %+v", got[0].Rating)
	}
	if len(got[0].Rating.History) != 3 {
		t.Errorf("Alice history len = %d, want 3", len(got[0].Rating.History))
	}
	// HistoryDates parallels History with each period's start date (YYYY-MM-DD),
	// so the frontend can date-label and window-clip the trajectory.
	if len(got[0].Rating.HistoryDates) != 3 {
		t.Fatalf("Alice history_dates len = %d, want 3", len(got[0].Rating.HistoryDates))
	}
	for j, p := range []string{"2024-P19", "2024-P21", "2024-P23"} {
		want, err := periodStart(p)
		if err != nil {
			t.Fatalf("periodStart(%s): %v", p, err)
		}
		if got[0].Rating.HistoryDates[j] != want.Format("2006-01-02") {
			t.Errorf("history_dates[%d] = %q, want %q", j, got[0].Rating.HistoryDates[j], want.Format("2006-01-02"))
		}
	}
	if got[1].Rating != nil {
		t.Errorf("unknown bucket should not get a Rating, got %+v", got[1].Rating)
	}
}

func TestAttachRatingsProvisionalFalseWhenEstablished(t *testing.T) {
	devs := []DevWindowMetrics{
		{Dev: config.DevIdentity{DisplayName: "Senior", GitHubLogins: []string{"senior"}}},
	}
	state := map[string]cache.DevRatingState{
		"gh:senior": {Current: 1100, PeriodsPlayed: 50, History: []cache.EloPoint{
			{Period: "2024-P23", Rating: 1100, Delta: 5},
		}},
	}
	got := attachRatings(devs, state, config.ScoringConfig{ProvisionalUntilPeriods: 12})
	if got[0].Rating.Provisional {
		t.Errorf("Senior (50 periods) should NOT be Provisional under threshold=12")
	}
}

func TestAttachRatingsProvisionalForNeverPlayed(t *testing.T) {
	// Dev in config but no state entry → never played → Provisional, baseline
	// rating exposed for UI.
	devs := []DevWindowMetrics{
		{Dev: config.DevIdentity{DisplayName: "NewHire", GitHubLogins: []string{"newhire"}}},
	}
	got := attachRatings(devs, map[string]cache.DevRatingState{}, config.ScoringConfig{ProvisionalUntilPeriods: 12})
	if got[0].Rating == nil {
		t.Fatal("never-played dev should still surface a baseline rating")
	}
	if got[0].Rating.Current != eloStartingRating {
		t.Errorf("never-played Current = %v, want %v", got[0].Rating.Current, eloStartingRating)
	}
	if !got[0].Rating.Provisional {
		t.Errorf("never-played dev should be Provisional")
	}
}
