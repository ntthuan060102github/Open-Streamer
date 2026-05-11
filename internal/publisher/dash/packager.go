package dash

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Eyevinn/mp4ff/aac"
	mpeg2 "github.com/yapingcat/gomedia/go-mpeg2"

	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/tsmux"
)

// packager.go — public Packager + Run loop.
//
// Lifecycle:
//
//  1. publisher.Service constructs a Packager via NewPackager and a
//     buffer.Subscriber for the stream.
//  2. Caller invokes Run(ctx) on its own goroutine. Run reads packets
//     from the subscriber, demuxes AV packets / TS chunks, fills the
//     frame queue, and on each cut writes fmp4 fragments + manifest.
//  3. ctx cancellation drains in-progress fragments and returns.
//
// One Packager instance serves either a single-rendition stream OR one
// shard of an ABR ladder. ABR coordination happens via the optional
// abrMaster; the master writes the root MPD, the per-shard packager
// writes ONLY media + (optionally) a per-shard MPD.

// Default constants — used when the caller passes zero in Config.
const (
	defaultPairingTimeout = 3 * time.Second
	defaultMaxSegFactor   = 3
	dashVideoMediaPattern = "seg_v_$Number%05d$.m4s"
	dashAudioMediaPattern = "seg_a_$Number%05d$.m4s"
	dashVideoInitFile     = "init_v.mp4"
	dashAudioInitFile     = "init_a.mp4"
	videoPSAccumCap       = 1 << 20 // 1 MiB cap on SPS/PPS accumulator before giving up
)

// Config carries the per-stream packager configuration.
type Config struct {
	// StreamID is used for log fields. Free-form; not used in any file path.
	StreamID string

	// StreamDir is the directory where media segments and (for single-
	// rendition streams) the per-stream manifest are written. The
	// directory is created with mkdir -p semantics if missing.
	StreamDir string

	// ManifestPath is the absolute path for the per-stream MPD file.
	// Leave empty for ABR shards — the ABR master writes the root MPD.
	ManifestPath string

	// SegDur is the target segment duration.
	SegDur time.Duration

	// Window is the number of segments held in the sliding manifest.
	Window int

	// History is the extra segments kept on disk past the manifest
	// window (0 = manifest window only).
	History int

	// Ephemeral controls whether old segments are deleted from disk
	// when they age past Window + History. False = keep forever
	// (DVR-style); true = delete (live-style).
	Ephemeral bool

	// PairingTimeout is the WaitingForPairing → Live timeout. Default 3 s.
	PairingTimeout time.Duration

	// ABR (optional): when set, the packager is one shard of a ladder.
	// ManifestPath becomes optional in this mode; the master writes the
	// root MPD via the snapshot pushed at every flush.
	ABRMaster *ABRMaster
	ABRSlug   string

	// PackAudio controls whether this shard emits audio segments.
	// In an ABR ladder, exactly ONE shard packs audio (typically the
	// best rendition); others set PackAudio=false. For single-rendition
	// streams pass true.
	PackAudio bool

	// OverrideBandwidth, when non-zero, replaces the per-rep bandwidth
	// value emitted in the manifest. Used by ABR ladders to declare
	// the transcoder's target bitrate rather than a measured one.
	OverrideBandwidth int
}

