package cache

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// SQLiteFile is the database filename at the cache root.
const SQLiteFile = "velocity.db"

// sqliteStore is the SQLite-backed Store (architecture-evolution Step 1b). It
// holds the corpus in a relational mirror of the records.go structs so the
// Step 2 query layer can window/filter on indexed columns, while reads
// reconstruct records that deep-equal the JSON store's so metrics.json stays
// byte-identical (the parity gate). Single-writer (cron) + read-many, so SQLite
// in-process is the right fit; the pure-Go modernc driver keeps the single
// binary (no CGo).
type sqliteStore struct {
	db   *sql.DB
	path string
}

var _ Store = (*sqliteStore)(nil)

// SQLitePath resolves the database path: the argument verbatim if non-empty,
// else Root()/velocity.db.
func SQLitePath(path string) (string, error) {
	if path != "" {
		return path, nil
	}
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, SQLiteFile), nil
}

// openSQLiteStore opens (creating if absent) the SQLite Store at path, applying
// the schema. Empty path → Root()/velocity.db.
func openSQLiteStore(path string) (Store, error) {
	dbPath, err := SQLitePath(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	// busy_timeout rides out the rare concurrent backfill lane; the corpus is
	// single-writer so contention is minimal. WAL + NORMAL sync make the
	// migration's bulk insert fast without risking torn writes on a clean stop.
	dsn := "file:" + dbPath + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(off)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", dbPath, err)
	}
	// modernc/database-sql can hand out multiple conns; a single writer keeps
	// WAL well-behaved and avoids "database is locked" under our access pattern.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(sqliteSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &sqliteStore{db: db, path: dbPath}, nil
}

func (s *sqliteStore) Close() error { return s.db.Close() }

// --- time helpers -----------------------------------------------------------

// RFC3339Nano round-trips a time.Time's instant + offset so a re-marshal
// (metrics.json) is byte-identical to the JSON store's.
func fmtTime(t time.Time) string { return t.Format(time.RFC3339Nano) }

// fmtTimePtr renders a *time.Time as a SQL argument: nil → NULL.
func fmtTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Format(time.RFC3339Nano)
}

func parseTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, s)
}

func parseTimePtr(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid {
		return nil, nil
	}
	t, err := parseTime(ns.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// boolToInt / intToBool bridge Go bools and SQLite's 0/1 INTEGER.
func boolToInt(b bool) int { return map[bool]int{true: 1, false: 0}[b] }

// --- manifest ----------------------------------------------------------------

func (s *sqliteStore) LoadManifest() (*Manifest, error) {
	rows, err := s.db.Query(`SELECT source, scope, month, pulled_at, records FROM manifest`)
	if err != nil {
		return nil, fmt.Errorf("query manifest: %w", err)
	}
	defer rows.Close()
	m := NewManifest()
	for rows.Next() {
		var e ManifestEntry
		var src, pulled string
		if err := rows.Scan(&src, &e.Scope, &e.Month, &pulled, &e.Records); err != nil {
			return nil, fmt.Errorf("scan manifest: %w", err)
		}
		e.Source = Source(src)
		t, err := parseTime(pulled)
		if err != nil {
			return nil, fmt.Errorf("parse manifest pulled_at %q: %w", pulled, err)
		}
		e.PulledAt = t
		m.Entries[entryKey(e.Source, e.Scope, mustMonth(e.Month))] = e
	}
	return m, rows.Err()
}

// mustMonth parses a manifest month label, falling back to a zero Month on bad
// input (the JSON manifest tolerates malformed labels by skipping; the key here
// just needs to be stable). Manifest months are always well-formed in practice.
func mustMonth(s string) Month {
	mo, err := ParseMonth(s)
	if err != nil {
		return Month{}
	}
	return mo
}

func (s *sqliteStore) SaveManifest(m *Manifest) error {
	if m == nil {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM manifest`); err != nil {
		return fmt.Errorf("clear manifest: %w", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO manifest (source, scope, month, pulled_at, records) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, e := range m.Entries {
		if _, err := stmt.Exec(string(e.Source), e.Scope, e.Month, fmtTime(e.PulledAt), e.Records); err != nil {
			return fmt.Errorf("insert manifest entry: %w", err)
		}
	}
	return tx.Commit()
}

// cellExists reports whether (source, scope, month) was pulled — the manifest
// entry is the source of truth, mirroring the JSON store where a present file
// (even "[]") means pulled and a missing file means never-pulled.
func (s *sqliteStore) cellExists(source Source, scope string, m Month) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM manifest WHERE source=? AND scope=? AND month=?`,
		string(source), scope, m.String()).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// missErr is the cache-miss error a never-pulled cell returns, wrapping
// fs.ErrNotExist so callers' errors.Is checks match the JSON store.
func missErr(source Source, scope string, m Month) error {
	return fmt.Errorf("cache miss %s/%s/%s: %w", source, scope, m, fs.ErrNotExist)
}

// --- write helpers -----------------------------------------------------------

// beginCellWrite opens a txn and clears (scope, month) from the parent table
// and every listed child table — the wholesale-rewrite contract.
func (s *sqliteStore) beginCellWrite(scope string, m Month, tables ...string) (*sql.Tx, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	for _, tbl := range tables {
		if _, err := tx.Exec(`DELETE FROM `+tbl+` WHERE scope=? AND month=?`, scope, m.String()); err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("clear %s: %w", tbl, err)
		}
	}
	return tx, nil
}

// Reset truncates every record + manifest table and removes metrics.json,
// mirroring the JSON store's Reset (which wipes the source dirs + manifest +
// metrics file). ratings.json lives outside the store and survives, by design.
func (s *sqliteStore) Reset(out io.Writer) ([]string, error) {
	var removed []string
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	for _, tbl := range allRecordTables {
		if _, err := tx.Exec(`DELETE FROM ` + tbl); err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("truncate %s: %w", tbl, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	removed = append(removed, s.path+" (tables truncated)")
	if out != nil {
		fmt.Fprintf(out, "  truncated all tables in %s\n", s.path)
	}

	// Drop metrics.json so a stale serving blob doesn't outlive the data.
	if metrics, err := MetricsPath(); err == nil {
		if rmErr := os.Remove(metrics); rmErr == nil {
			removed = append(removed, metrics)
			if out != nil {
				fmt.Fprintf(out, "  removed %s\n", metrics)
			}
		} else if !errors.Is(rmErr, fs.ErrNotExist) {
			return removed, fmt.Errorf("remove %s: %w", metrics, rmErr)
		}
	}
	return removed, nil
}
