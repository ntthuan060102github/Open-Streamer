package timeline

import (
	"testing"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// startWall is the synthetic wallclock origin used across tests. Any
// fixed value works; we keep one constant so test traces are stable.
var startWall = time.Unix(1_700_000_000, 0)

// h264 / aac shorthand to keep table cases short.
func vPkt(pts, dts uint64) *domain.AVPacket {
	return &domain.AVPacket{Codec: domain.AVCodecH264, PTSms: pts, DTSms: dts}
}

func aPkt(pts, dts uint64) *domain.AVPacket {
	return &domain.AVPacket{Codec: domain.AVCodecAAC, PTSms: pts, DTSms: dts}
}

// TestPassthroughCases groups the no-op shapes: disabled, nil, raw-TS,
// unknown codec. Each must leave PTS/DTS untouched and never record a
// HardReanchored diagnostic.
func TestPassthroughCases(t *testing.T) {
	cases := []struct {
		name string
		n    *Normaliser
		p    *domain.AVPacket
	}{
		{
			name: "disabled config",
			n:    New(Config{Enabled: false, JumpThresholdMs: 2000}),
			p:    &domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 12345, DTSms: 12340},
		},
		{
			name: "raw-TS marker codec",
			n:    New(DefaultConfig()),
			p:    &domain.AVPacket{Codec: domain.AVCodecRawTSChunk, PTSms: 99, DTSms: 99},
		},
		{
			name: "unknown codec",
			n:    New(DefaultConfig()),
			p:    &domain.AVPacket{Codec: domain.AVCodecUnknown, PTSms: 555, DTSms: 555},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			origPTS, origDTS := tc.p.PTSms, tc.p.DTSms
			keep := tc.n.Apply(tc.p, startWall)
			if !keep {
				t.Fatalf("pass-through case dropped: %+v", tc.p)
			}
			if tc.p.PTSms != origPTS || tc.p.DTSms != origDTS {
				t.Fatalf("pass-through mutated packet: pts %d→%d dts %d→%d",
					origPTS, tc.p.PTSms, origDTS, tc.p.DTSms)
			}
			if tc.n.LastDiagnostic().HardReanchored {
				t.Fatalf("pass-through must not record HardReanchored")
			}
		})
	}

	t.Run("nil receiver", func(t *testing.T) {
		var n *Normaliser
		if !n.Apply(&domain.AVPacket{}, startWall) {
			t.Fatal("nil receiver must pass-through, not drop")
		}
		n.OnSession(&domain.StreamSession{ID: 1})
		n.Reset()
	})

	t.Run("nil packet", func(t *testing.T) {
		if !New(DefaultConfig()).Apply(nil, startWall) {
			t.Fatal("nil packet must return keep=true")
		}
	})
}

// TestSeed verifies the seed semantics: the very first packet of each
// track anchors at elapsed-wallclock (0 ms for the wallclock-seed track,
// >0 for tracks arriving later), CTO is preserved, and the seed never
// records a HardReanchored diagnostic.
func TestSeed(t *testing.T) {
	n := New(DefaultConfig())
	p := vPkt(12350, 12345) // CTO = 5
	n.Apply(p, startWall)

	if p.DTSms != 0 || p.PTSms != 5 {
		t.Fatalf("seed: want dts=0 pts=5, got dts=%d pts=%d", p.DTSms, p.PTSms)
	}
	if n.LastDiagnostic().HardReanchored {
		t.Fatal("seed packet must not record HardReanchored")
	}

	// Audio first packet 5 ms later — anchored at elapsed (5), independent
	// timebase preserved (no re-anchor, no shared origin clobber).
	a := aPkt(50_000_000, 50_000_000)
	n.Apply(a, startWall.Add(5*time.Millisecond))
	if a.DTSms != 5 {
		t.Fatalf("audio seed at +5ms: want dts=5, got %d", a.DTSms)
	}
	if n.LastDiagnostic().HardReanchored {
		t.Fatal("audio seed must not record HardReanchored")
	}
}

