// Package timeline owns the single, authoritative PTS/DTS anchor for
// every elementary-stream packet that reaches the buffer hub.
//
// The Normaliser unified three legacy state machines that previously
// each owned a slice of "the stream's clock":
//
//   - internal/ingestor/ptsrebaser (deleted) — AV-path wallclock anchor.
//   - internal/ingestor/pull/mixer's videoPTSBase / audioPTSBase
//     (deleted).
//   - internal/coordinator/abr_mixer's ptsRebaser (deleted) — replaced
//     by per-cycle Normaliser instances seeded from entry.t0.
//
// All three used to set a `AVPacket.Discontinuity` flag with different
// meanings. That flag is also gone: session-boundary signalling moved
// to `buffer.Packet.SessionStart` and rebaser-internal re-anchor events
// surface only via Stats / LastDiagnostic for telemetry. Consumers
// never re-normalise.
//
// The Normaliser is wallclock-anchored, session-aware, V/A-pair-aware,
// monotonic-floored, drift-capped. The OWNER is the ingestor: each
// stream has exactly one Normaliser; the buffer hub guarantees PTS/DTS
// are anchored when a consumer reads them and that `SessionStart=true`
// fronts every new lifetime.
package timeline

import (
	"sync"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// Config controls Normaliser behaviour. The zero value is valid and
// produces a disabled Normaliser whose Apply is a pass-through — handy
// for tests and for the observer-disabled production default.
type Config struct {
	// Enabled gates the whole feature. False ⇒ Apply is a pass-through,
	// OnSession is a no-op.
	Enabled bool

	// JumpThresholdMs caps the burst-tolerant drift (output PTS minus
	// max(actualNow, lastOutputDts)) before a hard re-anchor fires. Same
	// semantics and units as ptsrebaser.Config.JumpThresholdMs.
	JumpThresholdMs int64

	// MaxAheadMs caps how far ahead of (now − sessionStart) the output
	// timeline may sit before incoming packets are dropped. Same as
	// ptsrebaser.Config.MaxAheadMs.
	MaxAheadMs int64

	// MaxBehindMs is the symmetric counterpart of MaxAheadMs — when the
	// proposed output sits more than MaxBehindMs behind wallclock the
	// packet is hard-re-anchored to the wallclock floor. Zero disables
	// this branch (the existing rebaser has no equivalent; the
	// monotonic-floor branch handles strict-monotonic regressions but
	// not "wallclock kept moving while source paused" cases).
	MaxBehindMs int64

	// CrossTrackSnapMs is the cross-track progression gap above which a
	// newly-seeded track snaps its output anchor onto the other track's
	// last emitted DTS. Mirrors ptsrebaser.crossTrackSnapMs (1000 ms),
	// exposed here as a knob so tests can vary it.
	CrossTrackSnapMs int64
}

// DefaultConfig matches ptsrebaser.DefaultConfig so a side-by-side run
// converges on the same outputs for well-behaved sources. Operators or
// tests that want the stricter ahead / behind cap can override per call.
func DefaultConfig() Config {
	return Config{
		Enabled:          true,
		JumpThresholdMs:  2000,
		CrossTrackSnapMs: 1000,
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

// trackKeyFor maps AVCodec to its V / A slot. Returns ok=false for
// codecs the Normaliser doesn't classify (marker, unknown).
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

// trackState is the per-(stream, track) anchor.
//
// inputOrigin is the source DTS observed on the first packet of the
// current session. outputAnchor is the wallclock-relative DTS to emit
// for that first packet. Subsequent packets emit
// outputAnchor + (input - inputOrigin), preserving the source's frame
// cadence within a session.
//
// lastOutputDts is the monotonicity floor for this track — every emit
// is clamped to ≥ lastOutputDts + 1 so downstream uint64 dur math
// can never underflow on backward jumps.
type trackState struct {
	seeded        bool
	inputOrigin   int64
	outputAnchor  int64
	lastOutputDts int64
}

// Normaliser is the per-stream timeline anchor. One instance per
// `buffer.StreamCode`. Safe for concurrent Apply / OnSession calls; the
// internal lock matches ptsrebaser's contract because push servers fan
// writes out across multiple goroutines.
type Normaliser struct {
	cfg Config

	mu          sync.Mutex
	sessionID   uint64
	wallOrigin  time.Time
	wallSeeded  bool
	tracks      [numTracks]trackState
	lastDiag    Diagnostic // observed outcome of the most recent Apply
	totalApply  uint64
	totalReanch uint64 // hard re-anchors
	totalDrops  uint64
}

// Diagnostic captures the outcome of the most recent Apply call. Used
// by the dual-path observer to compare against ptsrebaser's output
// for the same input without exposing internal state.
type Diagnostic struct {
	Track          trackKey
	Drift          int64 // expectedDts - effActualNow, ms
	HardReanchored bool
	Dropped        bool
	OutputDts      int64
	OutputPts      int64
	SessionID      uint64
}

// New returns a Normaliser configured with cfg. An Enabled=false
// Normaliser is valid; Apply will pass packets through untouched and
// OnSession is a no-op.
func New(cfg Config) *Normaliser {
	if cfg.CrossTrackSnapMs <= 0 {
		cfg.CrossTrackSnapMs = 1000
	}
	if cfg.JumpThresholdMs <= 0 {
		cfg.JumpThresholdMs = 2000
	}
	return &Normaliser{cfg: cfg}
}

// OnSession resets per-track anchor state (each track's first packet
// re-seeds on the next Apply) but PRESERVES the wallclock origin. This
// keeps output DTS strictly monotonic across session boundaries — a
// reconnect's next packet anchors at "elapsed since the worker started"
// rather than jumping back to zero. Downstream consumers see strict
// monotonicity AND the SessionStart=true marker on the boundary packet,
// which is everything they need to reset their own derived state.
//
// To wipe wallclock state too (e.g. shutdown / test teardown), call
// Reset instead.
//
// Calling OnSession with a nil session is a no-op. Calling it before
// any Apply is valid and seeds the session ID without touching tracks
// (which are already zero-valued).
func (n *Normaliser) OnSession(sess *domain.StreamSession) {
	if n == nil || !n.cfg.Enabled || sess == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sessionID = sess.ID
	n.tracks = [numTracks]trackState{}
}

// SeedWallclock installs an explicit wallclock origin. Calls before any
// Apply seed the Normaliser without waiting for the first packet, so
// multiple Normaliser instances can share a common timeline (used by
// the coordinator's abr_mixer where V taps + A fan-out write to the
// same downstream rendition buffer set and must produce output DTS on
// the same wallclock axis).
//
// Calling SeedWallclock AFTER Apply has lazy-seeded wallOrigin is
// allowed and overwrites; the per-track lastOutputDts is left alone so
// the monotonic floor still holds against the OLD output sequence.
// In practice the override should happen before the first Apply, so
// this corner case is a safety net rather than an intended workflow.
func (n *Normaliser) SeedWallclock(t time.Time) {
	if n == nil || !n.cfg.Enabled {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.wallOrigin = t
	n.wallSeeded = true
}

// Reset is the unconditional reset (no session tracking). Provided so
// callers without a session boundary still have a way to discard state
// — used by tests and during shutdown.
func (n *Normaliser) Reset() {
	if n == nil {
		return
	}
	n.mu.Lock()
	n.sessionID = 0
	n.wallSeeded = false
	n.wallOrigin = time.Time{}
	n.tracks = [numTracks]trackState{}
	n.mu.Unlock()
}

// Apply rewrites p.PTSms / p.DTSms in place and returns whether the
// caller should keep the packet. A false return means the Normaliser
// chose to drop this frame (sustained input ahead-of-wallclock past
// MaxAheadMs). The caller must NOT propagate a dropped packet.
//
// Pass-through (returns true without modification) when the Normaliser
// is disabled, the packet is nil, the codec is the raw-TS marker, or
// the codec doesn't classify as V or A.
//
// Hard re-anchor events (jump-threshold, regression, behind-cap) do NOT
// propagate to consumers via the packet — they surface only via Stats
// and LastDiagnostic for telemetry. Consumers learn about session
// boundaries from buffer.Packet.SessionStart instead.
func (n *Normaliser) Apply(p *domain.AVPacket, now time.Time) bool {
	if n == nil || !n.cfg.Enabled || p == nil {
		return true
	}
	if p.Codec == domain.AVCodecRawTSChunk {
		return true
	}
	tk, ok := trackKeyFor(p.Codec)
	if !ok {
		return true
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	n.totalApply++

	if !n.wallSeeded {
		n.wallOrigin = now
		n.wallSeeded = true
	}

	inDts := int64(p.DTSms) //nolint:gosec
	inPts := int64(p.PTSms) //nolint:gosec
	cto := inPts - inDts
	if cto < 0 {
		cto = 0
	}

	track := &n.tracks[tk]
	actualNowMs := now.Sub(n.wallOrigin).Milliseconds()

	if !track.seeded {
		n.seedTrackLocked(track, tk, inDts, actualNowMs, cto, p)
		n.lastDiag = Diagnostic{
			Track:     tk,
			OutputDts: track.outputAnchor,
			OutputPts: track.outputAnchor + cto,
			SessionID: n.sessionID,
		}
		return true
	}

	inputDelta := inDts - track.inputOrigin
	expectedDts := track.outputAnchor + inputDelta

	if n.cfg.MaxAheadMs > 0 && expectedDts-actualNowMs > n.cfg.MaxAheadMs {
		n.totalDrops++
		n.lastDiag = Diagnostic{
			Track:     tk,
			Drift:     expectedDts - actualNowMs,
			Dropped:   true,
			SessionID: n.sessionID,
		}
		return false
	}

	effActualNow := actualNowMs
	if effActualNow < track.lastOutputDts {
		effActualNow = track.lastOutputDts
	}
	drift := expectedDts - effActualNow

	tooFarAhead := absInt64(drift) > n.cfg.JumpThresholdMs
	regressed := expectedDts < track.lastOutputDts
	tooFarBehind := n.cfg.MaxBehindMs > 0 && drift < -n.cfg.MaxBehindMs

	if tooFarAhead || regressed || tooFarBehind {
		target := actualNowMs
		if target <= track.lastOutputDts {
			target = track.lastOutputDts + 1
		}
		track.inputOrigin = inDts
		track.outputAnchor = target
		track.lastOutputDts = target
		assignTimes(p, target, cto)
		n.totalReanch++
		n.lastDiag = Diagnostic{
			Track:          tk,
			Drift:          drift,
			HardReanchored: true,
			OutputDts:      target,
			OutputPts:      target + cto,
			SessionID:      n.sessionID,
		}
		return true
	}

	track.lastOutputDts = expectedDts
	assignTimes(p, expectedDts, cto)
	n.lastDiag = Diagnostic{
		Track:     tk,
		Drift:     drift,
		OutputDts: expectedDts,
		OutputPts: expectedDts + cto,
		SessionID: n.sessionID,
	}
	return true
}

// seedTrackLocked installs the per-track anchor on the first observed
// packet of a track within the current session. Mirrors ptsrebaser's
// progression-based cross-track snap: if the OTHER track has already
// progressed more than CrossTrackSnapMs since its own seed, the new
// track lands at the other track's lastOutputDts so V and A start in
// lockstep regardless of how the upstream chose to interleave them.
func (n *Normaliser) seedTrackLocked(
	track *trackState, tk trackKey, inDts, actualNowMs, cto int64, p *domain.AVPacket,
) {
	anchor := actualNowMs
	otherTrack := &n.tracks[numTracks-1-tk]
	if otherTrack.seeded {
		otherProgression := otherTrack.lastOutputDts - otherTrack.outputAnchor
		if otherProgression > n.cfg.CrossTrackSnapMs {
			anchor = otherTrack.lastOutputDts
		}
	}
	track.seeded = true
	track.inputOrigin = inDts
	track.outputAnchor = anchor
	track.lastOutputDts = anchor
	assignTimes(p, anchor, cto)
}

// LastDiagnostic returns a copy of the most recent Apply outcome. Used
// by the dual-path observer to compare Normaliser behaviour against
// ptsrebaser packet by packet.
func (n *Normaliser) LastDiagnostic() Diagnostic {
	if n == nil {
		return Diagnostic{}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.lastDiag
}

// Stats returns running counters for the lifetime of this Normaliser
// instance. Used by the dual-path observer for periodic divergence
// rate reporting (e.g. "Normaliser hard-re-anchored N times while
// ptsrebaser did M times — investigate the threshold delta").
type Stats struct {
	TotalApply       uint64
	TotalReanchored  uint64
	TotalDrops       uint64
	CurrentSessionID uint64
}

// Stats returns a snapshot of the running counters.
func (n *Normaliser) Stats() Stats {
	if n == nil {
		return Stats{}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return Stats{
		TotalApply:       n.totalApply,
		TotalReanchored:  n.totalReanch,
		TotalDrops:       n.totalDrops,
		CurrentSessionID: n.sessionID,
	}
}

// assignTimes writes outDts (clamped >= 0) and outDts + cto into the
// packet. Mirrors ptsrebaser.assignTimes so dual-path comparison sees
// identical clamping behaviour for well-behaved input.
func assignTimes(p *domain.AVPacket, outDts, cto int64) {
	if outDts < 0 {
		outDts = 0
	}
	outPts := outDts + cto
	if outPts < 0 {
		outPts = 0
	}
	p.DTSms = uint64(outDts) //nolint:gosec
	p.PTSms = uint64(outPts) //nolint:gosec
}

func absInt64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
