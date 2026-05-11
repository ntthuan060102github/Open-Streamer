package ptsrebaser

import (
	"testing"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// seedRebaserPastPairing primes both tracks past the V/A pairing gate
// using three matched-DTS Apply calls (V → A → V): the first V records
// firstSeen and drops; the A call detects the pair, picks origin =
// max(dtsV, dtsA) = dts, and seeds A (its DTS == origin); the second
// V call sees both firstSeens, its DTS == origin, and seeds V. After
// this returns both trackVideo and trackAudio are seeded with
// inputOrigin = dts and outputAnchor = (now − wallOrigin). Use this
// when a test wants to drive post-seed behaviour (drift caps,
// monotonicity, re-anchor) without the pairing gate adding noise.
//
// Tests of V/A pairing semantics MUST drive Apply directly — they
// can't use this helper because it bakes in the very seeding decision
// they want to observe.
func seedRebaserPastPairing(t *testing.T, r *Rebaser, dts uint64, now time.Time) {
	t.Helper()
	r.Apply(&domain.AVPacket{Codec: domain.AVCodecH264, DTSms: dts, PTSms: dts}, now)
	r.Apply(&domain.AVPacket{Codec: domain.AVCodecAAC, DTSms: dts, PTSms: dts}, now)
	r.Apply(&domain.AVPacket{Codec: domain.AVCodecH264, DTSms: dts, PTSms: dts}, now)
}

func TestApply_DisabledIsPassThrough(t *testing.T) {
	r := New(Config{Enabled: false, JumpThresholdMs: 2000})
	p := &domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 12345, DTSms: 12340}
	r.Apply(p, time.Now())
	if p.PTSms != 12345 || p.DTSms != 12340 {
		t.Fatalf("disabled rebaser modified packet: got pts=%d dts=%d", p.PTSms, p.DTSms)
	}
	if p.Discontinuity {
		t.Fatal("disabled rebaser must not set Discontinuity")
	}
}

func TestApply_NilSafeAndRawTSSkipped(t *testing.T) {
	var r *Rebaser
	r.Apply(&domain.AVPacket{}, time.Now()) // nil receiver — no panic

	r = New(DefaultConfig())
	p := &domain.AVPacket{Codec: domain.AVCodecRawTSChunk, PTSms: 99, DTSms: 99}
	r.Apply(p, time.Now())
	if p.PTSms != 99 || p.DTSms != 99 {
		t.Fatalf("raw-TS chunk should pass through: got pts=%d dts=%d", p.PTSms, p.DTSms)
	}
}

func TestApply_UnknownCodecIsPassThrough(t *testing.T) {
	r := New(DefaultConfig())
	p := &domain.AVPacket{Codec: domain.AVCodecUnknown, PTSms: 555, DTSms: 555}
	r.Apply(p, time.Now())
	if p.PTSms != 555 || p.DTSms != 555 {
		t.Fatalf("unclassified codec should pass through: got pts=%d dts=%d", p.PTSms, p.DTSms)
	}
}

func TestApply_FirstPostPairingPacketAnchorsAtZero(t *testing.T) {
	r := New(DefaultConfig())
	start := time.Unix(1_700_000_000, 0)
	// V/A arrive simultaneously at the same DTS — the pairing gate
	// resolves immediately, both tracks seed with origin = that DTS,
	// outputAnchor = (now − wallOrigin) = 0.
	r.Apply(&domain.AVPacket{Codec: domain.AVCodecH264, DTSms: 12345, PTSms: 12345}, start)
	r.Apply(&domain.AVPacket{Codec: domain.AVCodecAAC, DTSms: 12345, PTSms: 12345}, start)

	// First post-pairing V packet with a 5 ms CTO (PTS > DTS). Output
	// must land at dts=0, pts=5 (CTO preserved).
	p := &domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 12350, DTSms: 12345}
	r.Apply(p, start)
	if p.DTSms != 0 || p.PTSms != 5 {
		t.Fatalf("first post-pairing packet should map to dts=0 pts=5; got dts=%d pts=%d", p.DTSms, p.PTSms)
	}
	if p.Discontinuity {
		t.Fatal("first post-pairing packet must not flag Discontinuity")
	}
}

