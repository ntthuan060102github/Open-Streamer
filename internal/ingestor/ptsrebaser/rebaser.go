// Package ptsrebaser anchors AVPacket PTS/DTS to local wallclock so
// downstream consumers (HLS / DASH segmenters, DVR, republishers) never
// see drift accumulated by upstream encoders.
//
// Why this exists: live encoders out in the wild routinely run their
// frame clock a fraction of a percent off NTP. Open-Streamer preserves
// input PTS as-is through the buffer hub, so after enough hours the
// DASH SegmentTimeline's media time runs ahead of (now − AST) by tens
// or hundreds of seconds; players that compute live edge as
// (now − AST) − SPD then can't find segments at the requested media
// time and stall on a long startup buffer (or refuse to play at all).
//
// The rebaser sits between the PacketReader and the buffer.Service write
// step on the AV path. It rewrites each packet's PTSms / DTSms so the
// emitted timeline tracks wallclock since the first packet of the
// stream. Inter-frame deltas (frame rate) are preserved per track, A/V
// offset relationships at startup are preserved, the only thing
// dropped is the upstream's drift relative to the host.
//
// State is kept PER TRACK (video and audio independently) — this is
// load-bearing. RTSP / RTMP carry V and A on different timebases (RTP
// SSRCs, FLV channel timestamps); a single shared origin would see
// every audio packet as a huge delta from the last video origin and
// vice versa, ping-ponging the anchor on every other frame and
// emitting a non-monotonic PTS sequence that downstream uint64 dur
// math would amplify into many-second sample durations.
//
// Scope: AV-path packets only. Raw-TS chunks (UDP / HLS / HTTP-TS /
// SRT / file pull) carry PTS inside PES headers and need PES
// rewriting, which is a follow-up — they're not handled here.
package ptsrebaser

