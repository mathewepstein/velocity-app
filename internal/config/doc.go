// Package config loads and saves the velocity TOML config file, resolves the
// XDG path (honoring $VELOCITY_CONFIG as an override), applies defaults, and
// validates schema correctness. Secrets are NOT stored here — see internal/secrets.
package config