// V/A pairing: video and audio arrive at the same wallclock with
// matching first DTS. The pairing gate resolves on A's call (V's
// first Apply records firstSeen and drops); A seeds, V seeds on its
// next call. Both share origin = the matching DTS.
func TestApply_VAPairing_SimultaneousArrivalSeedsBothAtZero(t *testing.T) {
	r := New(DefaultConfig())
	start := time.Unix(1_700_000_000, 0)

	v1 := &domain.AVPacket{Codec: domain.AVCodecH264, DTSms: 1000, PTSms: 1000}
	if r.Apply(v1, start) {
		t.Fatal("first V packet must drop while waiting for A's firstSeen")
	}

	a1 := &domain.AVPacket{Codec: domain.AVCodecAAC, DTSms: 1000, PTSms: 1000}
	if !r.Apply(a1, start) {
		t.Fatal("A packet at the agreed origin must seed audio and emit")
	}
	if a1.DTSms != 0 {
		t.Fatalf("audio seed: want dts=0, got %d", a1.DTSms)
	}

	v2 := &domain.AVPacket{Codec: domain.AVCodecH264, DTSms: 1000, PTSms: 1000}
	if !r.Apply(v2, start) {
		t.Fatal("V packet at origin (post-pairing) must seed video and emit")
	}
	if v2.DTSms != 0 {
		t.Fatalf("video seed: want dts=0, got %d", v2.DTSms)
	}
}

// V/A pairing — pathological upstream skew (live.mediatech.vn pattern
// that broke bac_ninh and every republisher of it): audio's first PES
// PTS sits 30 s ahead of video's first PES PTS in source-time terms.
// The rebaser must promote the LATER source moment (max-DTS) to origin,
// drop the early track's catch-up packets, and seed both tracks once
// each crosses the origin — V immediately (its DTS == origin), A once
// upstream catches up.
func TestApply_VAPairing_BigSkewDropsEarlyTrackUntilCatchUp(t *testing.T) {
	r := New(DefaultConfig())
	start := time.Unix(1_700_000_000, 0)

	// Audio arrives first with PES DTS=1000. firstSeen recorded; drop.
	a0 := &domain.AVPacket{Codec: domain.AVCodecAAC, DTSms: 1000, PTSms: 1000}
	if r.Apply(a0, start) {
		t.Fatal("first A packet must drop while V's firstSeen not yet recorded")
	}

	// More audio frames arrive — still no V firstSeen. All drop.
	for i := 1; i < 5; i++ {
		a := &domain.AVPacket{Codec: domain.AVCodecAAC, DTSms: 1000 + uint64(i)*20, PTSms: 1000 + uint64(i)*20} //nolint:gosec
		if r.Apply(a, start.Add(time.Duration(i)*time.Millisecond)) {
			t.Fatalf("A packet %d should still drop (V not yet seen)", i)
		}
	}

	// Video first packet arrives with PES DTS=31000 — 30 s ahead of audio.
	// Pair detected, origin = max(1000, 31000) = 31000. V's DTS == origin
	// → seed and emit.
	v1 := &domain.AVPacket{Codec: domain.AVCodecH264, DTSms: 31000, PTSms: 31000}
	if !r.Apply(v1, start.Add(20*time.Millisecond)) {
		t.Fatal("V packet at origin must seed and emit")
	}

	// Next audio packet is still below origin → drops.
	a6 := &domain.AVPacket{Codec: domain.AVCodecAAC, DTSms: 1200, PTSms: 1200}
	if r.Apply(a6, start.Add(25*time.Millisecond)) {
		t.Fatal("A packet still below origin must drop after V seed")
	}

	// Audio upstream eventually catches up to origin.
	a7 := &domain.AVPacket{Codec: domain.AVCodecAAC, DTSms: 31000, PTSms: 31000}
	if !r.Apply(a7, start.Add(30*time.Millisecond)) {
		t.Fatal("A packet at origin must seed audio and emit")
	}
}

