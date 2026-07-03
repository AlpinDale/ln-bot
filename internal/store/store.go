// Package store persists releases and scrape-run history in SQLite.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/alpindale/ln-bot/internal/model"
)

const schema = `
CREATE TABLE IF NOT EXISTS schema_version (
	version INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS releases (
	id               INTEGER PRIMARY KEY AUTOINCREMENT,
	source_key       TEXT NOT NULL,
	publisher        TEXT NOT NULL,
	series_title     TEXT NOT NULL,
	volume_title     TEXT NOT NULL,
	normalized_title TEXT NOT NULL,
	format           TEXT NOT NULL,
	release_date     TEXT NOT NULL, -- YYYY-MM-DD
	url              TEXT NOT NULL DEFAULT '',
	cover_url        TEXT NOT NULL DEFAULT '',
	first_seen_at    TEXT NOT NULL,
	updated_at       TEXT NOT NULL,
	alerted_at       TEXT,
	UNIQUE (source_key, normalized_title, format)
);

CREATE INDEX IF NOT EXISTS idx_releases_release_date ON releases (release_date);

CREATE TABLE IF NOT EXISTS scrape_runs (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	source_key  TEXT NOT NULL,
	started_at  TEXT NOT NULL,
	finished_at TEXT NOT NULL,
	status      TEXT NOT NULL, -- ok | error
	error       TEXT NOT NULL DEFAULT '',
	count       INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_scrape_runs_source ON scrape_runs (source_key, id);
`

const dateLayout = "2006-01-02"

// ScrapeRun records one fetch attempt for one source.
type ScrapeRun struct {
	SourceKey  string
	StartedAt  time.Time
	FinishedAt time.Time
	Status     string
	Error      string
	Count      int
}

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if necessary) the database at path and ensures the
// schema exists.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// modernc sqlite is not safe for concurrent writers on one connection
	// pool without care; the bot is low-traffic, so serialize access.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := ensureVersion(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func ensureVersion(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_version`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		_, err := db.Exec(`INSERT INTO schema_version (version) VALUES (1)`)
		return err
	}
	return nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// UpsertRelease inserts the release or, when a row with the same natural
// key (source, normalized title, format) exists, updates its mutable
// fields (including release_date — dates slip). first_seen_at and
// alerted_at are preserved. Returns true when a new row was inserted.
func (s *Store) UpsertRelease(ctx context.Context, r model.Release, now time.Time) (bool, error) {
	norm := model.NormalizedTitle(r.VolumeTitle)
	nowStr := now.UTC().Format(time.RFC3339Nano)
	// RETURNING first_seen_at distinguishes insert (== nowStr) from
	// update (retains the original value).
	var firstSeen string
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO releases
			(source_key, publisher, series_title, volume_title, normalized_title,
			 format, release_date, url, cover_url, first_seen_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (source_key, normalized_title, format) DO UPDATE SET
			publisher    = excluded.publisher,
			series_title = excluded.series_title,
			volume_title = excluded.volume_title,
			release_date = excluded.release_date,
			url          = excluded.url,
			cover_url    = excluded.cover_url,
			updated_at   = excluded.updated_at
		RETURNING first_seen_at`,
		r.SourceKey, r.Publisher, r.SeriesTitle, r.VolumeTitle, norm,
		r.Format, model.DateOnly(r.ReleaseDate).Format(dateLayout),
		r.URL, r.CoverURL, nowStr, nowStr,
	).Scan(&firstSeen)
	if err != nil {
		return false, fmt.Errorf("upsert release: %w", err)
	}
	return firstSeen == nowStr, nil
}

// UnalertedInWindow returns releases with release_date in [from, to]
// (inclusive, date-only) that have not been alerted yet.
func (s *Store) UnalertedInWindow(ctx context.Context, from, to time.Time) ([]model.Release, error) {
	return s.queryReleases(ctx, `
		WHERE release_date >= ? AND release_date <= ? AND alerted_at IS NULL
		ORDER BY release_date, publisher, volume_title`,
		model.DateOnly(from).Format(dateLayout), model.DateOnly(to).Format(dateLayout))
}

// UnpostedReleases returns every release not yet posted to the channel
// (alerted_at IS NULL), in chronological release order — the backlog the
// archive command drains.
func (s *Store) UnpostedReleases(ctx context.Context) ([]model.Release, error) {
	return s.queryReleases(ctx,
		`WHERE alerted_at IS NULL ORDER BY release_date, publisher, volume_title`)
}

// MarkAlerted stamps a release as announced.
func (s *Store) MarkAlerted(ctx context.Context, id int64, when time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE releases SET alerted_at = ? WHERE id = ?`,
		when.UTC().Format(time.RFC3339), id)
	return err
}

