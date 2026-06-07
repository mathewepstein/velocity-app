// Package analyze reads the local cache and produces metrics.json consumed
// by the web UI. It computes the current-window view, comparison slices
// (prior / YoY / QoQ), a full-history trend, and detects project surges by
// grouping activity per Jira epic and applying configurable thresholds.
package analyze
