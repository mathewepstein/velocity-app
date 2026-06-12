package cache

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// RatingsFile is the bi-weekly Elo state filename at the cache root.
// Lives at the cache root (not under a source tree) so a `velocity refresh
// --reset` doesn't wipe rating history.
const RatingsFile = "ratings.json"

// CurrentRatingsVersion is the Ratings.Version emitted by today's writer.
// Bumped if the schema changes; loaders should accept any version <=
// CurrentRatingsVersion and migrate forward.
//
// v2 (Phase 7.3): adds DevRatingState.IdleStreak and EloPoint.Kind for the
// idle-period decay machinery. v1 files load with IdleStreak=0 across the
// board — for accurate streak counts, run `velocity analyze --rebuild` once
// after upgrade.
//
// v3 (Phase 4 Elo redesign): no schema change, but the per-period outcome
// model changed from logisticZ(score)-vs-teamMean to an averaged margin-scaled
// pairwise round-robin (see elo.go roundRobinScore/roundRobinExpected). Ratings
// computed under v2 are stale under the new model — bumping forces awareness
// that a full `velocity analyze --rebuild` is required to recompute history.
const CurrentRatingsVersion = 3

// Ratings is the persisted Elo state. Keyed by stable per-dev identity:
// the dev's Jira accountId if present, else "gh:<first-github-login>". The
// key shape lives in analyze and is reproduced here only as a comment to
// avoid an analyze→cache import cycle.
type Ratings struct {
	Version    int                       `json:"version"`
	LastPeriod string                    `json:"last_period,omitempty"`
	Devs       map[string]DevRatingState `json:"devs"`
}

// DevRatingState is one developer's full rating history. Current is the
// rating after the most recently played or decayed period; History is the
// per-period trajectory, in chronological order — one entry per period the
// dev was active in plus one entry per idle period that triggered decay.
// IdleStreak counts consecutive completed periods the dev has sat out since
// last playing; it resets to 0 on activity and feeds the idle-decay
// threshold check (ScoringConfig.IdleDecayAfter).
type DevRatingState struct {
	Current       float64    `json:"current"`
	PeriodsPlayed int        `json:"periods_played"`
	IdleStreak    int        `json:"idle_streak"`
	History       []EloPoint `json:"history"`
}

// EloPoint is one row in a dev's rating history. Score is the normalized
// outcome that drove this period's delta; Delta is the Elo change; Rating is
// the post-update value (== sum of all preceding Deltas + start). Kind is
// "" or "active" for periods the dev played; "decay" for idle-period decay
// ticks that pull a stale rating toward the active cohort's mean.
type EloPoint struct {
	Period string  `json:"period"`
	Rating float64 `json:"rating"`
	Delta  float64 `json:"delta"`
	Score  float64 `json:"score"`
	Kind   string  `json:"kind,omitempty"`
}

// RatingsPath returns the absolute path to ratings.json. Lives at the cache
// root next to manifest.json and metrics.json.
func RatingsPath() (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, RatingsFile), nil
}

// LoadRatings reads ratings.json from disk. A missing file is not an error
// — it returns a zero-valued Ratings ready to be populated.
func LoadRatings() (*Ratings, error) {
	path, err := RatingsPath()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Ratings{Version: CurrentRatingsVersion, Devs: map[string]DevRatingState{}}, nil
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	return decodeRatings(f, path)
}

func decodeRatings(r io.Reader, path string) (*Ratings, error) {
	var rt Ratings
	dec := json.NewDecoder(r)
	if err := dec.Decode(&rt); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if rt.Devs == nil {
		rt.Devs = map[string]DevRatingState{}
	}
	if rt.Version == 0 {
		rt.Version = CurrentRatingsVersion
	}
	if rt.Version > CurrentRatingsVersion {
		return nil, fmt.Errorf("ratings.json version %d is newer than this binary (%d) — upgrade velocity", rt.Version, CurrentRatingsVersion)
	}
	// v1 → v2 migration: IdleStreak fields zero-init via struct decoding.
	// History entries without a Kind field decode as "" which the walker
	// treats as "active" — semantically correct, no fixup needed. Streak
	// counts will be pessimistic (everyone starts at 0) until the next
	// --rebuild walks the full history under v2 rules; documented at the
	// CurrentRatingsVersion comment.
	if rt.Version < CurrentRatingsVersion {
		rt.Version = CurrentRatingsVersion
	}
	return &rt, nil
}

// SaveRatings atomically replaces ratings.json with rt. Writes to a
// tempfile in the same dir, fsyncs, copies the existing file to
// ratings.json.bak, then renames. Mirrors the config.SaveTo durability
// shape so a mid-write crash never leaves an empty ratings.json.
func SaveRatings(rt *Ratings) error {
	if rt == nil {
		return fmt.Errorf("nil ratings")
	}
	if rt.Version == 0 {
		rt.Version = CurrentRatingsVersion
	}
	path, err := RatingsPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, "ratings-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rt); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("encode ratings: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp: %w", err)
	}

	// Snapshot prior file to .bak (best effort: first write has nothing to back up).
	if data, err := os.ReadFile(path); err == nil {
		_ = os.WriteFile(path+".bak", data, 0o600)
	}

	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("rename %s → %s: %w", tmpName, path, err)
	}
	return nil
}
