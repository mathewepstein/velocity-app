package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestApplyDefaultsFillsScoringBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := writeFile(t, path, `
[profiles.default]
name = "default"

  [profiles.default.jira]
  base_url = "https://x.atlassian.net"
  email = "a@b"
  projects = ["X"]

  [profiles.default.github]
  username = "x"
  orgs = ["x"]
`); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	p := cfg.ActiveProfile()
	defaults := DefaultScoringConfig()

	if got, want := p.Scoring.KFactorNew, defaults.KFactorNew; got != want {
		t.Errorf("KFactorNew = %d, want %d", got, want)
	}
	if got, want := p.Scoring.KFactorEst, defaults.KFactorEst; got != want {
		t.Errorf("KFactorEst = %d, want %d", got, want)
	}
	if got, want := p.Scoring.NewThreshold, defaults.NewThreshold; got != want {
		t.Errorf("NewThreshold = %d, want %d", got, want)
	}
	if got, want := p.Scoring.PeriodWeeks, defaults.PeriodWeeks; got != want {
		t.Errorf("PeriodWeeks = %d, want %d", got, want)
	}
	if !reflect.DeepEqual(p.Scoring.Weights, defaults.Weights) {
		t.Errorf("Weights = %v, want %v", p.Scoring.Weights, defaults.Weights)
	}
}