// ReleasesBetween returns releases with release_date in [from, to]
// (inclusive). publisher, when non-empty, filters case-insensitively by
// substring match.
func (s *Store) ReleasesBetween(ctx context.Context, from, to time.Time, publisher string) ([]model.Release, error) {
	q := `WHERE release_date >= ? AND release_date <= ?`
	args := []any{
		model.DateOnly(from).Format(dateLayout),
		model.DateOnly(to).Format(dateLayout),
	}
	if publisher != "" {
		q += ` AND LOWER(publisher) LIKE ?`
		args = append(args, "%"+strings.ToLower(publisher)+"%")
	}
	q += ` ORDER BY release_date, publisher, volume_title`
	return s.queryReleases(ctx, q, args...)
}

func (s *Store) queryReleases(ctx context.Context, whereOrder string, args ...any) ([]model.Release, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, source_key, publisher, series_title, volume_title, format,
		       release_date, url, cover_url, first_seen_at, updated_at, alerted_at
		FROM releases `+whereOrder, args...)
	if err != nil {
		return nil, fmt.Errorf("query releases: %w", err)
	}
	defer rows.Close()

	var out []model.Release
	for rows.Next() {
		var (
			r                             model.Release
			dateStr, firstSeen, updatedAt string
			alertedAt                     sql.NullString
		)
		if err := rows.Scan(&r.ID, &r.SourceKey, &r.Publisher, &r.SeriesTitle,
			&r.VolumeTitle, &r.Format, &dateStr, &r.URL, &r.CoverURL,
			&firstSeen, &updatedAt, &alertedAt); err != nil {
			return nil, err
		}
		if r.ReleaseDate, err = time.ParseInLocation(dateLayout, dateStr, time.UTC); err != nil {
			return nil, fmt.Errorf("bad release_date %q: %w", dateStr, err)
		}
		r.FirstSeenAt, _ = time.Parse(time.RFC3339, firstSeen)
		r.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
		if alertedAt.Valid {
			t, err := time.Parse(time.RFC3339, alertedAt.String)
			if err == nil {
				r.AlertedAt = &t
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecordScrapeRun appends a scrape-run row.
func (s *Store) RecordScrapeRun(ctx context.Context, run ScrapeRun) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO scrape_runs (source_key, started_at, finished_at, status, error, count)
		VALUES (?, ?, ?, ?, ?, ?)`,
		run.SourceKey,
		run.StartedAt.UTC().Format(time.RFC3339),
		run.FinishedAt.UTC().Format(time.RFC3339),
		run.Status, run.Error, run.Count)
	return err
}

// LastRunPerSource returns the most recent scrape run for each source key.
func (s *Store) LastRunPerSource(ctx context.Context) (map[string]ScrapeRun, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT source_key, started_at, finished_at, status, error, count
		FROM scrape_runs
		WHERE id IN (SELECT MAX(id) FROM scrape_runs GROUP BY source_key)`)
	if err != nil {
		return nil, fmt.Errorf("query scrape runs: %w", err)
	}
	defer rows.Close()

	out := map[string]ScrapeRun{}
	for rows.Next() {
		var (
			run                  ScrapeRun
			startedAt, finishedAt string
		)
		if err := rows.Scan(&run.SourceKey, &startedAt, &finishedAt,
			&run.Status, &run.Error, &run.Count); err != nil {
			return nil, err
		}
		run.StartedAt, _ = time.Parse(time.RFC3339, startedAt)
		run.FinishedAt, _ = time.Parse(time.RFC3339, finishedAt)
		out[run.SourceKey] = run
	}
	return out, rows.Err()
}
