package push

// rtmp_server.go — RTMP push-ingest server using lal/pkg/rtmp.
//
// Architecture (Path B — no loopback): each encoder pushes to this server
// in publish mode. The server's lal session decodes RTMP chunks into
// `base.RtmpMsg` values, which we convert in place to domain.AVPacket via
// pull.RTMPMsgConverter and write directly into the Buffer Hub.
//
//   Encoder ──RTMP push──► lal.Server (in this file)
//                              │ OnReadRtmpAvMsg(RtmpMsg)
//                              ▼
//                         RTMPMsgConverter (shared with pull reader)
//                              │
//                              ▼
//                         buffer.Service.Write(bufID, Packet{AV: ...})
//
// External RTMP play clients (VLC / ffplay / OBS preview) connect to the
// same TCP port; lal raises OnNewRtmpSubSession which delegates to the
// publisher-side PlayFunc — see internal/publisher/serve_rtmp.go for the
// reader-side that streams from the buffer hub into the lal session.
//
// Lifecycle:
//
//   - NewRTMPServer(addr, registry, buf): construct, no I/O yet.
//   - SetPlayFunc(fn): optional, register external play handler.
//   - Run(ctx): bind listener, accept connections until ctx is cancelled.
//
// Each ServerSession runs on its own goroutine inside lal; our callbacks
// (OnReadRtmpAvMsg for pubs, the PlayFunc loop for subs) execute on those
// goroutines and must be cheap. Heavy work (codec config parsing,
// buffer-hub fan-out) happens inside RTMPMsgConverter / buffer.Service
// which are designed for concurrent callers.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/q191201771/lal/pkg/base"
	"github.com/q191201771/lal/pkg/rtmp"

	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/ingestor/pull"
)

// PlayFunc is invoked when an external play client connects to the RTMP
// server. Implementations should subscribe to the Buffer Hub for `key`
// and feed the lal session frames until ctx is cancelled or the session
// disconnects.
//
// Returning a non-nil error causes the session to be torn down with a
// "stream not found" status code.
type PlayFunc func(ctx context.Context, key string, info PlayInfo, session *rtmp.ServerSession) error

// PlayInfo describes the remote peer of an external RTMP play client.
type PlayInfo struct {
	// RemoteAddr is the peer's "ip:port" string from the underlying TCP
	// connection. Useful for the play-sessions tracker / abuse mitigation.
	RemoteAddr string
}

// RTMPServer accepts RTMP push connections from encoders, validates them
// against the push Registry, and writes decoded AVPackets into the Buffer
// Hub. External RTMP play clients are served via the optional PlayFunc.
type RTMPServer struct {
	addr     string
	registry Registry

	mu       sync.Mutex
	pubs     map[*rtmp.ServerSession]*pubState
	subs     map[*rtmp.ServerSession]*subState
	playFunc PlayFunc

	server *rtmp.Server
}

// pubState is the per-publish-session bookkeeping: codec converter +
// buffer hub target + Discontinuity flag for the first frame after a
// reconnect.
type pubState struct {
	key           string
	bufferWriteID domain.StreamCode
	streamID      domain.StreamCode
	buf           *buffer.Service

	converter   *pull.RTMPMsgConverter
	firstPacket bool
}

// subState is the per-play-session bookkeeping: cancel func to stop the
// PlayFunc goroutine when the session ends.
type subState struct {
	cancel context.CancelFunc
}

// NewRTMPServer creates an RTMPServer. `addr` is the TCP bind address
// (e.g. ":1935"). `registry` resolves push keys to buffer hub targets.
func NewRTMPServer(addr string, registry Registry) (*RTMPServer, error) {
	return &RTMPServer{
		addr:     addr,
		registry: registry,
		pubs:     make(map[*rtmp.ServerSession]*pubState),
		subs:     make(map[*rtmp.ServerSession]*subState),
	}, nil
}

// SetPlayFunc registers a handler for external RTMP play clients.
// Safe to call concurrently with Run.
func (s *RTMPServer) SetPlayFunc(fn PlayFunc) {
	s.mu.Lock()
	s.playFunc = fn
	s.mu.Unlock()
}

// Run binds the TCP listener and accepts RTMP connections until ctx is
// cancelled. lal's RunLoop blocks until the listener closes; we close
// the listener via Dispose() when ctx fires so RunLoop returns nil.
func (s *RTMPServer) Run(ctx context.Context) error {
	srv := rtmp.NewServer(s.addr, s)
	s.mu.Lock()
	s.server = srv
	s.mu.Unlock()

	if err := srv.Listen(); err != nil {
		return fmt.Errorf("rtmp server: listen %q: %w", s.addr, err)
	}
	slog.Info("rtmp server: listening", "addr", s.addr)

	// Watchdog: close the listener on context cancellation so RunLoop
	// unblocks. Spawned before RunLoop so it's already armed when accepts
	// start coming in.
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			srv.Dispose()
		case <-done:
		}
	}()

	err := srv.RunLoop()
	close(done)
	if ctx.Err() != nil {
		//nolint:nilerr // listener was closed by our watchdog on ctx.Done — shutdown, not failure.
		return nil
	}
	if err != nil {
		return fmt.Errorf("rtmp server: run loop: %w", err)
	}
	return nil
}

