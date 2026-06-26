package scoring

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"

	_ "modernc.org/sqlite"
)

// ScorerID identifies velocity's own deterministic band scorer. Results are
// keyed by (ticket, scorer) so a teammate's scorer can store its own rows for
// the same ticket without collision (plan S4).
const ScorerID = "velocity-band-v1"

// Source values for a ScoreRecord.
const (
	SourceAuto  = "auto"  // the deterministic band, written by the generator
	SourceHuman = "human" // a human override (e.g. after an LLM insight pass)
)

// ScoreRecord is one persisted score for a (ticket, scorer) pair. Points is the
// final value (the deterministic band, or a human override); the band-engine
// derivations (Band/Confidence/Drivers/…) always describe the deterministic
// computation, so the UI can show "your override: 5 (auto band was 8)".
type ScoreRecord struct {
	Ticket              string  `json:"ticket"`
	Scorer              string  `json:"scorer"`
	Points              int     `json:"points"`
	Source              string  `json:"source"` // auto | human
	AutoPoints          int     `json:"auto_points"`
	ExistingStoryPoints float64 `json:"existing_story_points"` // SP already on the Jira ticket (0 = unset)
	// CreatedAt/ResolvedAt are the ticket's Jira dates, captured at score time
	// so the SP page can filter by a date window client-side (Phase 6). Immutable
	// facts about the ticket, so freezing them at score time is correct. ResolvedAt
	// is nil for an open (unresolved) ticket; CreatedAt is zero only on rows scored
	// before the columns existed (treated as "undated" — never filtered out).
	CreatedAt     time.Time  `json:"created_at,omitempty"`
	ResolvedAt    *time.Time `json:"resolved_at,omitempty"`
	Band          string     `json:"band"`
	Confidence    string     `json:"confidence"`
	NeedsInsight  bool       `json:"needs_insight"`
	QuadrantCell  string     `json:"quadrant_cell"`
	Drivers       []string   `json:"drivers"`
	SignalSummary string     `json:"signal_summary"`
	HardestAspect string     `json:"hardest_aspect"`
	EvidenceHash  string     `json:"evidence_hash"`
	ScoredAt      time.Time  `json:"scored_at"`
	PostedToJira  bool       `json:"posted_to_jira"`
	JiraPostedAt  *time.Time `json:"jira_posted_at,omitempty"`
	// Disciplines is the set of FE/BE/DevOps disciplines the ticket's Jira labels
	// place it in (empty/omitted == untagged). Populated only on the List path by
	// reading the co-resident jira_labels table — it is not a stored column and is
	// not set by Get/SaveAuto. Drives the scoring page's discipline filter.
	Disciplines []Discipline `json:"disciplines,omitempty"`
}

// NewAutoRecord builds an auto-source ScoreRecord from a ticket's evidence and
// its deterministic band, stamping scored_at and the evidence hash.
func NewAutoRecord(ev *TicketEvidence, b BandResult, at time.Time) ScoreRecord {
	return ScoreRecord{
		Ticket:              ev.Key,
		Scorer:              ScorerID,
		Points:              b.Points,
		Source:              SourceAuto,
		AutoPoints:          b.Points,
		ExistingStoryPoints: ev.ExistingStoryPoints,
		CreatedAt:           ev.Created,
		ResolvedAt:          ev.Resolved,
		Band:                b.Band,
		Confidence:          b.Confidence,
		NeedsInsight:        b.NeedsInsight,
		QuadrantCell:        b.QuadrantCell,
		Drivers:             b.Drivers,
		SignalSummary:       b.SignalSummary,
		HardestAspect:       b.HardestAspectHint,
		EvidenceHash:        EvidenceHash(ev),
		ScoredAt:            at,
	}
}

// ScoreFilter narrows a List query.
type ScoreFilter struct {
	NeedsInsightOnly bool
	Scorer           string // "" = any
	Limit            int    // 0 = no limit
}

// ScoreStore persists ScoreRecords in a `scores` table inside the shared
// velocity.db. It opens its own handle to the database so the corpus Store
// interface (the closed 4-source + manifest seam) stays untouched; the table is
// created idempotently on open. Concurrency-safe (WAL + busy_timeout) so the
// daily generator and the live server can both write.
type ScoreStore struct {
	db   *sql.DB
	path string
}