// Packager is the per-stream (or per-shard) DASH publisher state.
type Packager struct {
	cfg Config

	mu        sync.Mutex
	state     *StateMachine
	queue     *FrameQueue
	seg       *Segmenter
	videoInit *VideoInit
	audioInit *AudioInit

	// videoPS accumulates the Annex-B byte stream until SPS+PPS are
	// available. Cleared once videoInit is built. videoPSGivenUp latches
	// when the accumulator exceeds videoPSAccumCap so the packager
	// drops trying to build a video init and proceeds audio-only.
	videoPS        []byte
	videoPSGivenUp bool
	isHEVC         bool

	availStart time.Time
	vSegN      uint64
	aSegN      uint64

	// pairingTruncated latches after truncateAtPairingLocked runs so it
	// fires exactly once per session lifetime. Without this latch, a
	// stream whose first Cut returns Ok=false (insufficient frames) would
	// stay in WaitingForPairing across ticks; truncateAtPairingLocked
	// would re-fire on every tick, re-thresholding against the now-
	// older first-frame PTSms and dropping whichever track keeps
	// arriving slower — an infinite-drop loop observed on ABR streams
	// where V and A timeline axes don't perfectly match.
	pairingTruncated bool

	// Sliding-window state for manifest.
	onDiskV     []string
	onDiskA     []string
	vSegEntries []SegmentEntry
	aSegEntries []SegmentEntry
}

// NewPackager constructs a Packager from cfg. Validates required
// fields; missing values fall to defaults.
func NewPackager(cfg Config) (*Packager, error) {
	if cfg.StreamDir == "" {
		return nil, errors.New("dash packager: StreamDir is required")
	}
	if cfg.SegDur <= 0 {
		cfg.SegDur = 2 * time.Second
	}
	if cfg.Window <= 0 {
		cfg.Window = 6
	}
	if cfg.PairingTimeout <= 0 {
		cfg.PairingTimeout = defaultPairingTimeout
	}
	if err := os.MkdirAll(cfg.StreamDir, 0o755); err != nil {
		return nil, fmt.Errorf("dash packager: mkdir stream dir: %w", err)
	}
	return &Packager{
		cfg:   cfg,
		state: NewStateMachine(cfg.PairingTimeout),
		queue: NewFrameQueue(),
		seg:   NewSegmenter(cfg.SegDur, defaultMaxSegFactor),
	}, nil
}

// Run drives the run loop. Returns on ctx cancellation, subscriber close,
// or unrecoverable I/O error. Caller invokes on its own goroutine.
//
// Raw-TS sources (UDP / HLS pull / HTTP-MPEG-TS / SRT pull / file pull /
// transcoder output) feed bytes via pkt.TS; an internal demuxer
// goroutine converts them into AV frames and dispatches through the
// SAME handleH264 / handleAAC paths the direct-AV path uses. The
// demuxer is lazily started on the first TS packet so AV-only streams
// pay no goroutine cost.
func (p *Packager) Run(ctx context.Context, sub *buffer.Subscriber) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	var tsCarry []byte
	var tb *tsBuffer
	defer func() {
		if tb != nil {
			tb.Close()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			p.finalFlush()
			return
		case pkt, ok := <-sub.Recv():
			if !ok {
				p.finalFlush()
				return
			}
			p.onPacket(pkt, &tsCarry, &tb)
		case <-ticker.C:
			p.tryCut(time.Now())
		}
	}
}

// onPacket dispatches one buffer.Packet through the appropriate path.
// tsCarry + tb belong to Run's local state — passed by reference so
// the demuxer is lazily started on the FIRST TS packet rather than
// always-on (AV-only streams pay nothing).
func (p *Packager) onPacket(pkt buffer.Packet, tsCarry *[]byte, tb **tsBuffer) {
	if pkt.SessionStart {
		p.onSessionBoundary()
	}
	switch {
	case pkt.AV != nil && len(pkt.AV.Data) > 0:
		p.onAVPacket(pkt.AV)
	case len(pkt.TS) > 0:
		if *tb == nil {
			*tb = newTSBuffer(p.cfg.StreamID)
			startDemuxer(*tb, p.onTSFrame)
		}
		aligned := alignTS(tsCarry, pkt.TS)
		if len(aligned) > 0 {
			_, _ = (*tb).Write(aligned)
		}
	}
}

