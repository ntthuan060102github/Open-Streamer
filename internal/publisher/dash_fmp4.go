package publisher

// dash_fmp4.go — ISO BMFF (fMP4) packager for live MPEG-DASH.
//
// Data flow:
//
//	Buffer → FeedWirePacket → aligned TS → io.Pipe → mpeg2.TSDemuxer
//	                                                         │
//	                        ┌────────────────────────────────┤
//	                        │ onTSFrame (H.264/H.265 AU, AAC ADTS) │
//	                        └────────────────────────────────┘
//	                                         │
//	                          queue: vAnnex/vDTS/vPTS, aRaw/aPTS
//	                                         │
//	                     ticker ─► flushSegmentLocked
//	                                         │
//	                          init_v.mp4 / init_a.mp4
//	                          seg_v_NNNNN.m4s / seg_a_NNNNN.m4s
//	                          index.mpd (per-shard or root via abrMaster)
//
// Codec support: H.264 + H.265 + AAC-LC.  MP3 is logged and skipped.
//
// Segment trigger: queued video duration ≥ segSec (in 90 kHz ticks) AND the
// queue starts with an IDR, OR wall-clock elapsed ≥ segSec (fallback for
// audio-only or streams with wide keyframe intervals).

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/hevc"
	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/prometheus/client_golang/prometheus"
	mpeg2 "github.com/yapingcat/gomedia/go-mpeg2"

	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/tsmux"
	"github.com/ntt0601zcoder/open-streamer/pkg/logger"
)

// dashVideoTimescale is the standard MPEG timescale for video tracks (90 kHz).
const dashVideoTimescale = 90000

// SegmentTemplate $Number$ patterns used by DASH clients (Shaka, dash.js, …).
const (
	dashVideoMediaPattern = `seg_v_$Number%05d$.m4s`
	dashAudioMediaPattern = `seg_a_$Number%05d$.m4s`

	// dashSourceSwitchJumpMs is the max tolerated inter-frame DTS/PTS
	// step (ms) before onTSFrame flushes the current segment. Protects
	// writeVideoSegmentLocked's per-sample `dur = (vDTS[i+1]-vDTS[i])*90`
	// math from amplifying a single discontinuity into a giant sample
	// (and a permanent +N seconds bump in videoNextDecode / tfdt).
	// Symmetric: backward step ⇒ source switch / wrap, forward step ⇒
	// CDN playlist resync, NVENC stall recovery, transcoder restart.
	dashSourceSwitchJumpMs = 1000
)

// dashRunOpts carries per-rendition ABR metadata.  nil = single-rendition mode.
type dashRunOpts struct {
	abrMaster         *dashABRMaster
	abrSlug           string
	videoBandwidthBps int
	packAudio         bool
	// segCount counts {video, audio} segment writes — pre-bound to
	// {stream_code,format=dash,profile} so the hot path is map-lookup-free.
	segCount prometheus.Counter
	// segWriteDur observes os.WriteFile latency for both audio and video
	// segments (label format="dash"). Single observer covers both because
	// the "where is the disk slow" question doesn't differ by track type.
	segWriteDur prometheus.Observer
}

// runDASHFMP4Packager is the entry point used by both serveDASH and serveDASHAdaptive.
func runDASHFMP4Packager(
	ctx context.Context,
	streamID domain.StreamCode,
	sub *buffer.Subscriber,
	streamDir string,
	manifestPath string,
	segSec, window, history int,
	ephemeral bool,
	opts *dashRunOpts,
) {
	if segSec <= 0 {
		segSec = domain.DefaultLiveSegmentSec
	}
	if window <= 0 {
		window = domain.DefaultLiveWindow
	}
	if history < 0 {
		history = domain.DefaultLiveHistory
	}

	p := &dashFMP4Packager{
		streamID:     streamID,
		streamDir:    streamDir,
		manifestPath: manifestPath,
		segSec:       segSec,
		window:       window,
		history:      history,
		ephemeral:    ephemeral,
		videoCodec:   "avc1.42E01E",
		audioCodec:   "mp4a.40.2",
		width:        1280,
		height:       720,
		packAudio:    true,
	}
	if opts != nil {
		p.abrMaster = opts.abrMaster
		p.abrSlug = opts.abrSlug
		if opts.videoBandwidthBps > 0 {
			p.videoBW = opts.videoBandwidthBps
		}
		p.packAudio = opts.packAudio
		p.segCount = opts.segCount
		p.segWriteDur = opts.segWriteDur
	}

	p.run(ctx, sub)
}

// dashFMP4Packager holds the mutable per-stream state for one DASH output shard.
type dashFMP4Packager struct {
	streamID     domain.StreamCode
	streamDir    string
	manifestPath string // empty when abrMaster writes the root MPD
	segSec       int
	window       int
	history      int
	ephemeral    bool

	mu sync.Mutex

	segmentStart time.Time

	// Video frame queue — Annex-B access units + ms timestamps from TSDemuxer.
	vAnnex  [][]byte
	vDTS    []uint64 // ms
	vPTS    []uint64 // ms
	videoPS []byte   // aggregate SPS+PPS bytes for init detection (cleared after init)

	// videoPSGiveUp is set when the accumulated SPS/PPS buffer exceeded
	// videoPSMaxBytes without a successful init — at that point we stop
	// appending new frames into videoPS so the buffer doesn't keep
	// growing forever on a source that never emits a parseable parameter
	// set. The DASH stream stays video-init-less in that case (audio-only
	// playback if audio is present) which is far better than OOM.
	videoPSGiveUp bool

	// Audio frame queue — raw AAC (ADTS header stripped).
	aRaw [][]byte
	aPTS []uint64 // ms

	// Init segments (written once on first codec header detection).
	videoInit *mp4.InitSegment
	audioInit *mp4.InitSegment
	videoTID  uint32
	audioTID  uint32
	audioSR   int // sample rate in Hz

	// Codec strings for MPD representations.
	videoCodec string // e.g. "avc1.4d401f"
	audioCodec string // "mp4a.40.2"
	width      int
	height     int

	// isHEVC is set once the video stream is identified as H.265.
	isHEVC bool

	// Segment counters (1-based, match $Number$ in filenames).
	vSegN uint64
	aSegN uint64

	// Continuous decode timeline (tfdt) in track timescale.
	videoNextDecode uint64 // 90 kHz
	audioNextDecode uint64 // audioSR Hz

	// Common-origin DTS used to seed the per-track tfdt offsets so audio
	// and video share a single timeline reference. Without this, both
	// nextDecode counters start at 0 regardless of the source's actual
	// inter-track offset, baking any pre-roll skew (mp4 video DTS=-33ms
	// for B-frame setup while audio DTS=0; HLS pull where audio starts
	// 100ms before the first IDR; etc.) permanently into the published
	// stream as observable A/V drift.
	originDTSms        uint64 // ms — first DTS seen on any track
	originDTSmsSet     bool
	videoFirstDTSmsSet bool // true once videoNextDecode has been seeded
	audioFirstDTSmsSet bool // true once audioNextDecode has been seeded

	// Sliding-window state for MPD generation.
	onDiskV    []string // filenames of written video segments
	onDiskA    []string // filenames of written audio segments
	vSegDurs   []uint64 // per-segment duration in 90 kHz ticks
	aSegDurs   []uint64 // per-segment duration in audioSR ticks
	vSegStarts []uint64 // tfdt of first sample per video segment
	aSegStarts []uint64 // tfdt of first sample per audio segment

	// Set on first segment available; needed for MPD availabilityStartTime.
	availabilityStart time.Time

	// ABR wiring.
	abrMaster *dashABRMaster
	abrSlug   string
	videoBW   int  // override bandwidth for ABR root MPD
	packAudio bool // false for non-best ABR renditions

	// Pre-bound DASH segment counter (covers both video + audio writes); nil-safe.
	segCount prometheus.Counter
	// Pre-bound segment-write-duration histogram observer; nil-safe.
	segWriteDur prometheus.Observer
}

