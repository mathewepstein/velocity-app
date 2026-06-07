// Package pull contains the per-month pullers for Jira (REST v3) and GitHub
// (REST, PRs + commits). Each puller accepts a single month, handles
// pagination and rate-limit backoff, and writes results to the cache.
package pull