// onTSFrame is invoked by the demuxer goroutine for every extracted
// access unit. Routes to the same handle* methods the direct AV-path
// uses, so init-segment building and queue management are codec-
// agnostic to which input path delivered the frame.
//
// Runs on the demuxer goroutine — handleH264 / handleAAC acquire
// p.mu inside, so concurrency with onAVPacket (run-loop goroutine) is
// safe.
//
//nolint:exhaustive // unsupported codecs (PCM / MPEG audio L1/L2 / AC-3) intentionally drop
func (p *Packager) onTSFrame(cid mpeg2.TS_STREAM_TYPE, frame []byte, pts, dts uint64) {
	if len(frame) == 0 {
		return
	}
	av := &domain.AVPacket{
		Data:     frame,
		PTSms:    pts,
		DTSms:    dts,
		KeyFrame: false,
	}
	switch cid {
	case mpeg2.TS_STREAM_H264:
		av.Codec = domain.AVCodecH264
		av.KeyFrame = tsmux.KeyFrameH264(frame)
		p.onAVPacket(av)
	case mpeg2.TS_STREAM_H265:
		av.Codec = domain.AVCodecH265
		av.KeyFrame = tsmux.KeyFrameH265(frame)
		p.onAVPacket(av)
	case mpeg2.TS_STREAM_AAC:
		av.Codec = domain.AVCodecAAC
		p.onAVPacket(av)
	}
}

// onAVPacket processes an AV packet from the Normaliser-anchored path
// (RTSP / RTMP / mixer source). Updates init segments + queue.
func (p *Packager) onAVPacket(av *domain.AVPacket) {
	p.mu.Lock()
	defer p.mu.Unlock()

	switch av.Codec { //nolint:exhaustive // unsupported codecs (mp2, mp3, ac3, eac3, av1, mpeg2video, raw-TS marker, unknown) intentionally drop — DASH packager handles H.264 / H.265 / AAC only
	case domain.AVCodecH264:
		p.handleH264(av)
	case domain.AVCodecH265:
		p.isHEVC = true
		p.handleH264(av) // same code path; isHEVC flips downstream behaviour
	case domain.AVCodecAAC:
		p.handleAAC(av)
	}
}

// handleH264 enqueues an H.264 access unit and tries to build the video
// init segment if it isn't already built.
func (p *Packager) handleH264(av *domain.AVPacket) {
	if p.videoInit == nil && !p.videoPSGivenUp {
		p.accumulateVideoPS(av.Data)
		p.tryBuildVideoInit()
	}
	// Queue the frame regardless — once init is built, the queue's
	// existing contents are still valid (Normaliser-anchored PTS).
	p.queue.PushVideo(VideoFrame{
		AnnexB: cloneBytes(av.Data),
		PTSms:  av.PTSms,
		DTSms:  av.DTSms,
		IsIDR:  av.KeyFrame,
	})
	if p.videoInit != nil {
		p.state.OpenPairingWindow(time.Now())
	}
}

// handleAAC enqueues an AAC frame and tries to build the audio init
// segment from the ADTS header on the first frame.
func (p *Packager) handleAAC(av *domain.AVPacket) {
	if p.audioInit == nil {
		if hdr := parseADTS(av.Data); hdr != nil {
			ai, err := BuildAACInit(hdr)
			if err == nil {
				p.audioInit = ai
				if data, encErr := EncodeInit(ai.Init); encErr == nil {
					_ = writeFileAtomic(filepath.Join(p.cfg.StreamDir, dashAudioInitFile), data)
				}
			} else {
				slog.Warn("dash: BuildAACInit", "stream_id", p.cfg.StreamID, "err", err)
			}
		}
	}
	p.queue.PushAudio(AudioFrame{
		Raw:   stripADTSHeader(av.Data),
		PTSms: av.PTSms,
	})
	if p.audioInit != nil {
		p.state.OpenPairingWindow(time.Now())
	}
}