// dashStreamTypeForCodec maps an AV packet codec to the gomedia TS stream
// type that onTSFrame's switch statement understands. Returns ok=false for
// codecs the DASH packager can't handle directly (e.g. RawTSChunk, AC3) —
// those fall through to the raw-TS pipeline if the source ever produces
// pkt.TS bytes.
func dashStreamTypeForCodec(c domain.AVCodec) (mpeg2.TS_STREAM_TYPE, bool) {
	switch c {
	case domain.AVCodecH264:
		return mpeg2.TS_STREAM_H264, true
	case domain.AVCodecH265:
		return mpeg2.TS_STREAM_H265, true
	case domain.AVCodecAAC:
		return mpeg2.TS_STREAM_AAC, true
	case domain.AVCodecUnknown,
		domain.AVCodecRawTSChunk,
		domain.AVCodecMP2,
		domain.AVCodecMP3,
		domain.AVCodecAC3,
		domain.AVCodecEAC3,
		domain.AVCodecAV1,
		domain.AVCodecMPEG2Video:
		// No direct DASH path — caller falls through to the raw-TS pipeline
		// (which only fires when pkt.TS is non-empty). Listing each codec
		// explicitly so the exhaustive linter catches new codec additions.
		return 0, false
	default:
		return 0, false
	}
}

// run is the main goroutine: wires the TS pipe, demuxer, and flush ticker.
func (p *dashFMP4Packager) run(ctx context.Context, sub *buffer.Subscriber) {
	// tb is a buffered pipe: Write never blocks, Read blocks only when empty.
	// This replaces io.Pipe (unbuffered) which caused the producer goroutine to
	// block on Write while the demuxer held p.mu in onTSFrame, preventing the
	// producer from reading new packets; the buffer hub then dropped them.
	tb := newTSBuffer(p.streamID)

	// Producer: buffer → either DIRECT AV path (bypass TS) or TS roundtrip.
	//
	// AV packets fed via tsmux + gomedia TSDemuxer roundtrip lose SPS/PPS
	// NAL units in the demux callback (gomedia's TSDemuxer.OnFrame for
	// H.264 only delivers slice-bearing access units; standalone SPS/PPS
	// NAL units before the slice are dropped). Without SPS/PPS reaching
	// the dashFMP4Packager, tryInitVideoLocked never finds a parseable
	// parameter set, videoPS grows until the cap, and DASH gives up
	// video init — observed in production for RTMP / RTSP-pull streams
	// (test3, test5). Raw-TS sources (SRT, file://, UDP) work because
	// pkt.TS bypasses tsmux already.
	//
	// Direct path: when pkt.AV is set and the codec maps to a known TS
	// stream type, feed pkt.AV.Data straight into onTSFrame. The AV
	// packet's Annex-B byte stream already contains SPS|PPS|IDR for
	// keyframes (the ingestor pull layers prepend the parameter set on
	// every IDR — see internal/ingestor/pull/rtmp_msg_converter.go and
	// the equivalent in rtsp.go), so the packager has everything it
	// needs without any TS round-trip.
	//
	// Raw-TS path (pkt.TS non-empty, pkt.AV nil) keeps the existing
	// align-to-188-then-write-to-tb pipeline so SRT / file:// /
	// passthrough sources continue to work.
	go func() {
		defer tb.Close()
		var tsCarry []byte

		for {
			select {
			case <-ctx.Done():
				return
			case pkt, ok := <-sub.Recv():
				if !ok {
					return
				}
				// Direct AV path: route past the TS round-trip.
				if pkt.AV != nil && len(pkt.AV.Data) > 0 {
					cid, ok := dashStreamTypeForCodec(pkt.AV.Codec)
					if ok {
						// Discontinuity also resets the demuxer-side carry
						// to keep the raw-TS branch idempotent if a future
						// source mixes both forms — defensive.
						if pkt.AV.Discontinuity {
							tsCarry = nil
						}
						p.onTSFrame(cid, pkt.AV.Data, pkt.AV.PTSms, pkt.AV.DTSms)
						continue
					}
				}
				// Raw-TS path (SRT, file://, UDP): align to 188 boundaries
				// and feed the demuxer via tb.
				if len(pkt.TS) == 0 {
					continue
				}
				tsCarry = append(tsCarry, pkt.TS...)
				for len(tsCarry) >= 188 {
					if tsCarry[0] != 0x47 {
						idx := bytes.IndexByte(tsCarry, 0x47)
						if idx < 0 {
							if len(tsCarry) > 187 {
								tsCarry = tsCarry[len(tsCarry)-187:]
							}
							break
						}
						tsCarry = tsCarry[idx:]
						if len(tsCarry) < 188 {
							break
						}
					}
					if len(tsCarry) >= 376 && tsCarry[188] != 0x47 {
						tsCarry = tsCarry[1:]
						continue
					}
					if _, err := tb.Write(tsCarry[:188]); err != nil {
						return
					}
					tsCarry = tsCarry[188:]
				}
			}
		}
	}()

	// Demuxer: tb → onTSFrame (H.264 AUs and AAC ADTS frames).
	demux := mpeg2.NewTSDemuxer()
	demux.OnFrame = p.onTSFrame

	demuxDone := make(chan error, 1)
	go func() {
		demuxDone <- demux.Input(tb)
	}()

	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()

	segDur := time.Duration(p.segSec) * time.Second

	for {
		select {
		case <-ctx.Done():
			tb.Close()
			<-demuxDone
			p.mu.Lock()
			_ = p.flushSegmentLocked()
			p.mu.Unlock()
			return

		case err := <-demuxDone:
			if err != nil && ctx.Err() == nil {
				slog.Warn("publisher: DASH TS demux ended",
					"stream_code", p.streamID, "err", err)
			}
			p.mu.Lock()
			_ = p.flushSegmentLocked()
			p.mu.Unlock()
			return

		case <-tick.C:
			p.mu.Lock()

			// No data yet.
			if p.videoInit == nil && p.audioInit == nil {
				p.mu.Unlock()
				continue
			}

			wallDue := !p.segmentStart.IsZero() &&
				time.Since(p.segmentStart) >= segDur

			targetV := uint64(p.segSec) * dashVideoTimescale
			queuedV := totalQueuedVideoDur90k(p.vDTS)
			videoByDur := p.videoInit != nil && len(p.vAnnex) > 0 &&
				queuedV >= targetV &&
				(p.vSegN > 0 || p.queuedVideoStartsWithIDRLocked())

			audioOnly := p.videoInit == nil && p.audioInit != nil &&
				len(p.aRaw) >= p.audioFramesPerSegment()

			if wallDue || videoByDur || audioOnly {
				if err := p.flushSegmentLocked(); err != nil {
					slog.Warn("publisher: DASH segment flush failed",
						"stream_code", p.streamID, "err", err)
				}
			}
			p.mu.Unlock()
		}
	}
}

