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
// stream. Inter-frame deltas (frame rate, A/V composition offset) are
// preserved; the only thing dropped is the upstream's drift relative
// to the host.
//
// Scope: AV-path packets only. Raw-TS chunks (UDP / HLS / HTTP-TS / SRT
// / file pull) carry PTS inside PES headers and need PES rewriting,
// which is a follow-up — they're not handled here.
package ptsrebaser

import (
	"sync"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// Config knobs the rebaser. Zero-value is a permissive default: feature
// disabled (Apply is a no-op), so wiring it in is safe even before
// operators flip the global config switch.
type Config struct {
	// Enabled gates the whole feature. False ⇒ Apply is a no-op.
	Enabled bool

	// JumpThresholdMs is the maximum drift (output PTS minus wallclock,
	// in ms) tolerated before we hard-reset the anchor and flag a
	// Discontinuity. Drift accumulates linearly at the upstream's
	// clock-skew rate; with the default 2000ms and a typical 0.28%
	// skew, hard reset fires roughly every 12 minutes. Larger values
	// space resets out at the cost of letting the timeline lead
	// wallclock further before correcting.
	JumpThresholdMs int64
}

// DefaultConfig returns the recommended steady-state settings.
func DefaultConfig() Config {
	return Config{
		Enabled:         true,
		JumpThresholdMs: 2000,
	}
}

// Rebaser is per-stream PTS anchor state. Safe for concurrent Apply calls
// — the typical caller is a single readLoop goroutine, but push servers
// fan out to multiple writer goroutines so the lock is necessary.
type Rebaser struct {
	cfg Config

	mu          sync.Mutex
	seeded      bool
	wallOrigin  time.Time
	inputOrigin int64 // first packet's DTSms (signed for safe arithmetic)
}

// New returns a Rebaser configured with cfg. A nil-equivalent (Enabled=false)
// rebaser is a perfectly valid object — Apply will pass packets through
// untouched.
func New(cfg Config) *Rebaser {
	return &Rebaser{cfg: cfg}
}

// Reset clears all anchoring state. Call when the source has provably
// torn down (failover, manual restart) so the next packet re-seeds
// against the new wallclock. Safe to call before any Apply.
func (r *Rebaser) Reset() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.seeded = false
	r.wallOrigin = time.Time{}
	r.inputOrigin = 0
	r.mu.Unlock()
}

// Apply rewrites p.PTSms / p.DTSms in place to be wallclock-anchored.
// It may set p.Discontinuity = true when a hard reset fires; existing
// Discontinuity flags are preserved (OR-ed in).
//
// Returns immediately if disabled, the packet is nil, or it is the
// AVCodecRawTSChunk marker — those are out of scope for this layer.
func (r *Rebaser) Apply(p *domain.AVPacket, now time.Time) {
	if r == nil || !r.cfg.Enabled || p == nil {
		return
	}
	if p.Codec == domain.AVCodecRawTSChunk {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	inDts := int64(p.DTSms) //nolint:gosec // bounded by upstream timebase
	inPts := int64(p.PTSms) //nolint:gosec // bounded by upstream timebase
	cto := inPts - inDts
	if cto < 0 {
		// Should never happen on well-formed streams (PTS >= DTS), but
		// don't propagate a value that would underflow downstream.
		cto = 0
	}

	if !r.seeded {
		r.wallOrigin = now
		r.inputOrigin = inDts
		r.seeded = true
		assignTimes(p, 0, cto)
		return
	}

	inputDelta := inDts - r.inputOrigin
	actualNowMs := now.Sub(r.wallOrigin).Milliseconds()
	expectedDts := inputDelta
	drift := expectedDts - actualNowMs

	if absInt64(drift) > r.cfg.JumpThresholdMs {
		// Hard re-anchor: pin THIS packet at the current wallclock and
		// resume accumulating from there. Downstream consumers see the
		// jump as a Discontinuity and reset their muxer / demuxer carry.
		r.wallOrigin = now
		r.inputOrigin = inDts
		assignTimes(p, 0, cto)
		p.Discontinuity = true
		return
	}

	assignTimes(p, expectedDts, cto)
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
