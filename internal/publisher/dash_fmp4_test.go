package publisher

// dash_fmp4_test.go — guards the cross-track tfdt seeding that keeps DASH
// audio and video aligned at the source's actual offset, instead of
// collapsing both to "start at 0" and producing the audible drift that
// surfaces on streams whose source has any pre-roll skew (mp4 edit lists,
// HLS feeds where audio starts before the first video IDR, etc.).

import (
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/stretchr/testify/require"
)

// scaleOffset is the unit of cross-track sync — guard against future
// refactors that might inadvertently reintroduce uint64 underflow on the
// "first frame predates origin" edge case.
func TestScaleOffset(t *testing.T) {
	t.Parallel()

	// Audio leads by 100ms — video tfdt should land 100ms past origin in
	// the 90 kHz video clock.
	require.Equal(t, uint64(100*90), scaleOffset(100, 0, dashVideoTimescale),
		"video at +100ms from origin → 9000 ticks at 90 kHz")

	// Same offset in the 48 kHz audio clock.
	require.Equal(t, uint64(100*48), scaleOffset(100, 0, 48000),
		"audio at +100ms from origin → 4800 ticks at 48 kHz")

	// Both DTS at origin → tfdt = 0 (typical synchronous-start case).
	require.Equal(t, uint64(0), scaleOffset(5000, 5000, dashVideoTimescale))

	// First DTS predates origin → clamp to 0 instead of wrapping to a
	// near-2^64 tfdt that would crash players.
	require.Equal(t, uint64(0), scaleOffset(900, 1000, dashVideoTimescale),
		"underflow must clamp to 0")
}

// recordOriginDTSLocked must capture the FIRST observation only — once
// audio sets the origin, a later video call with a different DTS must not
// overwrite it (otherwise the seeders compute negative offsets and lose
// the very alignment we are trying to preserve).
func TestRecordOriginDTSLockedSticksFirstObservation(t *testing.T) {
	t.Parallel()
	p := &dashFMP4Packager{}

	p.recordOriginDTSLocked(5000)
	require.True(t, p.originDTSmsSet)
	require.Equal(t, uint64(5000), p.originDTSms)

	p.recordOriginDTSLocked(10000)
	require.Equal(t, uint64(5000), p.originDTSms,
		"second call must not overwrite the established origin")

	p.recordOriginDTSLocked(0)
	require.Equal(t, uint64(5000), p.originDTSms,
		"a smaller subsequent DTS must not displace the origin either")
}

// Video seed runs once, only after the init segment exists, and only when
// there is a queued frame to anchor to. Re-arming would corrupt the running
// nextDecode counter that subsequent segments accumulate from.
func TestSeedVideoNextDecodeLockedRunsOnceAndRequiresInit(t *testing.T) {
	t.Parallel()
	p := &dashFMP4Packager{}
	p.recordOriginDTSLocked(1000)

	// No init yet → no seed.
	p.vDTS = []uint64{1100}
	p.seedVideoNextDecodeLocked()
	require.Equal(t, uint64(0), p.videoNextDecode)
	require.False(t, p.videoFirstDTSmsSet)

	// Init exists, queued frame at +100ms → seed lands at 9000 (90 kHz).
	p.videoInit = mp4.CreateEmptyInit()
	p.seedVideoNextDecodeLocked()
	require.True(t, p.videoFirstDTSmsSet)
	require.Equal(t, uint64(9000), p.videoNextDecode,
		"100ms past origin → 100*90 ticks")

	// Subsequent calls must be no-ops even with different vDTS values.
	p.vDTS = []uint64{2000}
	p.videoNextDecode = 9000 // simulate writeVideoSegmentLocked having advanced it
	p.seedVideoNextDecodeLocked()
	require.Equal(t, uint64(9000), p.videoNextDecode,
		"seed must not re-fire after first call")
}

