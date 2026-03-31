// Package publisher delivers transcoded streams to all outputs.
//
// It handles two distinct output types:
//   - Serve endpoints (HLS, DASH, RTSP, SRT listen, RTS/WebRTC): the server listens,
//     clients connect and pull. Managed by startServe().
//   - Push destinations (RTMP, SRT push, RIST, RTP): the server connects out and pushes.
//     Managed by startPush().
package publisher

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/open-streamer/open-streamer/config"
	"github.com/open-streamer/open-streamer/internal/buffer"
	"github.com/open-streamer/open-streamer/internal/domain"
	"github.com/samber/do/v2"
)

// Service manages all output workers for active streams.
type Service struct {
	cfg     config.PublisherConfig
	buf     *buffer.Service
	mu      sync.Mutex
	workers map[domain.StreamID]context.CancelFunc
}

// New creates a Service and registers it with the DI injector.
func New(i do.Injector) (*Service, error) {
	cfg := do.MustInvoke[*config.Config](i)
	buf := do.MustInvoke[*buffer.Service](i)

	return &Service{
		cfg:     cfg.Publisher,
		buf:     buf,
		workers: make(map[domain.StreamID]context.CancelFunc),
	}, nil
}

// Start launches all serve-endpoints and push-destination workers for a stream.
func (s *Service) Start(ctx context.Context, stream *domain.Stream) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.workers[stream.ID]; ok {
		return fmt.Errorf("publisher: stream %s already running", stream.ID)
	}

	workerCtx, cancel := context.WithCancel(ctx)
	s.workers[stream.ID] = cancel

	// --- Serve endpoints (clients pull from us) ---
	p := stream.Protocols
	if p.HLS {
		go s.serveHLS(workerCtx, stream.ID)
	}
	if p.DASH {
		go s.serveDASH(workerCtx, stream.ID)
	}
	if p.RTSP {
		go s.serveRTSP(workerCtx, stream.ID)
	}
	if p.SRT {
		go s.serveSRT(workerCtx, stream.ID)
	}
	if p.RTS {
		go s.serveRTS(workerCtx, stream.ID)
	}

	// --- Push destinations (we connect out and push) ---
	for _, dest := range stream.Push {
		if dest.Enabled {
			go s.pushToDestination(workerCtx, stream.ID, dest)
		}
	}

	return nil
}

// Stop cancels all output workers for a stream.
func (s *Service) Stop(streamID domain.StreamID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cancel, ok := s.workers[streamID]; ok {
		cancel()
		delete(s.workers, streamID)
	}
}

// ---- Serve endpoints --------------------------------------------------------
// Each function reads protocol config from s.cfg (global server config).

func (s *Service) serveHLS(ctx context.Context, streamID domain.StreamID) {
	sub, err := s.buf.Subscribe(streamID)
	if err != nil {
		slog.Error("publisher: HLS subscribe failed", "stream_id", streamID, "err", err)
		return
	}
	defer s.buf.Unsubscribe(streamID, sub)

	slog.Info("publisher: HLS serve started", "stream_id", streamID, "hls_dir", s.cfg.HLSDir)

	// TODO: implement HLS segmenter
	// 1. Buffer packets until segment boundary (keyframe-aligned)
	// 2. Write .ts segment to s.cfg.HLSDir/{streamID}/{idx}.ts
	// 3. Update sliding-window playlist.m3u8
	<-ctx.Done()
}

func (s *Service) serveDASH(ctx context.Context, streamID domain.StreamID) {
	sub, err := s.buf.Subscribe(streamID)
	if err != nil {
		slog.Error("publisher: DASH subscribe failed", "stream_id", streamID, "err", err)
		return
	}
	defer s.buf.Unsubscribe(streamID, sub)

	slog.Info("publisher: DASH serve started", "stream_id", streamID)

	// TODO: implement DASH/CMAF packager
	<-ctx.Done()
}

func (s *Service) serveRTSP(ctx context.Context, streamID domain.StreamID) {
	slog.Info("publisher: RTSP serve started", "stream_id", streamID)
	// TODO: register stream path in RTSP server; feed buffer packets to connected clients
	<-ctx.Done()
}

func (s *Service) serveSRT(ctx context.Context, streamID domain.StreamID) {
	slog.Info("publisher: SRT listener started", "stream_id", streamID)
	// TODO: open SRT listener socket; accept caller connections; fan-out buffer packets
	<-ctx.Done()
}

func (s *Service) serveRTS(ctx context.Context, streamID domain.StreamID) {
	slog.Info("publisher: RTS (WebRTC) serve started", "stream_id", streamID)
	// TODO: register WHEP endpoint; negotiate WebRTC sessions; push via RTP tracks
	<-ctx.Done()
}

// ---- Push destinations ------------------------------------------------------

func (s *Service) pushToDestination(ctx context.Context, streamID domain.StreamID, dest domain.PushDestination) {
	sub, err := s.buf.Subscribe(streamID)
	if err != nil {
		slog.Error("publisher: push subscribe failed", "stream_id", streamID, "url", dest.URL, "err", err)
		return
	}
	defer s.buf.Unsubscribe(streamID, sub)

	slog.Info("publisher: push destination started", "stream_id", streamID, "url", dest.URL)

	// TODO: spawn FFmpeg reading from buffer stdin, pushing to dest.URL
	// Retry up to dest.Limit times with dest.RetryTimeoutSec delay between attempts
	// Use dest.TimeoutSec as the connection/write timeout
	s.pushViaFFmpeg(ctx, streamID, dest, sub)
}

func (s *Service) pushViaFFmpeg(_ context.Context, streamID domain.StreamID, dest domain.PushDestination, _ *buffer.Subscriber) {
	// TODO: implement retry loop:
	// for attempt := 0; dest.Limit == 0 || attempt < dest.Limit; attempt++ {
	//     err := spawnFFmpeg(ctx, dest.URL, dest.TimeoutSec, sub)
	//     if err == nil { return }
	//     slog.Warn("push retry", "attempt", attempt, "delay", dest.RetryTimeoutSec)
	//     time.Sleep(time.Duration(dest.RetryTimeoutSec) * time.Second)
	// }
	slog.Info("publisher: FFmpeg push started", "stream_id", streamID, "url", dest.URL)
}
