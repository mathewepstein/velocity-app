package incognito

import (
	"path/filepath"
	"testing"
)

func TestEnsureDevIsStableAcrossCalls(t *testing.T) {
	m := newEmptyMapping()
	got1, added1 := m.EnsureDev("Christian Corey")
	got2, added2 := m.EnsureDev("Christian Corey")
	if !added1 {
		t.Errorf("first EnsureDev: added = false, want true")
	}
	if added2 {
		t.Errorf("second EnsureDev for same name: added = true, want false (already minted)")
	}
	if got1 != got2 {
		t.Errorf("alias drifted across calls: %+v vs %+v", got1, got2)
	}
}

func TestEnsureDevDifferentNamesGetDifferentAliases(t *testing.T) {
	m := newEmptyMapping()
	a, _ := m.EnsureDev("Christian Corey")
	b, _ := m.EnsureDev("Karl Weckwerth")
	if a.DisplayName == b.DisplayName {
		t.Errorf("two different real names produced the same alias: %q", a.DisplayName)
	}
	if a.Slug == b.Slug {
		t.Errorf("two different real names produced the same slug: %q", a.Slug)
	}
}

func TestEnsureDevPreservesExistingAssignmentsWhenNewDevsAdded(t *testing.T) {
	// "Once assigned, never changes" — the load-bearing invariant. Mint
	// alias for dev A, then add dev B, then check A's alias didn't shift.
	m := newEmptyMapping()
	aliasA, _ := m.EnsureDev("Mathew Epstein")
	_, _ = m.EnsureDev("Aaron Aardvark") // would sort earlier alphabetically
	again, _ := m.EnsureDev("Mathew Epstein")
	if aliasA != again {
		t.Errorf("Mathew's alias shifted when a new dev was added: was %+v, now %+v", aliasA, again)
	}
}

func TestEnsureDevSkipsUnknownBucket(t *testing.T) {
	m := newEmptyMapping()
	alias, added := m.EnsureDev("unknown")
	if added {
		t.Errorf("unknown bucket should not mint a new alias")
	}
	if alias.DisplayName != "unknown" {
		t.Errorf("unknown alias display = %q, want %q (left untouched)", alias.DisplayName, "unknown")
	}
}

func TestEnsureEpicIsSequentialAndStable(t *testing.T) {
	m := newEmptyMapping()
	first, added1 := m.EnsureEpic("CD-100")
	second, added2 := m.EnsureEpic("CD-200")
	third, _ := m.EnsureEpic("CD-100") // re-encounter
	if !added1 || !added2 {
		t.Errorf("first two epics should report added")
	}
	if first != "Project 1" {
		t.Errorf("first epic = %q, want %q", first, "Project 1")
	}
	if second != "Project 2" {
		t.Errorf("second epic = %q, want %q", second, "Project 2")
	}
	if third != "Project 1" {
		t.Errorf("re-encounter of CD-100 = %q, want Project 1 (stable)", third)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, NamesFile)

	m1 := newEmptyMapping()
	m1.EnsureDev("Christian Corey")
	m1.EnsureDev("Karl Weckwerth")
	m1.EnsureEpic("CD-100")
	if err := m1.SaveToPath(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	m2, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(m2.Devs) != 2 {
		t.Errorf("loaded devs = %d, want 2", len(m2.Devs))
	}
	if m1.Devs["Christian Corey"] != m2.Devs["Christian Corey"] {
		t.Errorf("Christian's alias didn't round-trip: %+v vs %+v",
			m1.Devs["Christian Corey"], m2.Devs["Christian Corey"])
	}
	if m2.Epics["CD-100"] != "Project 1" {
		t.Errorf("epic alias didn't round-trip: %q", m2.Epics["CD-100"])
	}

	// Reverse lookup must work after Load (indices rebuild).
	slug := m1.Devs["Christian Corey"].Slug
	real, ok := m2.DevForSlug(slug)
	if !ok || real != "Christian Corey" {
		t.Errorf("DevForSlug(%q) = (%q, %v), want (Christian Corey, true)", slug, real, ok)
	}
}

func TestLoadFromMissingFileReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	m, err := LoadFromPath(filepath.Join(dir, "does-not-exist.json"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(m.Devs) != 0 || len(m.Epics) != 0 {
		t.Errorf("missing-file map should be empty, got %+v", m)
	}
	// Subsequent mint should still work without crashing.
	alias, _ := m.EnsureDev("Test Dev")
	if alias.DisplayName == "" {
		t.Errorf("mint failed on empty mapping: %+v", alias)
	}
}

func TestSlugify(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Aldric Whitestone", "aldric-whitestone"},
		{"Brenna  Ashworth", "brenna-ashworth"}, // double space → single hyphen
		{"  Padded Spaces  ", "padded-spaces"},
		{"Dev 7", "dev-7"},
		{"weird!@#chars", "weirdchars"},
	}
	for _, c := range cases {
		if got := slugify(c.in); got != c.want {
			t.Errorf("slugify(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLoginForSlugRoundTrip(t *testing.T) {
	m := newEmptyMapping()
	m.EnsureDev("Christian Corey")
	m.SetLoginIndex(map[string]string{
		"christiancorey":  "Christian Corey",
		"mathewepstein":   "Mathew Epstein",
		"Mathew Epstein":  "Mathew Epstein",
	})
	slug := m.Devs["Christian Corey"].Slug
	got := m.LoginForSlug(slug)
	if got != "christiancorey" {
		t.Errorf("LoginForSlug(%q) = %q, want %q", slug, got, "christiancorey")
	}
	if m.LoginForSlug("unknown-slug") != "" {
		t.Errorf("unknown slug should return empty string")
	}
}

// newEmptyMapping is the test seam — a fresh in-memory mapping without any
// disk interaction. Production code goes through Load/LoadFromPath, but
// for unit tests we want to skip the filesystem entirely.
func newEmptyMapping() *Mapping {
	return &Mapping{
		Version:    CurrentMappingVersion,
		Devs:       map[string]DevAlias{},
		Epics:      map[string]string{},
		devBySlug:  map[string]string{},
		devByLogin: map[string]string{},
	}
}