// Audio seed mirrors video — once-only, requires init, scales to audioSR.
// The frame DTS is passed in directly (rather than reading from a queue)
// because by the time seedAudioNextDecodeLocked runs we have just appended
// it to aRaw; passing it explicitly keeps the helper testable in isolation.
func TestSeedAudioNextDecodeLockedRunsOnceAndScalesToSampleRate(t *testing.T) {
	t.Parallel()
	p := &dashFMP4Packager{}
	p.recordOriginDTSLocked(1000)

	// No init yet → no seed.
	p.seedAudioNextDecodeLocked(1100)
	require.Equal(t, uint64(0), p.audioNextDecode)

	// audioSR not yet set → no seed even with init.
	p.audioInit = mp4.CreateEmptyInit()
	p.seedAudioNextDecodeLocked(1100)
	require.Equal(t, uint64(0), p.audioNextDecode)

	// Full state → seed at +100ms on a 48 kHz clock = 4800 ticks.
	p.audioSR = 48000
	p.seedAudioNextDecodeLocked(1100)
	require.True(t, p.audioFirstDTSmsSet)
	require.Equal(t, uint64(4800), p.audioNextDecode)

	// 44.1 kHz scaling on a fresh packager (regression guard against the
	// helper hardcoding 48000 anywhere).
	p2 := &dashFMP4Packager{}
	p2.recordOriginDTSLocked(1000)
	p2.audioInit = mp4.CreateEmptyInit()
	p2.audioSR = 44100
	p2.seedAudioNextDecodeLocked(1100)
	require.Equal(t, uint64(100*44100/1000), p2.audioNextDecode)
}

// The cross-track scenario this whole change exists for: audio arrives
// first at DTS=5000, video arrives 100ms later at DTS=5100. The two
// nextDecode counters must reflect that 100ms gap — audio at tfdt=0, video
// at tfdt=9000 (in 90 kHz).
//
// Without the fix, both lined up at tfdt=0 and the player rendered audio
// 100ms ahead of video forever.
func TestSeedersPreserveAudioVideoOffset(t *testing.T) {
	t.Parallel()
	p := &dashFMP4Packager{}

	// Audio first.
	p.recordOriginDTSLocked(5000)
	p.audioInit = mp4.CreateEmptyInit()
	p.audioSR = 48000
	p.seedAudioNextDecodeLocked(5000)
	require.Equal(t, uint64(0), p.audioNextDecode,
		"audio anchors at the origin → tfdt 0")

	// Video 100ms later.
	p.videoInit = mp4.CreateEmptyInit()
	p.vDTS = []uint64{5100}
	p.seedVideoNextDecodeLocked()
	require.Equal(t, uint64(9000), p.videoNextDecode,
		"video starts 100ms past origin → 9000 ticks at 90 kHz")
}

// ─── tsBuffer leak guards ────────────────────────────────────────────────────
//
// Three production-observed leaks in the DASH packager paths:
//   - tsBuffer.Read advanced the slice header but kept the peak-capacity
//     underlying array alive, so a one-time burst pinned RAM forever.
//   - tsBuffer.Write was unbounded — slow demuxer → backlog grew without
//     limit until OOM.
//   - dashFMP4Packager.videoPS grew on every video frame until init
//     completed, AND tryInitVideoLocked called bytes.ReplaceAll on the
//     whole buffer per frame → quadratic memory + CPU when the source
//     never emitted parseable SPS/PPS.
//
// These tests pin the fix behaviour so future refactors don't regress.

// Read on a fully-drained buffer must release the underlying array, not
// just advance the slice header. Without this, a one-off Write of N MiB
// retains N MiB across the buffer's lifetime.
func TestTSBufferReadReleasesUnderlyingArrayWhenDrained(t *testing.T) {
	t.Parallel()
	b := newTSBuffer("")

	big := make([]byte, 4<<20) // 4 MiB
	_, err := b.Write(big)
	require.NoError(t, err)
	require.Equal(t, 4<<20, cap(b.buf), "underlying array should hold the burst")

	out := make([]byte, 4<<20)
	n, err := b.Read(out)
	require.NoError(t, err)
	require.Equal(t, 4<<20, n)
	require.Nil(t, b.buf,
		"after fully draining, buf must be reset to nil so the 4 MiB array is GC-eligible — slicing alone keeps the array pinned")
}

