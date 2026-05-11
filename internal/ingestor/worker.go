package ingestor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/ingestor/pull"
	"github.com/ntt0601zcoder/open-streamer/internal/timeline"
)

const (
	reconnectBaseDelay = time.Second
	reconnectMaxDelay  = 10 * time.Second
)

// httpStatusPattern matches HTTP status codes in error strings, case-insensitively,
// to cover both "HTTP 404" (typical ingestor errors) and "http 404".
var httpStatusPattern = regexp.MustCompile(`(?i)\bhttp (\d{3})\b`)

// pullWorkerCallbacks groups optional observer callbacks for runPullWorker.
type pullWorkerCallbacks struct {
	onPacket      func(streamID domain.StreamCode, inputPriority int)
	onInputError  func(streamID domain.StreamCode, inputPriority int, err error)
	onConnect     func(streamID domain.StreamCode, inputPriority int)
	onReconnect   func(streamID domain.StreamCode, inputPriority int, err error)
	onPacketBytes func(streamID domain.StreamCode, inputPriority, bytes int)
	onMedia       func(streamID domain.StreamCode, inputPriority int, p *domain.AVPacket)
	// onHandoff is called exactly once, after the very first successful Open.
	// It is used by startPullWorker to cancel the previous source worker only after
	// the new source has connected — eliminating the buffer gap during source transitions.
	onHandoff func()
	// onSession is called whenever the worker mints a new StreamSession on
	// the buffer hub (first connect, reconnect, failover-handoff). The caller
	// may publish a bus event or log the session for telemetry. The session
	// argument is the live record returned by buffer.Service.SetSession;
	// readers must not mutate it.
	onSession func(streamID domain.StreamCode, sess *domain.StreamSession)

	// firstSessionReason is the SessionStartReason used the first time this
	// worker successfully Opens its source. Caller picks Fresh for a cold
	// start and Failover when this worker is taking over from a predecessor.
	// Subsequent Open attempts within the same worker always mint a
	// Reconnect session, since by then we have already handed off.
	firstSessionReason domain.SessionStartReason

	// normaliserCfg controls AV-path PTS anchoring. The Normaliser is
	// constructed once per worker lifetime; OnSession resets per-track
	// state at each new StreamSession boundary (cold start, reconnect,
	// failover-handoff). See internal/timeline.
	normaliserCfg timeline.Config
}

// runPullWorker reads from reader in a loop, writing each chunk to the buffer.
// It reconnects automatically after transient failures using exponential backoff.
// The loop exits cleanly when ctx is cancelled.
func runPullWorker(
	ctx context.Context,
	streamID domain.StreamCode,
	bufferWriteID domain.StreamCode,
	input domain.Input,
	r PacketReader,
	buf *buffer.Service,
	cb pullWorkerCallbacks,
) {
	delay := reconnectBaseDelay
	handedOff := false     // onHandoff fires at most once (first successful Open)
	firstOpenDone := false // distinguishes first Open from subsequent Reconnect

	// Normaliser lives for the whole worker lifetime — across every
	// reconnect / readLoop iteration. State resets are explicit via
	// OnSession (called from mintSessionForOpen below) so a reconnect
	// re-anchors deterministically against the new StreamSession instead
	// of relying on a fresh-construction-per-cycle implicit reset.
	norm := timeline.New(cb.normaliserCfg)

	for {
		if !openSource(ctx, streamID, input, r, cb, &delay, &handedOff) {
			return
		}

		slog.Info("ingestor: source connected",
			"stream_code", streamID,
			"input_priority", input.Priority,
			"url", input.URL,
		)
		delay = reconnectBaseDelay // reset on successful open
		// Mint a StreamSession before any packet is written. On the very
		// first open we honour cb.firstSessionReason (Fresh or Failover);
		// every subsequent open within this worker is a Reconnect.
		mintSessionForOpen(streamID, bufferWriteID, buf, &cb, firstOpenDone, norm)
		firstOpenDone = true
		if cb.onConnect != nil {
			cb.onConnect(streamID, input.Priority)
		}

		readErr := readLoop(ctx, streamID, bufferWriteID, input, r, buf, &cb, norm)
		_ = r.Close()

		if ctx.Err() != nil {
			return
		}

		if done := handleReadError(ctx, streamID, input, readErr, cb, &delay); done {
			return
		}
	}
}

