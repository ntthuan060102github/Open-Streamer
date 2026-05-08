package ptsrebaser

import (
	"testing"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// fakeClock returns a now() that advances by the supplied per-call deltas.
// Each call to next() returns the next absolute time in the script.
func fakeClock(start time.Time, deltas ...time.Duration) func() time.Time {
	t := start
	i := 0
	return func() time.Time {
		if i >= len(deltas) {
			return t
		}
		t = t.Add(deltas[i])
		i++
		return t
	}
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
	r.Apply(&domain.AVPacket{}, time.Now()) // nil receiver should not panic

	r = New(DefaultConfig())
	p := &domain.AVPacket{Codec: domain.AVCodecRawTSChunk, PTSms: 99, DTSms: 99}
	r.Apply(p, time.Now())
	if p.PTSms != 99 || p.DTSms != 99 {
		t.Fatalf("raw-TS chunk should pass through: got pts=%d dts=%d", p.PTSms, p.DTSms)
	}
}

func TestApply_FirstPacketAnchorsAtZero(t *testing.T) {
	r := New(DefaultConfig())
	p := &domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 12350, DTSms: 12345}
	r.Apply(p, time.Now())
	// CTO = 5ms (PTS - DTS) is preserved; absolute origin collapses to 0.
	if p.DTSms != 0 || p.PTSms != 5 {
		t.Fatalf("first packet should map to dts=0 pts=5; got dts=%d pts=%d", p.DTSms, p.PTSms)
	}
	if p.Discontinuity {
		t.Fatal("first packet must not flag Discontinuity")
	}
}

func TestApply_SteadyStatePreservesInputDeltas(t *testing.T) {
	r := New(DefaultConfig())
	start := time.Unix(1_700_000_000, 0)
	clock := fakeClock(start, 0, 40*time.Millisecond, 40*time.Millisecond, 40*time.Millisecond)

	// Three frames at 25fps, input PTS spaced 40ms apart, no drift.
	for i, want := range []uint64{0, 40, 80} {
		p := &domain.AVPacket{
			Codec: domain.AVCodecH264,
			PTSms: 12340 + uint64(i)*40, //nolint:gosec
			DTSms: 12340 + uint64(i)*40, //nolint:gosec
		}
		r.Apply(p, clock())
		if p.DTSms != want {
			t.Fatalf("frame %d: want dts=%d got dts=%d", i, want, p.DTSms)
		}
		if p.Discontinuity {
			t.Fatalf("frame %d: unexpected Discontinuity", i)
		}
	}
}

func TestApply_PreservesPTSDTSOffset(t *testing.T) {
	// Simulates B-frame ordering: PTS leads DTS by 80ms.
	r := New(DefaultConfig())
	start := time.Unix(1_700_000_000, 0)
	clock := fakeClock(start, 0, 40*time.Millisecond)

	p1 := &domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 1080, DTSms: 1000}
	r.Apply(p1, clock())
	if p1.DTSms != 0 || p1.PTSms != 80 {
		t.Fatalf("first: want dts=0 pts=80, got dts=%d pts=%d", p1.DTSms, p1.PTSms)
	}

	p2 := &domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 1120, DTSms: 1040}
	r.Apply(p2, clock())
	if p2.DTSms != 40 || p2.PTSms != 120 {
		t.Fatalf("second: want dts=40 pts=120, got dts=%d pts=%d", p2.DTSms, p2.PTSms)
	}
}

