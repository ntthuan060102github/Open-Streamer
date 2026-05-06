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
