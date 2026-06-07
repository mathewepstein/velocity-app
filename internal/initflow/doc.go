// Package initflow drives the interactive `velocity init` experience:
// prompt for each field, validate by hitting the Jira + GitHub APIs,
// auto-discover instance-specific Jira field IDs, write the resolved
// config to disk, and store tokens in the keychain.
package initflow
