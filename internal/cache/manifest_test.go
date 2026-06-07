package cache

import (
	"testing"
	"time"
)

func TestManifestRoundtrip(t *testing.T) {
	// Point the cache root at a temp dir for this test.
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	m := NewManifest()
	now := time.Date(2024, time.March, 15, 12, 0, 0, 0, time.UTC)
	m.Update(SourceJira, "CD", MustParseMonth("2024-01"), 100, now)
	m.Update(SourceGitHubPRs, "consumerdirect", MustParseMonth("2024-01"), 17, now)

	if err := m.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := LoadManifest()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(loaded.Entries))
	}

	jira, ok := loaded.Entry(SourceJira, "CD", MustParseMonth("2024-01"))
	if !ok {
		t.Fatal("jira entry missing")
	}
	if jira.Records != 100 {
		t.Errorf("jira.Records = %d, want 100", jira.Records)
	}
	if !jira.PulledAt.Equal(now) {
		t.Errorf("jira.PulledAt = %v, want %v", jira.PulledAt, now)
	}
	if jira.Month != "2024-01" {
		t.Errorf("jira.Month = %q, want 2024-01", jira.Month)
	}
}

func TestLoadManifest_MissingFileIsEmpty(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("Load on missing manifest returned error: %v", err)
	}
	if len(m.Entries) != 0 {
		t.Errorf("expected empty manifest, got %d entries", len(m.Entries))
	}
}

func TestLatestCachedMonth(t *testing.T) {
	now := time.Date(2024, time.March, 15, 12, 0, 0, 0, time.UTC)

	if _, ok := NewManifest().LatestCachedMonth(); ok {
		t.Error("empty manifest should report ok=false")
	}

	m := NewManifest()
	m.Update(SourceJira, "CD", MustParseMonth("2023-11"), 1, now)
	m.Update(SourceGitHubPRs, "consumerdirect", MustParseMonth("2024-02"), 1, now)
	m.Update(SourceJira, "CD", MustParseMonth("2024-01"), 1, now)

	last, ok := m.LatestCachedMonth()
	if !ok {
		t.Fatal("populated manifest should report ok=true")
	}
	if !last.Equal(MustParseMonth("2024-02")) {
		t.Errorf("LatestCachedMonth = %s, want 2024-02", last)
	}
}

func TestManifestUpdate_Overwrite(t *testing.T) {
	m := NewManifest()
	key := MustParseMonth("2024-01")
	m.Update(SourceJira, "CD", key, 10, time.Now())
	m.Update(SourceJira, "CD", key, 20, time.Now())
	e, _ := m.Entry(SourceJira, "CD", key)
	if e.Records != 20 {
		t.Errorf("expected overwrite to 20, got %d", e.Records)
	}
	if len(m.Entries) != 1 {
		t.Errorf("expected 1 entry after overwrite, got %d", len(m.Entries))
	}
}