func TestApplyDefaultsMergesPartialWeights(t *testing.T) {
	// User overrides one weight; everything else should still come from defaults.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := writeFile(t, path, `
[profiles.default]
name = "default"

  [profiles.default.scoring.weights]
  prs_merged = 5.0
`); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	p := cfg.ActiveProfile()
	if got := p.Scoring.Weights["prs_merged"]; got != 5.0 {
		t.Errorf("user override lost: prs_merged = %v, want 5.0", got)
	}
	if got := p.Scoring.Weights["commits"]; got != DefaultScoringConfig().Weights["commits"] {
		t.Errorf("default merge lost: commits = %v, want %v", got, DefaultScoringConfig().Weights["commits"])
	}
}

func TestMatchesBotPattern(t *testing.T) {
	cases := []struct {
		name     string
		login    string
		patterns []string
		want     bool
	}{
		{"suffix bot bracket", "dependabot[bot]", []string{"*[bot]"}, true},
		{"literal", "renovate", []string{"renovate"}, true},
		{"literal case insensitive", "Dependabot", []string{"dependabot"}, true},
		{"prefix wildcard", "claude-helper", []string{"claude*"}, true},
		{"contains wildcard", "my-build-bot-x", []string{"*bot*"}, true},
		{"no match", "mathewepstein", []string{"*[bot]", "dependabot"}, false},
		{"empty patterns", "x", []string{"", "  "}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchesBotPattern(tc.login, tc.patterns); got != tc.want {
				t.Errorf("MatchesBotPattern(%q, %v) = %v, want %v", tc.login, tc.patterns, got, tc.want)
			}
		})
	}
}

func TestEffectiveExcludesMergesDefaultsAndConfig(t *testing.T) {
	s := ScoringConfig{
		Exclude: []string{"my-bot", "dependabot"}, // dependabot duplicates default
	}
	got := s.EffectiveExcludes()

	// Defaults must come first, in order, deduped against user list.
	want := append([]string{}, DefaultBotExcludes...)
	want = append(want, "my-bot") // dependabot dropped as duplicate
	if !reflect.DeepEqual(got, want) {
		t.Errorf("EffectiveExcludes() = %v\nwant %v", got, want)
	}
}

// writeFile is a tiny helper that mirrors os.WriteFile with t.Helper plumbing.
func writeFile(t *testing.T, path, body string) error {
	t.Helper()
	return os.WriteFile(path, []byte(body), 0o600)
}

func TestSaveToWritesBackupOnOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	// Seed an existing file with a marker value we can identify in the backup.
	if err := writeFile(t, path, `
[profiles.default]
name = "default"
[profiles.default.jira]
base_url = "https://OLD.example/"
email = "old@x"
projects = ["X"]
[profiles.default.github]
username = "old"
orgs = ["x"]
`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	p := cfg.ActiveProfile()
	p.Jira.BaseURL = "https://NEW.example/"
	cfg.Profiles[DefaultProfile] = p

	if err := cfg.SaveTo(path); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}

	bak, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if !contains(string(bak), "OLD.example") {
		t.Errorf("backup does not contain OLD value — saw: %s", string(bak))
	}
	live, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read live: %v", err)
	}
	if !contains(string(live), "NEW.example") {
		t.Errorf("live config does not contain NEW value — saw: %s", string(live))
	}
}

func TestSaveToFirstWriteHasNoBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	cfg := &Config{Profiles: map[string]Profile{DefaultProfile: DefaultProfileConfig()}}
	if err := cfg.SaveTo(path); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Errorf("backup file present on first write: err=%v", err)
	}
}

func TestSaveToLeavesNoTempFilesOnSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	cfg := &Config{Profiles: map[string]Profile{DefaultProfile: DefaultProfileConfig()}}
	if err := cfg.SaveTo(path); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") || strings.Contains(e.Name(), ".velocity-config-") {
			t.Errorf("orphan tempfile left behind: %s", e.Name())
		}
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func TestApplyDefaultsMigratesSingularGitHubLogin(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := writeFile(t, path, `
[profiles.default]
name = "default"
[profiles.default.jira]
base_url = "https://x.atlassian.net"
email = "a@b"
projects = ["X"]
[profiles.default.github]
username = "x"
orgs = ["x"]
[[profiles.default.devs]]
github_login = "alice"
jira_account_id = "acct-alice"
display_name = "Alice"
[[profiles.default.devs]]
github_logins = ["bob", "bobby"]
jira_account_id = "acct-bob"
display_name = "Bob"
`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	p := cfg.ActiveProfile()
	if len(p.Devs) != 2 {
		t.Fatalf("want 2 devs, got %d", len(p.Devs))
	}
	// Singular field gets migrated into the plural slice and cleared.
	if p.Devs[0].GitHubLogin != "" {
		t.Errorf("alice: GitHubLogin should be cleared, got %q", p.Devs[0].GitHubLogin)
	}
	if !reflect.DeepEqual(p.Devs[0].GitHubLogins, []string{"alice"}) {
		t.Errorf("alice: GitHubLogins = %v, want [alice]", p.Devs[0].GitHubLogins)
	}
	// Already-plural entry is left alone.
	if !reflect.DeepEqual(p.Devs[1].GitHubLogins, []string{"bob", "bobby"}) {
		t.Errorf("bob: GitHubLogins = %v, want [bob bobby]", p.Devs[1].GitHubLogins)
	}
}

func TestMatchesGitHubLoginFallsBackToLegacyField(t *testing.T) {
	// In-memory construction that bypasses Load() should still match via the
	// legacy singular field — most existing tests rely on this.
	d := DevIdentity{GitHubLogin: "alice"}
	if !d.MatchesGitHubLogin("alice") {
		t.Errorf("legacy field should match")
	}
	if d.MatchesGitHubLogin("bob") {
		t.Errorf("non-claimant should not match")
	}
	if d.MatchesGitHubLogin("") {
		t.Errorf("empty input should never match")
	}
}

func TestAllGitHubLoginsPrefersPluralOverSingular(t *testing.T) {
	d := DevIdentity{
		GitHubLogin:  "legacy",                 // legacy should be ignored
		GitHubLogins: []string{"alice", "ali"}, // plural wins
	}
	got := d.AllGitHubLogins()
	want := []string{"alice", "ali"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AllGitHubLogins() = %v, want %v", got, want)
	}
}

func TestEffectiveRoleDefaultsToDev(t *testing.T) {
	if got := (DevIdentity{}).EffectiveRole(); got != "dev" {
		t.Errorf("empty role = %q, want dev", got)
	}
	if got := (DevIdentity{Role: " QA "}).EffectiveRole(); got != "qa" {
		t.Errorf("trim/lower role = %q, want qa", got)
	}
}

func TestRoleExcluded(t *testing.T) {
	ex := []string{"qa", "exec", "excluded"}
	for _, r := range []string{"qa", "QA", "exec", "excluded"} {
		if !RoleExcluded(r, ex) {
			t.Errorf("RoleExcluded(%q) = false, want true", r)
		}
	}
	for _, r := range []string{"dev", "lead", "devops", ""} {
		if RoleExcluded(r, ex) {
			t.Errorf("RoleExcluded(%q) = true, want false", r)
		}
	}
}

func TestApplyDefaultsFillsExcludedRoles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := writeFile(t, path, `
[profiles.default]
name = "default"

  [profiles.default.jira]
  base_url = "https://x.atlassian.net"
  email = "a@b"
`); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvPathOverride, path)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.ActiveProfile().Scoring.ExcludedRoles
	want := []string{"qa", "exec", "excluded"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExcludedRoles = %v, want %v", got, want)
	}
}
