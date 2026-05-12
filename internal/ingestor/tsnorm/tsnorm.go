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
// # Pipeline
//
//	chunk → gomedia.TSDemuxer    ─OnFrame→ domain.AVPacket
//	                                          │
//	                          timeline.Normaliser.Apply (drop if Apply=false)
//	                                          │
//	                                astits.Muxer.WriteData
//	                          (RandomAccessIndicator=true on H.264 / H.265 IDR
//	                           forces PAT/PMT to be emitted IMMEDIATELY before
//	                           the IDR PES — closes the HLS "non-existing PPS
//	                           referenced" decoder-error window that gomedia's
//	                           fixed 400 ms PSI cadence left open)
//	                                          │
//	                                ─io.Writer→ outBuf → return
//
// Per-stream state lives in Normaliser; one Process call is atomic
// (mutex guarded so the demuxer / muxer / output buffer aren't shared
// across concurrent calls).
//
// # Library split
//
// Input demux still uses gomedia/go-mpeg2.TSDemuxer because its
// callback-style OnFrame fits the per-Process synchronous flow:
// astits's pull-only NextData API requires either a long-lived
// goroutine + chan-backed reader (complex sync) or a fresh demuxer
// per chunk (which loses cross-chunk PES assembly state for sources
// whose chunks aren't aligned to PES boundaries). Output mux uses
// astits.Muxer because its `RandomAccessIndicator → force-tables`
// path is the exact escape hatch gomedia's TSMuxer lacks. Migrating
// the demuxer side to astits is tracked as future work; gomedia's
// remaining footprint in this package is the input boundary only.
//
// # Session boundary
//
// On a stream-session boundary (failover, reconnect, stall recovery)
// the caller should invoke OnSession to rebuild the muxer (fresh
// continuity counters + PSI) and reset the Normaliser's per-track
// anchors so the first post-boundary packet is rebased against the
// new wallclock origin instead of being either dropped or hard
// re-anchored against stale state.
package tsnorm

import (
	"bytes"
	"context"
	"sync"
	"time"

	"github.com/asticode/go-astits"
	gocodec "github.com/yapingcat/gomedia/go-codec"
	gompeg2 "github.com/yapingcat/gomedia/go-mpeg2"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/timeline"
)

// Output PID convention. Matches tsmux.FromAV so diagnostic tools see
// the same PIDs across all our TS-emitting paths. H.265 sits on a
// dedicated PID so it can be lazy-added when (and only when) an H.265
// frame actually arrives — avoids a phantom HEVC entry in the PMT for
// H.264-only sources (which strict demuxers like ffprobe surface as
// a "second video stream" and which broke the DASH packager's
// init-segment build state in early s3-s4 testing).
const (
	videoPID = 0x100 // H.264
	audioPID = 0x101 // AAC
	h265PID  = 0x102 // H.265 (lazy-added on first H.265 frame)
)

// muxerTablesRetransmitPeriod is the PAT/PMT retransmit cadence in PES
// packets. The keyframe-triggered force-emit (RandomAccessIndicator
// in the adaptation field) is the primary mechanism; this catches the
// gap between IDRs for sources with very long GOPs.
const muxerTablesRetransmitPeriod = 40

// Normaliser wallclock-anchors the per-PES PTS / DTS values inside one
// stream's MPEG-TS byte feed. NOT safe for concurrent Process calls —
// the internal mutex serialises them; callers running in a single
// readLoop goroutine pay no contention cost.
type Normaliser struct {
	mu sync.Mutex

	norm    *timeline.Normaliser
	demuxer *gompeg2.TSDemuxer

	// Mux pipeline state. The astits.Muxer writes to outBuf via the
	// bufWriter; we drain outBuf at the end of each Process call.
	muxer  *astits.Muxer
	outBuf bytes.Buffer

	// Tracks whether the canonical H.264 + AAC streams have been
	// registered on the muxer (pre-registration happens in
	// buildMuxer; this flag is just for symmetry with the H.265
	// lazy-register path).
	hasH265 bool
}

// New constructs a Normaliser with the given timeline config. The
// caller usually passes timeline.DefaultConfig().
//
// astits's RandomAccessIndicator on PCR-PID PES auto-emits PAT + PMT
// immediately before that PES. We set RAI on every H.264 / H.265
// keyframe so each IDR is preceded by fresh PSI — that's what fixes
// the HLS "non-existing PPS 0 referenced" decode failures we hit when
// gomedia's fixed 400 ms PAT cadence happened to land mid-GOP.
func New(cfg timeline.Config) *Normaliser {
	n := &Normaliser{
		norm:    timeline.New(cfg),
		demuxer: gompeg2.NewTSDemuxer(),
	}
	n.buildMuxer()
	n.demuxer.OnFrame = n.onFrame
	return n
}