// Write past the cap must drop the existing backlog (not just truncate it
// to the cap) and append the fresh data. The demuxer recovers from the
// next PAT/PMT in the new bytes — partial backlog + fresh data would
// confuse it more than a clean reset.
func TestTSBufferWriteDropsBacklogOnOverflow(t *testing.T) {
	t.Parallel()
	b := newTSBuffer("test-stream")

	// Fill to just under the cap.
	first := make([]byte, tsBufferMaxBytes-1024)
	_, err := b.Write(first)
	require.NoError(t, err)
	require.Equal(t, int64(0), b.dropCount, "no overflow yet")

	// 2 KiB extra would push past the cap → triggers drop.
	overflow := make([]byte, 2048)
	n, err := b.Write(overflow)
	require.NoError(t, err)
	require.Equal(t, 2048, n, "Write must report success even on the drop path")
	require.Equal(t, int64(1), b.dropCount, "exactly one drop event")
	require.Equal(t, int64(tsBufferMaxBytes-1024), b.droppedSize,
		"droppedSize must reflect the discarded backlog")
	require.Equal(t, 2048, len(b.buf),
		"after drop, buf holds only the fresh chunk (the demuxer needs SOMETHING to resync from)")
}

// Multiple drops accumulate counters so operators can see chronic
// overflow in metrics, not just the most recent event.
func TestTSBufferWriteAccumulatesDropCounters(t *testing.T) {
	t.Parallel()
	b := newTSBuffer("")

	chunk := make([]byte, tsBufferMaxBytes)
	// First write fills exactly to cap.
	_, _ = b.Write(chunk)
	// Each subsequent write triggers a drop.
	_, _ = b.Write(chunk)
	_, _ = b.Write(chunk)

	require.Equal(t, int64(2), b.dropCount, "two overflow events")
	require.Equal(t, int64(2*tsBufferMaxBytes), b.droppedSize,
		"each drop discards a full cap's worth")
}

// Cap is enforced regardless of whether the producer writes one big chunk
// or many small chunks — the cap is on backlog size, not per-call.
func TestTSBufferWriteCapsAcrossSmallChunks(t *testing.T) {
	t.Parallel()
	b := newTSBuffer("")

	chunk := make([]byte, 1<<20) // 1 MiB chunks
	for range 16 {
		_, _ = b.Write(chunk)
	}
	require.Equal(t, 16<<20, len(b.buf), "exactly at cap")
	require.Equal(t, int64(0), b.dropCount, "no drops yet — exactly at cap")

	_, _ = b.Write(chunk) // one more MiB → drop
	require.Equal(t, int64(1), b.dropCount)
}

// videoPS append must stop growing past the cap and set the give-up flag.
// Without this, a stream that never emits a parseable SPS/PPS (malformed
// source, unsupported codec config) grew videoPS unbounded AND ran
// bytes.ReplaceAll on the whole buffer per frame — quadratic in memory
// and CPU, the dominant production OOM path observed in pprof.
func TestAppendVideoPSLockedCapsAndSetsGiveUp(t *testing.T) {
	t.Parallel()
	p := &dashFMP4Packager{}

	// First frame appends normally.
	require.True(t, p.appendVideoPSLocked(make([]byte, 1024)),
		"under cap → caller should run tryInit")
	require.Equal(t, 1024, len(p.videoPS))
	require.False(t, p.videoPSGiveUp)

	// Push a frame that crosses the cap.
	huge := make([]byte, videoPSMaxBytes)
	require.False(t, p.appendVideoPSLocked(huge),
		"frame would exceed cap → caller must skip tryInit")
	require.True(t, p.videoPSGiveUp,
		"give-up flag must be set so subsequent frames are also skipped")
	require.Nil(t, p.videoPS,
		"videoPS must be cleared on give-up so its capacity is released")
}

// Once give-up is latched, ALL subsequent calls return false even for
// tiny frames — no recovery path. This is intentional: a stream that
// failed to deliver SPS/PPS in the first 4 MiB of video frames is
// pathological enough that retrying forever wastes CPU on bytes.Replace.
func TestAppendVideoPSLockedStaysLatchedAfterGiveUp(t *testing.T) {
	t.Parallel()
	p := &dashFMP4Packager{videoPSGiveUp: true}

	for range 10 {
		require.False(t, p.appendVideoPSLocked([]byte{0x00, 0x00, 0x00, 0x01}),
			"give-up is sticky")
	}
	require.Nil(t, p.videoPS, "no append happens after give-up")
}