// TestSteadyState walks 25 frames at 40 ms cadence and asserts each
// emitted DTS sits at i*40 (no drift, no re-anchor).
func TestSteadyState(t *testing.T) {
	n := New(DefaultConfig())
	for i := 0; i < 25; i++ {
		p := vPkt(uint64(1_000_000+i*40), uint64(1_000_000+i*40)) //nolint:gosec
		n.Apply(p, startWall.Add(time.Duration(i*40)*time.Millisecond))
		want := uint64(i * 40) //nolint:gosec
		if p.DTSms != want {
			t.Fatalf("steady frame %d: want dts=%d, got %d", i, want, p.DTSms)
		}
		if n.LastDiagnostic().HardReanchored {
			t.Fatalf("steady frame %d: HardReanchored should not fire", i)
		}
	}
}

// TestMonotonicFloor — feed a backward-jumping input PTS that lands
// below the running output floor. Output must still be strictly
// monotonic (≥ lastOutputDts+1) and HardReanchored must record.
func TestMonotonicFloor(t *testing.T) {
	n := New(Config{Enabled: true, JumpThresholdMs: 500, CrossTrackSnapMs: 1000})

	// 25 frames at 40 ms in PTS, taking 1000 ms wallclock.
	for i := 0; i < 25; i++ {
		p := vPkt(uint64(1_000_000+i*40), uint64(1_000_000+i*40)) //nolint:gosec
		n.Apply(p, startWall.Add(time.Duration(i*40)*time.Millisecond))
	}
	// Now an upstream regression: PTS jumps 500 ms BACKWARDS.
	jump := vPkt(uint64(1_000_960-500), uint64(1_000_960-500))
	n.Apply(jump, startWall.Add(1040*time.Millisecond))

	if !n.LastDiagnostic().HardReanchored {
		t.Fatal("backward jump must record HardReanchored")
	}
	// Monotonic floor: output > 960 (the 25th frame's emit).
	if jump.DTSms <= 960 {
		t.Fatalf("backward jump must clamp ≥ lastOutputDts+1; got %d", jump.DTSms)
	}
}

// TestDriftCapAhead — sustained input running ahead of wallclock past
// MaxAheadMs must drop. Subsequent slower packets recover.
func TestDriftCapAhead(t *testing.T) {
	n := New(Config{
		Enabled:          true,
		JumpThresholdMs:  10_000, // wide so the drift cap fires before the jump branch
		MaxAheadMs:       500,
		CrossTrackSnapMs: 1000,
	})

	// Seed at t=0.
	n.Apply(vPkt(0, 0), startWall)
	// One frame "in the future" — input claims +2000 ms, wallclock at +200 ms.
	// Expected drift = 2000 - 200 = 1800 > MaxAheadMs(500) → drop.
	future := vPkt(2000, 2000)
	if keep := n.Apply(future, startWall.Add(200*time.Millisecond)); keep {
		t.Fatalf("drift-cap-ahead must drop packet that races wallclock; got keep=true, dts=%d", future.DTSms)
	}
	diag := n.LastDiagnostic()
	if !diag.Dropped {
		t.Fatalf("diagnostic must report Dropped=true; got %+v", diag)
	}
}

// TestDriftCapBehind — sustained input running BEHIND wallclock past
// MaxBehindMs must hard-re-anchor (the symmetric branch ptsrebaser
// lacks). This is the long-runtime drift class the refactor targets.
func TestDriftCapBehind(t *testing.T) {
	n := New(Config{
		Enabled:          true,
		JumpThresholdMs:  10_000, // wide so the |drift| branch doesn't pre-empt
		MaxBehindMs:      500,
		CrossTrackSnapMs: 1000,
	})

	// Seed at t=0; subsequent frames keep up at first.
	n.Apply(vPkt(0, 0), startWall)
	n.Apply(vPkt(40, 40), startWall.Add(40*time.Millisecond))

	// Wallclock leaps 2 s while the source delivers a single +80 ms frame.
	// Expected drift = 80 - 2080 = -2000 < -MaxBehindMs(-500) → re-anchor.
	lag := vPkt(80, 80)
	keep := n.Apply(lag, startWall.Add(2080*time.Millisecond))
	if !keep {
		t.Fatal("behind-drift packet must NOT be dropped (only ahead-drift drops)")
	}
	if !n.LastDiagnostic().HardReanchored {
		t.Fatal("behind-drift cap must record HardReanchored on re-anchor")
	}
	// Output anchored at actualNow = 2080 ms (≈), clamped above last emit.
	if lag.DTSms < 2000 {
		t.Fatalf("behind-drift re-anchor: want dts≈2080, got %d", lag.DTSms)
	}
}