// buildMuxer (re)builds the astits.Muxer and pre-registers the
// canonical H.264 + AAC elementary streams. Called from New and
// after OnSession so post-boundary output starts with fresh
// continuity counters + PSI version.
//
// H.264 + AAC are pre-registered (PMT lists them from byte 0). H.265
// is registered lazily on first H.265 frame to avoid a phantom HEVC
// PID confusing strict downstream demuxers on H.264-only sources.
func (n *Normaliser) buildMuxer() {
	n.outBuf.Reset()
	n.muxer = astits.NewMuxer(
		context.Background(),
		bufWriter{buf: &n.outBuf},
		astits.MuxerOptTablesRetransmitPeriod(muxerTablesRetransmitPeriod),
	)
	_ = n.muxer.AddElementaryStream(astits.PMTElementaryStream{
		ElementaryPID: videoPID,
		StreamType:    astits.StreamTypeH264Video,
	})
	_ = n.muxer.AddElementaryStream(astits.PMTElementaryStream{
		ElementaryPID: audioPID,
		StreamType:    astits.StreamTypeAACAudio,
	})
	n.muxer.SetPCRPID(videoPID)
	n.hasH265 = false
}

// bufWriter is the io.Writer astits emits TS bytes into. Process()
// drains it on every call so it never grows unbounded.
type bufWriter struct{ buf *bytes.Buffer }

func (w bufWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }

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
// Pre-registration of H.264 + AAC is re-applied so the post-boundary
// PMT lists both PIDs from the first PMT write.
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
	n.buildMuxer()
}

// onFrame is the TSDemuxer callback fired synchronously during
// demuxer.Input. Caller holds n.mu through Process so this runs
// inside that critical section.
//
// Dispatches the incoming PES payload to the correct astits muxer
// PID. Unsupported codecs (MP1/MP2/MP3 audio, AC-3, etc.) are
// silently dropped — buildAVPacket signals that via ok=false.
func (n *Normaliser) onFrame(cid gompeg2.TS_STREAM_TYPE, frame []byte, pts, dts uint64) {
	av, ok := buildAVPacket(cid, frame, pts, dts)
	if !ok {
		return
	}
	if !n.norm.Apply(av, time.Now()) {
		return
	}
	n.writeMuxed(av)
}

// writeMuxed routes a normalised AVPacket to the astits muxer's
// PID for its codec and sets RandomAccessIndicator on H.26x
// keyframes (which triggers astits to emit fresh PAT/PMT before
// this PES — the HLS fix this package exists for).
//
// H.265 PID is added lazily here so streams that never carry H.265
// don't get a phantom HEVC entry in their PMT.
func (n *Normaliser) writeMuxed(av *domain.AVPacket) {
	var (
		pid     uint16
		isVideo bool
	)
	switch av.Codec { //nolint:exhaustive // buildAVPacket constrains the codec set; unreachable codecs return early
	case domain.AVCodecH264:
		pid = videoPID
		isVideo = true
	case domain.AVCodecH265:
		if !n.hasH265 {
			_ = n.muxer.AddElementaryStream(astits.PMTElementaryStream{
				ElementaryPID: h265PID,
				StreamType:    astits.StreamTypeH265Video,
			})
			// Force PMT to refresh now so downstream demuxers learn the
			// HEVC PID before the first H.265 PES bytes hit the wire.
			// Without this the next PES would land on an unannounced PID
			// (the existing PMT only listed H.264 + AAC at construction)
			// and downstream demuxers silently drop it.
			_, _ = n.muxer.WriteTables()
			n.hasH265 = true
		}
		pid = h265PID
		isVideo = true
	case domain.AVCodecAAC:
		pid = audioPID
	default:
		return
	}

	indicator := uint8(astits.PTSDTSIndicatorOnlyPTS)
	dts := av.DTSms
	if dts == 0 {
		dts = av.PTSms
	}
	if dts != av.PTSms {
		indicator = astits.PTSDTSIndicatorBothPresent
	}

	var af *astits.PacketAdaptationField
	if isVideo && av.KeyFrame {
		af = &astits.PacketAdaptationField{RandomAccessIndicator: true}
	}

	_, _ = n.muxer.WriteData(&astits.MuxerData{
		PID:             pid,
		AdaptationField: af,
		PES: &astits.PESData{
			Data: av.Data,
			Header: &astits.PESHeader{
				OptionalHeader: &astits.PESOptionalHeader{
					PTS:             &astits.ClockReference{Base: int64(av.PTSms) * 90}, //nolint:gosec
					DTS:             &astits.ClockReference{Base: int64(dts) * 90},      //nolint:gosec
					PTSDTSIndicator: indicator,
				},
			},
		},
	})
}

// buildAVPacket constructs a domain.AVPacket from a TSDemuxer frame
// callback. Returns ok=false for stream types we don't currently
// re-mux (every supported codec maps here).
//
// Duplicated from ingestor/pull/tsdemux_packet_reader.go to avoid a
// circular import — both packages live under internal/ingestor and
// adding a shared subpackage purely for one helper isn't worth the
// new directory.
func buildAVPacket(cid gompeg2.TS_STREAM_TYPE, frame []byte, pts, dts uint64) (*domain.AVPacket, bool) {
	var cdc domain.AVCodec
	var keyFrame bool
	switch cid { //nolint:exhaustive // MPEG audio (MP1/MP2/MP3) and AC-3 variants intentionally drop — tsnorm's muxer pre-registers only H.264 / H.265 / AAC, and rebuilding a passthrough path purely for codec types we never re-mux isn't worth the complexity.
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
