// Package store is the only package that talks SQL.
package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"sci1.uk/catical/migrations"
)

// ErrNotFound is returned when a feed id does not exist.
var ErrNotFound = errors.New("store: not found")

// Store is a Postgres-backed feed repository.
type Store struct {
	pool *pgxpool.Pool
	fs   fs.FS
}

// Open connects to Postgres. dsn is a DATABASE_URL (postgres://...).
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.RuntimeParams["timezone"] = "UTC"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool, fs: migrations.FS}, nil
}

// Close releases the pool.
func (s *Store) Close() {
	s.pool.Close()
}

// Feed is a calendar mix and its hashed capability tokens.
type Feed struct {
	ID              uuid.UUID
	Name            string
	PrefixSummaries bool
	FeedTokenSalt   []byte
	FeedTokenHash   []byte
	ManageTokenSalt []byte
	ManageTokenHash []byte
	MergedICS       []byte
	MergedAt        *time.Time
	LastRequestAt   time.Time
	CreatedAt       time.Time
	Sources         []Source
}

// Source is one origin ICS URL belonging to a feed.
type Source struct {
	ID            uuid.UUID
	FeedID        uuid.UUID
	URL           string
	Label         string
	Position      int
	LastICS       []byte
	LastSuccessAt *time.Time
	LastError     *string
	LastAttemptAt *time.Time
}

// CreateFeedParams is the insert payload. Secrets are never accepted — only
// salt and hash from tokens.Generate.
type CreateFeedParams struct {
	Name            string
	PrefixSummaries bool
	FeedTokenSalt   []byte
	FeedTokenHash   []byte
	ManageTokenSalt []byte
	ManageTokenHash []byte
	LastRequestAt   time.Time
	Sources         []CreateSource
}

// CreateSource is a source row at insert time.
type CreateSource struct {
	URL      string
	Label    string
	Position int
}

// UpdateFeedParams replaces name, prefix flag, and the full source list.
type UpdateFeedParams struct {
	Name            string
	PrefixSummaries bool
	Sources         []CreateSource
}

