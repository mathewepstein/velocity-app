package secrets

import (
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"
)

// Namespace is the prefix for every keychain entry velocity writes.
// Keeping it as a package constant so tests (or future rename work) can
// reference it instead of duplicating the literal.
const Namespace = "velocity"

// keychainUser is the fixed "account" field in the keychain entry. go-keyring
// requires a (service, user) pair; we only need the service dimension, so this
// is a constant rather than something the caller plumbs through.
const keychainUser = "token"

// KnownServices is the closed set of service identifiers v1 supports.
// Enforced by the CLI so typos surface immediately rather than silently
// creating a keychain entry nobody reads.
var KnownServices = []string{"jira", "github"}

// ErrNotFound is returned by Get when the keychain has no entry for the key.
// Callers use errors.Is to branch on it (so we don't leak go-keyring types).
var ErrNotFound = errors.New("token not found in keychain")

// Key returns the keychain service string for a (profile, service) pair,
// e.g. Key("default", "jira") → "velocity:default:jira".
func Key(profile, service string) string {
	return fmt.Sprintf("%s:%s:%s", Namespace, profile, service)
}

// Set stores a token under (profile, service). Overwrites any existing entry.
func Set(profile, service, token string) error {
	if profile == "" || service == "" {
		return fmt.Errorf("profile and service required")
	}
	if token == "" {
		return fmt.Errorf("token is empty")
	}
	if err := keyring.Set(Key(profile, service), keychainUser, token); err != nil {
		return fmt.Errorf("store token in keychain: %w", err)
	}
	return nil
}

// Get retrieves a token for (profile, service). Returns ErrNotFound if
// nothing is stored.
func Get(profile, service string) (string, error) {
	tok, err := keyring.Get(Key(profile, service), keychainUser)
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("read token from keychain: %w", err)
	}
	return tok, nil
}

// Delete removes a token for (profile, service). Returns ErrNotFound if
// nothing is stored (treat as idempotent at the CLI layer).
func Delete(profile, service string) error {
	err := keyring.Delete(Key(profile, service), keychainUser)
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return ErrNotFound
		}
		return fmt.Errorf("delete token from keychain: %w", err)
	}
	return nil
}

// Has reports whether a token is stored for (profile, service) without
// returning the token value.
func Has(profile, service string) (bool, error) {
	_, err := Get(profile, service)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return false, err
}

// IsKnownService reports whether name is one of KnownServices.
func IsKnownService(name string) bool {
	for _, s := range KnownServices {
		if s == name {
			return true
		}
	}
	return false
}