func TestApply_HardResetWhenDriftExceedsJumpThreshold(t *testing.T) {
	r := New(Config{Enabled: true, JumpThresholdMs: 500}) // tight threshold for test
	start := time.Unix(1_700_000_000, 0)
	// First call seeds; clock returns start exactly.
	// Second call: wallclock advances 100ms but input PTS jumps 5000ms
	// → drift = +4900ms, well above 500ms threshold.
	clock := fakeClock(start, 0, 100*time.Millisecond)

	p1 := &domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 1000, DTSms: 1000}
	r.Apply(p1, clock())
	if p1.DTSms != 0 {
		t.Fatalf("first: want dts=0, got %d", p1.DTSms)
	}

	p2 := &domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 6000, DTSms: 6000}
	r.Apply(p2, clock())
	if !p2.Discontinuity {
		t.Fatal("hard reset should flag Discontinuity")
	}
	// Re-anchor pins THIS packet at zero (relative to the new wallOrigin).
	if p2.DTSms != 0 || p2.PTSms != 0 {
		t.Fatalf("after reset: want dts=0 pts=0, got dts=%d pts=%d", p2.DTSms, p2.PTSms)
	}

	// Subsequent steady-state frame: 40ms wallclock + 40ms input → no reset,
	// output dts grows to 40 from the new anchor (p2 was at start+100ms).
	p3 := &domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 6040, DTSms: 6040}
	r.Apply(p3, start.Add(140*time.Millisecond))
	if p3.Discontinuity {
		t.Fatal("steady-state frame after reset should not flag Discontinuity")
	}
	if p3.DTSms != 40 {
		t.Fatalf("post-reset frame: want dts=40, got %d", p3.DTSms)
	}
}

func TestApply_NegativeDriftAlsoResets(t *testing.T) {
	// Wallclock moves forward but input PTS stalls (paused source).
	// drift = expected - actualNow goes very negative → hard reset.
	r := New(Config{Enabled: true, JumpThresholdMs: 500})
	start := time.Unix(1_700_000_000, 0)

	p1 := &domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 5000, DTSms: 5000}
	r.Apply(p1, start)

	// 10s wallclock pass, only 100ms input progress → drift = 100 - 10000 = -9900
	p2 := &domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 5100, DTSms: 5100}
	r.Apply(p2, start.Add(10*time.Second))
	if !p2.Discontinuity {
		t.Fatalf("negative drift past threshold should reset; got Discontinuity=%v dts=%d", p2.Discontinuity, p2.DTSms)
	}
}

func TestApply_DriftBelowThresholdNoReset(t *testing.T) {
	r := New(Config{Enabled: true, JumpThresholdMs: 2000})
	start := time.Unix(1_700_000_000, 0)
	r.Apply(&domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 1000, DTSms: 1000}, start)

	// Walk 30 frames at 40ms input cadence and 40.05ms wallclock cadence
	// (input ~0.12% slower). Total drift = 30 * 0.05 = 1.5ms, far under 2000.
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
		t.Fatal("existing Discontinuity must be preserved (caller-set boundary)")
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

	// After Reset, next packet anchors fresh (DTS again = 0).
	p2 := &domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 9999, DTSms: 9999}
	r.Apply(p2, start.Add(5*time.Second))
	if p2.DTSms != 0 {
		t.Fatalf("after Reset, first packet should be dts=0; got %d", p2.DTSms)
	}
	if p2.Discontinuity {
		t.Fatal("re-seed after Reset should not flag Discontinuity (caller's responsibility)")
	}
}

func TestApply_NilPacketIsNoOp(t *testing.T) {
	r := New(DefaultConfig())
	r.Apply(nil, time.Now()) // must not panic
}

func TestApply_ClampsNegativeOutput(t *testing.T) {
	// Exercise the clamp-at-zero branch by forcing a hard reset (which
	// already pins to zero) and verifying no underflow when CTO is 0.
	r := New(Config{Enabled: true, JumpThresholdMs: 100})
	start := time.Unix(1_700_000_000, 0)
	r.Apply(&domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 5000, DTSms: 5000}, start)

	p := &domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 100, DTSms: 100}
	// 1ms wallclock vs −4900ms input → huge negative drift → reset.
	r.Apply(p, start.Add(1*time.Millisecond))
	if p.PTSms != 0 || p.DTSms != 0 {
		t.Fatalf("clamp: want pts=0 dts=0, got pts=%d dts=%d", p.PTSms, p.DTSms)
	}
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
