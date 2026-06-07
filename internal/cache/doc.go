// Package cache manages the month-partitioned JSON cache of Jira + GitHub
// activity and a manifest that tracks when each (source, scope, month) was
// last pulled. Storage lives under $XDG_DATA_HOME/velocity (or ~/Library/
// Application Support/velocity on macOS).
//
// Freshness rules:
//   - Months before the current month are frozen (never re-pulled unless --force).
//   - The current month is always re-pulled.
//   - The last closed month is re-pulled during days 1–7 of the next month
//     to catch late resolutions.
package cache