// ─── lal IServerObserver ────────────────────────────────────────────────

// OnRtmpConnect is invoked once per accepted TCP connection at the end of
// the RTMP `connect` command. We log the remote app/tcUrl combo for ops
// and don't reject anything here — rejections happen at OnNewRtmpPubSession
// / OnNewRtmpSubSession when we know whether the key is registered.
func (s *RTMPServer) OnRtmpConnect(session *rtmp.ServerSession, _ rtmp.ObjectPairArray) {
	slog.Debug("rtmp server: client connected",
		"remote", session.GetStat().RemoteAddr,
		"app", session.AppName(),
	)
}

// OnNewRtmpPubSession is invoked when an encoder issues a `publish`
// command. We acquire the registry slot for the stream key and wire the
// session's AV-message observer to feed our converter.
//
// Returning an error rejects the session — lal sends a NetStream.Publish.BadName
// status code and tears down the connection.
func (s *RTMPServer) OnNewRtmpPubSession(session *rtmp.ServerSession) error {
	key := session.StreamName()
	bufWriteID, streamID, buf, err := s.registry.Acquire(key)
	if err != nil {
		slog.Warn("rtmp server: rejected publisher",
			"key", key,
			"remote", session.GetStat().RemoteAddr,
			"err", err,
		)
		return err
	}

	state := &pubState{
		key:           key,
		bufferWriteID: bufWriteID,
		streamID:      streamID,
		buf:           buf,
		converter:     pull.NewRTMPMsgConverter(),
		firstPacket:   true,
	}
	session.SetPubSessionObserver(state)

	s.mu.Lock()
	s.pubs[session] = state
	s.mu.Unlock()

	slog.Info("rtmp server: publisher accepted",
		"key", key, "stream_id", streamID,
		"remote", session.GetStat().RemoteAddr,
	)
	return nil
}

// OnDelRtmpPubSession is invoked when a publish session ends (encoder
// disconnect, network error, server shutdown). Releases the registry
// slot so the next pusher can claim the key.
func (s *RTMPServer) OnDelRtmpPubSession(session *rtmp.ServerSession) {
	s.mu.Lock()
	state, ok := s.pubs[session]
	delete(s.pubs, session)
	s.mu.Unlock()
	if !ok {
		return
	}
	s.registry.Release(state.key)
	slog.Info("rtmp server: publisher disconnected",
		"key", state.key, "stream_id", state.streamID,
	)
}

// OnNewRtmpSubSession is invoked when a play client connects. We delegate
// to the registered PlayFunc (publisher side). If no PlayFunc is wired,
// reject — playback isn't available without the publisher being present.
func (s *RTMPServer) OnNewRtmpSubSession(session *rtmp.ServerSession) error {
	s.mu.Lock()
	fn := s.playFunc
	s.mu.Unlock()
	if fn == nil {
		slog.Warn("rtmp server: play rejected (no PlayFunc registered)",
			"key", session.StreamName(),
		)
		return errors.New("rtmp server: play handler not configured")
	}

	key := session.StreamName()
	info := PlayInfo{RemoteAddr: session.GetStat().RemoteAddr}

	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.subs[session] = &subState{cancel: cancel}
	s.mu.Unlock()

	// Run the play handler on its own goroutine — lal will block this
	// callback until we return, so we can't drive the read loop from
	// here. The handler is responsible for stopping when ctx fires.
	go func() {
		err := fn(ctx, key, info, session)
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Debug("rtmp server: play session ended",
				"key", key, "err", err)
		}
		_ = session.Dispose()
	}()

	slog.Info("rtmp server: play client accepted",
		"key", key, "remote", info.RemoteAddr,
	)
	return nil
}

// OnDelRtmpSubSession is invoked when a play session ends. Cancels the
// PlayFunc goroutine so it stops reading from the buffer hub.
func (s *RTMPServer) OnDelRtmpSubSession(session *rtmp.ServerSession) {
	s.mu.Lock()
	state, ok := s.subs[session]
	delete(s.subs, session)
	s.mu.Unlock()
	if ok {
		state.cancel()
	}
}

// ─── lal IPubSessionObserver (implemented by *pubState) ────────────────

// OnReadRtmpAvMsg is invoked by lal for every audio/video RtmpMsg the
// publisher sends. Convert to AVPackets and write to the buffer hub.
//
// The first AV packet after acquisition is marked Discontinuity=true so
// downstream consumers (HLS / DASH segmenters) flush their accumulated
// buffer and start fresh. Mirrors the pull-worker firstPacket logic.
func (p *pubState) OnReadRtmpAvMsg(msg base.RtmpMsg) {
	for _, av := range p.converter.Convert(msg) {
		cl := av.Clone()
		if p.firstPacket {
			cl.Discontinuity = true
			p.firstPacket = false
		}
		if err := p.buf.Write(p.bufferWriteID, buffer.Packet{AV: cl}); err != nil {
			slog.Debug("rtmp server: buffer write failed",
				"key", p.key, "err", err)
		}
	}
}
