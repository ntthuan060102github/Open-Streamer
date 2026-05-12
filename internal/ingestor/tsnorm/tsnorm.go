// Package tsnorm wallclock-anchors the PTS / DTS of an upstream MPEG-TS
// chunk by running every PES through timeline.Normaliser and remuxing.
//
// # Why this exists
//
// The AV-path (`internal/ingestor/worker.writeOnePacket`) already routes
// each domain.AVPacket through a per-stream timeline.Normaliser before
// writing to the buffer hub, so consumers (DASH packager, HLS segmenter,
// RTSP / RTMP re-stream) see one consistent wallclock-anchored timeline.
//
// The raw-TS path (`writeRawTSChunk`) historically passed bytes through
// untouched. Sources delivered via the raw-TS path (UDP, HLS-pull, SRT,
// file, copy://, mixer://) carry whatever PTS the upstream encoder
// chose: clock drift, sudden CDN-resync jumps, V/A skew on mixer-style
// composition, occasional 0-second stalls. All of that propagated
// directly into downstream timeline math — root cause of MPD overlap,
// audio under-emit, large segment durs, "DASH live edge ahead of
// publishTime", and player freezes documented in
// docs/DASH_OUTSTANDING_BUGS.md.
//
// Pipeline
//
//	chunk → mpeg2.TSDemuxer    ─OnFrame→ domain.AVPacket
//	                                          │
//	                          timeline.Normaliser.Apply (drop if Apply=false)
//	                                          │
//	                                  tsmux.FromAV.Write
//	                                          │
//	                                ─onPacket→ accumulated TS bytes → return
//
// Per-stream state lives in Normaliser; one Process call is atomic
// (mutex guarded so the demuxer / muxer / output buffer aren't shared
// across concurrent calls).
//
// On a stream-session boundary (failover, reconnect, stall recovery)
// the caller should invoke OnSession to rebuild the muxer (fresh
// continuity counters + PMT) and reset the Normaliser's per-track
// anchors so the first post-boundary packet is rebased against the
// new wallclock origin instead of being either dropped or hard
// re-anchored against stale state.
package tsnorm

import (
	"bytes"
	"sync"
	"time"

	gocodec "github.com/yapingcat/gomedia/go-codec"
	gompeg2 "github.com/yapingcat/gomedia/go-mpeg2"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/timeline"
	"github.com/ntt0601zcoder/open-streamer/internal/tsmux"
)

// Normaliser wallclock-anchors the per-PES PTS / DTS values inside one
// stream's MPEG-TS byte feed. NOT safe for concurrent Process calls —
// the internal mutex serialises them; callers running in a single
// readLoop goroutine pay no contention cost.
type Normaliser struct {
	mu sync.Mutex

	norm    *timeline.Normaliser
	muxer   *tsmux.FromAV
	demuxer *gompeg2.TSDemuxer

	// outBuf collects 188-byte TS packets emitted by the muxer during
	// the current Process call. Reset on every Process entry.
	outBuf bytes.Buffer
}

// New constructs a Normaliser with the given timeline config. The
// caller usually passes timeline.DefaultConfig().
func New(cfg timeline.Config) *Normaliser {
	n := &Normaliser{
		norm:    timeline.New(cfg),
		muxer:   tsmux.NewFromAV(),
		demuxer: gompeg2.NewTSDemuxer(),
	}
	n.demuxer.OnFrame = n.onFrame
	return n
}

// Process feeds one upstream TS chunk through the demux → normalise →
// mux pipeline and returns the remuxed bytes. The returned slice is
// safe to retain across Process calls (a fresh allocation, not aliasing
// the internal buffer). Empty return when no PES yielded output (the
// chunk contained only PSI, or every frame failed Apply).
//
// Returns an error only when the demuxer fails to parse a recognisable
// TS sync byte sequence — the caller can use that to decide whether to
// reconnect upstream.
func (n *Normaliser) Process(chunk []byte) ([]byte, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.outBuf.Reset()
	if err := n.demuxer.Input(bytes.NewReader(chunk)); err != nil {
		return nil, err
	}
	if n.outBuf.Len() == 0 {
		return nil, nil
	}
	out := make([]byte, n.outBuf.Len())
	copy(out, n.outBuf.Bytes())
	return out, nil
}

// OnSession resets per-track timeline anchors so the first post-
// boundary packet seeds against a fresh wallclock origin, and rebuilds
// the muxer so output continuity counters + PSI restart cleanly.
//
// nil session is a no-op (matches timeline.Normaliser.OnSession
// semantics).
func (n *Normaliser) OnSession(sess *domain.StreamSession) {
	if sess == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.norm.OnSession(sess)
	n.muxer = tsmux.NewFromAV()
}

// onFrame is the TSDemuxer callback fired synchronously during
// demuxer.Input. Caller holds n.mu through Process so this runs
// inside that critical section.
func (n *Normaliser) onFrame(cid gompeg2.TS_STREAM_TYPE, frame []byte, pts, dts uint64) {
	av, ok := buildAVPacket(cid, frame, pts, dts)
	if !ok {
		return
	}
	if !n.norm.Apply(av, time.Now()) {
		return
	}
	n.muxer.Write(av, func(b []byte) {
		n.outBuf.Write(b)
	})
}

// buildAVPacket constructs a domain.AVPacket from a TSDemuxer frame
// callback. Returns ok=false for stream types we don't currently
// re-mux through tsmux.FromAV (every supported codec maps here).
//
// Duplicated from ingestor/pull/tsdemux_packet_reader.go to avoid a
// circular import — both packages live under internal/ingestor and
// adding a shared subpackage purely for one helper isn't worth the
// new directory.
func buildAVPacket(cid gompeg2.TS_STREAM_TYPE, frame []byte, pts, dts uint64) (*domain.AVPacket, bool) {
	var cdc domain.AVCodec
	var keyFrame bool
	switch cid { //nolint:exhaustive // MPEG audio (MP1/MP2/MP3) and AC-3 variants intentionally drop — tsmux.FromAV doesn't accept them via the AVCodec switch and rebuilding a passthrough path purely for codec types we never re-mux isn't worth the complexity.
	case gompeg2.TS_STREAM_H264:
		cdc = domain.AVCodecH264
		keyFrame = gocodec.IsH264IDRFrame(frame)
	case gompeg2.TS_STREAM_H265:
		cdc = domain.AVCodecH265
		keyFrame = gocodec.IsH265IDRFrame(frame)
	case gompeg2.TS_STREAM_AAC:
		cdc = domain.AVCodecAAC
	default:
		return nil, false
	}
	return &domain.AVPacket{
		Codec:    cdc,
		Data:     bytes.Clone(frame),
		PTSms:    pts,
		DTSms:    dts,
		KeyFrame: keyFrame,
	}, true
}