// accumulateVideoPS appends Annex-B byte stream to the SPS/PPS buffer,
// capped at videoPSAccumCap. Latches videoPSGivenUp when the cap is hit
// without success.
func (p *Packager) accumulateVideoPS(annexB []byte) {
	if len(p.videoPS)+len(annexB) > videoPSAccumCap {
		p.videoPSGivenUp = true
		slog.Warn("dash: videoPS cap exceeded without init — giving up video",
			"stream_id", p.cfg.StreamID, "cap_bytes", videoPSAccumCap)
		return
	}
	p.videoPS = append(p.videoPS, annexB...)
}

// tryBuildVideoInit attempts to build the H.264 or H.265 init segment
// from the accumulated SPS/PPS buffer. Writes init_v.mp4 to disk on
// success and clears the accumulator.
func (p *Packager) tryBuildVideoInit() {
	if p.isHEVC {
		vpss, spss, ppss := ExtractHEVCParameterSets(p.videoPS)
		if len(spss) == 0 || len(ppss) == 0 {
			return
		}
		vi, err := BuildH265Init(vpss, spss, ppss)
		if err != nil {
			slog.Warn("dash: BuildH265Init", "stream_id", p.cfg.StreamID, "err", err)
			return
		}
		p.installVideoInit(vi)
		return
	}
	spss, ppss := ExtractParameterSets(p.videoPS)
	if len(spss) == 0 || len(ppss) == 0 {
		return
	}
	vi, err := BuildH264Init(spss, ppss)
	if err != nil {
		slog.Warn("dash: BuildH264Init", "stream_id", p.cfg.StreamID, "err", err)
		return
	}
	p.installVideoInit(vi)
}

func (p *Packager) installVideoInit(vi *VideoInit) {
	p.videoInit = vi
	p.videoPS = nil
	data, err := EncodeInit(vi.Init)
	if err != nil {
		slog.Warn("dash: EncodeInit video", "stream_id", p.cfg.StreamID, "err", err)
		return
	}
	_ = writeFileAtomic(filepath.Join(p.cfg.StreamDir, dashVideoInitFile), data)
}

// onSessionBoundary handles a buffer.Packet with SessionStart=true.
// Flushes whatever's queued, drops the queue, signals the state machine.
//
// Per-track decode counters are preserved across the boundary so the
// MPD timeline stays monotonic — players keep advancing through the
// same SegmentTimeline without seeking back. Segment numbers also
// survive for the same reason.
//
// pairingTruncated is NOT reset — a session boundary mid-stream isn't a
// "fresh pairing window"; both tracks are already advancing and the
// post-boundary content joins their existing timelines. Resetting the
// latch would re-engage pairing-truncate and drop incoming frames
// while waiting for the pairing condition to re-fire.
func (p *Packager) onSessionBoundary() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state.OnSessionStart()
	// Drop queues — old session content shouldn't bleed into new session
	// segments. Init segments + AST + segment counters + nextDecode
	// counters survive.
	_ = p.queue.PopVideo(p.queue.VideoLen())
	_ = p.queue.PopAudio(p.queue.AudioLen())
	p.seg.Reset()
	p.state.OnSessionBoundaryHandled()
}

