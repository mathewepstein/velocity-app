package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// ManifestEntry records the last pull for one (source, scope, month) cell.
// "Records" is the count written on that pull — useful for `velocity doctor`
// sanity checks and for `velocity refresh` progress reporting.
type ManifestEntry struct {
	Source   Source    `json:"source"`
	Scope    string    `json:"scope"`
	Month    string    `json:"month"` // "YYYY-MM" — string rather than Month so the file stays obvious to hand-read
	PulledAt time.Time `json:"pulled_at"`
	Records  int       `json:"records"`
}

// Manifest is the on-disk registry of what's been pulled and when.
// Entries is keyed by entryKey(source, scope, month).
type Manifest struct {
	Entries map[string]ManifestEntry `json:"entries"`
}

// entryKey builds the composite lookup key. Kept unexported so callers don't
// hand-build keys and drift from the canonical format.
func entryKey(source Source, scope string, m Month) string {
	return fmt.Sprintf("%s:%s:%s", source, scope, m)
}

// NewManifest returns an empty manifest — used when the file doesn't exist yet.
func NewManifest() *Manifest {
	return &Manifest{Entries: map[string]ManifestEntry{}}
}

// LoadManifest reads manifest.json. A missing file returns an empty Manifest
// and a nil error; callers shouldn't need to special-case first runs.
func LoadManifest() (*Manifest, error) {
	path, err := ManifestPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return NewManifest(), nil
		}
		return nil, fmt.Errorf("read manifest %s: %w", path, err)
	}
	m := NewManifest()
	if err := json.Unmarshal(data, m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	if m.Entries == nil {
		m.Entries = map[string]ManifestEntry{}
	}
	return m, nil
}

// Save writes the manifest back atomically (tmp + rename).
func (m *Manifest) Save() error {
	path, err := ManifestPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("commit manifest: %w", err)
	}
	return nil
}

// Entry returns the manifest entry for the given cell, if any.
func (m *Manifest) Entry(source Source, scope string, month Month) (ManifestEntry, bool) {
	e, ok := m.Entries[entryKey(source, scope, month)]
	return e, ok
}

// FirstCachedMonth returns the earliest month present in the manifest
// across every source/scope. ok is false on an empty manifest. Used by
// the Elo walker to clamp the period range — bi-weekly rating updates
// should only start once the cache has actual data underneath them.
func (m *Manifest) FirstCachedMonth() (Month, bool) {
	var first Month
	have := false
	for _, e := range m.Entries {
		mo, err := ParseMonth(e.Month)
		if err != nil {
			continue
		}
		if !have || mo.Before(first) {
			first = mo
			have = true
		}
	}
	return first, have
}

// LatestCachedMonth returns the most recent month present in the manifest
// across every source/scope. ok is false on an empty manifest. Used by
// refresh to anchor the lookback window to the last actual pull rather than
// to a fixed offset from now — so a lapse longer than the window doesn't
// silently skip the intervening months (see effectiveStart).
func (m *Manifest) LatestCachedMonth() (Month, bool) {
	var last Month
	have := false
	for _, e := range m.Entries {
		mo, err := ParseMonth(e.Month)
		if err != nil {
			continue
		}
		if !have || last.Before(mo) {
			last = mo
			have = true
		}
	}
	return last, have
}

// Update records that (source, scope, month) was pulled now with recordCount
// records. now is injected for testability.
func (m *Manifest) Update(source Source, scope string, month Month, recordCount int, now time.Time) {
	if m.Entries == nil {
		m.Entries = map[string]ManifestEntry{}
	}
	m.Entries[entryKey(source, scope, month)] = ManifestEntry{
		Source:   source,
		Scope:    scope,
		Month:    month.String(),
		PulledAt: now.UTC(),
		Records:  recordCount,
	}
}
