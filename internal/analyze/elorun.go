package analyze

import (
	"fmt"
	"sort"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

// devKey returns the stable per-dev identity key used inside ratings.json.
// Jira accountId wins when available since it's the canonical CD identity;
// GH-only devs fall back to "gh:<first-login>". Multi-GH-login devs hash to
// the same key regardless of which login appears first because
// AllGitHubLogins() is order-preserving and the first slot is the primary.
func devKey(id config.DevIdentity) string {
	if id.JiraAccountID != "" {
		return id.JiraAccountID
	}
	logins := id.AllGitHubLogins()
	if len(logins) > 0 {
		return "gh:" + logins[0]
	}
	return ""
}

// periodTotals builds a Totals for one dev across one bi-weekly period
// (date range [start, end], inclusive). Mirrors the field-by-field
// attribution rules in rollupMonthly/rollupWeekly but skips the
// per-month/per-week bookkeeping since we only need one summed row.
func periodTotals(data *Loaded, id config.DevIdentity, start, end time.Time) Totals {
	var t Totals
	if id.JiraAccountID != "" {
		weeksSeen := map[string]struct{}{}
		for _, iss := range data.Issues {
			matched := iss.Assignee == id.JiraAccountID || iss.Reporter == id.JiraAccountID
			if !matched {
				continue
			}
			if inRange(iss.Updated, start, end) {
				t.JiraIssuesTouched++
				weeksSeen[isoWeek(iss.Updated)] = struct{}{}
			}
			// JiraIssuesCreated is set reporter-attributed below (B5), not here.
			// B1: resolution is assignee-only — the reporter filed it, the
			// assignee did the work. Shares resolvedCreditedTo with the
			// composite path so the two can't drift.
			if resolvedCreditedTo(iss, id) && iss.Resolved != nil && inRange(*iss.Resolved, start, end) {
				t.JiraIssuesResolved++
			}
		}
		t.ActiveWeeks += len(weeksSeen)
		// B2: scored Jira-progress signal (replaces touched). Dev-authored,
		// actor-attributed; can't be inherited.
		t.JiraIssuesProgressed = devProgressedIssues(data, id, func(tt time.Time) bool {
			return inRange(tt, start, end)
		})
		// B5: planning credit — reporter-attributed distinct issues filed.
		t.JiraIssuesCreated = devCreatedIssues(data, id, func(tt time.Time) bool {
			return inRange(tt, start, end)
		})
	}

	logins := id.AllGitHubLogins()
	if len(logins) > 0 {
		weeksSeen := map[string]struct{}{}
		for _, pr := range data.PRs {
			if !id.MatchesGitHubLogin(pr.Author) {
				continue
			}
			if inRange(pr.Created, start, end) {
				t.PRsCreated++
				weeksSeen[isoWeek(pr.Created)] = struct{}{}
			}
			if pr.Merged != nil && inRange(*pr.Merged, start, end) {
				t.PRsMerged++
				t.LOCAdded += pr.Additions
				t.LOCDeleted += pr.Deletions
				weeksSeen[isoWeek(*pr.Merged)] = struct{}{}
			}
		}
		for _, c := range data.Commits {
			if !id.MatchesGitHubLogin(c.Author) {
				continue
			}
			if inRange(c.Committed, start, end) {
				t.Commits++
				weeksSeen[isoWeek(c.Committed)] = struct{}{}
			}
		}
		for _, r := range data.Reviews {
			if !id.MatchesGitHubLogin(r.Reviewer) {
				continue
			}
			if inRange(r.Submitted, start, end) {
				t.PRsReviewed++
				weeksSeen[isoWeek(r.Submitted)] = struct{}{}
			}
		}
		// Merge the GH-side activity weeks into ActiveWeeks. Avoid double
		// counting a week already seen by the Jira-side loop.
		if id.JiraAccountID != "" && t.ActiveWeeks > 0 {
			// Re-scan Jira's updates to dedupe.
			jiraWeeks := map[string]struct{}{}
			for _, iss := range data.Issues {
				if (iss.Assignee == id.JiraAccountID || iss.Reporter == id.JiraAccountID) && inRange(iss.Updated, start, end) {
					jiraWeeks[isoWeek(iss.Updated)] = struct{}{}
				}
			}
			for w := range weeksSeen {
				if _, ok := jiraWeeks[w]; !ok {
					t.ActiveWeeks++
				}
			}
		} else {
			t.ActiveWeeks += len(weeksSeen)
		}
	}
	return t
}

// inRange reports whether t falls inside [start, end] (inclusive).
func inRange(t, start, end time.Time) bool {
	if t.Before(start) {
		return false
	}
	if t.After(end) {
		return false
	}
	return true
}

// hasAnyActivity reports whether a Totals carries at least one GH-actor signal
// (authored PRs, merges, reviews, or commits). Used to decide which devs
// "played" a given period — silent / idle devs sit out and their rating stays
// put.
//
// Jira signals are deliberately excluded (A). They attribute a ticket's
// current assignee/reporter against the ticket's historical timestamps, so a
// dev inherits the full footprint of every ticket they now own — manufacturing
// phantom pre-onset periods (a new hire "plays" periods before they wrote a
// line). Only GH signals are actor-attributed against the event's own time and
// cannot be inherited, so participation gates on them alone. Reviews stay in so
// non-coding leads still play every period they review in. Jira still feeds the
// composite score within periods a dev was independently GH-active.
func hasAnyActivity(t Totals) bool {
	return t.PRsCreated+t.PRsMerged+t.PRsReviewed+t.Commits > 0
}

// applyEloPeriod runs one period's Elo update against the in-memory state.
// devs is the configured [[devs]] table (excluded devs already filtered out
// upstream). state is the persistent ratings map keyed by devKey(); it's
// mutated in place. weights/scoring config control the per-period score
// computation that drives "actual".
//
// Idle devs (no activity this period) sit out — their state is untouched,
// their rating stays put, and they do not append a history entry.
func applyEloPeriod(
	state map[string]cache.DevRatingState,
	devs []config.DevIdentity,
	data *Loaded,
	period string,
	scoring config.ScoringConfig,
) error {
	start, err := periodStart(period)
	if err != nil {
		return err
	}
	end, err := periodEnd(period)
	if err != nil {
		return err
	}

	// 1. Build per-dev Totals + collect active devs.
	type active struct {
		idx int // index into devs
		key string
		t   Totals
	}
	var actives []active
	for i, id := range devs {
		key := devKey(id)
		if key == "" {
			continue
		}
		t := periodTotals(data, id, start, end)
		if !hasAnyActivity(t) {
			continue
		}
		actives = append(actives, active{idx: i, key: key, t: t})
	}
	if len(actives) == 0 {
		return nil
	}

	// 2. Score the active subset with the same A4 machinery we use for the
	// current-window leaderboard. Build a minimal DevWindowMetrics slice
	// so we can reuse computeContributorScores directly.
	mini := make([]DevWindowMetrics, len(actives))
	for i, a := range actives {
		mini[i] = DevWindowMetrics{Dev: devs[a.idx], Totals: a.t}
	}
	mini = computeContributorScores(mini, scoring.Weights, scoring.Normalize)

	// 3. Period outcome = an averaged margin-scaled round-robin on the output
	// axis (Phase 4, C3). S_i is dev i's mean pairwise game result vs the
	// active field, scaled by the period's score spread. This replaces
	// logisticZ(score): there is no ~0.73 cap, so a dev who out-produces the
	// whole field reaches S≈1 and keeps climbing (fixes the flatten-top
	// plateau), and a dominant ~2σ period lands at ~0.85-0.9 by construction
	// (folds in C2). scale→0 (uniform period) ⇒ all draws.
	scores := make([]float64, len(mini))
	for i, m := range mini {
		if m.Score != nil {
			scores[i] = m.Score.Total
		}
	}
	sd := stdevPop(scores)
	scale := scoring.EloMarginScale * sd
	band := scoring.EloMarginDeadzone * sd
	actuals := roundRobinScore(scores, scale, band)

	// 4. Each active dev's current rating (defaulting to the starting rating
	// for first-time devs). teamMean is retained only for the idle-decay pull
	// below; the Elo update no longer uses it.
	var sumR float64
	currentR := make([]float64, len(actives))
	for i, a := range actives {
		r := eloStartingRating
		if existing, ok := state[a.key]; ok {
			r = existing.Current
		}
		currentR[i] = r
		sumR += r
	}
	teamMean := sumR / float64(len(actives))
	// E_i: averaged pairwise expected score against the real field (each peer
	// at their actual current rating), replacing the single shifting-teamMean
	// opponent — a durable standing, and a strong cohort is genuinely harder.
	expecteds := roundRobinExpected(currentR)

	// 5. Per-dev Elo delta, computed pool-neutrally so the provisional
	// loss-softening doesn't inflate the cohort.
	//
	// The raw round-robin signal sums to ~0 across the period (Σ(S−E)=0; only
	// the per-dev K-tier introduces a tiny wobble), so rating mass is a shared
	// pool. Softening a provisional dev's loss in isolation would mint that
	// forgiven mass out of nothing → every rating drifts up. Instead we measure
	// the mass we forgive on the loss side and claw it back from the period's
	// GAINERS, in proportion to each one's gain: the points a learner doesn't
	// lose come out of the winners' gains, not the pool. Net effect — provisional
	// devs lose less AND the field gains a little less, so learners are protected
	// *relative* to the cohort while the period stays balanced (no inflation).
	deltas := make([]float64, len(actives))
	var forgiven, gainSum float64
	for i, a := range actives {
		entry := state[a.key]
		pp := entry.PeriodsPlayed
		k := eloKFactor(pp, scoring.KTiers)
		raw := float64(k) * (actuals[i] - expecteds[i])
		d := raw
		// Provisional devs carry the highest K, so without this they'd fall
		// fastest. Soften only the downside; pp is the count BEFORE this period,
		// matching attachRatings' provisional flag (< ProvisionalUntilPeriods).
		if raw < 0 && scoring.ProvisionalLossFactor > 0 && pp < scoring.ProvisionalUntilPeriods {
			d = raw * scoring.ProvisionalLossFactor
			forgiven += d - raw // = (1−f)·|raw| > 0: the loss we didn't take
		}
		deltas[i] = d
		if d > 0 {
			gainSum += d
		}
	}
	// Reclaim the forgiven mass from the gainers, proportional to each gain, so
	// Σdelta is left at its pre-softening value. A degenerate all-loss/draw
	// period has no gainers to rebalance against — the tiny residual is left.
	if forgiven > 0 && gainSum > 0 {
		for i := range deltas {
			if deltas[i] > 0 {
				deltas[i] -= forgiven * (deltas[i] / gainSum)
			}
		}
	}

	// Apply per-dev Elo update, persist history snapshot.
	activeKeys := make(map[string]bool, len(actives))
	for i, a := range actives {
		activeKeys[a.key] = true
		entry := state[a.key]
		if entry.Current == 0 {
			entry.Current = eloStartingRating
		}
		delta := deltas[i]
		newR := currentR[i] + delta
		entry.Current = newR
		entry.PeriodsPlayed++
		entry.IdleStreak = 0
		entry.History = append(entry.History, cache.EloPoint{
			Period: period,
			Rating: newR,
			Delta:  delta,
			Score:  actuals[i],
			Kind:   "active",
		})
		state[a.key] = entry
	}

	// 6. Idle-period decay: for every dev in state who is also a configured
	// dev but did NOT play this period, increment IdleStreak. Once the streak
	// exceeds IdleDecayAfter, pull the rating toward the active cohort's
	// teamMean by IdleDecayDelta — clamped so it never overshoots the mean.
	// PeriodsPlayed does not increment for decay ticks (the dev didn't play);
	// the decay history entry carries Kind="decay" so consumers can render
	// it differently from active-period entries.
	//
	// Devs in state who are no longer in the configured devs list (orphans)
	// are skipped — they shouldn't decay against a cohort they're not part
	// of. Devs in devs but never in state (never played) also don't decay;
	// there's nothing to decay from.
	if scoring.IdleDecayDelta > 0 {
		configured := make(map[string]bool, len(devs))
		for _, id := range devs {
			if k := devKey(id); k != "" {
				configured[k] = true
			}
		}
		for key, entry := range state {
			if activeKeys[key] || !configured[key] {
				continue
			}
			entry.IdleStreak++
			if entry.IdleStreak > scoring.IdleDecayAfter {
				gap := teamMean - entry.Current
				absGap := gap
				if absGap < 0 {
					absGap = -absGap
				}
				pull := scoring.IdleDecayDelta
				if pull > absGap {
					pull = absGap
				}
				if gap < 0 {
					pull = -pull
				}
				entry.Current += pull
				entry.History = append(entry.History, cache.EloPoint{
					Period: period,
					Rating: entry.Current,
					Delta:  pull,
					Score:  0,
					Kind:   "decay",
				})
			}
			state[key] = entry
		}
	}
	return nil
}

// advanceRatings walks every completed period from rt.LastPeriod (exclusive)
// up to the most recent completed period as of `now`, applying one Elo
// update per period. Backfills from start (typically backfill_start) when
// rt has no history yet. Returns the last period applied or "" if no work
// was needed.
func advanceRatings(
	rt *cache.Ratings,
	devs []config.DevIdentity,
	data *Loaded,
	start, end cache.Month,
	scoring config.ScoringConfig,
	now time.Time,
) (string, error) {
	periods, err := completedPeriodsBetween(start, end, now)
	if err != nil {
		return "", err
	}
	if len(periods) == 0 {
		return rt.LastPeriod, nil
	}
	// Skip periods we've already applied.
	if rt.LastPeriod != "" {
		idx := sort.SearchStrings(periods, rt.LastPeriod)
		if idx < len(periods) && periods[idx] == rt.LastPeriod {
			periods = periods[idx+1:]
		}
	}
	if len(periods) == 0 {
		return rt.LastPeriod, nil
	}

	if rt.Devs == nil {
		rt.Devs = map[string]cache.DevRatingState{}
	}
	for _, p := range periods {
		if err := applyEloPeriod(rt.Devs, devs, data, p, scoring); err != nil {
			return "", fmt.Errorf("apply period %s: %w", p, err)
		}
		rt.LastPeriod = p
	}
	return rt.LastPeriod, nil
}

// attachRatings copies state for each mapped dev onto its DevWindowMetrics.
// Idle / never-played devs surface with Current=1000 (the Elo baseline) and
// PeriodsPlayed=0 so the UI can show them as unranked-but-present. The
// scoring config controls the Provisional cutoff — devs with fewer than
// ProvisionalUntilPeriods periods played are flagged so the leaderboard can
// downplay their rating (excluded from medal highlights, badge in the row).
func attachRatings(devs []DevWindowMetrics, state map[string]cache.DevRatingState, scoring config.ScoringConfig) []DevWindowMetrics {
	for i := range devs {
		if devs[i].Dev.DisplayName == "unknown" {
			continue
		}
		key := devKey(devs[i].Dev)
		if key == "" {
			continue
		}
		s, ok := state[key]
		if !ok {
			devs[i].Rating = &EloRating{
				Current:     eloStartingRating,
				Provisional: scoring.ProvisionalUntilPeriods > 0,
			}
			continue
		}
		hist := make([]float64, len(s.History))
		dates := make([]string, len(s.History))
		var delta float64
		for j, p := range s.History {
			hist[j] = p.Rating
			// Convert the stored period label ("YYYY-Pnn") to its start date so
			// the frontend can date-label and window-clip the trajectory. The
			// label is produced by our own walker, so periodStart should never
			// fail; leave the date empty on the off chance it does rather than
			// dropping the whole rating.
			if t, err := periodStart(p.Period); err == nil {
				dates[j] = t.Format("2006-01-02")
			}
			if j == len(s.History)-1 {
				delta = p.Delta
			}
		}
		devs[i].Rating = &EloRating{
			Current:       s.Current,
			DeltaPeriod:   delta,
			PeriodsPlayed: s.PeriodsPlayed,
			Provisional:   s.PeriodsPlayed < scoring.ProvisionalUntilPeriods,
			History:       hist,
			HistoryDates:  dates,
		}
	}
	return devs
}