// tryCut checks the segmenter at every tick. Holds the mutex throughout
// — segmenter decisions + writes are short and locking the entire path
// avoids state-tearing under concurrent onPacket.
func (p *Packager) tryCut(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.videoInit == nil && p.audioInit == nil {
		return
	}

	haveVideo := p.videoInit != nil
	haveAudio := p.audioInit != nil

	videoReady := haveVideo && p.queue.VideoLen() > 0
	audioReady := haveAudio && p.queue.AudioLen() > 0

	if !p.state.CanEmitFirstSegment(now, videoReady, audioReady) {
		return
	}

	// First-segment moment: align queues so V and A start at the LATER
	// of the two firsts. This drops the earlier-arrived track's pre-
	// pairing accumulation (e.g. 4 s of audio that flowed while the
	// NVENC pipeline warmed up, or 16 s of mixer-drained V frames). The
	// pre-pairing content is lost, but media-time anchors stay aligned
	// — no perpetual A/V drift in the published timeline.
	//
	// Latch via pairingTruncated so this runs exactly once per session.
	// Otherwise, when Cut returns Ok=false on the post-truncate queue
	// (insufficient video), we'd loop and re-truncate every tick,
	// dropping each newly-arrived V frame because A's first PTS stays
	// ahead — observed on ABR streams as audio outrunning video by
	// tens of seconds in the live-edge MPD timeline.
	if !p.pairingTruncated && p.state.State() == StateWaitingForPairing && haveVideo && haveAudio {
		p.truncateAtPairingLocked()
		p.pairingTruncated = true
	}

	d := p.seg.Cut(now, p.queue, haveVideo, haveAudio)
	if !d.Ok {
		return
	}

	flushed := p.writeSegments(now, d)
	if flushed {
		p.seg.MarkCut(now)
		p.state.OnFirstSegmentFlushed()
		if p.availStart.IsZero() {
			p.availStart = now
		}
		p.trimWindow()
		p.publishManifest(now)
	}
}

// truncateAtPairingLocked drops frames whose PTSms is earlier than the
// later track's first frame. Called once at WaitingForPairing → Live
// transition so the first emitted segment for V and A starts at the
// SAME media-time anchor. Caller must hold p.mu.
func (p *Packager) truncateAtPairingLocked() {
	vFirst, ok1 := p.queue.FirstVideo()
	aFirst, ok2 := p.queue.FirstAudio()
	if !ok1 || !ok2 {
		return
	}
	threshold := vFirst.PTSms
	if aFirst.PTSms > threshold {
		threshold = aFirst.PTSms
	}
	if vd, ad := p.queue.TruncateBefore(threshold); vd > 0 || ad > 0 {
		slog.Info("dash: pairing-truncate",
			"stream_id", p.cfg.StreamID,
			"threshold_ms", threshold,
			"video_dropped", vd,
			"audio_dropped", ad,
		)
	}
}

// writeSegments drains the cut decision's frames, builds fmp4 fragments,
// and writes them to disk. Returns true if at least one segment was
// written (used by caller to gate AST/state advancement).
func (p *Packager) writeSegments(now time.Time, d CutDecision) bool {
	flushed := false

	if d.VideoCount > 0 && p.videoInit != nil {
		frames := p.queue.PopVideo(d.VideoCount)
		if len(frames) > 0 {
			if p.writeVideoSegment(now, frames) {
				flushed = true
			}
		}
	}
	if d.AudioCount > 0 && p.audioInit != nil && p.cfg.PackAudio {
		frames := p.queue.PopAudio(d.AudioCount)
		if len(frames) > 0 {
			if p.writeAudioSegment(now, frames) {
				flushed = true
			}
		} else if d.AudioCount > 0 {
			// d said drain N but the queue had nothing (e.g. audio-only
			// stream emptied between Cut and Pop). Still a benign case.
			_ = frames
		}
	} else if d.AudioCount > 0 && !p.cfg.PackAudio {
		// Non-primary ABR shard: drop the audio frames since the
		// primary shard packs audio for the whole ladder.
		_ = p.queue.PopAudio(d.AudioCount)
	}

	return flushed
}

