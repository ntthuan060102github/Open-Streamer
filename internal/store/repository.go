// Package store defines the persistence layer interfaces.
// Only this package and its sub-packages may import database drivers.
// Business logic always depends on these interfaces, never on concrete implementations.
package store

import (
	"context"

	"github.com/open-streamer/open-streamer/internal/domain"
)

// StreamFilter holds optional filters for listing streams.
type StreamFilter struct {
	Status *domain.StreamStatus
}

// StreamRepository persists stream configurations and state.
type StreamRepository interface {
	Save(ctx context.Context, stream *domain.Stream) error
	FindByID(ctx context.Context, id domain.StreamID) (*domain.Stream, error)
	List(ctx context.Context, filter StreamFilter) ([]*domain.Stream, error)
	Delete(ctx context.Context, id domain.StreamID) error
}

// RecordingRepository persists DVR recording metadata.
type RecordingRepository interface {
	Save(ctx context.Context, rec *domain.Recording) error
	FindByID(ctx context.Context, id domain.RecordingID) (*domain.Recording, error)
	ListByStream(ctx context.Context, streamID domain.StreamID) ([]*domain.Recording, error)
	Delete(ctx context.Context, id domain.RecordingID) error
}

// HookRepository persists registered webhook/integration configurations.
type HookRepository interface {
	Save(ctx context.Context, hook *domain.Hook) error
	FindByID(ctx context.Context, id domain.HookID) (*domain.Hook, error)
	List(ctx context.Context) ([]*domain.Hook, error)
	Delete(ctx context.Context, id domain.HookID) error
}
