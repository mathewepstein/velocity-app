// Package incognito provides the anonymization layer for `velocity serve
// --incognito`. Real dev / epic identifiers are replaced with persistent
// pseudonyms drawn from a fantasy-leaning name pool. The persistence layer
// guarantees that once a name is assigned to a real identity, it never
// changes across runs — only newly-encountered identities pull from the
// pool.
//
// Architecture: pure data + pure functions. The scrubber takes a fully-loaded
// analyze.Result plus a Mapping and returns a deep-copied scrubbed view.
// The server is responsible for owning the Mapping for its lifetime and for
// persisting it back to disk when new assignments are minted.
package incognito

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"

	"github.com/mathewepstein/velocity/internal/cache"
)

// NamesFile is the on-disk filename for the persisted anon-to-real map.
// Lives at the cache root alongside ratings.json so it survives both
// `velocity refresh --reset` (which only wipes per-source trees) and
// `velocity analyze --rebuild` (which only touches ratings.json).
const NamesFile = "incognito-names.json"

// CurrentMappingVersion is bumped on schema changes. The decoder accepts
// any value ≤ this and migrates forward.
const CurrentMappingVersion = 1

// Mapping is the in-process anonymization state: bidirectional dev/epic
// lookups plus a slug index for /dev/<slug> URL routing. Build via Load (or
// LoadFromPath in tests) and pass to Scrub for each /metrics.json response.
//
// Assignments mutate the maps in place; call Save to persist after every
// successful Ensure path that may have minted new names.
type Mapping struct {
	Version int `json:"version"`

	// Devs maps a real dev's display_name to its fantasy pseudonym.
	// "unknown" is intentionally absent — the synthetic bucket stays
	// labeled "unknown" so gaps in the [[devs]] config remain visible.
	Devs map[string]DevAlias `json:"devs"`

	// Epics maps real Jira epic_key (e.g. "CD-12345") to its anonymized
	// label ("Project 1", "Project 2", ...). Sequential assignment order is
	// preserved by EpicOrder so a saved+reloaded file produces the same
	// numbering on every run.
	Epics      map[string]string `json:"epics"`
	EpicOrder  []string          `json:"epic_order"`

	// reverse lookups, rebuilt on Load — not persisted.
	devBySlug    map[string]string // "aldric-whitestone" → real display_name
	devByLogin   map[string]string // real github login → real display_name
}

// DevAlias is one dev's persistent pseudonym set. DisplayName is the
// rendered "Aldric Whitestone"; Slug is the URL-safe variant ("aldric-
// whitestone") used as the synthetic github login + URL path.
type DevAlias struct {
	DisplayName string `json:"display_name"`
	Slug        string `json:"slug"`
}

// Load reads the persisted map from $XDG_DATA_HOME/velocity/. A missing file
// is not an error — the returned Mapping is empty, ready to mint names on
// first Ensure call.
func Load() (*Mapping, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	return LoadFromPath(path)
}

// LoadFromPath is the test seam — same shape as Load but works against an
// arbitrary path.
func LoadFromPath(path string) (*Mapping, error) {
	m := &Mapping{
		Version: CurrentMappingVersion,
		Devs:    map[string]DevAlias{},
		Epics:   map[string]string{},
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			m.rebuildIndices()
			return m, nil
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := json.Unmarshal(data, m); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if m.Devs == nil {
		m.Devs = map[string]DevAlias{}
	}
	if m.Epics == nil {
		m.Epics = map[string]string{}
	}
	if m.Version > CurrentMappingVersion {
		return nil, fmt.Errorf("incognito-names.json version %d is newer than this binary (%d)", m.Version, CurrentMappingVersion)
	}
	m.rebuildIndices()
	return m, nil
}

// Path returns the absolute path to incognito-names.json. Lives at the
// cache root next to ratings.json + metrics.json.
func Path() (string, error) {
	root, err := cache.Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, NamesFile), nil
}

// Save atomically replaces the persisted map. Mirrors cache.SaveRatings'
// durability shape: tempfile + fsync + best-effort backup + rename.
func (m *Mapping) Save() error {
	if m == nil {
		return fmt.Errorf("nil mapping")
	}
	path, err := Path()
	if err != nil {
		return err
	}
	return m.SaveToPath(path)
}