// writeVideoSegment serialises one .m4s file and updates window state.
// Returns true on success.
//
// tfdt is wallclock-since-AST × 90 kHz. This anchors the segment's
// presentation time to wallclock so the DASH live edge math
// (live_edge = (now − AST) − liveDelay) finds the segment immediately
// after it's available. The earlier "sequential accumulator" approach
// (tfdt = sum of previous durs) interacted catastrophically with
// audio under-emission: when audio frames were missing, audio dur sums
// fell behind wallclock and the audio MPD timeline drifted >100 s
// behind video, making playback impossible. Wallclock-based tfdt
// hides under-emission as small per-segment gaps that players tolerate.
func (p *Packager) writeVideoSegment(now time.Time, frames []VideoFrame) bool {
	p.vSegN++
	name := fmt.Sprintf("seg_v_%05d.m4s", p.vSegN)

	tfdt := wallclockTicks(now, p.availStart, uint64(VideoTimescale))
	segDurTicks := computeVideoSegDurTicks(frames)

	data, err := BuildVideoFragment(uint32(p.vSegN), p.videoInit.TrackID, tfdt, frames, p.isHEVC, segDurTicks) //nolint:gosec // segN fits uint32 for the stream's lifetime
	if err != nil {
		slog.Warn("dash: BuildVideoFragment", "stream_id", p.cfg.StreamID, "err", err)
		p.vSegN--
		return false
	}
	if err := writeFileAtomic(filepath.Join(p.cfg.StreamDir, name), data); err != nil {
		slog.Warn("dash: write video segment", "stream_id", p.cfg.StreamID, "err", err)
		p.vSegN--
		return false
	}
	p.onDiskV = append(p.onDiskV, name)
	p.vSegEntries = append(p.vSegEntries, SegmentEntry{StartTicks: tfdt, DurTicks: segDurTicks})
	return true
}

// writeAudioSegment is the audio counterpart of writeVideoSegment.
func (p *Packager) writeAudioSegment(now time.Time, frames []AudioFrame) bool {
	p.aSegN++
	name := fmt.Sprintf("seg_a_%05d.m4s", p.aSegN)

	sr := uint64(p.audioInit.SampleRate) //nolint:gosec // SampleRate > 0
	tfdt := wallclockTicks(now, p.availStart, sr)
	durTicks := uint64(len(frames)) * 1024 // AAC: 1024 samples per frame

	data, err := BuildAudioFragment(uint32(p.aSegN), p.audioInit.TrackID, tfdt, frames) //nolint:gosec // segN fits uint32
	if err != nil {
		slog.Warn("dash: BuildAudioFragment", "stream_id", p.cfg.StreamID, "err", err)
		p.aSegN--
		return false
	}
	if err := writeFileAtomic(filepath.Join(p.cfg.StreamDir, name), data); err != nil {
		slog.Warn("dash: write audio segment", "stream_id", p.cfg.StreamID, "err", err)
		p.aSegN--
		return false
	}
	p.onDiskA = append(p.onDiskA, name)
	p.aSegEntries = append(p.aSegEntries, SegmentEntry{StartTicks: tfdt, DurTicks: durTicks})
	return true
}

// wallclockTicks returns (now − ast) × timescale, clamping to 0 when
// ast is unset (the very first segment whose tfdt should be 0). Used
// by both writeVideoSegment and writeAudioSegment so the two tracks
// share a wallclock anchor.
func wallclockTicks(now, ast time.Time, timescale uint64) uint64 {
	if ast.IsZero() {
		return 0
	}
	elapsed := now.Sub(ast).Milliseconds()
	if elapsed <= 0 {
		return 0
	}
	return uint64(elapsed) * timescale / 1000 //nolint:gosec // elapsed positive
}

// computeVideoSegDurTicks returns the total segment duration in 90 kHz
// ticks. For a non-empty queue it's PTSms of last frame minus first,
// scaled to 90 kHz. Zero falls back to a tiny non-zero value to keep
// downstream uint32 dur from being 0 (mp4ff rejects 0 dur on samples).
func computeVideoSegDurTicks(frames []VideoFrame) uint64 {
	if len(frames) < 2 {
		// Single-frame segment is degenerate but valid; pick 1 frame
		// at typical 40 ms (25 fps) so the player has SOMETHING.
		return uint64(VideoTimescale) * 40 / 1000
	}
	first := frames[0].PTSms
	last := frames[len(frames)-1].PTSms
	if last <= first {
		return uint64(VideoTimescale) * 40 / 1000
	}
	return (last - first) * uint64(VideoTimescale) / 1000
}

