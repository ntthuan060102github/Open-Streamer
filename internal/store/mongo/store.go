// Package mongo provides a MongoDB implementation of the store repositories.
// Uses go.mongodb.org/mongo-driver/v2. Index definitions are applied at startup.
// Collection naming: plural snake_case (streams, recordings, hooks).
package mongo

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"github.com/open-streamer/open-streamer/internal/domain"
	"github.com/open-streamer/open-streamer/internal/store"
)

// Store is a MongoDB-backed implementation of all repositories.
type Store struct {
	client *mongo.Client
	db     *mongo.Database
}

// New opens a MongoDB connection and selects the target database.
func New(ctx context.Context, uri, database string) (*Store, error) {
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		return nil, fmt.Errorf("mongo store: connect: %w", err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		return nil, fmt.Errorf("mongo store: ping: %w", err)
	}
	return &Store{
		client: client,
		db:     client.Database(database),
	}, nil
}

// Close disconnects from MongoDB.
func (s *Store) Close(ctx context.Context) error {
	return s.client.Disconnect(ctx)
}

// EnsureIndexes creates required indexes. Call once at startup.
func (s *Store) EnsureIndexes(ctx context.Context) error {
	// TODO: define indexes for streams, recordings, hooks
	return nil
}

// Streams returns a StreamRepository backed by this Store.
func (s *Store) Streams() store.StreamRepository {
	return &streamRepo{col: s.db.Collection("streams")}
}

// Recordings returns a RecordingRepository backed by this Store.
func (s *Store) Recordings() store.RecordingRepository {
	return &recordingRepo{col: s.db.Collection("recordings")}
}

// Hooks returns a HookRepository backed by this Store.
func (s *Store) Hooks() store.HookRepository {
	return &hookRepo{col: s.db.Collection("hooks")}
}

// --- StreamRepository ---

type streamRepo struct{ col *mongo.Collection }

func (r *streamRepo) Save(ctx context.Context, stream *domain.Stream) error {
	// TODO: implement upsert with filter {_id: stream.ID}
	return fmt.Errorf("mongo store: streams.Save: not implemented")
}

func (r *streamRepo) FindByID(ctx context.Context, id domain.StreamID) (*domain.Stream, error) {
	return nil, fmt.Errorf("mongo store: streams.FindByID: not implemented")
}

func (r *streamRepo) List(ctx context.Context, filter store.StreamFilter) ([]*domain.Stream, error) {
	return nil, fmt.Errorf("mongo store: streams.List: not implemented")
}

func (r *streamRepo) Delete(ctx context.Context, id domain.StreamID) error {
	return fmt.Errorf("mongo store: streams.Delete: not implemented")
}

// --- RecordingRepository ---

type recordingRepo struct{ col *mongo.Collection }

func (r *recordingRepo) Save(ctx context.Context, rec *domain.Recording) error {
	return fmt.Errorf("mongo store: recordings.Save: not implemented")
}

func (r *recordingRepo) FindByID(ctx context.Context, id domain.RecordingID) (*domain.Recording, error) {
	return nil, fmt.Errorf("mongo store: recordings.FindByID: not implemented")
}

func (r *recordingRepo) ListByStream(ctx context.Context, streamID domain.StreamID) ([]*domain.Recording, error) {
	return nil, fmt.Errorf("mongo store: recordings.ListByStream: not implemented")
}

func (r *recordingRepo) Delete(ctx context.Context, id domain.RecordingID) error {
	return fmt.Errorf("mongo store: recordings.Delete: not implemented")
}

// --- HookRepository ---

type hookRepo struct{ col *mongo.Collection }

func (r *hookRepo) Save(ctx context.Context, hook *domain.Hook) error {
	return fmt.Errorf("mongo store: hooks.Save: not implemented")
}

func (r *hookRepo) FindByID(ctx context.Context, id domain.HookID) (*domain.Hook, error) {
	return nil, fmt.Errorf("mongo store: hooks.FindByID: not implemented")
}

func (r *hookRepo) List(ctx context.Context) ([]*domain.Hook, error) {
	return nil, fmt.Errorf("mongo store: hooks.List: not implemented")
}

func (r *hookRepo) Delete(ctx context.Context, id domain.HookID) error {
	return fmt.Errorf("mongo store: hooks.Delete: not implemented")
}