const scoreSchema = `
CREATE TABLE IF NOT EXISTS scores (
	ticket          TEXT    NOT NULL,
	scorer          TEXT    NOT NULL,
	points          INTEGER NOT NULL,
	source          TEXT    NOT NULL,
	auto_points     INTEGER NOT NULL,
	existing_story_points REAL NOT NULL DEFAULT 0,
	created_at      TEXT,
	resolved_at     TEXT,
	band            TEXT,
	confidence      TEXT,
	needs_insight   INTEGER NOT NULL DEFAULT 0,
	quadrant_cell   TEXT,
	drivers         TEXT,
	signal_summary  TEXT,
	hardest_aspect  TEXT,
	evidence_hash   TEXT,
	scored_at       TEXT    NOT NULL,
	posted_to_jira  INTEGER NOT NULL DEFAULT 0,
	jira_posted_at  TEXT,
	PRIMARY KEY (ticket, scorer)
);
CREATE INDEX IF NOT EXISTS idx_scores_needs_insight ON scores(needs_insight);
`

// OpenScoreStore opens (creating if absent) the scores table in the database at
// path; empty path resolves to the standard velocity.db. Callers must Close it.
func OpenScoreStore(path string) (*ScoreStore, error) {
	dbPath, err := cache.SQLitePath(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	dsn := "file:" + dbPath + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(off)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", dbPath, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(scoreSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply scores schema: %w", err)
	}
	// Forward-migrate a pre-existing scores table to the current column set
	// (CREATE … IF NOT EXISTS leaves an old table's columns untouched). The
	// scores table is a derived artifact, but rows survive across `score
	// generate` runs, so additive columns need an ALTER for older DBs.
	if err := ensureScoreColumns(db); err != nil {
		db.Close()
		return nil, err
	}
	return &ScoreStore{db: db, path: dbPath}, nil
}

