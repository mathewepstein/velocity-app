package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// EnvPathOverride is the environment variable that, if set, is used verbatim
// as the config file path (bypassing XDG resolution).
const EnvPathOverride = "VELOCITY_CONFIG"

// AppDirName is the per-user subdirectory velocity owns under $XDG_CONFIG_HOME
// and $XDG_DATA_HOME.
const AppDirName = "velocity"

// ConfigFileName is the standard filename within the velocity config dir.
const ConfigFileName = "config.toml"

// Path returns the resolved absolute path to the config file.
//
// Resolution order:
//  1. $VELOCITY_CONFIG if set (used verbatim; may point anywhere).
//  2. $XDG_CONFIG_HOME/velocity/config.toml if XDG_CONFIG_HOME is set.
//  3. ~/.config/velocity/config.toml otherwise.
//
// The file itself is not required to exist. Callers that need to create it
// should call EnsureDir first.
func Path() (string, error) {
	if override := os.Getenv(EnvPathOverride); override != "" {
		abs, err := filepath.Abs(override)
		if err != nil {
			return "", fmt.Errorf("resolve %s=%q: %w", EnvPathOverride, override, err)
		}
		return abs, nil
	}

	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ConfigFileName), nil
}

// ConfigDir returns the velocity config directory (parent of the config file).
// Honors $XDG_CONFIG_HOME; falls back to ~/.config/velocity.
func ConfigDir() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, AppDirName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determine home directory: %w", err)
	}
	return filepath.Join(home, ".config", AppDirName), nil
}

// DataDir returns the velocity data directory, used for the cache.
// Honors $XDG_DATA_HOME; falls back to ~/.local/share/velocity.
// Not used by this package directly but lives here so all path logic is
// co-located.
func DataDir() (string, error) {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, AppDirName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determine home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", AppDirName), nil
}

// EnsureDir creates the parent directory of path with 0o700 perms if missing.
// 0o700 because the config may eventually hold semi-sensitive values (email,
// account IDs) even though tokens themselves live in the keychain.
func EnsureDir(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o700)
}