// trimWindow removes old segments from disk past Window + History.
func (p *Packager) trimWindow() {
	maxKeep := p.cfg.Window + p.cfg.History
	if !p.cfg.Ephemeral {
		return
	}
	for len(p.onDiskV) > maxKeep {
		_ = os.Remove(filepath.Join(p.cfg.StreamDir, p.onDiskV[0]))
		p.onDiskV = p.onDiskV[1:]
		if len(p.vSegEntries) > 0 {
			p.vSegEntries = p.vSegEntries[1:]
		}
	}
	for len(p.onDiskA) > maxKeep {
		_ = os.Remove(filepath.Join(p.cfg.StreamDir, p.onDiskA[0]))
		p.onDiskA = p.onDiskA[1:]
		if len(p.aSegEntries) > 0 {
			p.aSegEntries = p.aSegEntries[1:]
		}
	}
}

// publishManifest writes the per-stream MPD (when ManifestPath is set)
// and/or notifies the ABR master with a snapshot.
func (p *Packager) publishManifest(now time.Time) {
	in := p.buildManifestInputLocked(now)
	if in == nil {
		return
	}
	if p.cfg.ManifestPath != "" {
		if data, err := BuildManifest(in); err == nil && data != nil {
			_ = writeFileAtomic(p.cfg.ManifestPath, data)
		}
	}
	if p.cfg.ABRMaster != nil {
		p.cfg.ABRMaster.UpdateShard(ShardSnapshot{
			Slug:              p.cfg.ABRSlug,
			AvailabilityStart: p.availStart,
			Video:             firstTrack(in.Video),
			Audio:             in.Audio,
		})
	}
}

// buildManifestInputLocked snapshots window state into a ManifestInput.
// Returns nil when no track has emitted any segment yet.
//
// For ABR shards (ABRSlug non-empty + ManifestPath empty), init and
// media paths are prefixed with the shard slug so the root MPD —
// served at <stream>/index.mpd by the master — can reference
// <stream>/<slug>/init_v.mp4 etc. without further URL rewriting.
// Per-stream MPDs (single-rendition mode) keep the bare filenames
// since the MPD sits next to the segments in the stream dir.
func (p *Packager) buildManifestInputLocked(now time.Time) *ManifestInput {
	if len(p.vSegEntries) == 0 && len(p.aSegEntries) == 0 {
		return nil
	}
	in := &ManifestInput{
		AvailabilityStart: p.availStart,
		PublishTime:       now,
		SegDur:            p.cfg.SegDur,
		Window:            p.cfg.Window,
	}
	pathPrefix := ""
	if p.cfg.ABRSlug != "" && p.cfg.ManifestPath == "" {
		pathPrefix = p.cfg.ABRSlug + "/"
	}
	if p.videoInit != nil && len(p.vSegEntries) > 0 {
		bw := p.cfg.OverrideBandwidth
		if bw == 0 {
			bw = 5_000_000 // sensible default for video
		}
		in.Video = []TrackManifest{{
			RepID:        repIDOr(p.cfg.ABRSlug, "v0"),
			Codec:        p.videoInit.Info.Codec,
			Bandwidth:    bw,
			Width:        p.videoInit.Info.Width,
			Height:       p.videoInit.Info.Height,
			Timescale:    VideoTimescale,
			InitFile:     pathPrefix + dashVideoInitFile,
			MediaPattern: pathPrefix + dashVideoMediaPattern,
			StartNumber:  startNumberFor(p.onDiskV, p.cfg.Window),
			Segments:     windowTail(p.vSegEntries, p.cfg.Window),
		}}
	}
	if p.audioInit != nil && p.cfg.PackAudio && len(p.aSegEntries) > 0 {
		in.Audio = &TrackManifest{
			RepID:        "a0",
			Codec:        p.audioInit.Codec,
			Bandwidth:    128_000,
			SampleRate:   p.audioInit.SampleRate,
			Timescale:    uint32(p.audioInit.SampleRate), //nolint:gosec // SampleRate fits uint32
			InitFile:     pathPrefix + dashAudioInitFile,
			MediaPattern: pathPrefix + dashAudioMediaPattern,
			StartNumber:  startNumberFor(p.onDiskA, p.cfg.Window),
			Segments:     windowTail(p.aSegEntries, p.cfg.Window),
		}
	}
	return in
}