// openSource tries r.Open in a backoff loop until it succeeds or ctx is cancelled.
// On the very first successful open it fires cb.onHandoff exactly once — this is the
// pre-connect handoff that releases the previous ingestor without a buffer gap.
//
// To keep the UI honest while the loop is stuck (typo'd interface, dead host,
// permission denied), the first failure AND every failure once backoff has
// reached its ceiling is surfaced to cb.onInputError so the manager marks
// the input Degraded instead of leaving stale Status=Active. The transient
// in-between failures are suppressed to avoid event spam during normal
// network blips.
//
// Returns false when ctx is done (caller must return).
func openSource(
	ctx context.Context,
	streamID domain.StreamCode,
	input domain.Input,
	r PacketReader,
	cb pullWorkerCallbacks,
	delay *time.Duration,
	handedOff *bool,
) bool {
	openFails := 0
	for {
		// r.Open is context-aware; if ctx is already done it returns immediately.
		err := r.Open(ctx)
		if err == nil {
			fireHandoffOnce(cb, handedOff)
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		openFails++
		if !handleOpenFailure(ctx, streamID, input, err, cb, openFails, delay) {
			return false
		}
	}
}

// fireHandoffOnce invokes cb.onHandoff exactly once across the lifetime of a
// pull worker — on its first successful Open. Subsequent reconnects do not
// re-fire it because there is no previous worker left to release.
func fireHandoffOnce(cb pullWorkerCallbacks, handedOff *bool) {
	if *handedOff {
		return
	}
	*handedOff = true
	if cb.onHandoff != nil {
		cb.onHandoff()
	}
}

// firstSessionReasonFor picks the SessionStartReason to use on a freshly
// scheduled pull worker's first Open. A non-nil prevCancel means there's
// an active predecessor being handed off — that is a failover by Stream
// Manager. A nil prevCancel means this is the first worker for this stream
// in this Service's lifetime — a fresh cold start.
func firstSessionReasonFor(prevCancel context.CancelFunc) domain.SessionStartReason {
	if prevCancel != nil {
		return domain.SessionStartFailover
	}
	return domain.SessionStartFresh
}

// mintSessionForOpen tells the buffer hub that a new StreamSession is
// starting AND resets the Normaliser's per-track anchor state to match.
// firstOpenDone distinguishes the two cases: false means this is the very
// first successful Open for this worker (use the caller-provided
// firstSessionReason — Fresh or Failover); true means we are re-opening
// within the same worker after a transient read error (always Reconnect).
// The worker doesn't have init params on hand at this point — SPS/PPS / ASC
// live inside the per-reader state and arrive on the first keyframe — so we
// pass nil configs; downstream consumers fall back to in-band parsing
// exactly as they do today.
func mintSessionForOpen(
	streamID, bufferWriteID domain.StreamCode,
	buf *buffer.Service,
	cb *pullWorkerCallbacks,
	firstOpenDone bool,
	norm *timeline.Normaliser,
) {
	reason := domain.SessionStartReconnect
	if !firstOpenDone {
		reason = cb.firstSessionReason
		if reason == domain.SessionStartUnknown {
			reason = domain.SessionStartFresh
		}
	}
	sess := buf.SetSession(bufferWriteID, reason, nil, nil)
	if sess == nil {
		return
	}
	// Reset the Normaliser's per-track anchor state in lockstep with the
	// session boundary. The very next AV packet re-seeds at elapsed=0
	// against the fresh wallclock origin.
	norm.OnSession(sess)
	slog.Debug("ingestor: stream session minted",
		"stream_code", streamID,
		"buffer_id", bufferWriteID,
		"session_id", sess.ID,
		"reason", sess.Reason.String(),
	)
	if cb.onSession != nil {
		cb.onSession(streamID, sess)
	}
}

// handleOpenFailure logs the failure, optionally reports it to the manager,
// and waits the current backoff. Returns true if the caller should retry,
// false if ctx was cancelled during backoff.
func handleOpenFailure(
	ctx context.Context,
	streamID domain.StreamCode,
	input domain.Input,
	err error,
	cb pullWorkerCallbacks,
	attempt int,
	delay *time.Duration,
) bool {
	slog.Error("ingestor: open failed",
		"stream_code", streamID,
		"input_priority", input.Priority,
		"url", input.URL,
		"err", err,
		"attempt", attempt,
	)
	// Surface to manager on the first attempt so the UI flips to Degraded
	// immediately, then again once backoff has plateaued so recovery never
	// goes unreported during long outages.
	if cb.onInputError != nil && (attempt == 1 || *delay >= reconnectMaxDelay) {
		cb.onInputError(streamID, input.Priority, err)
	}
	if !waitBackoff(ctx, *delay) {
		return false
	}
	*delay = minDur(*delay*2, reconnectMaxDelay)
	return true
}

// handleReadError inspects the result of readLoop and decides whether to retry.
// Returns true when the worker should stop entirely, false when it should reconnect.
func handleReadError(
	ctx context.Context,
	streamID domain.StreamCode,
	input domain.Input,
	readErr error,
	cb pullWorkerCallbacks,
	delay *time.Duration,
) bool {
	if errors.Is(readErr, io.EOF) {
		// Source ended cleanly (file finished, live stream closed by server).
		// Notify the manager so it can immediately failover to a backup input
		// rather than waiting for the packet-timeout to expire.
		slog.Info("ingestor: source ended (EOF)",
			"stream_code", streamID,
			"input_priority", input.Priority,
		)
		if cb.onInputError != nil {
			cb.onInputError(streamID, input.Priority, io.EOF)
		}
		return true
	}

	slog.Warn("ingestor: read error, reconnecting",
		"stream_code", streamID,
		"input_priority", input.Priority,
		"err", readErr,
		"backoff", *delay,
	)
	if shouldFailoverImmediately(readErr) {
		slog.Warn("ingestor: non-retriable source error, stop and trigger failover",
			"stream_code", streamID,
			"input_priority", input.Priority,
			"err", readErr,
		)
		if cb.onInputError != nil {
			cb.onInputError(streamID, input.Priority, readErr)
		}
		return true
	}

	if cb.onReconnect != nil {
		cb.onReconnect(streamID, input.Priority, readErr)
	}
	if !waitBackoff(ctx, *delay) {
		return true
	}
	*delay = minDur(*delay*2, reconnectMaxDelay)
	return false
}

// readLoop reads from reader until error or ctx cancellation.
//
// PTS/DTS anchoring is delegated to the worker-scoped Normaliser (passed
// in from runPullWorker). State resets between reconnects happen at the
// session boundary, not at readLoop construction — see mintSessionForOpen.
//
// For raw-TS sources (AVCodecRawTSChunk) the data path bypasses demux/remux,
// which leaves the manager's input "tracks" panel empty. A side-channel
// StatsDemuxer is lazily initialised on the first raw chunk to surface
// codec / bitrate / resolution into onMedia without touching the data path.
func readLoop(
	ctx context.Context,
	streamID domain.StreamCode,
	bufferWriteID domain.StreamCode,
	input domain.Input,
	r PacketReader,
	buf *buffer.Service,
	cb *pullWorkerCallbacks,
	norm *timeline.Normaliser,
) error {
	var stats *pull.StatsDemuxer
	defer func() {
		if stats != nil {
			stats.Close()
		}
	}()
	for {
		batch, err := r.ReadPackets(ctx)
		if err != nil {
			return err
		}
		for _, p := range batch {
			if len(p.Data) == 0 {
				continue
			}
			ensureStatsDemuxer(&stats, &p, streamID, input.Priority, cb)
			wctx := writeContext{
				streamID:      streamID,
				bufferWriteID: bufferWriteID,
				input:         input,
				buf:           buf,
				cb:            cb,
				stats:         stats,
				normaliser:    norm,
			}
			if writeErr := writeOnePacket(wctx, &p); writeErr != nil {
				return writeErr
			}
		}
	}
}

// ensureStatsDemuxer lazily allocates the side-channel demuxer the first
// time a raw-TS chunk arrives AND the manager wants media info. AV-source
// readers (RTSP / RTMP) hit the AV path which already feeds onMedia, so
// they never trigger allocation here.
func ensureStatsDemuxer(
	stats **pull.StatsDemuxer,
	p *domain.AVPacket,
	streamID domain.StreamCode,
	priority int,
	cb *pullWorkerCallbacks,
) {
	if *stats != nil {
		return
	}
	if p.Codec != domain.AVCodecRawTSChunk {
		return
	}
	if cb == nil || cb.onMedia == nil {
		return
	}
	onMedia := cb.onMedia
	*stats = pull.NewStatsDemuxer(func(av *domain.AVPacket) {
		onMedia(streamID, priority, av)
	})
}

// writeContext bundles the per-packet write parameters that don't change
// across iterations. Centralised so writeOnePacket / writeRawTSChunk stay
// under the cognitive-complexity / parameter-count ceilings.
type writeContext struct {
	streamID      domain.StreamCode
	bufferWriteID domain.StreamCode
	input         domain.Input
	buf           *buffer.Service
	cb            *pullWorkerCallbacks
	stats         *pull.StatsDemuxer
	normaliser    *timeline.Normaliser // nil-safe; only acts on AV path
}

// writeOnePacket forwards one source packet into the buffer, dispatching on
// the marker codec. Raw-TS sources (UDP/HLS/SRT/File via TSPassthroughPacketReader)
// emit AVCodecRawTSChunk so we write Packet.TS to preserve the original bytes;
// AV sources (RTSP/RTMP) write Packet.AV. Extracted from readLoop to keep
// each function under the cognitive-complexity ceiling.
//
// The per-packet "first packet of a fresh source" cue is no longer set
// on the AVPacket — downstream consumers read SessionStart=true from the
// buffer.Packet wrapper, which buffer.Service auto-stamps after the
// preceding SetSession call (see mintSessionForOpen).
//
// wctx.stats (raw-TS path) is supplied by readLoop so onMedia fires for raw
// chunks too — without that, the manager's input "tracks" panel stays empty
// for every UDP / HTTP-MPEG-TS / SRT / File source.
func writeOnePacket(wctx writeContext, p *domain.AVPacket) error {
	if p.Codec == domain.AVCodecRawTSChunk {
		return writeRawTSChunk(wctx, p.Data)
	}
	cl := p.Clone()
	// Anchor PTS/DTS via the Normaliser. Returns false when the
	// Normaliser dropped the packet (input running ahead of wallclock
	// past MaxAheadMs); in that case skip the buffer write so downstream
	// consumers see a wallclock-paced stream.
	if !wctx.normaliser.Apply(cl, time.Now()) {
		return nil
	}
	if err := wctx.buf.Write(wctx.bufferWriteID, buffer.Packet{AV: cl}); err != nil {
		slog.Error("ingestor: buffer write failed",
			"stream_code", wctx.streamID,
			"input_priority", wctx.input.Priority,
			"err", err,
		)
		return err
	}
	cb := wctx.cb
	if cb != nil && cb.onPacket != nil {
		cb.onPacket(wctx.streamID, wctx.input.Priority)
	}
	if cb != nil && cb.onPacketBytes != nil {
		cb.onPacketBytes(wctx.streamID, wctx.input.Priority, len(p.Data))
	}
	if cb != nil && cb.onMedia != nil {
		// Observer must not retain p.Data; if it needs persisted bytes
		// (e.g. SPS extracted on a keyframe) it should copy them out itself.
		pCopy := *p
		cb.onMedia(wctx.streamID, wctx.input.Priority, &pCopy)
	}
	return nil
}

// writeRawTSChunk handles the AVCodecRawTSChunk path — copies the chunk so
// the source-side reader buffer can be reused, writes Packet.TS to the buffer
// hub, fires the byte-count + packet-count observers, and tees a copy into
// the side-channel stats demuxer so onMedia (codec / bitrate / resolution)
// fires for raw-TS sources too.
func writeRawTSChunk(wctx writeContext, chunk []byte) error {
	cp := append([]byte(nil), chunk...)
	if err := wctx.buf.Write(wctx.bufferWriteID, buffer.Packet{TS: cp}); err != nil {
		slog.Error("ingestor: buffer write failed (TS passthrough)",
			"stream_code", wctx.streamID,
			"input_priority", wctx.input.Priority,
			"err", err,
		)
		return err
	}
	cb := wctx.cb
	if cb != nil && cb.onPacket != nil {
		cb.onPacket(wctx.streamID, wctx.input.Priority)
	}
	if cb != nil && cb.onPacketBytes != nil {
		cb.onPacketBytes(wctx.streamID, wctx.input.Priority, len(cp))
	}
	// Best-effort: stats demuxer drops on backpressure, never blocks the
	// data path. Feed the copy we already own so the source reader can
	// reuse `chunk`'s underlying buffer.
	wctx.stats.Feed(cp)
	return nil
}

// waitBackoff sleeps for d, returning false if ctx is cancelled first.
func waitBackoff(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func shouldFailoverImmediately(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// TLS / x509 / DNS — source-side configuration errors that won't
	// recover by reconnect. Without this branch the worker spins in the
	// reconnect loop forever, manager never sees the input as degraded,
	// and failover to a backup input never fires (e.g. an HLS source with
	// an untrusted CA cert: previously cycled "fetch failed, retrying"
	// every few seconds without surfacing as an input error).
	if strings.Contains(msg, "x509:") ||
		strings.Contains(msg, "tls:") ||
		strings.Contains(msg, "no such host") {
		return true
	}
	m := httpStatusPattern.FindStringSubmatch(msg)
	if len(m) != 2 {
		return false
	}
	code, convErr := strconv.Atoi(m[1])
	if convErr != nil {
		return false
	}
	switch code {
	case 401, 403, 404, 410, 429, 500, 502, 503, 504:
		return true
	default:
		return false
	}
}