// TestJumpAhead — input PTS leaps forward past JumpThresholdMs. Must
// hard-re-anchor to wallclock (not blindly trust the source) and record
// HardReanchored.
func TestJumpAhead(t *testing.T) {
	n := New(Config{Enabled: true, JumpThresholdMs: 500, CrossTrackSnapMs: 1000})
	n.Apply(vPkt(0, 0), startWall)
	n.Apply(vPkt(40, 40), startWall.Add(40*time.Millisecond))

	// Source jumps +10 s suddenly while wallclock only advanced 80 ms.
	jump := vPkt(10_040, 10_040)
	n.Apply(jump, startWall.Add(80*time.Millisecond))

	if !n.LastDiagnostic().HardReanchored {
		t.Fatal("forward jump past threshold must record HardReanchored")
	}
	// Anchored to actualNow ≈ 80 ms, not to the upstream's claimed 10_040.
	if jump.DTSms > 200 {
		t.Fatalf("forward jump re-anchor: want dts≈80 (wallclock), got %d", jump.DTSms)
	}
}

// TestCrossTrackSnap — when V drains a multi-second burst before A
// seeds, A snaps onto V's last emit. Mirrors ptsrebaser's progression-
// based snap.
func TestCrossTrackSnap(t *testing.T) {
	n := New(DefaultConfig())

	// 100 video frames at 40 ms PTS cadence, all delivered within ~500 µs.
	for i := 0; i < 100; i++ {
		v := vPkt(uint64(i*40), uint64(i*40)) //nolint:gosec
		n.Apply(v, startWall.Add(time.Duration(i)*5*time.Microsecond))
	}
	// V's last output ≈ 99*40 = 3960 ms.
	a := aPkt(0, 0)
	n.Apply(a, startWall.Add(600*time.Microsecond))
	if a.DTSms < 3900 {
		t.Fatalf("cross-track snap: A should anchor onto V's progression (≈3960), got %d", a.DTSms)
	}
	if n.LastDiagnostic().HardReanchored {
		t.Fatal("cross-track snap must NOT record HardReanchored (it's a clean seed, not a jump)")
	}
}

// TestIndependentVATimelines — V and A may have wildly different
// upstream timebases (RTP per-track). The Normaliser must NOT use a
// single shared origin or every other packet would ping-pong it.
func TestIndependentVATimelines(t *testing.T) {
	n := New(DefaultConfig())

	v := vPkt(1_000_000, 1_000_000)
	a := aPkt(50_000_000, 50_000_000)
	n.Apply(v, startWall)
	n.Apply(a, startWall.Add(5*time.Millisecond))

	if v.DTSms != 0 {
		t.Fatalf("V seed: want dts=0, got %d", v.DTSms)
	}
	if a.DTSms != 5 {
		t.Fatalf("A seed (5ms later): want dts=5, got %d", a.DTSms)
	}
	if n.LastDiagnostic().HardReanchored {
		t.Fatal("independent A seed must not record HardReanchored")
	}

	v2 := vPkt(1_000_040, 1_000_040)
	a2 := aPkt(50_000_021, 50_000_021)
	n.Apply(v2, startWall.Add(40*time.Millisecond))
	n.Apply(a2, startWall.Add(45*time.Millisecond))
	if v2.DTSms != 40 {
		t.Fatalf("V2: want 40, got %d", v2.DTSms)
	}
	if a2.DTSms != 26 {
		t.Fatalf("A2: want 26 (=5+21), got %d", a2.DTSms)
	}
}

