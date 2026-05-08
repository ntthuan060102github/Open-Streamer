package ptsrebaser

import (
	"testing"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

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

func TestApply_FirstPacketAnchorsAtZero(t *testing.T) {
	r := New(DefaultConfig())
	p := &domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 12350, DTSms: 12345}
	r.Apply(p, time.Unix(1_700_000_000, 0))
	// CTO = 5ms (PTS - DTS) preserved; first-of-track output anchored at
	// elapsed-since-wallOrigin = 0 (this packet IS the wallclock seed).
	if p.DTSms != 0 || p.PTSms != 5 {
		t.Fatalf("first packet should map to dts=0 pts=5; got dts=%d pts=%d", p.DTSms, p.PTSms)
	}
	if p.Discontinuity {
		t.Fatal("first packet must not flag Discontinuity")
	}
}

func TestApply_VideoAndAudioTracksAreIndependent(t *testing.T) {
	// RTSP/RTMP-style scenario: V and A on entirely different timebases
	// (e.g. RTP per-track timestamps). Pre-fix this caused every packet
	// to ping-pong the shared origin and emit non-monotonic outputs.
	r := New(DefaultConfig())
	start := time.Unix(1_700_000_000, 0)

	// First video packet arrives at t=0.
	v1 := &domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 1_000_000, DTSms: 1_000_000}
	r.Apply(v1, start)

	// First audio packet arrives 5ms later, on a totally different
	// timebase. Pre-fix: huge delta vs the (video) origin → reset.
	// Post-fix: audio gets its own anchor — no Discontinuity, no jitter.
	a1 := &domain.AVPacket{Codec: domain.AVCodecAAC, PTSms: 50_000_000, DTSms: 50_000_000}
	r.Apply(a1, start.Add(5*time.Millisecond))

	if v1.DTSms != 0 {
		t.Fatalf("video first: want dts=0, got %d", v1.DTSms)
	}
	if a1.Discontinuity {
		t.Fatal("audio first packet must not flag Discontinuity even with wildly different timebase")
	}
	if a1.DTSms != 5 {
		t.Fatalf("audio first packet: want dts=5 (=5ms after wallOrigin), got %d", a1.DTSms)
	}

	// Subsequent video and audio packets advance independently.
	v2 := &domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 1_000_040, DTSms: 1_000_040}
	r.Apply(v2, start.Add(40*time.Millisecond))
	a2 := &domain.AVPacket{Codec: domain.AVCodecAAC, PTSms: 50_000_021, DTSms: 50_000_021}
	r.Apply(a2, start.Add(45*time.Millisecond))

	if v2.DTSms != 40 {
		t.Fatalf("video second: want dts=40, got %d", v2.DTSms)
	}
	if a2.DTSms != 26 {
		t.Fatalf("audio second: want dts=26 (5+21), got %d", a2.DTSms)
	}
	if v2.Discontinuity || a2.Discontinuity {
		t.Fatal("steady-state packets must not flag Discontinuity")
	}
}

func TestApply_OutputIsMonotonicPerTrackAcrossResets(t *testing.T) {
	// Force an upstream backward jump WITHIN a single track. Pre-fix
	// the rebaser emitted output=0 again, which produced backward dts
	// in the buffer hub and underflowed downstream uint64 dur math.
	// Post-fix: monotonic floor pushes each emit ≥ lastOutputDts+1.
	r := New(Config{Enabled: true, JumpThresholdMs: 500})
	start := time.Unix(1_700_000_000, 0)

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

	p1 := &domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 1000, DTSms: 1000}
	r.Apply(p1, start)

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

	p1 := &domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 1000, DTSms: 1000}
	r.Apply(p1, start)
	if p1.DTSms != 0 {
		t.Fatalf("first: want dts=0, got %d", p1.DTSms)
	}

	r.Reset()

	// After Reset, next packet anchors fresh (DTS = 0 again).
	p2 := &domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 9999, DTSms: 9999}
	r.Apply(p2, start.Add(5*time.Second))
	if p2.DTSms != 0 {
		t.Fatalf("post-Reset first packet should be dts=0; got %d", p2.DTSms)
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
}
