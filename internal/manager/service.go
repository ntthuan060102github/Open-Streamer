// Package manager implements the Stream Manager — the failover engine.
// It monitors input health (bitrate, FPS, packet loss, timeout) and seamlessly
// switches to the best available input when the active one degrades or fails.
// Failover is handled entirely in Go — FFmpeg is never restarted for this purpose.
package manager

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/open-streamer/open-streamer/internal/domain"
	"github.com/open-streamer/open-streamer/internal/events"
	"github.com/open-streamer/open-streamer/internal/ingestor"
	"github.com/samber/do/v2"
)

// InputHealth tracks the runtime health state of a single input source.
type InputHealth struct {
	Input       domain.Input
	LastPacketAt time.Time
	Bitrate     float64  // kbps
	PacketLoss  float64  // percent
	Status      domain.StreamStatus
}

// streamState holds the monitoring context for a stream.
type streamState struct {
	inputs  map[string]*InputHealth  // key = Input.ID
	active  string                   // active Input.ID
	cancel  context.CancelFunc
}

// Service monitors all streams and orchestrates failover.
type Service struct {
	bus      events.Bus
	ingestor *ingestor.Service
	mu       sync.RWMutex
	streams  map[domain.StreamID]*streamState
}

// New creates a Service and registers it with the DI injector.
func New(i do.Injector) (*Service, error) {
	bus := do.MustInvoke[events.Bus](i)
	ing := do.MustInvoke[*ingestor.Service](i)

	return &Service{
		bus:      bus,
		ingestor: ing,
		streams:  make(map[domain.StreamID]*streamState),
	}, nil
}

// Register starts health monitoring for a stream's inputs.
func (s *Service) Register(ctx context.Context, stream *domain.Stream) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	monCtx, cancel := context.WithCancel(ctx)

	state := &streamState{
		inputs: make(map[string]*InputHealth),
		cancel: cancel,
	}
	for _, input := range stream.Inputs {
		state.inputs[input.ID] = &InputHealth{
			Input:  input,
			Status: domain.StatusIdle,
		}
	}

	s.streams[stream.ID] = state
	go s.monitor(monCtx, stream.ID)
	return nil
}

// Unregister stops monitoring for a stream.
func (s *Service) Unregister(streamID domain.StreamID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state, ok := s.streams[streamID]; ok {
		state.cancel()
		delete(s.streams, streamID)
	}
}

// RecordPacket updates the last-seen timestamp for an input — called by the Ingestor.
func (s *Service) RecordPacket(streamID domain.StreamID, inputID string) {
	s.mu.RLock()
	state, ok := s.streams[streamID]
	s.mu.RUnlock()
	if !ok {
		return
	}
	state.inputs[inputID].LastPacketAt = time.Now()
	state.inputs[inputID].Status = domain.StatusActive
}

func (s *Service) monitor(ctx context.Context, streamID domain.StreamID) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkHealth(ctx, streamID)
		}
	}
}

func (s *Service) checkHealth(ctx context.Context, streamID domain.StreamID) {
	s.mu.RLock()
	state, ok := s.streams[streamID]
	s.mu.RUnlock()
	if !ok {
		return
	}

	const timeout = 5 * time.Second
	for id, h := range state.inputs {
		if time.Since(h.LastPacketAt) > timeout && h.Status == domain.StatusActive {
			slog.Warn("manager: input timeout detected",
				"stream_id", streamID,
				"input_id", id,
			)
			h.Status = domain.StatusDegraded
			s.bus.Publish(ctx, domain.Event{
				Type:     domain.EventInputDegraded,
				StreamID: streamID,
				Payload:  map[string]any{"input_id": id},
			})
			s.tryFailover(ctx, streamID, state)
		}
	}
}

func (s *Service) tryFailover(ctx context.Context, streamID domain.StreamID, state *streamState) {
	best := s.selectBest(state)
	if best == nil {
		slog.Error("manager: no healthy input available", "stream_id", streamID)
		return
	}
	if best.Input.ID == state.active {
		return
	}

	slog.Info("manager: switching input",
		"stream_id", streamID,
		"from", state.active,
		"to", best.Input.ID,
	)

	if err := s.ingestor.Start(ctx, streamID, best.Input); err != nil {
		slog.Error("manager: failed to start new ingestor", "stream_id", streamID, "err", err)
		return
	}

	prev := state.active
	state.active = best.Input.ID

	s.bus.Publish(ctx, domain.Event{
		Type:     domain.EventInputFailover,
		StreamID: streamID,
		Payload:  map[string]any{"from": prev, "to": best.Input.ID},
	})
}

// selectBest picks the highest-priority alive input (lower Priority value = higher priority).
func (s *Service) selectBest(state *streamState) *InputHealth {
	var best *InputHealth
	for _, h := range state.inputs {
		if h.Status == domain.StatusStopped {
			continue
		}
		if best == nil || h.Input.Priority < best.Input.Priority {
			best = h
		}
	}
	return best
}