// SaveToPath is the test seam for Save.
func (m *Mapping) SaveToPath(path string) error {
	if m.Version == 0 {
		m.Version = CurrentMappingVersion
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "incognito-names-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("encode: %w", err)
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
	if data, err := os.ReadFile(path); err == nil {
		_ = os.WriteFile(path+".bak", data, 0o600)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// rebuildIndices reconstructs the unexported reverse-lookup maps from the
// canonical Devs/Epics tables. Called by Load and whenever Ensure mutates.
func (m *Mapping) rebuildIndices() {
	m.devBySlug = make(map[string]string, len(m.Devs))
	for real, alias := range m.Devs {
		if alias.Slug != "" {
			m.devBySlug[alias.Slug] = real
		}
	}
}

// SetLoginIndex feeds the real-login-to-display-name index from the caller
// (typically the analyze result's [[devs]] data). Called once per /metrics
// request before any Lookup queries to keep the index fresh against the
// current cohort, since logins may shift independently of display names.
func (m *Mapping) SetLoginIndex(loginToDisplayName map[string]string) {
	m.devByLogin = make(map[string]string, len(loginToDisplayName))
	for login, name := range loginToDisplayName {
		m.devByLogin[login] = name
	}
}

// LoginIndex exposes the real-github-login → real-display-name lookup so
// the server can answer "what's my login's anonymized slug?" for the "Me"
// nav link. The map is empty until ScrubResult has been called at least
// once with a populated devs slice.
func (m *Mapping) LoginIndex() map[string]string {
	out := make(map[string]string, len(m.devByLogin))
	for k, v := range m.devByLogin {
		out[k] = v
	}
	return out
}

// EnsureDev returns the alias for realDisplayName, minting a new one if
// this is the first encounter. Mutates m.Devs; the caller is responsible
// for Save() after all Ensure* calls for one render cycle have completed.
// Returns mutated=true when a new entry was added — that's the cheap signal
// callers use to decide whether to bother saving.
func (m *Mapping) EnsureDev(realDisplayName string) (DevAlias, bool) {
	if realDisplayName == "" || realDisplayName == "unknown" {
		// Leave the "unknown" bucket as-is. It's not identifying and special-
		// casing it would surprise callers who walk the devs slice.
		return DevAlias{DisplayName: realDisplayName, Slug: realDisplayName}, false
	}
	if existing, ok := m.Devs[realDisplayName]; ok {
		return existing, false
	}
	alias := m.mintDevAlias(realDisplayName)
	m.Devs[realDisplayName] = alias
	if m.devBySlug == nil {
		m.devBySlug = map[string]string{}
	}
	m.devBySlug[alias.Slug] = realDisplayName
	return alias, true
}

// EnsureEpic returns the anonymized label for realEpicKey, minting a new
// "Project N" if first encounter. N is the next free integer; the
// assignment order is persisted via EpicOrder so reloads stay stable.
func (m *Mapping) EnsureEpic(realEpicKey string) (string, bool) {
	if realEpicKey == "" {
		return "", false
	}
	if existing, ok := m.Epics[realEpicKey]; ok {
		return existing, false
	}
	label := fmt.Sprintf("%s%d", projectPrefix, len(m.EpicOrder)+1)
	m.Epics[realEpicKey] = label
	m.EpicOrder = append(m.EpicOrder, realEpicKey)
	return label, true
}

// DevForSlug resolves a URL slug back to the real display_name. Used by the
// server's /dev/{slug} handler to look up real-cache data before scrubbing
// the response.
func (m *Mapping) DevForSlug(slug string) (string, bool) {
	real, ok := m.devBySlug[slug]
	return real, ok
}

// LoginForSlug resolves a URL slug back to the real github login the page
// expects (matching the original path shape). It picks the dev's display
// name → finds any one of their real github logins → returns it. Best-
// effort: returns "" if the slug isn't mapped or the dev has no known login.
//
// Callers should treat "" as "render the no-dev-found page" rather than
// fall through to a real-login dump.
func (m *Mapping) LoginForSlug(slug string) string {
	real, ok := m.devBySlug[slug]
	if !ok {
		return ""
	}
	for login, displayName := range m.devByLogin {
		if displayName == real {
			return login
		}
	}
	return ""
}

// mintDevAlias picks a (first, last) pair for realDisplayName by hashing the
// real name into the pools, then linear-probing if the resulting full name is
// already taken. Linear probing keeps the assignment deterministic given the
// same Mapping state, which means tests pin to specific outputs and the
// human reading the persisted file can verify which dev got which name.
func (m *Mapping) mintDevAlias(realDisplayName string) DevAlias {
	used := make(map[string]struct{}, len(m.Devs))
	for _, a := range m.Devs {
		used[a.DisplayName] = struct{}{}
	}

	h := fnv.New64a()
	_, _ = h.Write([]byte(realDisplayName))
	seed := h.Sum64()
	firstIdx := int(seed % uint64(len(firstNames)))
	lastIdx := int((seed / uint64(len(firstNames))) % uint64(len(lastNames)))

	for attempt := 0; attempt < len(firstNames)*len(lastNames); attempt++ {
		fi := (firstIdx + attempt) % len(firstNames)
		li := (lastIdx + attempt/len(firstNames)) % len(lastNames)
		candidate := firstNames[fi] + " " + lastNames[li]
		if _, taken := used[candidate]; taken {
			continue
		}
		return DevAlias{
			DisplayName: candidate,
			Slug:        slugify(candidate),
		}
	}

	// Fully exhausted pool — fall back to a numbered alias. Practically
	// unreachable for any sane cohort (3,600 combinations), but the
	// fallback keeps Scrub deterministic in pathological tests.
	fallback := fmt.Sprintf("Dev %d", len(m.Devs)+1)
	return DevAlias{DisplayName: fallback, Slug: slugify(fallback)}
}

// slugify renders a display name into a URL-safe form. Lowercased, spaces
// → hyphens, everything else dropped. ASCII-only — the pool itself is ASCII,
// so we don't need full Unicode normalization.
func slugify(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			out = append(out, c+32)
		case c >= 'a' && c <= 'z':
			out = append(out, c)
		case c >= '0' && c <= '9':
			out = append(out, c)
		case c == ' ' || c == '-' || c == '_':
			if len(out) > 0 && out[len(out)-1] != '-' {
				out = append(out, '-')
			}
		}
	}
	for len(out) > 0 && out[len(out)-1] == '-' {
		out = out[:len(out)-1]
	}
	return string(out)
}

// SortedRealDevNames returns the real dev names with persisted aliases, in
// stable assignment-encounter order. Used by tests and debug tooling.
func (m *Mapping) SortedRealDevNames() []string {
	out := make([]string, 0, len(m.Devs))
	for k := range m.Devs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
