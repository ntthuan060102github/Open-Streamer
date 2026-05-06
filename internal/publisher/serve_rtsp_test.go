package publisher

import (
	"context"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/stretchr/testify/require"
)

// computeVideoRTP must never emit an RTP timestamp ≤ the last one sent.
// Real-world trigger: HLS source jitter where DTS dips a few tens of ms
// inside the rebase window (rtspMaxBackJumpMs = 250). Without the clamp,
// the resulting rtpTS goes backwards and ffmpeg copy mode rejects every
// downstream packet with "non monotonically increasing dts to muxer".
func TestComputeVideoRTPMonotonicClampOnSmallJitter(t *testing.T) {
	sess := &rtspSession{}

	// Frame 1 anchors the timeline.
	got := sess.computeVideoRTP(1000, 1000)
	require.Equal(t, uint32(0), got, "first frame anchors at rtpTS=0")
	sess.lastVideoRTP = got
	sess.lastVideoSrc = 1000

	// Frame 2: forward by 40ms — normal 25fps tick.
	got = sess.computeVideoRTP(1040, 1040)
	require.Equal(t, uint32(40*90), got)
	sess.lastVideoRTP = got
	sess.lastVideoSrc = 1040

	// Frame 3: source jitter — pts dips 50ms back, well inside the 250ms
	// rebase threshold. Without the clamp this returns (-10ms)*90 wrapped
	// or 30ms*90 = 2700 which is < lastVideoRTP=3600. With the clamp it
	// must exceed lastVideoRTP by at least 1.
	got = sess.computeVideoRTP(1030, 1030)
	require.Greater(t, got, sess.lastVideoRTP, "jitter inside rebase window must still emit a strictly greater rtpTS")
	require.Equal(t, sess.lastVideoRTP+1, got, "clamp emits exactly +1 tick on backwards jitter")
}

// computeVideoRTP large-jump rebase still produces a monotonic value (legacy
// behaviour) — the clamp must not break the existing rebase path.
func TestComputeVideoRTPMonotonicAcrossLargeRebase(t *testing.T) {
	sess := &rtspSession{}

	_ = sess.computeVideoRTP(1000, 1000)
	sess.lastVideoRTP = 0
	sess.lastVideoSrc = 1000

	got := sess.computeVideoRTP(1040, 1040)
	require.Equal(t, uint32(40*90), got)
	sess.lastVideoRTP = got
	sess.lastVideoSrc = 1040

	// Big back-jump — looped source. dts dives below baseDTS, triggers
	// the rebase branch (not just the clamp).
	got = sess.computeVideoRTP(500, 500)
	require.Greater(t, got, sess.lastVideoRTP, "large back-jump must still produce a strictly greater rtpTS via rebase")
}

// Audio counterpart of TestComputeVideoRTPMonotonicClampOnSmallJitter.
func TestComputeAudioRTPMonotonicClampOnSmallJitter(t *testing.T) {
	sess := &rtspSession{
		aacCfg: &mpeg4audio.AudioSpecificConfig{SampleRate: 48000},
	}

	got := sess.computeAudioRTP(1000)
	require.Equal(t, uint32(0), got)
	sess.lastAudioRTP = got
	sess.lastAudioSrc = 1000

	// 21ms forward = 1 AAC AU at 48 kHz.
	got = sess.computeAudioRTP(1021)
	require.Equal(t, uint32(21*48000/1000), got)
	sess.lastAudioRTP = got
	sess.lastAudioSrc = 1021

	// Jitter back 10ms, inside the 250ms rebase threshold. Without the
	// clamp this returns 11*48 = 528 ticks which is < lastAudioRTP=1008.
	got = sess.computeAudioRTP(1011)
	require.Greater(t, got, sess.lastAudioRTP, "jitter inside rebase window must still emit a strictly greater rtpTS")
	require.Equal(t, sess.lastAudioRTP+1, got, "clamp emits exactly +1 tick on backwards jitter")
}