// TestOnSessionResetsPerTrackButPreservesWallclock — OnSession wipes
// per-track input anchors so the next packet's source PTS becomes the
// new per-track baseline, but wallOrigin persists across session
// boundaries so output DTS stays strictly monotonic. The boundary cue
// for downstream consumers is the buffer-hub-level SessionStart=true
// marker, not a DTS reset.
func TestOnSessionResetsPerTrackButPreservesWallclock(t *testing.T) {
	n := New(DefaultConfig())

	// Session 1: V runs to ~1 s of output DTS.
	n.OnSession(&domain.StreamSession{ID: 1, Reason: domain.SessionStartFresh})
	for i := 0; i < 25; i++ {
		v := vPkt(uint64(i*40), uint64(i*40)) //nolint:gosec
		n.Apply(v, startWall.Add(time.Duration(i*40)*time.Millisecond))
	}
	if got := n.Stats().TotalApply; got != 25 {
		t.Fatalf("after S1: TotalApply=%d, want 25", got)
	}

	// Session 2: fresh upstream PTS at a totally different epoch.
	n.OnSession(&domain.StreamSession{ID: 2, Reason: domain.SessionStartFailover})
	if got := n.Stats().CurrentSessionID; got != 2 {
		t.Fatalf("CurrentSessionID after OnSession(2) = %d, want 2", got)
	}
	// 500 ms after the last S1 packet, S2's first V arrives with a
	// wildly different upstream PTS. The per-track input baseline
	// resets onto that upstream PTS, but the OUTPUT anchors at
	// elapsed-since-wallOrigin — i.e. ≈ 1500 ms (1000 ms of S1 plus
	// the 500 ms gap).
	s2start := startWall.Add(1500 * time.Millisecond)
	v := vPkt(99_999_000, 99_999_000)
	n.Apply(v, s2start)
	if v.DTSms < 1000 {
		t.Fatalf("S2 first V must anchor past S1's last output for monotonicity; got DTS=%d", v.DTSms)
	}
	if n.LastDiagnostic().HardReanchored {
		t.Fatal("S2 first packet seed must not record HardReanchored (caller sets SessionStart instead)")
	}

	// S2 second V: 40 ms later upstream, 40 ms later wallclock — must
	// advance 40 ms in output too.
	prevDTS := v.DTSms
	v2 := vPkt(99_999_040, 99_999_040)
	n.Apply(v2, s2start.Add(40*time.Millisecond))
	if v2.DTSms != prevDTS+40 {
		t.Fatalf("S2 second V: want %d, got %d", prevDTS+40, v2.DTSms)
	}
}

// TestSessionStartReasons covers the five reasons reset state
// identically — the Normaliser's reset behaviour is reason-agnostic.
func TestSessionStartReasons(t *testing.T) {
	reasons := []domain.SessionStartReason{
		domain.SessionStartFresh,
		domain.SessionStartReconnect,
		domain.SessionStartFailover,
		domain.SessionStartMixerCycle,
		domain.SessionStartConfigChange,
	}
	for _, r := range reasons {
		t.Run(r.String(), func(t *testing.T) {
			n := New(DefaultConfig())
			n.Apply(vPkt(0, 0), startWall)
			prevDTS := uint64(0)
			n.Apply(vPkt(40, 40), startWall.Add(40*time.Millisecond))

			n.OnSession(&domain.StreamSession{ID: 7, Reason: r})

			// Next packet uses fresh per-track baseline but continues
			// the wallclock-anchored output sequence (no backward jump).
			v := vPkt(99_999_000, 99_999_000)
			n.Apply(v, startWall.Add(500*time.Millisecond))
			if v.DTSms <= prevDTS+40 {
				t.Fatalf("%s: first packet after OnSession must continue monotonic output, got %d <= prev %d",
					r, v.DTSms, prevDTS+40)
			}
		})
	}
}

// TestSeedWallclockSharesTimeline — multiple Normaliser instances seeded
// with the same wallclock origin produce output on a common timeline,
// regardless of when each instance saw its first packet. Used by the
// coordinator's abr_mixer to keep V-tap and A-fan-out outputs aligned.
func TestSeedWallclockSharesTimeline(t *testing.T) {
	origin := startWall

	vNorm := New(DefaultConfig())
	aNorm := New(DefaultConfig())
	vNorm.SeedWallclock(origin)
	aNorm.SeedWallclock(origin)

	// V's first packet arrives 100 ms after the shared origin.
	v := vPkt(0, 0)
	vNorm.Apply(v, origin.Add(100*time.Millisecond))
	if v.DTSms != 100 {
		t.Fatalf("V seeded against shared origin: want DTS=100, got %d", v.DTSms)
	}

	// A's first packet arrives 600 ms after the shared origin — different
	// upstream, different timebase, but anchored to the SAME wallclock
	// origin so its output DTS is 600.
	a := aPkt(50_000_000, 50_000_000)
	aNorm.Apply(a, origin.Add(600*time.Millisecond))
	if a.DTSms != 600 {
		t.Fatalf("A seeded against shared origin: want DTS=600, got %d", a.DTSms)
	}
}

