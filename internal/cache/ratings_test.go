package cache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRatingsReturnsEmptyWhenMissing(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	rt, err := LoadRatings()
	if err != nil {
		t.Fatalf("LoadRatings: %v", err)
	}
	if rt.Version != CurrentRatingsVersion {
		t.Errorf("Version = %d, want %d", rt.Version, CurrentRatingsVersion)
	}
	if rt.Devs == nil || len(rt.Devs) != 0 {
		t.Errorf("Devs map should be initialized and empty, got %+v", rt.Devs)
	}
}

func TestSaveAndLoadRatingsRoundTrip(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	in := &Ratings{
		Version:    CurrentRatingsVersion,
		LastPeriod: "2024-P19",
		Devs: map[string]DevRatingState{
			"acct-alice": {
				Current:       1023.5,
				PeriodsPlayed: 2,
				History: []EloPoint{
					{Period: "2024-P17", Rating: 1016, Delta: 16, Score: 1.0},
					{Period: "2024-P19", Rating: 1023.5, Delta: 7.5, Score: 0.75},
				},
			},
		},
	}
	if err := SaveRatings(in); err != nil {
		t.Fatalf("SaveRatings: %v", err)
	}
	out, err := LoadRatings()
	if err != nil {
		t.Fatalf("LoadRatings: %v", err)
	}
	if out.LastPeriod != "2024-P19" {
		t.Errorf("LastPeriod = %q, want 2024-P19", out.LastPeriod)
	}
	state := out.Devs["acct-alice"]
	if state.PeriodsPlayed != 2 {
		t.Errorf("PeriodsPlayed = %d, want 2", state.PeriodsPlayed)
	}
	if len(state.History) != 2 || state.History[1].Period != "2024-P19" {
		t.Errorf("history wrong: %+v", state.History)
	}
}

func TestSaveRatingsBacksUpExistingFile(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	path, _ := RatingsPath()
	// First write: no .bak (nothing to back up).
	if err := SaveRatings(&Ratings{Version: CurrentRatingsVersion, LastPeriod: "first"}); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Errorf("first save shouldn't produce .bak yet, err = %v", err)
	}
	// Second write: .bak should hold prior contents.
	if err := SaveRatings(&Ratings{Version: CurrentRatingsVersion, LastPeriod: "second"}); err != nil {
		t.Fatalf("second save: %v", err)
	}
	bak, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("read .bak: %v", err)
	}
	if !strings.Contains(string(bak), "first") {
		t.Errorf(".bak should contain prior contents, got %q", string(bak))
	}
}

func TestSaveRatingsLeavesNoTempFiles(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	if err := SaveRatings(&Ratings{Version: CurrentRatingsVersion}); err != nil {
		t.Fatalf("SaveRatings: %v", err)
	}
	path, _ := RatingsPath()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "ratings-") && strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("orphan tempfile %q left behind", e.Name())
		}
	}
}

func TestLoadRatingsMigratesV1ToV2(t *testing.T) {
	// A pre-Phase-7.3 ratings.json on disk: version=1, no IdleStreak field on
	// DevRatingState, no Kind field on EloPoint. Loader should accept it,
	// bump the in-memory version to 2, and leave streaks at their zero value
	// (the soft-migration policy — accurate counts require --rebuild).
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	path, _ := RatingsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	v1 := `{
  "version": 1,
  "last_period": "2024-P19",
  "devs": {
    "gh:alice": {
      "current": 1042,
      "periods_played": 3,
      "history": [
        {"period": "2024-P17", "rating": 1030, "delta": 14, "score": 0.8},
        {"period": "2024-P19", "rating": 1042, "delta": 12, "score": 0.75}
      ]
    }
  }
}`
	if err := os.WriteFile(path, []byte(v1), 0o600); err != nil {
		t.Fatalf("seed v1 file: %v", err)
	}
	out, err := LoadRatings()
	if err != nil {
		t.Fatalf("LoadRatings: %v", err)
	}
	if out.Version != CurrentRatingsVersion {
		t.Errorf("Version = %d, want %d (migrated)", out.Version, CurrentRatingsVersion)
	}
	alice := out.Devs["gh:alice"]
	if alice.IdleStreak != 0 {
		t.Errorf("IdleStreak = %d, want 0 (zero-init on v1 → v2 migration)", alice.IdleStreak)
	}
	if len(alice.History) != 2 {
		t.Fatalf("history len = %d, want 2", len(alice.History))
	}
	if alice.History[0].Kind != "" {
		t.Errorf("v1 history Kind = %q, want \"\" (zero-init, walker treats as active)", alice.History[0].Kind)
	}
}

func TestResetPreservesRatingsJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	if err := SaveRatings(&Ratings{Version: CurrentRatingsVersion, LastPeriod: "stays"}); err != nil {
		t.Fatalf("seed save: %v", err)
	}
	if _, err := Reset(nil); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	rt, err := LoadRatings()
	if err != nil {
		t.Fatalf("LoadRatings after reset: %v", err)
	}
	if rt.LastPeriod != "stays" {
		t.Errorf("Reset wiped ratings.json (LastPeriod = %q, want 'stays')", rt.LastPeriod)
	}
}