// V/A pairing — single-track source (audio-only or video-only): the
// partner never arrives, so the pairing gate must time out and let
// the present track seed solo. Origin uses the timed-out packet's DTS
// (not its firstSeenDts) so the wait period doesn't bake into output.
// V/A pairing — buffer-and-shift anchor: when the late-to-catch-up
// track finally seeds, its outputAnchor must be the OTHER track's
// outputAnchor (not the current wallclock) so V at source PES X and
// A at source PES X emit at the same output PTS. Without this, the
// audio-leads-video offset observed on test_puhser (a-v = -0.72 s)
// re-appears as a permanent timeline offset equal to the wallclock
// distance between V's and A's first-emit moments.
func TestApply_VAPairing_LateTrackInheritsOtherAnchor(t *testing.T) {
	r := New(DefaultConfig())
	start := time.Unix(1_700_000_000, 0)

	// A arrives first with PES=1000 — recorded, dropped (waiting for V).
	a0 := &domain.AVPacket{Codec: domain.AVCodecAAC, DTSms: 1000, PTSms: 1000}
	r.Apply(a0, start)

	// V arrives 5 ms wallclock later with PES=2000 — pair detected,
	// origin = max(1000, 2000) = 2000, V's DTS == origin → V seeds.
	// V.outputAnchor at this moment = (start+5ms) − wallOrigin = 5.
	v1 := &domain.AVPacket{Codec: domain.AVCodecH264, DTSms: 2000, PTSms: 2000}
	r.Apply(v1, start.Add(5*time.Millisecond))
	if v1.DTSms != 5 {
		t.Fatalf("V at origin seeds at outputAnchor=5; got DTSms=%d", v1.DTSms)
	}

	// 1 s wallclock later, A's upstream catches up to PES=2000. With
	// the OLD per-seed-anchors behaviour A.outputAnchor would land at
	// 1005 ms — A's output_PTS would then sit 1000 ms ahead of where
	// V was at the same source PES, producing the audio-leads-video
	// offset. With the buffer-and-shift fix, A inherits V's anchor (5).
	a1 := &domain.AVPacket{Codec: domain.AVCodecAAC, DTSms: 2000, PTSms: 2000}
	r.Apply(a1, start.Add(1005*time.Millisecond))
	if a1.DTSms != 5 {
		t.Fatalf("A at origin should inherit V's outputAnchor (5); got DTSms=%d", a1.DTSms)
	}

	// Sanity: subsequent V and A at SAME source PES must emit at SAME
	// output PTS. This is the property the fix guarantees.
	v2 := &domain.AVPacket{Codec: domain.AVCodecH264, DTSms: 3000, PTSms: 3000}
	r.Apply(v2, start.Add(2005*time.Millisecond))
	a2 := &domain.AVPacket{Codec: domain.AVCodecAAC, DTSms: 3000, PTSms: 3000}
	r.Apply(a2, start.Add(2010*time.Millisecond))
	if v2.DTSms != a2.DTSms {
		t.Fatalf("V and A at same source PES must emit at same output PTS; v=%d a=%d", v2.DTSms, a2.DTSms)
	}
}

func TestApply_VAPairing_TimesOutForSingleTrack(t *testing.T) {
	r := New(DefaultConfig())
	start := time.Unix(1_700_000_000, 0)

	p1 := &domain.AVPacket{Codec: domain.AVCodecH264, DTSms: 1000, PTSms: 1000}
	if r.Apply(p1, start) {
		t.Fatal("first packet must drop while waiting for partner")
	}

	// Inside the wait window — still drops.
	p2 := &domain.AVPacket{Codec: domain.AVCodecH264, DTSms: 1080, PTSms: 1080}
	if r.Apply(p2, start.Add(2*time.Second)) {
		t.Fatal("packet inside wait window must still drop")
	}

	// Past the timeout — seeds solo using this packet's DTS as origin.
	p3 := &domain.AVPacket{Codec: domain.AVCodecH264, DTSms: 6000, PTSms: 6000}
	if !r.Apply(p3, start.Add(6*time.Second)) {
		t.Fatal("packet past timeout must seed solo and emit")
	}
}

