// Package secrets wraps go-keyring for storing and retrieving API tokens.
// Tokens are keyed as "velocity:{profile}:{service}" (e.g. "velocity:default:jira").
// macOS only for v1; Linux/Windows support is a future expansion.
package secrets