import (
	"log/slog"
	"sync"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// Config knobs the rebaser. Zero value disables the feature; Apply is
// then a no-op so wiring it in is safe before any caller is ready.
type Config struct {
	// Enabled gates the whole feature. False ⇒ Apply is a pass-through.
	Enabled bool

	// JumpThresholdMs caps the burst-tolerant drift (output PTS minus
	// max(actualNow, lastOutputDts)) before we hard-re-anchor and flag
	// a Discontinuity. Catches large input PTS jumps while letting
	// short bursts pass through (the bursty-delivery test_mixer/test5
	// case from real RTMP republishes).
	JumpThresholdMs int64

	// MaxAheadMs caps how far the OUTPUT timeline may sit ahead of
	// (now − wallOrigin) before incoming packets are dropped. Unlike
	// JumpThresholdMs (which compares against the per-track running
	// position), this compares against real wallclock — so sustained
	// "input bursting faster than realtime" cases (mixer drain, slow-
	// consumer catch-up at startup) get throttled instead of letting
	// the segment timeline race indefinitely ahead of publishTime and
	// breaking downstream HLS / DASH liveness math.
	MaxAheadMs int64

	// StreamCode is observability-only — included as a log field on
	// re-anchor events so operators can correlate Discontinuity bursts
	// with the affected stream without having to inspect call stacks.
	// Optional: empty string is acceptable and just drops the field.
	StreamCode domain.StreamCode
}

// reanchorLogEveryN throttles the re-anchor warn log. The first event
// per (track, reason) always logs; subsequent events log once every N.
// 100 keeps a sustained ~5 re-anchor/s pathology (as seen on test3)
// down to one log entry every ~20s — enough cadence to diagnose, low
// enough not to drown the log.
const reanchorLogEveryN = 100

// DefaultConfig returns the recommended steady-state settings. MaxAheadMs
// is intentionally left at 0 (disabled): the drop semantics create a stuck
// state when sustained drift exceeds the cap (drift never decreases for
// real-time input → every subsequent packet drops indefinitely), which
// breaks downstream consumers fed from a drifted buffer hub (mixer://,
// republish RTSP/RTMP). The DASH packager keeps its own per-track drift
// cap (shouldSkipVideoLocked / shouldSkipAudioLocked) which fires at the
// PACKAGER boundary instead of the ingest boundary — that one re-anchors
// videoNextDecode to wallclock at IDR boundary, so manifest tfdt stays
// healthy without forcing zero V on consumers that subscribe to the
// upstream's main buffer directly. Operators / tests that want the
// stricter ingest cap can set MaxAheadMs explicitly.
func DefaultConfig() Config {
	return Config{
		Enabled:         true,
		JumpThresholdMs: 2000,
	}
}

// trackKey buckets AVCodec into the V / A slot used for per-track
// anchor state. Codecs that classify as neither (the raw-TS marker,
// AVCodecUnknown) are skipped at the call site.
type trackKey int

const (
	trackVideo trackKey = iota
	trackAudio
	numTracks
)

// crossTrackSnapMs is the cross-track progression gap (ms) above which
// a newly-seeded track snaps its output anchor onto the already-seeded
// other track instead of starting from current wallclock-elapsed.
//
// We compare against the OTHER track's CONTENT progression, not its
// wallclock arrival time, because mixer-style sources (mixer://,
// copy:// of a busy buffer) can drain a multi-second burst of one
// track's frames into the rebaser within microseconds — wallclock-
// based snap would never fire (both tracks "arrived within 1 ms")
// while the late-seeding track is content-wise seconds behind. The
// progression check sees the burst directly and snaps the anchor
// accordingly so V and A play in lockstep regardless of how the
// upstream chose to deliver them.
//
// RTSP / RTMP same-source A/V interleave within tens of ms — neither
// has time to progress past 1 s before the other seeds — so the
// typical sub-second intrinsic A/V offset still survives.
const crossTrackSnapMs int64 = 1000

// trackState is the per-(stream, track) anchor.
//
//   - inputOrigin: input DTS at this track's first observed packet.
//   - outputAnchor: output DTS to emit for that first packet — equal
//     to elapsed ms since wallOrigin at the time it arrived. Subsequent
//     packets emit outputAnchor + (input - inputOrigin), preserving the
//     track's frame cadence.
//   - lastOutputDts: monotonicity floor for this track. After a hard
//     re-anchor we always emit ≥ lastOutputDts + 1 so downstream uint64
//     dur math (vDTS[i+1] − vDTS[i]) can never underflow.
type trackState struct {
	seeded        bool
	inputOrigin   int64
	outputAnchor  int64
	lastOutputDts int64

	// Per-(track, reason) re-anchor counters drive the log throttle in
	// Apply. Cumulative across the rebaser lifetime; reset along with
	// the rest of the track state in Reset / on a fresh New.
	reanchorsDrift      int64
	reanchorsRegression int64
}

// Rebaser is per-stream PTS anchor state. Safe for concurrent Apply
// calls — pull workers use a single goroutine, but push servers can
// fan out to multiple writer goroutines so the lock is necessary.
type Rebaser struct {
	cfg Config

	mu         sync.Mutex
	wallOrigin time.Time
	wallSeeded bool
	tracks     [numTracks]trackState
}

// New returns a Rebaser configured with cfg. An Enabled=false rebaser
// is valid; Apply will pass packets through untouched.
func New(cfg Config) *Rebaser {
	return &Rebaser{cfg: cfg}
}

// Reset clears all anchoring state. Call when the source has provably
// torn down so the next packet re-seeds against the new wallclock.
// Safe to call before any Apply or on a nil receiver.
func (r *Rebaser) Reset() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.wallSeeded = false
	r.wallOrigin = time.Time{}
	r.tracks = [numTracks]trackState{}
	r.mu.Unlock()
}