// TestDiagnosticReflectsLastApply — the dual-path observer relies on
// LastDiagnostic to compare per-packet outcomes against ptsrebaser.
func TestDiagnosticReflectsLastApply(t *testing.T) {
	n := New(DefaultConfig())
	n.OnSession(&domain.StreamSession{ID: 42})

	n.Apply(vPkt(0, 0), startWall)
	d := n.LastDiagnostic()
	if d.Track != trackVideo {
		t.Fatalf("seed diagnostic Track = %v, want trackVideo", d.Track)
	}
	if d.SessionID != 42 {
		t.Fatalf("seed diagnostic SessionID = %d, want 42", d.SessionID)
	}
	if d.HardReanchored || d.Dropped {
		t.Fatal("seed must not be marked as re-anchored or dropped")
	}

	// Force a hard re-anchor via forward jump.
	n.Apply(vPkt(10_000, 10_000), startWall.Add(50*time.Millisecond))
	d = n.LastDiagnostic()
	if !d.HardReanchored {
		t.Fatal("forward-jump diagnostic must report HardReanchored=true")
	}
}

// TestResetClearsEverything — Reset zeroes session ID + all per-track
// state, so subsequent Apply behaves like a freshly-constructed
// Normaliser.
func TestResetClearsEverything(t *testing.T) {
	n := New(DefaultConfig())
	n.OnSession(&domain.StreamSession{ID: 9})
	n.Apply(vPkt(0, 0), startWall)

	n.Reset()

	if got := n.Stats().CurrentSessionID; got != 0 {
		t.Fatalf("Reset must clear SessionID, got %d", got)
	}
	// Apply again with very different PTS — must seed at 0.
	v := vPkt(99_999_000, 99_999_000)
	n.Apply(v, startWall.Add(time.Second))
	if v.DTSms != 0 {
		t.Fatalf("Apply after Reset must re-seed at 0, got %d", v.DTSms)
	}
}

// TestCTOPreserved — Composition Time Offset (PTS − DTS) must survive
// every code path. B-frames depend on this for correct presentation
// order in the muxer downstream.
func TestCTOPreserved(t *testing.T) {
	n := New(DefaultConfig())

	cases := []struct {
		name string
		pts  uint64
		dts  uint64
		want int64
	}{
		{"non-B (PTS=DTS)", 1000, 1000, 0},
		{"B-frame with CTO=33", 1033, 1000, 33},
		{"B-frame with CTO=80", 1080, 1000, 80},
		{"clamp negative CTO (clamps to 0)", 1000, 1050, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n.Reset()
			p := vPkt(tc.pts, tc.dts)
			n.Apply(p, startWall)
			got := int64(p.PTSms) - int64(p.DTSms) //nolint:gosec
			if got != tc.want {
				t.Fatalf("CTO: want %d, got %d (pts=%d dts=%d)",
					tc.want, got, p.PTSms, p.DTSms)
			}
		})
	}
}

// TestDefaultConfigMatchesPTSRebaser — sanity-check that the default
// settings line up with ptsrebaser.DefaultConfig so the dual-path
// observer doesn't drown in trivial divergences.
func TestDefaultConfigMatchesPTSRebaser(t *testing.T) {
	c := DefaultConfig()
	if !c.Enabled {
		t.Fatal("DefaultConfig must be enabled")
	}
	if c.JumpThresholdMs != 2000 {
		t.Fatalf("DefaultConfig JumpThresholdMs = %d, want 2000 (parity with ptsrebaser)", c.JumpThresholdMs)
	}
	if c.MaxAheadMs != 0 {
		t.Fatalf("DefaultConfig MaxAheadMs = %d, want 0 (parity with ptsrebaser)", c.MaxAheadMs)
	}
	if c.CrossTrackSnapMs != 1000 {
		t.Fatalf("DefaultConfig CrossTrackSnapMs = %d, want 1000 (parity)", c.CrossTrackSnapMs)
	}
}