// pendingInitWrite holds init segment data that needs to be written to disk
// outside the hot-path mutex.
type pendingInitWrite struct {
	init     *mp4.InitSegment
	filename string // e.g. "init_v.mp4"
}

// timestampJumpFromLast reports whether ts has stepped past
// dashSourceSwitchJumpMs in either direction relative to the queue tail.
// Returns false for an empty queue (first frame). Shared by H264 / H265 /
// AAC paths in onTSFrame so the threshold and direction logic stay in
// one place.
func timestampJumpFromLast(queue []uint64, ts uint64) bool {
	if len(queue) == 0 {
		return false
	}
	delta := int64(ts) - int64(queue[len(queue)-1]) //nolint:gosec // bounded by upstream timebase
	return delta < -dashSourceSwitchJumpMs || delta > dashSourceSwitchJumpMs
}

// onTSFrame is the TSDemuxer callback; it runs in the demuxer goroutine.
// Init segment file writes are deferred to outside the mutex to avoid blocking
// the flush ticker during expensive I/O.
func (p *dashFMP4Packager) onTSFrame(cid mpeg2.TS_STREAM_TYPE, frame []byte, pts, dts uint64) {
	var pending *pendingInitWrite

	p.mu.Lock()
	switch cid {
	case mpeg2.TS_STREAM_AUDIO_MPEG1, mpeg2.TS_STREAM_AUDIO_MPEG2:
		// MP3 in TS — not supported in DASH fMP4.
		p.mu.Unlock()
		return

	case mpeg2.TS_STREAM_H264:
		if len(frame) == 0 {
			p.mu.Unlock()
			return
		}
		// Detect source-edge or upstream resync: timestamps should advance
		// roughly at frame cadence. A jump in either direction past
		// dashSourceSwitchJumpMs indicates one of:
		//   - backward: source switch / failover, PTS wrap-around — would
		//     underflow (vDTS[i+1]-vDTS[i]) and produce 2^64-ish sample dur.
		//   - forward: CDN HLS playlist resync, NVENC stall recovery,
		//     transcoder restart — would compute one giant sample dur and
		//     bake a permanent +N s offset into videoNextDecode / tfdt
		//     (root cause of "DASH live edge runs N seconds ahead of
		//     wallclock" reports). Flush before appending so the jump
		//     becomes a segment boundary, not an inter-sample dur.
		if timestampJumpFromLast(p.vDTS, dts) {
			_ = p.flushSegmentLocked()
		}
		p.recordOriginDTSLocked(dts)
		cp := append([]byte(nil), frame...)
		p.vAnnex = append(p.vAnnex, cp)
		p.vDTS = append(p.vDTS, dts)
		p.vPTS = append(p.vPTS, pts)

		// Accumulate SPS/PPS until the init segment is written. Capped by
		// videoPSMaxBytes — see appendVideoPSLocked for the rationale.
		if p.videoInit == nil && p.appendVideoPSLocked(cp) {
			pending = p.tryInitVideoLocked()
		}
		p.seedVideoNextDecodeLocked()
		if p.segmentStart.IsZero() && p.videoInit != nil {
			p.segmentStart = time.Now()
		}

	case mpeg2.TS_STREAM_H265:
		if len(frame) == 0 {
			p.mu.Unlock()
			return
		}
		// Same forward+backward jump guard as the H264 case — see above.
		if timestampJumpFromLast(p.vDTS, dts) {
			_ = p.flushSegmentLocked()
		}
		p.recordOriginDTSLocked(dts)
		cp := append([]byte(nil), frame...)
		p.vAnnex = append(p.vAnnex, cp)
		p.vDTS = append(p.vDTS, dts)
		p.vPTS = append(p.vPTS, pts)

		if p.videoInit == nil {
			p.isHEVC = true
			if p.appendVideoPSLocked(cp) {
				pending = p.tryInitVideoH265Locked()
			}
		}
		p.seedVideoNextDecodeLocked()
		if p.segmentStart.IsZero() && p.videoInit != nil {
			p.segmentStart = time.Now()
		}

	case mpeg2.TS_STREAM_AAC:
		if !p.packAudio {
			p.mu.Unlock()
			return
		}
		// Same forward+backward jump guard as for video: flush before
		// mixing frames from two different sources in the audio queue.
		// Audio's own dur math is sample-count-locked (1024/frame) so
		// the per-sample amplification described in the H264 comment
		// doesn't apply, but a source switch still wants to land on
		// a clean segment boundary so the player sees one seamless
		// audio elementary stream per Period.
		if timestampJumpFromLast(p.aPTS, pts) {
			_ = p.flushSegmentLocked()
		}
		p.recordOriginDTSLocked(dts)
		// A PES payload may contain multiple concatenated ADTS frames.
		pos := 0
		for pos+7 <= len(frame) {
			hdr, _, err := aac.DecodeADTSHeader(bytes.NewReader(frame[pos:]))
			if err != nil {
				pos++
				continue
			}
			hLen := int(hdr.HeaderLength)
			pLen := int(hdr.PayloadLength)
			if hLen <= 0 || pLen <= 0 {
				pos++
				continue
			}
			if pos+hLen+pLen > len(frame) {
				break
			}
			raw := append([]byte(nil), frame[pos+hLen:pos+hLen+pLen]...)
			if p.audioInit == nil {
				pw, err := p.prepareAudioInitLocked(hdr)
				if err != nil {
					pos += hLen + pLen
					continue
				}
				pending = pw
			}
			p.aRaw = append(p.aRaw, raw)
			p.aPTS = append(p.aPTS, pts)
			p.seedAudioNextDecodeLocked(dts)
			if p.segmentStart.IsZero() && p.videoInit == nil && p.audioInit != nil {
				p.segmentStart = time.Now()
			}
			pos += hLen + pLen
		}

	default:
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	// Write init segment to disk outside the mutex — this is I/O-bound and
	// only happens once per stream, but we must not block the flush ticker.
	if pending != nil {
		path := filepath.Join(p.streamDir, pending.filename)
		if err := encodeInitToFile(path, pending.init); err != nil {
			slog.Error("publisher: DASH write init segment failed",
				"stream_code", p.streamID, "file", pending.filename, "err", err)
			// Roll back: clear the init so the next frame retries.
			p.mu.Lock()
			switch pending.filename {
			case "init_v.mp4":
				p.videoInit = nil
			case "init_a.mp4":
				p.audioInit = nil
			}
			p.mu.Unlock()
		}
	}
}

// isKeyFrameAnnexB reports whether the Annex-B frame contains an IDR /
// IRAP slice. Picks the codec-specific scanner based on the packager's
// HEVC flag — set in onTSFrame the first time an H265 stream type is
// observed, false for the H264 default.
func isKeyFrameAnnexB(frame []byte, isHEVC bool) bool {
	if isHEVC {
		return tsmux.KeyFrameH265(frame)
	}
	return tsmux.KeyFrameH264(frame)
}

// appendVideoPSLocked appends an Annex-B keyframe's bytes to videoPS for
// SPS/PPS extraction, capped at videoPSMaxBytes. Returns true when the
// caller should run tryInitVideo*Locked.
//
// P-frames are skipped (return false, no append) because they never
// contain SPS/PPS — only IDR access units do. Without this filter, a
// long-GOP source (e.g. NVENC's 10-second default) would fill videoPS
// with megabytes of P-frame slices BEFORE the first IDR arrived, hit
// the cap, and latch videoPSGiveUp permanently. Production observed
// this with RTMP-loopback streams: 250 P-frames × ~150 KB = ~37 MB
// well past the 4 MB cap, give-up triggered before the first IDR's
// SPS|PPS prefix could be seen, and the stream stayed audio-only.
//
// Caller must hold p.mu.
func (p *dashFMP4Packager) appendVideoPSLocked(frame []byte) bool {
	if p.videoPSGiveUp {
		return false
	}
	if !isKeyFrameAnnexB(frame, p.isHEVC) {
		// P-frame — has no SPS/PPS, contributes nothing to init detection.
		// Skip silently to keep videoPS bounded by ONE GOP's worth of
		// keyframes (typical: 1-2 IDRs) rather than every frame in the
		// stream until init is built.
		return false
	}
	if len(p.videoPS)+len(frame) > videoPSMaxBytes {
		slog.Error("publisher: DASH videoPS exceeded cap without finding SPS/PPS — giving up video init for this stream",
			"stream_code", p.streamID,
			"videoPS_size", len(p.videoPS),
			"cap_bytes", videoPSMaxBytes,
		)
		p.videoPS = nil
		p.videoPSGiveUp = true
		return false
	}
	p.videoPS = append(p.videoPS, frame...)
	return true
}

// tryInitVideoLocked scans videoPS for SPS/PPS and, when found, prepares init_v.mp4.
// Returns a pending write to be flushed outside the mutex. Caller must hold p.mu.
func (p *dashFMP4Packager) tryInitVideoLocked() *pendingInitWrite {
	if p.videoInit != nil || len(p.videoPS) < 20 {
		return nil
	}
	// ExtractNalusOfTypeFromByteStream expects 3-byte start codes (0x00 0x00 0x01).
	psBuf := annexB4To3ForPSExtract(p.videoPS)
	spss := avc.ExtractNalusOfTypeFromByteStream(avc.NALU_SPS, psBuf, false)
	ppss := avc.ExtractNalusOfTypeFromByteStream(avc.NALU_PPS, psBuf, false)
	if len(spss) == 0 || len(ppss) == 0 {
		return nil
	}

	init := mp4.CreateEmptyInit()
	trak := init.AddEmptyTrack(dashVideoTimescale, "video", "und")
	if err := trak.SetAVCDescriptor("avc1", spss, ppss, true); err != nil {
		slog.Error("publisher: DASH SetAVCDescriptor failed",
			"stream_code", p.streamID, "err", err)
		return nil
	}

	if sps, err := avc.ParseSPSNALUnit(spss[0], false); err == nil && sps != nil {
		p.width = int(sps.Width)
		p.height = int(sps.Height)
		p.videoCodec = avc.CodecString("avc1", sps)
	}

	p.videoInit = init
	p.videoTID = trak.Tkhd.TrackID
	p.videoPS = nil // no longer needed

	return &pendingInitWrite{init: init, filename: "init_v.mp4"}
}

// tryInitVideoH265Locked scans videoPS for VPS/SPS/PPS and, when found, prepares init_v.mp4.
// Returns a pending write to be flushed outside the mutex. Caller must hold p.mu.
func (p *dashFMP4Packager) tryInitVideoH265Locked() *pendingInitWrite {
	if p.videoInit != nil || len(p.videoPS) < 20 {
		return nil
	}
	psBuf := annexB4To3ForPSExtract(p.videoPS)
	vpss, spss, ppss := hevc.GetParameterSetsFromByteStream(psBuf)
	if len(spss) == 0 || len(ppss) == 0 {
		return nil
	}

	init := mp4.CreateEmptyInit()
	trak := init.AddEmptyTrack(dashVideoTimescale, "video", "und")
	if err := trak.SetHEVCDescriptor("hvc1", vpss, spss, ppss, nil, true); err != nil {
		slog.Error("publisher: DASH SetHEVCDescriptor failed",
			"stream_code", p.streamID, "err", err)
		return nil
	}

	if sps, err := hevc.ParseSPSNALUnit(spss[0]); err == nil && sps != nil {
		w, h := sps.ImageSize()
		p.width = int(w)
		p.height = int(h)
		p.videoCodec = hevc.CodecString("hvc1", sps)
	}

	p.videoInit = init
	p.videoTID = trak.Tkhd.TrackID
	p.videoPS = nil

	return &pendingInitWrite{init: init, filename: "init_v.mp4"}
}

// prepareAudioInitLocked creates the audio init segment from the first ADTS header.
// Returns a pending write to be flushed outside the mutex. Caller must hold p.mu.
func (p *dashFMP4Packager) prepareAudioInitLocked(hdr *aac.ADTSHeader) (*pendingInitWrite, error) {
	sr := int(hdr.Frequency())
	if sr <= 0 {
		return nil, fmt.Errorf("invalid AAC sample rate")
	}
	init := mp4.CreateEmptyInit()
	trak := init.AddEmptyTrack(uint32(sr), "audio", "und")
	if err := trak.SetAACDescriptor(aac.AAClc, sr); err != nil {
		return nil, err
	}
	p.audioInit = init
	p.audioTID = trak.Tkhd.TrackID
	p.audioSR = sr
	p.audioCodec = "mp4a.40.2"
	// audioNextDecode is seeded by seedAudioNextDecodeLocked() on the first
	// AAC frame after init, using the actual frame DTS rather than a
	// hardcoded 0 — that's how cross-track tfdt offsets stay correct.

	return &pendingInitWrite{init: init, filename: "init_a.mp4"}, nil
}

// recordOriginDTSLocked seeds the shared per-stream origin on the first
// frame of any track. Both per-track tfdt counters are subsequently anchored
// to this origin so that the published DASH timeline preserves whatever
// inter-track offset the source had at startup (encoder pre-roll, mp4 edit
// list, HLS source where audio leads video by a few hundred ms, …).
//
// Without an explicit origin, video and audio nextDecode counters both
// defaulted to 0, which collapsed the source's actual offset into "both
// tracks start at the same moment" — players then play audio and video
// out of sync by exactly that lost offset. Caller must hold p.mu.
func (p *dashFMP4Packager) recordOriginDTSLocked(dts uint64) {
	if !p.originDTSmsSet {
		p.originDTSms = dts
		p.originDTSmsSet = true
	}
}

// seedVideoNextDecodeLocked initialises videoNextDecode from the first
// queued video DTS, expressed as the offset from the shared origin in 90
// kHz ticks. No-op once seeded, after the video init segment exists, and
// when there is at least one queued AU to anchor to.
//
// Subsequent video segments accumulate from this seed via the existing
// `p.videoNextDecode += segDur` path, so the offset persists for the full
// stream lifetime — meaning "audio leads video by 100ms" stays "audio leads
// video by 100ms" all the way to the player. Caller must hold p.mu.
func (p *dashFMP4Packager) seedVideoNextDecodeLocked() {
	if p.videoFirstDTSmsSet || p.videoInit == nil || len(p.vDTS) == 0 {
		return
	}
	p.videoNextDecode = scaleOffset(p.vDTS[0], p.originDTSms, dashVideoTimescale)
	p.videoFirstDTSmsSet = true
}

// seedAudioNextDecodeLocked is the audio counterpart of
// seedVideoNextDecodeLocked, scaling into the audio sample rate. Called
// once after the audio init exists and the first AAC frame has been
// queued. Caller must hold p.mu.
func (p *dashFMP4Packager) seedAudioNextDecodeLocked(firstFrameDTSms uint64) {
	if p.audioFirstDTSmsSet || p.audioInit == nil || p.audioSR <= 0 {
		return
	}
	p.audioNextDecode = scaleOffset(firstFrameDTSms, p.originDTSms, uint64(p.audioSR))
	p.audioFirstDTSmsSet = true
}

// scaleOffset returns (firstDTSms - originDTSms) converted into `timescale`
// ticks, clamping to 0 when first predates origin. The clamp is conservative
// — predating origin shouldn't happen given recordOriginDTSLocked fires
// before any track-specific seed call, but uint64 underflow would silently
// emit a tfdt near 2^64 and break every player downstream.
func scaleOffset(firstDTSms, originDTSms, timescale uint64) uint64 {
	if firstDTSms < originDTSms {
		return 0
	}
	return (firstDTSms - originDTSms) * timescale / 1000
}

// audioFramesPerSegment returns the expected number of 1024-sample AAC frames per segment.
func (p *dashFMP4Packager) audioFramesPerSegment() int {
	if p.audioSR <= 0 {
		return 94 // safe default for 48 kHz × 2 s
	}
	return (p.segSec*p.audioSR + 1023) / 1024
}

// queuedVideoStartsWithIDRLocked returns true when the first queued AU is an IDR/IRAP.
// Caller must hold p.mu.
func (p *dashFMP4Packager) queuedVideoStartsWithIDRLocked() bool {
	if len(p.vAnnex) == 0 {
		return false
	}
	if p.isHEVC {
		return tsmux.KeyFrameH265(p.vAnnex[0])
	}
	return tsmux.KeyFrameH264(p.vAnnex[0])
}

// effectiveVideoBW returns the video bandwidth for MPD representations.
func (p *dashFMP4Packager) effectiveVideoBW() int {
	if p.videoBW > 0 {
		return p.videoBW
	}
	return 5_000_000
}

// flushSegmentLocked writes all queued video and audio frames as .m4s segments,
// trims old segments from disk, and updates the manifest.
// Caller must hold p.mu.
func (p *dashFMP4Packager) flushSegmentLocked() error {
	flushed := false

	if p.videoInit != nil && len(p.vAnnex) > 0 {
		if err := p.writeVideoSegmentLocked(); err != nil {
			slog.Warn("publisher: DASH write video segment",
				"stream_code", p.streamID, "err", err)
		} else {
			flushed = true
		}
	}

	if p.audioInit != nil && len(p.aRaw) > 0 {
		if err := p.writeAudioSegmentLocked(); err != nil {
			slog.Warn("publisher: DASH write audio segment",
				"stream_code", p.streamID, "err", err)
		} else {
			flushed = true
		}
	}

	// Reset queues.
	p.vAnnex = p.vAnnex[:0]
	p.vDTS = p.vDTS[:0]
	p.vPTS = p.vPTS[:0]
	p.aRaw = p.aRaw[:0]
	p.aPTS = p.aPTS[:0]
	p.segmentStart = time.Now()

	if !flushed {
		return nil
	}
	if p.availabilityStart.IsZero() {
		p.availabilityStart = time.Now()
	}
	p.trimDiskLocked()
	return p.writeManifestLocked()
}

// writeVideoSegmentLocked packages all queued H.264 AUs into one fMP4 .m4s file.
// Caller must hold p.mu.
func (p *dashFMP4Packager) writeVideoSegmentLocked() error {
	p.vSegN++
	name := fmt.Sprintf("seg_v_%05d.m4s", p.vSegN)
	path := filepath.Join(p.streamDir, name)

	frag, err := mp4.CreateFragment(uint32(p.vSegN), p.videoTID)
	if err != nil {
		p.vSegN--
		return fmt.Errorf("create video fragment: %w", err)
	}

	segStart := p.videoNextDecode
	var segDur uint64

	for i, au := range p.vAnnex {
		var avcc []byte
		if p.isHEVC {
			avcc = hevcAnnexBToAVCC(au)
		} else {
			avcc = h264AnnexBToAVCC(au)
		}
		if len(avcc) == 0 {
			continue
		}

		// Sample duration in 90 kHz ticks from inter-frame DTS delta.
		var dur uint32
		if i+1 < len(p.vDTS) {
			if d := p.vDTS[i+1] - p.vDTS[i]; d > 0 {
				dur = uint32(d * 90)
			}
		}
		if dur == 0 {
			// Fallback: spread total queued duration evenly.
			if len(p.vDTS) >= 2 {
				total := (p.vDTS[len(p.vDTS)-1] - p.vDTS[0]) * 90
				dur = uint32(total / uint64(len(p.vAnnex)))
			}
			if dur == 0 {
				dur = uint32(p.segSec) * dashVideoTimescale / uint32(len(p.vAnnex))
			}
		}

		flags := mp4.NonSyncSampleFlags
		isKey := p.isHEVC && tsmux.KeyFrameH265(au) || !p.isHEVC && tsmux.KeyFrameH264(au)
		if isKey {
			flags = mp4.SyncSampleFlags
		}

		var cto int32
		if i < len(p.vPTS) && i < len(p.vDTS) {
			cto = int32((int64(p.vPTS[i]) - int64(p.vDTS[i])) * 90)
		}

		frag.AddFullSample(mp4.FullSample{
			Sample: mp4.Sample{
				Flags:                 flags,
				Dur:                   dur,
				Size:                  uint32(len(avcc)),
				CompositionTimeOffset: cto,
			},
			DecodeTime: p.videoNextDecode + segDur,
			Data:       avcc,
		})
		segDur += uint64(dur)
	}

	if segDur == 0 {
		p.vSegN--
		return nil
	}

	seg := mp4.NewMediaSegment()
	seg.AddFragment(frag)
	var buf bytes.Buffer
	if err := seg.Encode(&buf); err != nil {
		p.vSegN--
		return fmt.Errorf("encode video segment: %w", err)
	}
	writeStart := time.Now()
	if err := writeFileAtomic(path, buf.Bytes()); err != nil {
		p.vSegN--
		return fmt.Errorf("write video segment: %w", err)
	}
	if p.segWriteDur != nil {
		p.segWriteDur.Observe(time.Since(writeStart).Seconds())
	}

	if p.segCount != nil {
		p.segCount.Inc()
	}

	logger.Trace("publisher: DASH video segment",
		"stream_code", p.streamID, "segment", name,
		"frames", len(p.vAnnex), "bytes", buf.Len())

	p.onDiskV = append(p.onDiskV, name)
	p.vSegDurs = append(p.vSegDurs, segDur)
	p.vSegStarts = append(p.vSegStarts, segStart)
	p.videoNextDecode += segDur
	return nil
}

// writeAudioSegmentLocked packages all queued raw AAC frames into one fMP4 .m4s file.
// Caller must hold p.mu.
func (p *dashFMP4Packager) writeAudioSegmentLocked() error {
	p.aSegN++
	name := fmt.Sprintf("seg_a_%05d.m4s", p.aSegN)
	path := filepath.Join(p.streamDir, name)

	frag, err := mp4.CreateFragment(uint32(p.aSegN), p.audioTID)
	if err != nil {
		p.aSegN--
		return fmt.Errorf("create audio fragment: %w", err)
	}

	segStart := p.audioNextDecode
	var segDur uint64

	// Each AAC frame contains exactly 1024 samples at p.audioSR.
	const aacFrameSamples = uint32(1024)

	for _, raw := range p.aRaw {
		frag.AddFullSample(mp4.FullSample{
			Sample: mp4.Sample{
				Flags: mp4.SyncSampleFlags,
				Dur:   aacFrameSamples,
				Size:  uint32(len(raw)),
			},
			DecodeTime: p.audioNextDecode + segDur,
			Data:       raw,
		})
		segDur += uint64(aacFrameSamples)
	}

	if segDur == 0 {
		p.aSegN--
		return nil
	}

	seg := mp4.NewMediaSegment()
	seg.AddFragment(frag)
	var buf bytes.Buffer
	if err := seg.Encode(&buf); err != nil {
		p.aSegN--
		return fmt.Errorf("encode audio segment: %w", err)
	}
	writeStart := time.Now()
	if err := writeFileAtomic(path, buf.Bytes()); err != nil {
		p.aSegN--
		return fmt.Errorf("write audio segment: %w", err)
	}
	if p.segWriteDur != nil {
		p.segWriteDur.Observe(time.Since(writeStart).Seconds())
	}

	if p.segCount != nil {
		p.segCount.Inc()
	}

	p.onDiskA = append(p.onDiskA, name)
	p.aSegDurs = append(p.aSegDurs, segDur)
	p.aSegStarts = append(p.aSegStarts, segStart)
	p.audioNextDecode += segDur
	return nil
}

// trimDiskLocked removes old segments beyond window+history.
// Caller must hold p.mu.
func (p *dashFMP4Packager) trimDiskLocked() {
	maxKeep := p.window + p.history
	if maxKeep < p.window {
		maxKeep = p.window
	}
	if !p.ephemeral {
		return
	}
	for len(p.onDiskV) > maxKeep {
		_ = os.Remove(filepath.Join(p.streamDir, p.onDiskV[0]))
		p.onDiskV = p.onDiskV[1:]
		if len(p.vSegDurs) > 0 {
			p.vSegDurs = p.vSegDurs[1:]
		}
		if len(p.vSegStarts) > 0 {
			p.vSegStarts = p.vSegStarts[1:]
		}
	}
	for len(p.onDiskA) > maxKeep {
		_ = os.Remove(filepath.Join(p.streamDir, p.onDiskA[0]))
		p.onDiskA = p.onDiskA[1:]
		if len(p.aSegDurs) > 0 {
			p.aSegDurs = p.aSegDurs[1:]
		}
		if len(p.aSegStarts) > 0 {
			p.aSegStarts = p.aSegStarts[1:]
		}
	}
}

// writeManifestLocked serialises the sliding-window MPD.
// In ABR mode it notifies the master and returns without writing a per-shard file.
// Caller must hold p.mu.
func (p *dashFMP4Packager) writeManifestLocked() error {
	if p.manifestPath == "" && p.abrMaster == nil {
		return nil
	}

	vSegs := windowTail(p.onDiskV, p.window)
	vDurs := windowTailUint64(p.vSegDurs, p.window)
	vStarts := windowTailUint64(p.vSegStarts, p.window)
	aSegs := windowTail(p.onDiskA, p.window)
	aDurs := windowTailUint64(p.aSegDurs, p.window)
	aStarts := windowTailUint64(p.aSegStarts, p.window)

	if p.abrMaster != nil {
		p.abrMaster.onShardUpdated(p)
		return nil
	}

	doc := buildMPD(
		p.availabilityStart, p.segSec, p.window,
		p.videoInit, p.videoCodec, p.effectiveVideoBW(), p.width, p.height,
		vSegs, vDurs, vStarts,
		p.audioInit, p.audioCodec, p.audioSR,
		aSegs, aDurs, aStarts,
		"",
	)
	if doc == nil {
		return nil
	}
	out, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(p.manifestPath, append([]byte(xml.Header), out...))
}

// ─── helpers ───────────────────────────────────────────────────────────────

// totalQueuedVideoDur90k returns the total duration of queued video in 90 kHz ticks.
func totalQueuedVideoDur90k(vDTS []uint64) uint64 {
	if len(vDTS) < 2 {
		return 0
	}
	return (vDTS[len(vDTS)-1] - vDTS[0]) * 90
}

// windowTailUint64 returns the last n elements of vals.
func windowTailUint64(vals []uint64, n int) []uint64 {
	if n <= 0 || len(vals) == 0 {
		return vals
	}
	if len(vals) <= n {
		return vals
	}
	return vals[len(vals)-n:]
}

// h264AnnexBToAVCC converts an Annex-B H.264 access unit to AVCC (length-prefixed NALUs).
func h264AnnexBToAVCC(annexB []byte) []byte {
	if len(annexB) == 0 {
		return nil
	}
	avcc := avc.ConvertByteStreamToNaluSample(annexB)
	if len(avcc) > 0 {
		return avcc
	}
	// Fallback: extract NALUs manually and prefix each with a 4-byte big-endian length.
	nalus := avc.ExtractNalusFromByteStream(annexB)
	if len(nalus) == 0 {
		return nil
	}
	out := make([]byte, 0, len(nalus)*5) // 4-byte length prefix + approximate nalu size
	for _, nalu := range nalus {
		n := len(nalu)
		out = append(out, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
		out = append(out, nalu...)
	}
	return out
}

// hevcAnnexBToAVCC converts an Annex-B H.265 access unit to length-prefixed NALUs.
func hevcAnnexBToAVCC(annexB []byte) []byte {
	if len(annexB) == 0 {
		return nil
	}
	nalus := splitAnnexBNALUs(annexB)
	if len(nalus) == 0 {
		return nil
	}
	out := make([]byte, 0, len(annexB))
	for _, nalu := range nalus {
		n := len(nalu)
		out = append(out, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
		out = append(out, nalu...)
	}
	return out
}

// splitAnnexBNALUs splits Annex-B byte stream on 3- or 4-byte start codes.
func splitAnnexBNALUs(data []byte) [][]byte {
	var nalus [][]byte
	start := -1
	n := len(data)
	for i := 0; i < n-3; i++ {
		if data[i] == 0 && data[i+1] == 0 {
			if data[i+2] == 1 {
				if start >= 0 {
					end := i
					for end > start && data[end-1] == 0 {
						end--
					}
					if end > start {
						nalus = append(nalus, data[start:end])
					}
				}
				start = i + 3
				i += 2
			} else if data[i+2] == 0 && i+3 < n && data[i+3] == 1 {
				if start >= 0 {
					end := i
					for end > start && data[end-1] == 0 {
						end--
					}
					if end > start {
						nalus = append(nalus, data[start:end])
					}
				}
				start = i + 4
				i += 3
			}
		}
	}
	if start >= 0 && start < n {
		nalus = append(nalus, data[start:])
	}
	return nalus
}

// annexB4To3ForPSExtract replaces 4-byte Annex-B start codes with 3-byte ones
// so that mp4ff's Annex-B scanners (which use 0x00 0x00 0x01) work correctly.
func annexB4To3ForPSExtract(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte{0, 0, 0, 1}, []byte{0, 0, 1})
}

// encodeInitToFile writes an mp4.InitSegment atomically to the given path.
func encodeInitToFile(path string, init *mp4.InitSegment) error {
	var buf bytes.Buffer
	if err := init.Encode(&buf); err != nil {
		return err
	}
	return writeFileAtomic(path, buf.Bytes())
}

// videoPSMaxBytes caps the SPS/PPS accumulation buffer used by
// tryInitVideoLocked. A normal H.264/H.265 parameter set is < 1 KiB; we
// allow 4 MiB which is ~16 seconds of 25 fps video frames mistakenly
// accumulated before the source's first parseable SPS. Past that we
// log ERROR and stop appending — the prior code grew videoPS unbounded
// and called bytes.ReplaceAll on the whole buffer per frame, which is
// quadratic in memory and CPU when init never completes (observed in
// production: 170 MB held by bytes.Replace from this path).
const videoPSMaxBytes = 4 << 20

// tsBufferMaxBytes caps the in-flight backlog tsBuffer holds before dropping
// the oldest data. Sized at 16 MiB which is ~1 second of the highest-bitrate
// rendition we publish (4500 kbps video + 128 kbps audio) plus generous
// headroom for keyframe spikes. A normal demuxer drains within a few
// milliseconds, so any backlog approaching this cap means the consumer is
// seriously stalled — at that point dropping is preferable to OOM since
// the demuxer can resync from the next PAT/PMT once the cap is cleared.
const tsBufferMaxBytes = 16 << 20

// tsBuffer is a goroutine-safe, bounded byte buffer implementing io.Reader and
// io.Writer. Unlike io.Pipe, Write never blocks; instead, when the buffered
// backlog would exceed tsBufferMaxBytes it discards the entire backlog and
// logs a warning. Read blocks only when the buffer is empty, and returns
// io.EOF once the buffer is closed and drained.
//
// This is used to decouple the packet-producer goroutine from the TS demuxer
// goroutine. With io.Pipe, a slow demuxer (holding p.mu in onTSFrame) would
// block the producer's Write, preventing it from reading new packets; the buffer
// hub then drops them with the "slow consumer" path, starving the segment queue.
//
// Drop policy: when an incoming Write would push the backlog past the cap,
// we drop the ENTIRE buffered backlog (not just the oldest N bytes). MPEG-TS
// is self-synchronising — the demuxer recovers cleanly from the next PAT/PMT
// in the new data — so dropping at a packet boundary is unnecessary. Dropping
// the whole backlog also bounds the worst-case held memory to ~tsBufferMaxBytes
// no matter how persistent the consumer stall is.
type tsBuffer struct {
	mu          sync.Mutex
	cond        *sync.Cond
	buf         []byte
	done        bool
	streamID    domain.StreamCode // for log context on drop; "" in tests
	maxBytes    int               // overflow threshold; 0 = no cap (rare; tests only)
	dropCount   int64             // cumulative drop events (not bytes) — for metrics later if needed
	droppedSize int64             // cumulative bytes dropped on overflow
}

// newTSBuffer creates a tsBuffer tagged with streamID for log context on
// overflow drops. The cap is fixed at tsBufferMaxBytes for production —
// tests override per-instance via newTSBufferWithCap to keep allocations
// small under -race without affecting concurrent tests.
func newTSBuffer(streamID domain.StreamCode) *tsBuffer {
	return newTSBufferWithCap(streamID, tsBufferMaxBytes)
}

// newTSBufferWithCap is the testable constructor. Production code uses
// newTSBuffer; tests use this to verify the overflow branch with KiB-
// scale buffers instead of allocating real MiB.
func newTSBufferWithCap(streamID domain.StreamCode, maxBytes int) *tsBuffer {
	b := &tsBuffer{streamID: streamID, maxBytes: maxBytes}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// Write appends p to the internal buffer and wakes any blocked Read. Never
// blocks. When the existing backlog plus p would exceed tsBufferMaxBytes the
// existing backlog is discarded before appending p — the demuxer resyncs from
// the new data's next PAT/PMT. Returns len(p), nil even on the drop path so
// callers don't see partial writes (the producer goroutine has nothing useful
// to do with a "buffer full" error here; the drop is the recovery action).
func (b *tsBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	if b.done {
		b.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	if b.maxBytes > 0 && len(b.buf)+len(p) > b.maxBytes && len(b.buf) > 0 {
		dropped := len(b.buf)
		b.buf = nil
		b.dropCount++
		b.droppedSize += int64(dropped)
		// Log inside the lock so the drop accounting and the log line can't
		// race against a concurrent drop. The lock is held briefly anyway.
		slog.Warn("publisher: tsBuffer overflow, dropped backlog (demuxer will resync from next PAT/PMT)",
			"stream_code", string(b.streamID),
			"dropped_bytes", dropped,
			"incoming_bytes", len(p),
			"cap_bytes", b.maxBytes,
			"drop_count", b.dropCount,
			"dropped_total_bytes", b.droppedSize,
		)
	}
	// If incoming alone exceeds the cap (pathological producer chunk), still
	// accept it so the demuxer at least sees ONE recent packet to resync from.
	// This is unbounded only for that single chunk — append doesn't grow
	// across calls because of the drop above.
	b.buf = append(b.buf, p...)
	b.cond.Signal()
	b.mu.Unlock()
	return len(p), nil
}

// Read fills p from the internal buffer, blocking until data is available or
// the buffer is closed.
//
// When the slice is fully drained we reset b.buf to nil so the underlying
// array becomes GC-eligible. Without this, `b.buf = b.buf[n:]` only advances
// the slice header — the array allocated by Write retains its peak capacity
// for the lifetime of the buffer. With a slow-consumer burst that pushed the
// array to e.g. 100 MiB, that 100 MiB stays pinned across all subsequent
// drain/refill cycles. Multiplied across one tsBuffer per DASH stream, this
// is the dominant heap leak observed in pprof when multiple DASH publishers
// run concurrently (peak burst size never reclaimed).
func (b *tsBuffer) Read(p []byte) (int, error) {
	b.mu.Lock()
	for len(b.buf) == 0 && !b.done {
		b.cond.Wait()
	}
	if len(b.buf) == 0 {
		b.mu.Unlock()
		return 0, io.EOF
	}
	n := copy(p, b.buf)
	b.buf = b.buf[n:]
	if len(b.buf) == 0 {
		// Drop the underlying array reference so GC reclaims the peak
		// capacity. Subsequent Write triggers a fresh allocation sized
		// to the actual incoming chunk, not the historical high-water mark.
		b.buf = nil
	}
	b.mu.Unlock()
	return n, nil
}

// Close marks the buffer as done, unblocking any blocked Read.
func (b *tsBuffer) Close() {
	b.mu.Lock()
	b.done = true
	b.cond.Broadcast()
	b.mu.Unlock()
}

// parseDashSegNum extracts the segment number from "seg_v_00042.m4s" / "seg_a_00012.m4s".
func parseDashSegNum(kind byte, name string) int {
	prefix := "seg_" + string(kind) + "_"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".m4s") {
		return 0
	}
	s := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".m4s")
	n, _ := strconv.Atoi(s)
	return n
}

// buildSegTimeline constructs a SegmentTimeline from a window of (name, dur, start) triples.
// Only the first entry gets an explicit @t attribute; the rest are implied.
func buildSegTimeline(segs []string, durs []uint64, starts []uint64) *mpdSegTimeline {
	if len(segs) == 0 {
		return nil
	}
	tl := &mpdSegTimeline{S: make([]mpdSTimeline, 0, len(segs))}
	for i := range segs {
		var d uint64
		if i < len(durs) {
			d = durs[i]
		}
		if d == 0 {
			continue
		}
		s := mpdSTimeline{D: d}
		if i == 0 && len(starts) > 0 {
			t := starts[0]
			s.T = &t
		}
		tl.S = append(tl.S, s)
	}
	return tl
}

// buildMPD creates a dynamic MPD document from the provided per-track window slices.
// baseURL is prepended to all init/media patterns (empty for per-shard MPD).
// Returns nil when there are no segments available yet.
func buildMPD(
	availStart time.Time, segSec, window int,
	videoInit *mp4.InitSegment, videoCodec string, videoBW, vW, vH int,
	vSegs []string, vDurs, vStarts []uint64,
	audioInit *mp4.InitSegment, audioCodec string, audioSR int,
	aSegs []string, aDurs, aStarts []uint64,
	baseURL string,
) *mpdRoot {
	ast := ""
	if !availStart.IsZero() {
		ast = availStart.UTC().Format(time.RFC3339)
	}
	pub := time.Now().UTC().Format(time.RFC3339)
	minBuf := max(4, segSec*2)
	sugDelay := max(6, segSec*3)
	maxSegDur := max(segSec*3, segSec+1)

	doc := &mpdRoot{
		XMLName:                    xml.Name{Local: "MPD"},
		XMLNS:                      "urn:mpeg:dash:schema:mpd:2011",
		Type:                       "dynamic",
		Profiles:                   "urn:mpeg:dash:profile:isoff-live:2011",
		MinBuffer:                  fmt.Sprintf("PT%dS", minBuf),
		SuggestedPresentationDelay: fmt.Sprintf("PT%dS", sugDelay),
		MaxSegmentDuration:         fmt.Sprintf("PT%dS", maxSegDur),
		AvailabilityStartTime:      ast,
		MinUpdate:                  fmt.Sprintf("PT%dS", segSec),
		BufferDepth:                fmt.Sprintf("PT%dS", segSec*window),
		PublishTime:                pub,
		Periods:                    []mpdPeriod{{ID: "0", Start: "PT0S"}},
		UTCTiming: &mpdUTCTiming{
			SchemeIDURI: "urn:mpeg:dash:utc:direct:2014",
			Value:       pub,
		},
	}
	per := &doc.Periods[0]

	if videoInit != nil && len(vSegs) > 0 {
		vTL := buildSegTimeline(vSegs, vDurs, vStarts)
		if vTL != nil {
			vStartNum := parseDashSegNum('v', vSegs[0])
			if vStartNum <= 0 {
				vStartNum = 1
			}
			initV := baseURL + "init_v.mp4"
			mediaV := baseURL + dashVideoMediaPattern
			per.AdaptationSets = append(per.AdaptationSets, mpdAdaptationSet{
				ID:               "0",
				ContentType:      "video",
				MimeType:         "video/mp4",
				SegmentAlignment: "true",
				StartWithSAP:     "1",
				Representations: []mpdRepresentation{{
					ID:        "v0",
					MimeType:  "video/mp4",
					Codecs:    videoCodec,
					Bandwidth: videoBW,
					Width:     vW,
					Height:    vH,
					SegmentTemplate: mpdSegmentTemplate{
						Timescale:      dashVideoTimescale,
						Initialization: initV,
						Media:          mediaV,
						StartNumber:    vStartNum,
						Timeline:       vTL,
					},
				}},
			})
		}
	}

	if audioInit != nil && len(aSegs) > 0 {
		aTL := buildSegTimeline(aSegs, aDurs, aStarts)
		if aTL != nil {
			aStartNum := parseDashSegNum('a', aSegs[0])
			if aStartNum <= 0 {
				aStartNum = 1
			}
			asr := audioSR
			initA := baseURL + "init_a.mp4"
			mediaA := baseURL + dashAudioMediaPattern
			per.AdaptationSets = append(per.AdaptationSets, mpdAdaptationSet{
				ID:               "1",
				ContentType:      "audio",
				MimeType:         "audio/mp4",
				SegmentAlignment: "true",
				StartWithSAP:     "1",
				Representations: []mpdRepresentation{{
					ID:                "a0",
					MimeType:          "audio/mp4",
					Codecs:            audioCodec,
					Bandwidth:         128_000,
					AudioSamplingRate: &asr,
					SegmentTemplate: mpdSegmentTemplate{
						Timescale:      audioSR,
						Initialization: initA,
						Media:          mediaA,
						StartNumber:    aStartNum,
						Timeline:       aTL,
					},
				}},
			})
		}
	}

	if len(per.AdaptationSets) == 0 {
		return nil
	}
	return doc
}

// ─── MPD XML structs (MPEG-DASH schema subset) ─────────────────────────────

type mpdRoot struct {
	XMLName                    xml.Name      `xml:"MPD"`
	XMLNS                      string        `xml:"xmlns,attr"`
	Type                       string        `xml:"type,attr"`
	Profiles                   string        `xml:"profiles,attr"`
	MinBuffer                  string        `xml:"minBufferTime,attr"`
	SuggestedPresentationDelay string        `xml:"suggestedPresentationDelay,attr,omitempty"`
	MaxSegmentDuration         string        `xml:"maxSegmentDuration,attr,omitempty"`
	AvailabilityStartTime      string        `xml:"availabilityStartTime,attr,omitempty"`
	MinUpdate                  string        `xml:"minimumUpdatePeriod,attr"`
	BufferDepth                string        `xml:"timeShiftBufferDepth,attr"`
	PublishTime                string        `xml:"publishTime,attr"`
	Periods                    []mpdPeriod   `xml:"Period"`
	UTCTiming                  *mpdUTCTiming `xml:"UTCTiming,omitempty"`
}

type mpdUTCTiming struct {
	SchemeIDURI string `xml:"schemeIdUri,attr"`
	Value       string `xml:"value,attr"`
}

type mpdPeriod struct {
	ID             string             `xml:"id,attr"`
	Start          string             `xml:"start,attr"`
	AdaptationSets []mpdAdaptationSet `xml:"AdaptationSet"`
}

type mpdAdaptationSet struct {
	ID               string              `xml:"id,attr"`
	ContentType      string              `xml:"contentType,attr"`
	MimeType         string              `xml:"mimeType,attr"`
	SegmentAlignment string              `xml:"segmentAlignment,attr"`
	StartWithSAP     string              `xml:"startWithSAP,attr"`
	Representations  []mpdRepresentation `xml:"Representation"`
}

type mpdRepresentation struct {
	ID                string             `xml:"id,attr"`
	MimeType          string             `xml:"mimeType,attr"`
	Codecs            string             `xml:"codecs,attr"`
	Bandwidth         int                `xml:"bandwidth,attr"`
	Width             int                `xml:"width,attr,omitempty"`
	Height            int                `xml:"height,attr,omitempty"`
	AudioSamplingRate *int               `xml:"audioSamplingRate,attr,omitempty"`
	BaseURL           string             `xml:"BaseURL,omitempty"`
	SegmentTemplate   mpdSegmentTemplate `xml:"SegmentTemplate"`
}

type mpdSegmentTemplate struct {
	Timescale      int             `xml:"timescale,attr"`
	Initialization string          `xml:"initialization,attr"`
	Media          string          `xml:"media,attr"`
	StartNumber    int             `xml:"startNumber,attr"`
	Timeline       *mpdSegTimeline `xml:"SegmentTimeline"`
}

type mpdSegTimeline struct {
	S []mpdSTimeline `xml:"S"`
}

type mpdSTimeline struct {
	T *uint64 `xml:"t,attr,omitempty"`
	D uint64  `xml:"d,attr"`
}