func TestApply_OutputIsMonotonicPerTrackAcrossResets(t *testing.T) {
	// Force an upstream backward jump WITHIN a single track. Pre-fix
	// the rebaser emitted output=0 again, which produced backward dts
	// in the buffer hub and underflowed downstream uint64 dur math.
	// Post-fix: monotonic floor pushes each emit ≥ lastOutputDts+1.
	r := New(Config{Enabled: true, JumpThresholdMs: 500})
	start := time.Unix(1_700_000_000, 0)
	seedRebaserPastPairing(t, r, 1000, start)

	// Steady run of video at 25fps for 1 s (25 packets).
	for i := 0; i < 25; i++ {
		p := &domain.AVPacket{
			Codec: domain.AVCodecH264,
			PTSms: 1000 + uint64(i)*40, //nolint:gosec
			DTSms: 1000 + uint64(i)*40, //nolint:gosec
		}
		r.Apply(p, start.Add(time.Duration(i)*40*time.Millisecond))
		if i > 0 && p.Discontinuity {
			t.Fatalf("frame %d: unexpected Discontinuity in steady state", i)
		}
	}
	// Last emitted dts ≈ 24*40 = 960 ms.

	// Source resets PTS down — backward jump > threshold.
	p := &domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 100, DTSms: 100}
	r.Apply(p, start.Add(1000*time.Millisecond))
	if !p.Discontinuity {
		t.Fatal("backward jump past threshold should flag Discontinuity")
	}
	if p.DTSms <= 960 {
		t.Fatalf("post-reset dts must be strictly greater than the last emitted (960); got %d", p.DTSms)
	}
}

func TestApply_HardResetWhenForwardJumpExceedsThreshold(t *testing.T) {
	r := New(Config{Enabled: true, JumpThresholdMs: 500})
	start := time.Unix(1_700_000_000, 0)
	seedRebaserPastPairing(t, r, 1000, start)

	// 100 ms wallclock, 5 s input PTS jump → drift +4900 ms, way over 500.
	p2 := &domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 6000, DTSms: 6000}
	r.Apply(p2, start.Add(100*time.Millisecond))
	if !p2.Discontinuity {
		t.Fatal("forward-jump reset should flag Discontinuity")
	}
	// Re-anchor pinned to wallclock (~100 ms), not back to 0 — preserves
	// monotonicity vs the previously emitted dts (=0).
	if p2.DTSms < 100 {
		t.Fatalf("post-reset should be >= wallclock-elapsed (100); got %d", p2.DTSms)
	}
}

// Verifies that re-anchors classify into "drift" vs "regression" counts
// on the trackState — the per-reason counters are the throttle key for
// the slog.Warn that drives test3-style live diagnosis.
func TestApply_ReanchorCountersClassifyDriftVsRegression(t *testing.T) {
	r := New(Config{Enabled: true, JumpThresholdMs: 500, StreamCode: "diag_stream"})
	start := time.Unix(1_700_000_000, 0)
	seedRebaserPastPairing(t, r, 1000, start)

	// Forward jump > threshold → "drift" reason.
	p2 := &domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 10000, DTSms: 10000}
	r.Apply(p2, start.Add(50*time.Millisecond))
	if got := r.tracks[trackVideo].reanchorsDrift; got != 1 {
		t.Fatalf("forward jump should bump reanchorsDrift to 1; got %d", got)
	}
	if got := r.tracks[trackVideo].reanchorsRegression; got != 0 {
		t.Fatalf("forward jump must not bump regression counter; got %d", got)
	}

	// Inject a regression — input DTS goes backward past lastOutputDts.
	// p2 just re-anchored; lastOutputDts ≈ 50 ms (wallclock floor). Send a
	// packet whose expectedDts would fall below that.
	p3 := &domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 0, DTSms: 0}
	r.Apply(p3, start.Add(60*time.Millisecond))
	if got := r.tracks[trackVideo].reanchorsRegression; got != 1 {
		t.Fatalf("backward jump should bump reanchorsRegression to 1; got %d", got)
	}
	if got := r.tracks[trackVideo].reanchorsDrift; got != 1 {
		t.Fatalf("regression must not double-count as drift; got %d", got)
	}
}

