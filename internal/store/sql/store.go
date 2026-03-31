// Package sql provides a SQL (Postgres/MySQL) implementation of the store repositories.
// Uses pgx/v5 for Postgres and sqlx for query helpers.
// All queries use named parameters. Migrations live in scripts/migrations/.
package sql

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"
	"github.com/open-streamer/open-streamer/internal/domain"
	"github.com/open-streamer/open-streamer/internal/store"
)

// Store is a SQL-backed implementation of all repositories.
type Store struct {
	db *sqlx.DB
}

// New opens a SQL connection using the provided DSN.
func New(dsn string) (*Store, error) {
	db, err := sqlx.Connect("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("sql store: connect: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the underlying connection pool.
func (s *Store) Close() error {
	return s.db.Close()
}

// Streams returns a StreamRepository backed by this Store.
func (s *Store) Streams() store.StreamRepository { return &streamRepo{db: s.db} }

// Recordings returns a RecordingRepository backed by this Store.
func (s *Store) Recordings() store.RecordingRepository { return &recordingRepo{db: s.db} }

// Hooks returns a HookRepository backed by this Store.
func (s *Store) Hooks() store.HookRepository { return &hookRepo{db: s.db} }

// --- StreamRepository ---

type streamRepo struct{ db *sqlx.DB }

func (r *streamRepo) Save(ctx context.Context, stream *domain.Stream) error {
	// TODO: implement upsert
	return fmt.Errorf("sql store: streams.Save: not implemented")
}

func (r *streamRepo) FindByID(ctx context.Context, id domain.StreamID) (*domain.Stream, error) {
	// TODO: implement SELECT
	return nil, fmt.Errorf("sql store: streams.FindByID: not implemented")
}

func (r *streamRepo) List(ctx context.Context, filter store.StreamFilter) ([]*domain.Stream, error) {
	// TODO: implement SELECT with optional WHERE status = ?
	return nil, fmt.Errorf("sql store: streams.List: not implemented")
}

func (r *streamRepo) Delete(ctx context.Context, id domain.StreamID) error {
	// TODO: implement DELETE
	return fmt.Errorf("sql store: streams.Delete: not implemented")
}

// --- RecordingRepository ---

type recordingRepo struct{ db *sqlx.DB }

func (r *recordingRepo) Save(ctx context.Context, rec *domain.Recording) error {
	return fmt.Errorf("sql store: recordings.Save: not implemented")
}

func (r *recordingRepo) FindByID(ctx context.Context, id domain.RecordingID) (*domain.Recording, error) {
	return nil, fmt.Errorf("sql store: recordings.FindByID: not implemented")
}

func (r *recordingRepo) ListByStream(ctx context.Context, streamID domain.StreamID) ([]*domain.Recording, error) {
	return nil, fmt.Errorf("sql store: recordings.ListByStream: not implemented")
}

func (r *recordingRepo) Delete(ctx context.Context, id domain.RecordingID) error {
	return fmt.Errorf("sql store: recordings.Delete: not implemented")
}

// --- HookRepository ---

type hookRepo struct{ db *sqlx.DB }

func (r *hookRepo) Save(ctx context.Context, hook *domain.Hook) error {
	return fmt.Errorf("sql store: hooks.Save: not implemented")
}

func (r *hookRepo) FindByID(ctx context.Context, id domain.HookID) (*domain.Hook, error) {
	return nil, fmt.Errorf("sql store: hooks.FindByID: not implemented")
}

func (r *hookRepo) List(ctx context.Context) ([]*domain.Hook, error) {
	return nil, fmt.Errorf("sql store: hooks.List: not implemented")
}

func (r *hookRepo) Delete(ctx context.Context, id domain.HookID) error {
	return fmt.Errorf("sql store: hooks.Delete: not implemented")
}