// finalFlush is called on ctx cancel / subscriber close to write any
// in-progress segment. Best-effort.
func (p *Packager) finalFlush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.queue.VideoLen() == 0 && p.queue.AudioLen() == 0 {
		return
	}
	d := CutDecision{
		Ok:         true,
		VideoCount: p.queue.VideoLen(),
		AudioCount: p.queue.AudioLen(),
	}
	if p.writeSegments(time.Now(), d) {
		p.trimWindow()
		p.publishManifest(time.Now())
	}
}

// ─── helpers ─────────────────────────────────────────────────────────

func cloneBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

func parseADTS(frame []byte) *aac.ADTSHeader {
	hdr, _, err := aac.DecodeADTSHeader(bytes.NewReader(frame))
	if err != nil {
		return nil
	}
	return hdr
}

// stripADTSHeader returns frame with the leading ADTS header removed.
// AAC inside MP4 must not carry ADTS headers (those are an MPEG-TS
// transport convention; mp4 uses sample size + ESDS for the same info).
//
// When the input doesn't start with a valid ADTS header (e.g. the
// ingestor stripped it upstream already), returns frame unchanged.
func stripADTSHeader(frame []byte) []byte {
	hdr, _, err := aac.DecodeADTSHeader(bytes.NewReader(frame))
	if err != nil || hdr == nil {
		return cloneBytes(frame)
	}
	hs := int(hdr.HeaderLength)
	if hs <= 0 || hs >= len(frame) {
		return cloneBytes(frame)
	}
	return cloneBytes(frame[hs:])
}

// firstTrack returns &tracks[0] or nil when empty.
func firstTrack(tracks []TrackManifest) *TrackManifest {
	if len(tracks) == 0 {
		return nil
	}
	t := tracks[0]
	return &t
}

// repIDOr returns slug when non-empty, otherwise fallback. Used to make
// ABR shard reps inherit the slug name in the manifest.
func repIDOr(slug, fallback string) string {
	if slug != "" {
		return slug
	}
	return fallback
}

// startNumberFor derives the StartNumber for SegmentTemplate from the
// filenames in the sliding window. The first filename's numeric suffix
// is the start number.
func startNumberFor(names []string, window int) uint64 {
	if len(names) == 0 {
		return 1
	}
	if len(names) > window {
		names = names[len(names)-window:]
	}
	return parseSegNum(names[0])
}

// parseSegNum extracts the numeric suffix from "seg_v_00042.m4s".
func parseSegNum(name string) uint64 {
	const minLen = len("seg_v_00001.m4s")
	if len(name) < minLen {
		return 1
	}
	// Find the underscore before the number.
	i := len(name) - len(".m4s") - 1
	j := i
	for j >= 0 && name[j] >= '0' && name[j] <= '9' {
		j--
	}
	if j == i {
		return 1
	}
	var n uint64
	for _, ch := range name[j+1 : i+1] {
		n = n*10 + uint64(ch-'0')
	}
	if n == 0 {
		return 1
	}
	return n
}

// windowTail returns the last `n` elements of segs, or all when fewer.
func windowTail(segs []SegmentEntry, n int) []SegmentEntry {
	if len(segs) <= n {
		return append([]SegmentEntry(nil), segs...)
	}
	return append([]SegmentEntry(nil), segs[len(segs)-n:]...)
}