func TestApply_BurstyDeliveryDoesNotPingPongReset(t *testing.T) {
	// RTMP / SRT pulls often deliver a batch of frames within a few
	// wallclock ms, each spaced ~frame-cadence in DTS. A drift check
	// against instantaneous wallclock would manufacture a "drift"
	// against actualNow inside the burst, trip the threshold, push
	// output past wallclock via the monotonicity floor, then trip the
	// threshold again on every subsequent packet — emitting a stream
	// of Discontinuity-flagged packets and microsecond-sized HLS
	// segments. Confirmed empirically against the staging test5 RTMP
	// pull: every video packet was flagged as a discontinuity boundary
	// pre-fix.
	r := New(Config{Enabled: true, JumpThresholdMs: 2000})
	start := time.Unix(1_700_000_000, 0)

	discontinuities := 0
	for i := 0; i < 60; i++ {
		p := &domain.AVPacket{
			Codec: domain.AVCodecH264,
			PTSms: 1000 + uint64(i)*40, //nolint:gosec
			DTSms: 1000 + uint64(i)*40, //nolint:gosec
		}
		// Pack 60 frames into ~5 ms of wallclock — the worst-case
		// burst the rebaser must absorb without flagging every packet.
		r.Apply(p, start.Add(time.Duration(i)*83*time.Microsecond))
		if p.Discontinuity {
			discontinuities++
		}
	}
	if discontinuities != 0 {
		t.Fatalf("bursty delivery flagged %d packets as Discontinuity (want 0)", discontinuities)
	}
}

func TestApply_DriftBelowThresholdPassesThrough(t *testing.T) {
	r := New(Config{Enabled: true, JumpThresholdMs: 2000})
	start := time.Unix(1_700_000_000, 0)
	r.Apply(&domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 1000, DTSms: 1000}, start)

	// 30 frames at 40 ms input cadence and 40.05 ms wallclock cadence
	// (input slightly slower). Total drift = 30 × 0.05 = 1.5 ms, far
	// below the 2000-ms threshold.
	for i := 1; i <= 30; i++ {
		p := &domain.AVPacket{
			Codec: domain.AVCodecH264,
			PTSms: 1000 + uint64(i)*40, //nolint:gosec
			DTSms: 1000 + uint64(i)*40, //nolint:gosec
		}
		r.Apply(p, start.Add(time.Duration(i)*40050*time.Microsecond))
		if p.Discontinuity {
			t.Fatalf("frame %d: unexpected Discontinuity within drift band", i)
		}
	}
}

func TestApply_PreservesPTSDTSOffset(t *testing.T) {
	// B-frame ordering: PTS leads DTS by 80 ms. Offset must survive.
	r := New(DefaultConfig())
	start := time.Unix(1_700_000_000, 0)
	seedRebaserPastPairing(t, r, 1000, start)

	p1 := &domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 1080, DTSms: 1000}
	r.Apply(p1, start)
	if p1.DTSms != 0 || p1.PTSms != 80 {
		t.Fatalf("first: want dts=0 pts=80, got dts=%d pts=%d", p1.DTSms, p1.PTSms)
	}

	p2 := &domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 1120, DTSms: 1040}
	r.Apply(p2, start.Add(40*time.Millisecond))
	if p2.DTSms != 40 || p2.PTSms != 120 {
		t.Fatalf("second: want dts=40 pts=120, got dts=%d pts=%d", p2.DTSms, p2.PTSms)
	}
}

func TestApply_PreservesExistingDiscontinuity(t *testing.T) {
	r := New(DefaultConfig())
	p := &domain.AVPacket{
		Codec:         domain.AVCodecH264,
		PTSms:         100,
		DTSms:         100,
		Discontinuity: true,
	}
	r.Apply(p, time.Now())
	if !p.Discontinuity {
		t.Fatal("caller-set Discontinuity must be preserved")
	}
}

