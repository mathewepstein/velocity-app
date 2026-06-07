package cache

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOpenStore_BackendSelection locks the contract that the [cache] backend
// config key drives the substrate: absent/empty resolves to sqlite (no config
// needed for the standard backend), "json" opts into the legacy store, and an
// unknown value is a loud error rather than a silent fallback.
func TestOpenStore_BackendSelection(t *testing.T) {
	cases := []struct {
		name    string
		toml    string
		wantErr bool
		wantJSON bool
	}{
		{name: "no cache section defaults to sqlite", toml: "", wantJSON: false},
		{name: "explicit sqlite", toml: "[cache]\nbackend = \"sqlite\"\n", wantJSON: false},
		{name: "json opt-in", toml: "[cache]\nbackend = \"json\"\n", wantJSON: true},
		{name: "case-insensitive", toml: "[cache]\nbackend = \"SQLite\"\n", wantJSON: false},
		{name: "unknown backend errors", toml: "[cache]\nbackend = \"mysql\"\n", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("XDG_DATA_HOME", dir)
			cfgPath := filepath.Join(dir, "config.toml")
			if err := os.WriteFile(cfgPath, []byte(tc.toml), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			t.Setenv("VELOCITY_CONFIG", cfgPath)

			st, err := OpenStore()
			if tc.wantErr {
				if err == nil {
					st.Close()
					t.Fatalf("expected error, got store %T", st)
				}
				return
			}
			if err != nil {
				t.Fatalf("OpenStore: %v", err)
			}
			defer st.Close()
			if _, gotJSON := st.(JSONStore); gotJSON != tc.wantJSON {
				t.Errorf("store type = %T, wantJSON = %v", st, tc.wantJSON)
			}
		})
	}
}