// pace anchors on the first call and returns immediately.
func TestPaceFirstCallAnchorsWithoutSleep(t *testing.T) {
	sess := &rtspSession{ctx: context.Background()}

	start := time.Now()
	sess.pace(5000)
	require.Less(t, time.Since(start), 5*time.Millisecond, "first pace call must not sleep")
	require.True(t, sess.paceSet)
	require.Equal(t, uint64(5000), sess.paceMedia)
}

// pace blocks for approximately (dts2-dts1) when the wallclock has not yet
// advanced that far past the anchor.
func TestPaceSleepsToWallclockTarget(t *testing.T) {
	sess := &rtspSession{ctx: context.Background()}
	sess.pace(0)

	target := 60 * time.Millisecond
	start := time.Now()
	sess.pace(uint64(target.Milliseconds()))
	elapsed := time.Since(start)

	require.GreaterOrEqual(t, elapsed, target-10*time.Millisecond,
		"pace must sleep close to the wallclock target")
	require.Less(t, elapsed, target+50*time.Millisecond,
		"pace must not over-sleep significantly past target")
}

// pace returns immediately when the wallclock is already past the target —
// catch-up scenario after a long upstream stall.
func TestPaceNoSleepWhenAlreadyLate(t *testing.T) {
	sess := &rtspSession{ctx: context.Background()}
	sess.paceBase = time.Now().Add(-200 * time.Millisecond)
	sess.paceMedia = 0
	sess.paceSet = true

	start := time.Now()
	sess.pace(50)
	require.Less(t, time.Since(start), 5*time.Millisecond,
		"already-late pace call must not sleep")
}

// PTS jumps far past wallclock (looped source, fresh keyframe burst) trip
// the max-sleep cap and re-anchor instead of sleeping for hours.
func TestPaceReanchorsOnHugeForwardJump(t *testing.T) {
	sess := &rtspSession{ctx: context.Background()}
	sess.pace(0)
	prevBase := sess.paceBase

	start := time.Now()
	sess.pace(uint64((10 * time.Minute).Milliseconds())) // way past max sleep
	require.Less(t, time.Since(start), 5*time.Millisecond,
		"huge forward jump must re-anchor instead of sleeping")
	require.NotEqual(t, prevBase, sess.paceBase, "anchor must move forward")
	require.Equal(t, uint64((10 * time.Minute).Milliseconds()), sess.paceMedia)
}

// Sustained underrun (we are >2s behind realtime) re-anchors instead of
// chasing the stale baseline forever.
func TestPaceReanchorsWhenFarBehind(t *testing.T) {
	sess := &rtspSession{ctx: context.Background()}
	sess.paceBase = time.Now().Add(-10 * time.Second)
	sess.paceMedia = 0
	sess.paceSet = true

	start := time.Now()
	sess.pace(100) // wallclock target = base + 100ms = -9.9s ago → way behind
	require.Less(t, time.Since(start), 5*time.Millisecond,
		"far-behind pace call must not sleep")
	require.WithinDuration(t, time.Now(), sess.paceBase, 10*time.Millisecond,
		"anchor must reset to ~now")
	require.Equal(t, uint64(100), sess.paceMedia)
}

// Backwards source DTS without the Discontinuity flag (jitter, reorder)
// must re-anchor instead of underflowing the unsigned subtraction.
func TestPaceReanchorsOnBackwardsDTS(t *testing.T) {
	sess := &rtspSession{ctx: context.Background()}
	sess.pace(5000)

	start := time.Now()
	sess.pace(4900)
	require.Less(t, time.Since(start), 5*time.Millisecond,
		"backwards dts must re-anchor without sleeping")
	require.Equal(t, uint64(4900), sess.paceMedia)
}

// Context cancellation aborts the in-flight sleep so stream teardown is
// not held up for up to rtspPaceMaxSleep.
func TestPaceAbortsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sess := &rtspSession{ctx: ctx}
	sess.pace(0)

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	sess.pace(5000) // would sleep ~5s without cancellation
	elapsed := time.Since(start)
	require.Less(t, elapsed, 200*time.Millisecond,
		"context cancellation must abort the pace sleep")
}