func TestReset_ReseedsFromNextPacket(t *testing.T) {
	r := New(DefaultConfig())
	start := time.Unix(1_700_000_000, 0)
	seedRebaserPastPairing(t, r, 1000, start)

	r.Reset()

	// After Reset, the next packet must re-enter pairing — single
	// V packet drops, then A pairs and seeds, then V seeds. Output
	// dts lands at 0 (fresh wallOrigin set on the first post-Reset
	// Apply call).
	resetWall := start.Add(5 * time.Second)
	r.Apply(&domain.AVPacket{Codec: domain.AVCodecH264, DTSms: 9999, PTSms: 9999}, resetWall)
	r.Apply(&domain.AVPacket{Codec: domain.AVCodecAAC, DTSms: 9999, PTSms: 9999}, resetWall)
	p2 := &domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 9999, DTSms: 9999}
	r.Apply(p2, resetWall)
	if p2.DTSms != 0 {
		t.Fatalf("post-Reset first emitted packet should be dts=0; got %d", p2.DTSms)
	}
	if p2.Discontinuity {
		t.Fatal("post-Reset re-seed must not auto-flag Discontinuity")
	}
}

func TestApply_NilPacketIsNoOp(t *testing.T) {
	r := New(DefaultConfig())
	r.Apply(nil, time.Now()) // must not panic
}

func TestDefaultConfig(t *testing.T) {
	c := DefaultConfig()
	if !c.Enabled {
		t.Fatal("default should be enabled")
	}
	if c.JumpThresholdMs <= 0 {
		t.Fatalf("default JumpThresholdMs must be > 0, got %d", c.JumpThresholdMs)
	}
	// MaxAheadMs is intentionally 0 by default — see rebaser.go DefaultConfig
	// rationale (drop semantics create a stuck state on real-time input).
	if c.MaxAheadMs != 0 {
		t.Fatalf("default MaxAheadMs must be 0 (disabled), got %d", c.MaxAheadMs)
	}
}

func TestApply_DropsFrameWhenSustainedAhead(t *testing.T) {
	// MaxAheadMs cap fires when output PTS would land >cap ahead of
	// wallclock-since-anchor. Distinguishes a brief burst (ok, passes)
	// from a sustained "input runs faster than realtime" producer
	// (mixer drain, transcoder catch-up). Caller must drop on false.
	r := New(Config{Enabled: true, JumpThresholdMs: 10_000, MaxAheadMs: 2000})
	start := time.Unix(1_700_000_000, 0)
	r.Apply(&domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 0, DTSms: 0}, start)

	// 100 frames spaced 40 ms in PTS but only 1 ms wallclock each →
	// expected drift after frame 100 ≈ 4000 − 100 = 3900 ms ⇒ above cap.
	dropped := 0
	for i := 1; i <= 100; i++ {
		p := &domain.AVPacket{
			Codec: domain.AVCodecH264,
			PTSms: uint64(i) * 40, //nolint:gosec
			DTSms: uint64(i) * 40, //nolint:gosec
		}
		keep := r.Apply(p, start.Add(time.Duration(i)*time.Millisecond))
		if !keep {
			dropped++
		}
	}
	if dropped == 0 {
		t.Fatal("expected drop signals once sustained drift exceeds MaxAheadMs")
	}
}

func TestApply_DropDoesNotMutateOutput(t *testing.T) {
	// When a frame is dropped, the caller skips the buffer write — but
	// the packet's fields shouldn't be silently rewritten in a way
	// that hides the drop, because the original PTS is still useful for
	// metrics / debug. (We don't strictly require pristine fields, but
	// guarantee no Discontinuity is set on a drop.)
	r := New(Config{Enabled: true, JumpThresholdMs: 10_000, MaxAheadMs: 100})
	start := time.Unix(1_700_000_000, 0)
	r.Apply(&domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 0, DTSms: 0}, start)

	p := &domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 5000, DTSms: 5000}
	if keep := r.Apply(p, start.Add(10*time.Millisecond)); keep {
		t.Fatal("expected drop on huge sustained drift")
	}
	if p.Discontinuity {
		t.Fatal("dropped packet must not be flagged as Discontinuity")
	}
}
