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

// seedPairWaitTimeoutSec caps how long Apply defers seeding a track
// while waiting for the OTHER track's first packet to arrive. After
// this elapses without the partner showing up the track seeds solo —
// the assumption is video-only / audio-only source, or a partner
// track that simply never connects (rare RTSP / RTMP encoder
// configs). Picked at 5 s: shorter than the 6-8 s shaka-player
// startup tolerance, longer than the typical sub-second A/V
// interleave gap so we don't false-trigger on legitimately paired
// streams whose partner is briefly delayed.
const seedPairWaitTimeoutSec = 5

// trackState is the per-(stream, track) anchor.
//
//   - inputOrigin: input DTS used as the source-side reference point.
//     Set at seed time; subsequent output PTS = outputAnchor +
//     (input - inputOrigin) so the track's frame cadence carries
//     through.
//   - outputAnchor: where this track's first emitted packet lands in
//     the output timeline, in ms since r.wallOrigin. Seed sets it to
//     actualNowMs of the first packet that passes seeding.
//   - lastOutputDts: monotonicity floor. After a hard re-anchor we
//     always emit ≥ lastOutputDts + 1 so downstream uint64 dur math
//     (vDTS[i+1] − vDTS[i]) can never underflow.
//
// V/A-pairing fields drive the cross-track delay-align logic that
// replaced the older cross-track-snap heuristic:
//
//   - firstSeenSet / firstSeenDts / firstSeenAt: the track's first
//     observed packet. Recorded immediately, BEFORE seeding, so the
//     other track's later first-packet arrival can decide whether to
//     promote the late track's first DTS to a shared origin.
//   - Until BOTH tracks have firstSeenSet (or seedPairWaitTimeoutSec
//     elapses), Apply drops incoming packets for the unseed track —
//     we'd rather lose a fraction of a second of one track at startup
//     than bake a multi-second A/V offset into the buffer hub that
//     downstream packagers cannot compensate for.
type trackState struct {
	seeded        bool
	inputOrigin   int64
	outputAnchor  int64
	lastOutputDts int64

	firstSeenSet bool
	firstSeenDts int64
	firstSeenAt  time.Time

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
	otherTrack := &r.tracks[numTracks-1-tk]
	actualNowMs := now.Sub(r.wallOrigin).Milliseconds()

	if !track.seeded {
		return r.maybeSeedTrackLocked(track, otherTrack, inDts, actualNowMs, cto, now, p)
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

// maybeSeedTrackLocked is the V/A-pairing seed gate. Replaces the old
// per-track "seed immediately on first packet" path that preserved
// whatever arrival-time skew the source produced — perfectly fine for
// well-behaved encoders (audio leading video by 100 ms after RTSP
// jitter-buffering, AAC encoder pre-roll of 22-44 ms) but disastrous
// for upstream HLS feeds whose V and A PES PTS happen to differ by
// many seconds. Origin-tracking + per-track seeds preserved the gap
// all the way into DASH tfdt, and downstream packagers couldn't
// recover.
//
// Algorithm:
//
//  1. Record this track's first observed DTS + arrival time. Cumulative
//     across calls — only set once.
//  2. If the OTHER track has NOT yet recorded its firstSeen AND the
//     wait timeout hasn't fired: drop this packet (return false). The
//     caller MUST NOT write a dropped packet into the buffer hub.
//     Subsequent packets of this same track also drop until either
//     pairing resolves or the timeout fires — so we lose up to
//     seedPairWaitTimeoutSec of the early-arriving track at startup.
//  3. Once both firstSeens are recorded (the common case, partner
//     usually arrives within tens of ms) OR the timeout fires (audio-
//     only / video-only sources), decide the shared origin:
//     - Both present: origin = max(firstSeenDts, otherTrack.firstSeenDts).
//       The LATER source moment becomes the timeline zero so the early
//     - Single track (timeout): origin = this packet's DTS, so we
//       don't bake the wait into the output timeline.
//  4. If this packet's DTS is still below the chosen origin, drop it
//     (the early track is catching up to the late track's first
//     moment). Subsequent packets keep dropping until DTS >= origin
//     naturally — this is the "skip-until" phase.
//  5. Otherwise, seed: inputOrigin = origin, outputAnchor = wallclock
//     elapsed NOW. Emit the packet rebased.
//
// Returns true iff the packet was rebased and should be written to
// the buffer hub. Caller must hold r.mu.
func (r *Rebaser) maybeSeedTrackLocked(
	track, otherTrack *trackState,
	inDts, actualNowMs, cto int64,
	now time.Time,
	p *domain.AVPacket,
) bool {
	if !track.firstSeenSet {
		track.firstSeenSet = true
		track.firstSeenDts = inDts
		track.firstSeenAt = now
	}

	bothSeen := otherTrack.firstSeenSet
	waitExpired := now.Sub(track.firstSeenAt) >= seedPairWaitTimeoutSec*time.Second
	if !bothSeen && !waitExpired {
		return false
	}

	var origin int64
	if bothSeen {
		origin = track.firstSeenDts
		if otherTrack.firstSeenDts > origin {
			origin = otherTrack.firstSeenDts
		}
	} else {
		// Timed out without partner. Anchor the timeline on this
		// packet's DTS — using firstSeenDts here would output a packet
		// at media time `seedPairWaitTimeoutSec` seconds, since
		// outputAnchor = now − wallOrigin elapsed-while-waiting.
		origin = inDts
	}

	if inDts < origin {
		return false
	}

	// Buffer-and-shift anchor: if the OTHER track already seeded, this
	// track's first emission must land at the OTHER's outputAnchor so
	// V and A at the same source PES emit at the same output PTS.
	// Without this, the late-to-catch-up track's outputAnchor lands at
	// its OWN (later) seed wallclock — the output timeline carries a
	// gap equal to the catch-up period (audio-leads-video by 720ms
	// when V upstream took 720ms to reach origin, as observed on
	// test_puhser). The track that seeded first is still anchored at
	// its actualNowMs, so the wallclock relationship to the buffer
	// hub stays sensible for downstream packagers.
	outputAnchor := actualNowMs
	if otherTrack.seeded {
		outputAnchor = otherTrack.outputAnchor
	}
	track.seeded = true
	track.inputOrigin = origin
	track.outputAnchor = outputAnchor
	track.lastOutputDts = outputAnchor
	assignTimes(p, outputAnchor, cto)
	return true
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