// ensureScoreColumns adds any columns missing from an older scores table. It is
// idempotent: it reads the live column set via PRAGMA and only ALTERs what is
// absent, so fresh DBs (which already have every column from scoreSchema) are
// a no-op.
func ensureScoreColumns(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(scores)`)
	if err != nil {
		return fmt.Errorf("inspect scores columns: %w", err)
	}
	have := map[string]bool{}
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return fmt.Errorf("scan scores column: %w", err)
		}
		have[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// canonical → DDL for the additive (nullable) columns.
	additive := []struct{ name, ddl string }{
		{"created_at", `ALTER TABLE scores ADD COLUMN created_at TEXT`},
		{"resolved_at", `ALTER TABLE scores ADD COLUMN resolved_at TEXT`},
	}
	for _, c := range additive {
		if have[c.name] {
			continue
		}
		if _, err := db.Exec(c.ddl); err != nil {
			return fmt.Errorf("add scores column %s: %w", c.name, err)
		}
	}
	return nil
}

func (s *ScoreStore) Close() error { return s.db.Close() }

// SaveOutcome reports what SaveAuto did to a row.
type SaveOutcome string

const (
	OutcomeInserted  SaveOutcome = "inserted"  // new auto row
	OutcomeUpdated   SaveOutcome = "updated"   // existing auto row, evidence changed
	OutcomeSkipped   SaveOutcome = "skipped"   // existing auto row, evidence unchanged (idempotent)
	OutcomePreserved SaveOutcome = "preserved" // existing human override kept; auto columns refreshed
)

// SaveAuto upserts the deterministic band for a ticket while never discarding a
// human override. For an existing human row it refreshes only the band-engine
// columns (so the UI sees the current auto band beside the override) and keeps
// points/source/posted state. For an auto row it skips the write when the
// evidence hash is unchanged, making the generator idempotent.
func (s *ScoreStore) SaveAuto(rec ScoreRecord) (SaveOutcome, error) {
	rec.Scorer = orDefault(rec.Scorer, ScorerID)
	rec.Source = SourceAuto
	rec.AutoPoints = rec.Points

	existing, ok, err := s.Get(rec.Ticket, rec.Scorer)
	if err != nil {
		return "", err
	}
	switch {
	case !ok:
		if err := s.upsert(rec); err != nil {
			return "", err
		}
		return OutcomeInserted, nil
	case existing.Source == SourceHuman:
		// Keep the human decision; refresh the auto-derived view + hash.
		merged := *existing
		merged.AutoPoints = rec.Points
		merged.Band = rec.Band
		merged.Confidence = rec.Confidence
		merged.NeedsInsight = rec.NeedsInsight
		merged.QuadrantCell = rec.QuadrantCell
		merged.Drivers = rec.Drivers
		merged.SignalSummary = rec.SignalSummary
		merged.HardestAspect = rec.HardestAspect
		merged.EvidenceHash = rec.EvidenceHash
		if err := s.upsert(merged); err != nil {
			return "", err
		}
		return OutcomePreserved, nil
	case existing.EvidenceHash == rec.EvidenceHash && rec.EvidenceHash != "":
		// Evidence unchanged → skip the re-score, but patch the ticket dates if
		// the row predates the created_at/resolved_at columns (Phase 6). Dates
		// are immutable facts independent of the evidence hash, so this lets a
		// plain `score generate` backfill them without a forced re-score.
		if existing.CreatedAt.IsZero() && !rec.CreatedAt.IsZero() {
			if err := s.patchDates(rec.Ticket, rec.Scorer, rec.CreatedAt, rec.ResolvedAt); err != nil {
				return "", err
			}
		}
		return OutcomeSkipped, nil
	default:
		// Preserve any prior posted state on the auto row across recompute.
		rec.PostedToJira = existing.PostedToJira
		rec.JiraPostedAt = existing.JiraPostedAt
		if err := s.upsert(rec); err != nil {
			return "", err
		}
		return OutcomeUpdated, nil
	}
}

// SetHumanOverride records a human's final points for a ticket, preserving the
// deterministic band columns for audit/calibration. Used by the approve-in-UI
// path (Phase 4/5). autoPoints is the deterministic band at decision time.
func (s *ScoreStore) SetHumanOverride(ticket, scorer string, points, autoPoints int, at time.Time) error {
	scorer = orDefault(scorer, ScorerID)
	existing, ok, err := s.Get(ticket, scorer)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no score row for %s/%s to override", ticket, scorer)
	}
	existing.Points = points
	existing.AutoPoints = autoPoints
	existing.Source = SourceHuman
	existing.ScoredAt = at
	return s.upsert(*existing)
}

// patchDates updates only the ticket-date columns on an existing row, leaving
// the score + posted state untouched. Used to backfill dates onto rows scored
// before the Phase-6 columns existed.
func (s *ScoreStore) patchDates(ticket, scorer string, created time.Time, resolved *time.Time) error {
	_, err := s.db.Exec(
		`UPDATE scores SET created_at=?, resolved_at=? WHERE ticket=? AND scorer=?`,
		fmtZeroableTime(created), fmtTimePtr(resolved), ticket, scorer)
	return err
}

// MarkPosted records that the ticket's score was written to Jira.
func (s *ScoreStore) MarkPosted(ticket, scorer string, at time.Time) error {
	scorer = orDefault(scorer, ScorerID)
	res, err := s.db.Exec(
		`UPDATE scores SET posted_to_jira=1, jira_posted_at=? WHERE ticket=? AND scorer=?`,
		fmtTimePtr(&at), ticket, scorer)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no score row for %s/%s to mark posted", ticket, scorer)
	}
	return nil
}

// Get returns one record. ok is false when absent.
func (s *ScoreStore) Get(ticket, scorer string) (*ScoreRecord, bool, error) {
	scorer = orDefault(scorer, ScorerID)
	row := s.db.QueryRow(selectCols+` WHERE ticket=? AND scorer=?`, ticket, scorer)
	rec, err := scanScore(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return rec, true, nil
}

// List returns records matching filter, ordered needs-insight first then ticket.
func (s *ScoreStore) List(f ScoreFilter) ([]ScoreRecord, error) {
	q := selectCols + ` WHERE 1=1`
	var args []any
	if f.NeedsInsightOnly {
		q += ` AND needs_insight=1`
	}
	if f.Scorer != "" {
		q += ` AND scorer=?`
		args = append(args, f.Scorer)
	}
	q += ` ORDER BY needs_insight DESC, ticket ASC`
	if f.Limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, f.Limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScoreRecord
	for rows.Next() {
		rec, err := scanScore(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.attachDisciplines(out); err != nil {
		return nil, err
	}
	return out, nil
}

// attachDisciplines decorates each record with the FE/BE/DevOps disciplines its
// ticket is labeled with (empty == untagged), via a single read of the
// co-resident jira_labels table. [Disciplines] is authoritative for the
// label→discipline mapping; the SQL prefilter is built from [DisciplineLabelKeys]
// so the two can't drift. If the database has no jira_labels table (a scores-only
// DB with no corpus), every row is left untagged rather than erroring.
func (s *ScoreStore) attachDisciplines(recs []ScoreRecord) error {
	if len(recs) == 0 {
		return nil
	}
	var hasTable int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='jira_labels'`,
	).Scan(&hasTable); err != nil {
		return err
	}
	if hasTable == 0 {
		return nil
	}
	keys := DisciplineLabelKeys()
	args := make([]any, len(keys))
	for i, k := range keys {
		args[i] = k
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	// DISTINCT collapses the (scope, month) cell duplication in jira_labels;
	// the lower(value) IN (…) prefilter narrows to discipline-bearing labels only.
	q := `SELECT DISTINCT issue_key, value FROM jira_labels WHERE lower(value) IN (` + placeholders + `)`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	labelsByKey := make(map[string][]string)
	for rows.Next() {
		var key, val string
		if err := rows.Scan(&key, &val); err != nil {
			return err
		}
		labelsByKey[key] = append(labelsByKey[key], val)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range recs {
		recs[i].Disciplines = Disciplines(labelsByKey[recs[i].Ticket])
	}
	return nil
}

const selectCols = `SELECT ticket, scorer, points, source, auto_points, existing_story_points,
	created_at, resolved_at,
	band, confidence, needs_insight, quadrant_cell, drivers, signal_summary, hardest_aspect,
	evidence_hash, scored_at, posted_to_jira, jira_posted_at FROM scores`

func (s *ScoreStore) upsert(rec ScoreRecord) error {
	drivers, _ := json.Marshal(rec.Drivers)
	_, err := s.db.Exec(`
INSERT INTO scores (ticket, scorer, points, source, auto_points, existing_story_points,
	created_at, resolved_at,
	band, confidence, needs_insight, quadrant_cell, drivers, signal_summary, hardest_aspect,
	evidence_hash, scored_at, posted_to_jira, jira_posted_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(ticket, scorer) DO UPDATE SET
	points=excluded.points, source=excluded.source, auto_points=excluded.auto_points,
	existing_story_points=excluded.existing_story_points,
	created_at=excluded.created_at, resolved_at=excluded.resolved_at,
	band=excluded.band, confidence=excluded.confidence, needs_insight=excluded.needs_insight,
	quadrant_cell=excluded.quadrant_cell, drivers=excluded.drivers,
	signal_summary=excluded.signal_summary, hardest_aspect=excluded.hardest_aspect,
	evidence_hash=excluded.evidence_hash, scored_at=excluded.scored_at,
	posted_to_jira=excluded.posted_to_jira, jira_posted_at=excluded.jira_posted_at`,
		rec.Ticket, rec.Scorer, rec.Points, rec.Source, rec.AutoPoints, rec.ExistingStoryPoints,
		fmtZeroableTime(rec.CreatedAt), fmtTimePtr(rec.ResolvedAt),
		rec.Band, rec.Confidence,
		boolToInt(rec.NeedsInsight), rec.QuadrantCell, string(drivers), rec.SignalSummary, rec.HardestAspect,
		rec.EvidenceHash, fmtTime(rec.ScoredAt), boolToInt(rec.PostedToJira), fmtTimePtr(rec.JiraPostedAt))
	return err
}

// scanner abstracts *sql.Row and *sql.Rows for scanScore.
type scanner interface {
	Scan(dest ...any) error
}

func scanScore(sc scanner) (*ScoreRecord, error) {
	var (
		rec        ScoreRecord
		needs      int
		posted     int
		drivers    sql.NullString
		createdAt  sql.NullString
		resolvedAt sql.NullString
		scoredAt   string
		postedAt   sql.NullString
	)
	err := sc.Scan(&rec.Ticket, &rec.Scorer, &rec.Points, &rec.Source, &rec.AutoPoints,
		&rec.ExistingStoryPoints, &createdAt, &resolvedAt, &rec.Band, &rec.Confidence, &needs,
		&rec.QuadrantCell, &drivers, &rec.SignalSummary, &rec.HardestAspect, &rec.EvidenceHash,
		&scoredAt, &posted, &postedAt)
	if err != nil {
		return nil, err
	}
	rec.NeedsInsight = needs != 0
	rec.PostedToJira = posted != 0
	if drivers.Valid && drivers.String != "" {
		_ = json.Unmarshal([]byte(drivers.String), &rec.Drivers)
	}
	if createdAt.Valid {
		if t, err := parseTime(createdAt.String); err == nil {
			rec.CreatedAt = t
		}
	}
	if resolvedAt.Valid {
		if t, err := parseTime(resolvedAt.String); err == nil {
			rec.ResolvedAt = &t
		}
	}
	if t, err := parseTime(scoredAt); err == nil {
		rec.ScoredAt = t
	}
	if postedAt.Valid {
		if t, err := parseTime(postedAt.String); err == nil {
			rec.JiraPostedAt = &t
		}
	}
	return &rec, nil
}

// --- local sqlite helpers (mirrors internal/cache; kept local so ScoreStore
// owns its persistence without exporting cache internals). ---

func fmtTime(t time.Time) string { return t.Format(time.RFC3339Nano) }

func fmtTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Format(time.RFC3339Nano)
}

// fmtZeroableTime stores a NULL for the zero time (an unset/unknown date) and
// the RFC3339Nano text otherwise — so a row scored before a date was known
// round-trips back to the zero value rather than a bogus 0001-01-01 string.
func fmtZeroableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Format(time.RFC3339Nano)
}

func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