// CreateFeed inserts a feed and its sources. last_request_at is set from
// params (create uses now() for the 14-day GDPR grace).
func (s *Store) CreateFeed(ctx context.Context, p CreateFeedParams) (Feed, error) {
	id := uuid.New()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Feed{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var created time.Time
	err = tx.QueryRow(ctx, `
		INSERT INTO feeds (
			id, name, prefix_summaries,
			feed_token_salt, feed_token_hash,
			manage_token_salt, manage_token_hash,
			last_request_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING created_at
	`, id, p.Name, p.PrefixSummaries, p.FeedTokenSalt, p.FeedTokenHash,
		p.ManageTokenSalt, p.ManageTokenHash, p.LastRequestAt,
	).Scan(&created)
	if err != nil {
		return Feed{}, err
	}

	sources := make([]Source, 0, len(p.Sources))
	for _, src := range p.Sources {
		sid := uuid.New()
		_, err := tx.Exec(ctx, `
			INSERT INTO sources (id, feed_id, url, label, position)
			VALUES ($1,$2,$3,$4,$5)
		`, sid, id, src.URL, src.Label, src.Position)
		if err != nil {
			return Feed{}, err
		}
		sources = append(sources, Source{
			ID:       sid,
			FeedID:   id,
			URL:      src.URL,
			Label:    src.Label,
			Position: src.Position,
		})
	}
	if err := tx.Commit(ctx); err != nil {
		return Feed{}, err
	}
	return Feed{
		ID:              id,
		Name:            p.Name,
		PrefixSummaries: p.PrefixSummaries,
		FeedTokenSalt:   append([]byte(nil), p.FeedTokenSalt...),
		FeedTokenHash:   append([]byte(nil), p.FeedTokenHash...),
		ManageTokenSalt: append([]byte(nil), p.ManageTokenSalt...),
		ManageTokenHash: append([]byte(nil), p.ManageTokenHash...),
		LastRequestAt:   p.LastRequestAt,
		CreatedAt:       created,
		Sources:         sources,
	}, nil
}

// UpdateFeed replaces name, prefix_summaries, and sources, and clears the
// merged ICS cache so the next refresh rebuilds it.
func (s *Store) UpdateFeed(ctx context.Context, id uuid.UUID, p UpdateFeedParams) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE feeds SET name = $2, prefix_summaries = $3,
			merged_ics = NULL, merged_at = NULL
		WHERE id = $1
	`, id, p.Name, p.PrefixSummaries)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sources WHERE feed_id = $1`, id); err != nil {
		return err
	}
	for _, src := range p.Sources {
		sid := uuid.New()
		_, err := tx.Exec(ctx, `
			INSERT INTO sources (id, feed_id, url, label, position)
			VALUES ($1,$2,$3,$4,$5)
		`, sid, id, src.URL, src.Label, src.Position)
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// RotateFeedToken replaces the ICS capability hash. The new secret is not stored.
func (s *Store) RotateFeedToken(ctx context.Context, id uuid.UUID, salt, hash []byte) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE feeds SET feed_token_salt = $2, feed_token_hash = $3 WHERE id = $1
	`, id, salt, hash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// GetFeed loads a feed and its sources ordered by position.
func (s *Store) GetFeed(ctx context.Context, id uuid.UUID) (Feed, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, name, prefix_summaries,
			feed_token_salt, feed_token_hash,
			manage_token_salt, manage_token_hash,
			merged_ics, merged_at, last_request_at, created_at
		FROM feeds WHERE id = $1
	`, id)
	f, err := scanFeed(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Feed{}, ErrNotFound
		}
		return Feed{}, err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id, feed_id, url, label, position,
			last_ics, last_success_at, last_error, last_attempt_at
		FROM sources WHERE feed_id = $1 ORDER BY position ASC
	`, id)
	if err != nil {
		return Feed{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var src Source
		if err := rows.Scan(
			&src.ID, &src.FeedID, &src.URL, &src.Label, &src.Position,
			&src.LastICS, &src.LastSuccessAt, &src.LastError, &src.LastAttemptAt,
		); err != nil {
			return Feed{}, err
		}
		f.Sources = append(f.Sources, src)
	}
	return f, rows.Err()
}

type scannable interface {
	Scan(dest ...any) error
}

func scanFeed(row scannable) (Feed, error) {
	var f Feed
	err := row.Scan(
		&f.ID, &f.Name, &f.PrefixSummaries,
		&f.FeedTokenSalt, &f.FeedTokenHash,
		&f.ManageTokenSalt, &f.ManageTokenHash,
		&f.MergedICS, &f.MergedAt, &f.LastRequestAt, &f.CreatedAt,
	)
	return f, err
}

// UpdateMerged writes the last-good merged ICS blob. It does not change
// last_request_at.
func (s *Store) UpdateMerged(ctx context.Context, id uuid.UUID, ics []byte, mergedAt time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE feeds SET merged_ics = $2, merged_at = $3 WHERE id = $1
	`, id, ics, mergedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateSourceFetch writes per-origin cache fields after a refresh attempt.
func (s *Store) UpdateSourceFetch(ctx context.Context, src Source) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE sources SET
			last_ics = $2,
			last_success_at = $3,
			last_error = $4,
			last_attempt_at = $5
		WHERE id = $1
	`, src.ID, src.LastICS, src.LastSuccessAt, src.LastError, src.LastAttemptAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchLastRequest records an ICS feed GET for GDPR retention.
func (s *Store) TouchLastRequest(ctx context.Context, id uuid.UUID, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE feeds SET last_request_at = $2 WHERE id = $1
	`, id, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteFeed removes a feed; sources cascade.
func (s *Store) DeleteFeed(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM feeds WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteStaleFeeds deletes feeds whose ICS has not been fetched within
// retention of now. Manage views do not affect last_request_at.
func (s *Store) DeleteStaleFeeds(ctx context.Context, now time.Time, retention time.Duration) (int64, error) {
	cutoff := now.Add(-retention)
	tag, err := s.pool.Exec(ctx, `DELETE FROM feeds WHERE last_request_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// CountSources returns the number of source rows (tests / reaper diagnostics).
func (s *Store) CountSources(ctx context.Context) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM sources`).Scan(&n)
	return n, err
}

// Truncate clears application tables. Used by integration tests only.
func (s *Store) Truncate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `TRUNCATE feeds CASCADE`)
	return err
}

// Migrate applies numbered SQL files from migrations/ once each.
func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`)
	if err != nil {
		return err
	}

	entries, err := fs.ReadDir(s.fs, ".")
	if err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		var exists bool
		err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)`, name).Scan(&exists)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		body, err := fs.ReadFile(s.fs, name)
		if err != nil {
			return err
		}
		if _, err := s.pool.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := s.pool.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, name); err != nil {
			return err
		}
	}
	return nil
}
