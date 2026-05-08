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
	"sync"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// Config knobs the rebaser. Zero value disables the feature; Apply is
// then a no-op so wiring it in is safe before any caller is ready.
type Config struct {
	// Enabled gates the whole feature. False ⇒ Apply is a no-op.
	Enabled bool

	// JumpThresholdMs caps how far an output PTS may stray from
	// (now − wallOrigin) before we hard-re-anchor and flag a
	// Discontinuity. Drift below this band passes through untouched.
	JumpThresholdMs int64
}

// DefaultConfig returns the recommended steady-state settings.
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

// Apply rewrites p.PTSms / p.DTSms in place. May set p.Discontinuity =
// true on hard re-anchor; existing Discontinuity flags are preserved.
//
// Returns immediately if disabled, the packet is nil, the codec is the
// AVCodecRawTSChunk marker, or the codec doesn't classify as V or A.
func (r *Rebaser) Apply(p *domain.AVPacket, now time.Time) {
	if r == nil || !r.cfg.Enabled || p == nil {
		return
	}
	if p.Codec == domain.AVCodecRawTSChunk {
		return
	}

	tk, ok := trackKeyFor(p.Codec)
	if !ok {
		return
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
		// First packet of this track — anchor its output at the
		// current elapsed wallclock so V and A retain whatever
		// startup arrival offset the source produced.
		track.seeded = true
		track.inputOrigin = inDts
		track.outputAnchor = actualNowMs
		track.lastOutputDts = actualNowMs
		assignTimes(p, actualNowMs, cto)
		return
	}

	inputDelta := inDts - track.inputOrigin
	expectedDts := track.outputAnchor + inputDelta
	drift := expectedDts - actualNowMs

	// Hard re-anchor when:
	//   - drift exceeds threshold (upstream PTS jumped or rolled over), OR
	//   - the proposed output would regress past the monotonicity
	//     floor (downstream dur math is uint64 — never let it
	//     underflow, no matter what the input did).
	if absInt64(drift) > r.cfg.JumpThresholdMs || expectedDts < track.lastOutputDts {
		target := actualNowMs
		if target <= track.lastOutputDts {
			// Wallclock too close to (or behind) last emitted; nudge
			// forward by 1 ms to preserve strict monotonicity.
			target = track.lastOutputDts + 1
		}
		track.inputOrigin = inDts
		track.outputAnchor = target
		track.lastOutputDts = target
		assignTimes(p, target, cto)
		p.Discontinuity = true
		return
	}

	track.lastOutputDts = expectedDts
	assignTimes(p, expectedDts, cto)
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
