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

// StreamCallbacks receives per-publish-session events for a single stream.
// The ingestor.Service registers these via SetStreamCallbacks at the same
// time it registers the routing slot — closures capture the input priority
// (and any other manager-side context) so the push server stays agnostic
// of those concerns. Without these the Stream Manager never sees that an
// encoder is connected and the stream stays "Exhausted" forever (Path B
// has no loopback pull worker to surface packets).
//
// Any field may be nil. Callbacks fire on lal's read goroutine and must
// be cheap; defer heavy work.
type StreamCallbacks struct {
	// OnConnect fires once when the encoder finishes the publish handshake.
	OnConnect func()
	// OnPacket fires for every RTMP AV message — drives the manager's
	// Active-status / clear-Exhausted logic.
	OnPacket func()
	// OnPacketBytes fires for every RTMP AV message with the payload byte
	// count, for ingest bytes / packets metrics.
	OnPacketBytes func(n int)
	// OnMedia fires for every emitted AVPacket — drives the manager's
	// per-track codec / bitrate / resolution panel.
	OnMedia func(p *domain.AVPacket)
	// OnDisconnect fires when the publish session ends (encoder closed,
	// network error, server shutdown). The manager uses this to mark
	// the input degraded and trigger failover.
	OnDisconnect func(err error)
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

	// streamCallbacks: streamID → per-stream callback set. Populated by
	// the ingestor.Service at startPushRegistration time so the closures
	// can capture priority / metric labels without the push server having
	// to know those concepts.
	cbMu            sync.Mutex
	streamCallbacks map[domain.StreamCode]StreamCallbacks

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

	cb StreamCallbacks

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

// SetStreamCallbacks installs the per-session callback set for streamID.
// The ingestor.Service calls this once it has registered the routing
// slot (see startPushRegistration). Without callbacks the Stream Manager
// never sees that an encoder connected and the stream stays Exhausted.
//
// Pass an empty StreamCallbacks{} to detach (or call ClearStreamCallbacks).
// Replaces any previous callbacks for the same stream.
func (s *RTMPServer) SetStreamCallbacks(streamID domain.StreamCode, cb StreamCallbacks) {
	s.cbMu.Lock()
	if s.streamCallbacks == nil {
		s.streamCallbacks = make(map[domain.StreamCode]StreamCallbacks)
	}
	s.streamCallbacks[streamID] = cb
	s.cbMu.Unlock()
}

// ClearStreamCallbacks removes the callbacks for streamID. Safe to call
// for an unregistered stream; subsequent push sessions for streamID will
// simply have no observer hooks (the buffer-write path is unaffected).
func (s *RTMPServer) ClearStreamCallbacks(streamID domain.StreamCode) {
	s.cbMu.Lock()
	delete(s.streamCallbacks, streamID)
	s.cbMu.Unlock()
}

// callbacksFor returns the registered callbacks for streamID, or a zero
// value if none were installed. Zero StreamCallbacks fields are safe to
// invoke (each pubState method nil-checks before calling).
func (s *RTMPServer) callbacksFor(streamID domain.StreamCode) StreamCallbacks {
	s.cbMu.Lock()
	defer s.cbMu.Unlock()
	return s.streamCallbacks[streamID]
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

	cb := s.callbacksFor(streamID)
	state := &pubState{
		key:           key,
		bufferWriteID: bufWriteID,
		streamID:      streamID,
		buf:           buf,
		cb:            cb,
		converter:     pull.NewRTMPMsgConverter(),
		firstPacket:   true,
	}
	session.SetPubSessionObserver(state)

	s.mu.Lock()
	s.pubs[session] = state
	s.mu.Unlock()

	if cb.OnConnect != nil {
		cb.OnConnect()
	}

	slog.Info("rtmp server: publisher accepted",
		"key", key, "stream_id", streamID,
		"remote", session.GetStat().RemoteAddr,
	)
	return nil
}

// OnDelRtmpPubSession is invoked when a publish session ends (encoder
// disconnect, network error, server shutdown). Releases the registry
// slot so the next pusher can claim the key, and tells the manager the
// input has gone away so it can fail over.
func (s *RTMPServer) OnDelRtmpPubSession(session *rtmp.ServerSession) {
	s.mu.Lock()
	state, ok := s.pubs[session]
	delete(s.pubs, session)
	s.mu.Unlock()
	if !ok {
		return
	}
	s.registry.Release(state.key)
	if state.cb.OnDisconnect != nil {
		state.cb.OnDisconnect(errPusherDisconnected)
	}
	slog.Info("rtmp server: publisher disconnected",
		"key", state.key, "stream_id", state.streamID,
	)
}

// errPusherDisconnected is the sentinel handed to OnDisconnect. The manager
// only uses it for logging / event payload — the failover decision is
// driven by the input priority going degraded.
var errPusherDisconnected = errors.New("rtmp pusher disconnected")

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
// publisher sends. Convert to AVPackets, write to the buffer hub, and
// fire ingest observers so the Stream Manager sees liveness and codec
// metadata for this push input (without these the manager treats the
// stream as Exhausted forever — there is no loopback worker to surface
// packets in Path B).
//
// The first AV packet after acquisition is marked Discontinuity=true so
// downstream consumers (HLS / DASH segmenters) flush their accumulated
// buffer and start fresh. Mirrors the pull-worker firstPacket logic.
func (p *pubState) OnReadRtmpAvMsg(msg base.RtmpMsg) {
	if p.cb.OnPacket != nil {
		p.cb.OnPacket()
	}
	if p.cb.OnPacketBytes != nil {
		p.cb.OnPacketBytes(len(msg.Payload))
	}
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
		if p.cb.OnMedia != nil {
			p.cb.OnMedia(cl)
		}
	}
}