// Apply rewrites p.PTSms / p.DTSms in place and returns whether the
// caller should keep the packet. A false return means the rebaser
// chose to drop this frame — typically because emitting it would
// push the output timeline more than MaxAheadMs ahead of wallclock
// (sustained input burst that downstream packagers cannot honour
// without violating live-edge math). The caller must NOT write a
// dropped packet into the buffer hub.
//
// Returns true (pass through, no rewrite) when the rebaser is
// disabled, the packet is nil, the codec is the AVCodecRawTSChunk
// marker, or the codec doesn't classify as V or A.
//
// May set p.Discontinuity = true on hard re-anchor; existing
// Discontinuity flags are preserved.
func (r *Rebaser) Apply(p *domain.AVPacket, now time.Time) bool {
	if r == nil || !r.cfg.Enabled || p == nil {
		return true
	}
	if p.Codec == domain.AVCodecRawTSChunk {
		return true
	}

	tk, ok := trackKeyFor(p.Codec)
	if !ok {
		return true
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.wallSeeded {
		r.wallOrigin = now
		r.wallSeeded = true
	}

	inDts := int64(p.DTSms) //nolint:gosec // bounded by upstream timebase
	inPts := int64(p.PTSms) //nolint:gosec // bounded by upstream timebase
	cto := inPts - inDts
	if cto < 0 {
		// Well-formed streams have PTS >= DTS; clamp rather than
		// propagate a value that would invert the offset downstream.
		cto = 0
	}

	track := &r.tracks[tk]
	actualNowMs := now.Sub(r.wallOrigin).Milliseconds()

	if !track.seeded {
		r.seedTrackLocked(track, tk, inDts, actualNowMs, cto, p)
		return true
	}

	inputDelta := inDts - track.inputOrigin
	expectedDts := track.outputAnchor + inputDelta

	// Real drift = how far the proposed output PTS would sit ahead of
	// wallclock-since-wallOrigin. When a single producer sustains an
	// above-realtime pace (mixer drain, slow-consumer catch-up at
	// startup) drift accumulates indefinitely; cap it by DROPPING the
	// frame when it would push us past MaxAheadMs. State is left
	// untouched so subsequent frames keep getting evaluated against
	// the same anchor — wallclock catches up, drift falls below the
	// cap, and frames pass through again. Net result: output PTS
	// rate ≈ wallclock rate (with a small bounded burst-tolerance
	// window), which is what the segmenters and players need.
	if r.cfg.MaxAheadMs > 0 && expectedDts-actualNowMs > r.cfg.MaxAheadMs {
		return false
	}

	// Burst-tolerant drift uses max(actualNow, lastOutputDts) so a
	// short burst — RTMP/SRT often ship a batch of packets in a few
	// wallclock ms each spaced 40 ms in DTS — doesn't manufacture a
	// fake drift against the slow-growing actualNow inside the burst.
	// Only a real input jump that overshoots even the running output
	// position is a re-anchor signal.
	effActualNow := actualNowMs
	if effActualNow < track.lastOutputDts {
		effActualNow = track.lastOutputDts
	}
	drift := expectedDts - effActualNow

	// Hard re-anchor when:
	//   - |drift| exceeds threshold (input PTS jumped past where any
	//     reasonable smoothing would expect), OR
	//   - the proposed output would regress past the monotonicity
	//     floor (downstream dur math is uint64 — never let it
	//     underflow, no matter what the input did).
	if absInt64(drift) > r.cfg.JumpThresholdMs || expectedDts < track.lastOutputDts {
		r.reanchorLocked(track, tk, p, reanchorInputs{
			inDts:       inDts,
			expectedDts: expectedDts,
			actualNowMs: actualNowMs,
			drift:       drift,
			cto:         cto,
		})
		return true
	}

	track.lastOutputDts = expectedDts
	assignTimes(p, expectedDts, cto)
	return true
}

// seedTrackLocked installs the per-track anchor on a track's first
// observed packet. Default anchor is current elapsed wallclock — this
// preserves any small intrinsic A/V offset the source produced
// (RTSP audio leading video by ~100 ms, RTMP codec-config pre-roll).
//
// Mixer-style sources (mixer://, copy:// from a non-AV upstream) can
// deliver V and A from independent producers whose first packets
// arrive seconds apart. Preserving that arrival skew bakes a multi-
// second A/V offset into the output, which downstream HLS / DASH
// packagers can't compensate for and hls.js refuses to sync. When the
// OTHER track is already seeded AND its arrival was more than
// crossTrackSnapMs ago, snap this track's anchor to where the other
// track is now so both streams start in lockstep.
//
// Caller must hold r.mu.
func (r *Rebaser) seedTrackLocked(
	track *trackState, tk trackKey, inDts, actualNowMs, cto int64, p *domain.AVPacket,
) {
	anchor := actualNowMs
	otherTrack := &r.tracks[numTracks-1-tk]
	if otherTrack.seeded {
		// Progression-based snap: how far the other track has already
		// moved its output PTS since its own seed. Catches both
		// "arrived seconds late" (RTSP A behind V) and "drained burst"
		// (mixer V emitted seconds of frames before A's first packet).
		otherProgression := otherTrack.lastOutputDts - otherTrack.outputAnchor
		if otherProgression > crossTrackSnapMs {
			anchor = otherTrack.lastOutputDts
		}
	}
	track.seeded = true
	track.inputOrigin = inDts
	track.outputAnchor = anchor
	track.lastOutputDts = anchor
	assignTimes(p, anchor, cto)
}

// reanchorInputs bundles the read-only values reanchorLocked needs.
// Kept as a struct so Apply doesn't blow past the parameter-count lint
// when it hands them across.
type reanchorInputs struct {
	inDts       int64
	expectedDts int64
	actualNowMs int64
	drift       int64
	cto         int64
}

// reanchorLocked applies the monotonic-floor target, bumps the per-
// reason re-anchor counter, throttle-logs, and rewrites the packet.
// Extracted from Apply purely to keep Apply's cognitive complexity
// within budget — semantics live inline at the call site, this is
// the body that used to live inside the `if absInt64(drift) > …` block.
//
// Caller must hold r.mu.
func (r *Rebaser) reanchorLocked(track *trackState, tk trackKey, p *domain.AVPacket, in reanchorInputs) {
	target := in.actualNowMs
	if target <= track.lastOutputDts {
		// Wallclock too close to (or behind) last emitted; nudge
		// forward by 1 ms to preserve strict monotonicity.
		target = track.lastOutputDts + 1
	}
	reason := "drift"
	count := &track.reanchorsDrift
	if in.expectedDts < track.lastOutputDts {
		reason = "regression"
		count = &track.reanchorsRegression
	}
	*count++
	if *count == 1 || *count%reanchorLogEveryN == 0 {
		slog.Warn("ptsrebaser: hard re-anchor — flagging Discontinuity",
			"stream_code", string(r.cfg.StreamCode),
			"track", trackKeyName(tk),
			"reason", reason,
			"count", *count,
			"in_dts_ms", in.inDts,
			"expected_dts_ms", in.expectedDts,
			"actual_now_ms", in.actualNowMs,
			"drift_ms", in.drift,
			"last_output_dts_ms", track.lastOutputDts,
			"target_ms", target,
			"jump_threshold_ms", r.cfg.JumpThresholdMs,
		)
	}
	track.inputOrigin = in.inDts
	track.outputAnchor = target
	track.lastOutputDts = target
	assignTimes(p, target, in.cto)
	p.Discontinuity = true
}

// trackKeyName returns the slog-friendly track label.
func trackKeyName(tk trackKey) string {
	if tk == trackVideo {
		return "video"
	}
	return "audio"
}

// trackKeyFor maps AVCodec to its V / A slot. Returns ok=false for
// codecs the rebaser doesn't classify (marker, unknown).
func trackKeyFor(c domain.AVCodec) (trackKey, bool) {
	switch {
	case c.IsVideo():
		return trackVideo, true
	case c.IsAudio():
		return trackAudio, true
	default:
		return 0, false
	}
}

// assignTimes writes outDts (>= 0) and outDts + cto into the packet.
func assignTimes(p *domain.AVPacket, outDts, cto int64) {
	if outDts < 0 {
		outDts = 0
	}
	outPts := outDts + cto
	if outPts < 0 {
		outPts = 0
	}
	p.DTSms = uint64(outDts) //nolint:gosec // outDts clamped >= 0 above
	p.PTSms = uint64(outPts) //nolint:gosec // outPts clamped >= 0 above
}

func absInt64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
