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
	mpeg2 "github.com/yapingcat/gomedia/go-mpeg2"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
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

// testTSBufferCap is the cap used by tsBuffer overflow tests. KiB-scale
// keeps allocations small under -race so these tests don't starve the
// scheduler and flake unrelated time-sensitive tests in this package.
// Per-instance via newTSBufferWithCap so concurrent Parallel tests don't
// share state.
const testTSBufferCap = 8192

// Read on a fully-drained buffer must release the underlying array, not
// just advance the slice header. Without this, a one-off Write of N bytes
// retains N bytes across the buffer's lifetime.
func TestTSBufferReadReleasesUnderlyingArrayWhenDrained(t *testing.T) {
	t.Parallel()
	b := newTSBuffer("")

	big := make([]byte, 4096)
	_, err := b.Write(big)
	require.NoError(t, err)
	require.Equal(t, 4096, cap(b.buf), "underlying array should hold the burst")

	out := make([]byte, 4096)
	n, err := b.Read(out)
	require.NoError(t, err)
	require.Equal(t, 4096, n)
	require.Nil(t, b.buf,
		"after fully draining, buf must be reset to nil so the array is GC-eligible — slicing alone keeps the array pinned")
}

// Write past the cap must drop the existing backlog (not just truncate it
// to the cap) and append the fresh data. The demuxer recovers from the
// next PAT/PMT in the new bytes — partial backlog + fresh data would
// confuse it more than a clean reset.
func TestTSBufferWriteDropsBacklogOnOverflow(t *testing.T) {
	t.Parallel()
	b := newTSBufferWithCap("test-stream", testTSBufferCap)

	// Fill to just under the cap.
	first := make([]byte, testTSBufferCap-1024)
	_, err := b.Write(first)
	require.NoError(t, err)
	require.Equal(t, int64(0), b.dropCount, "no overflow yet")

	// 2 KiB extra would push past the cap → triggers drop.
	overflow := make([]byte, 2048)
	n, err := b.Write(overflow)
	require.NoError(t, err)
	require.Equal(t, 2048, n, "Write must report success even on the drop path")
	require.Equal(t, int64(1), b.dropCount, "exactly one drop event")
	require.Equal(t, int64(testTSBufferCap-1024), b.droppedSize,
		"droppedSize must reflect the discarded backlog")
	require.Equal(t, 2048, len(b.buf),
		"after drop, buf holds only the fresh chunk (the demuxer needs SOMETHING to resync from)")
}

// Multiple drops accumulate counters so operators can see chronic
// overflow in metrics, not just the most recent event.
func TestTSBufferWriteAccumulatesDropCounters(t *testing.T) {
	t.Parallel()
	b := newTSBufferWithCap("", testTSBufferCap)

	chunk := make([]byte, testTSBufferCap)
	// First write fills exactly to cap.
	_, _ = b.Write(chunk)
	// Each subsequent write triggers a drop.
	_, _ = b.Write(chunk)
	_, _ = b.Write(chunk)

	require.Equal(t, int64(2), b.dropCount, "two overflow events")
	require.Equal(t, int64(2*testTSBufferCap), b.droppedSize,
		"each drop discards a full cap's worth")
}

// Cap is enforced regardless of whether the producer writes one big chunk
// or many small chunks — the cap is on backlog size, not per-call.
func TestTSBufferWriteCapsAcrossSmallChunks(t *testing.T) {
	t.Parallel()
	b := newTSBufferWithCap("", testTSBufferCap)

	chunk := make([]byte, 512)
	for range 16 {
		_, _ = b.Write(chunk)
	}
	require.Equal(t, testTSBufferCap, len(b.buf), "exactly at cap")
	require.Equal(t, int64(0), b.dropCount, "no drops yet — exactly at cap")

	_, _ = b.Write(chunk) // one more chunk → drop
	require.Equal(t, int64(1), b.dropCount)
}

// videoPS append must stop growing past the cap and set the give-up flag.
// Without this, a stream that never emits a parseable SPS/PPS (malformed
// source, unsupported codec config) grew videoPS unbounded AND ran
// bytes.ReplaceAll on the whole buffer per frame — quadratic in memory
// and CPU, the dominant production OOM path observed in pprof.
// keyframeAnnexB returns a synthetic Annex-B keyframe payload of approximately
// `payloadSize` bytes — an IDR NAL (type 5) start code + 0x65 + filler.
// tsmux.KeyFrameH264 detects NAL type 5 in the byte stream, so only the
// header bytes need to be exact. Filler is arbitrary, used to inflate the
// frame to the desired total size without altering the IDR detection.
func keyframeAnnexB(payloadSize int) []byte {
	if payloadSize < 5 {
		payloadSize = 5
	}
	out := make([]byte, payloadSize)
	out[0], out[1], out[2], out[3] = 0x00, 0x00, 0x00, 0x01
	out[4] = 0x65 // NAL type 5 = IDR slice
	return out
}

func TestAppendVideoPSLockedCapsAndSetsGiveUp(t *testing.T) {
	t.Parallel()
	p := &dashFMP4Packager{}

	// First IDR frame appends normally.
	require.True(t, p.appendVideoPSLocked(keyframeAnnexB(1024)),
		"under cap → caller should run tryInit")
	require.Equal(t, 1024, len(p.videoPS))
	require.False(t, p.videoPSGiveUp)

	// Push an IDR frame that crosses the cap.
	require.False(t, p.appendVideoPSLocked(keyframeAnnexB(videoPSMaxBytes)),
		"keyframe would exceed cap → caller must skip tryInit")
	require.True(t, p.videoPSGiveUp,
		"give-up flag must be set so subsequent frames are also skipped")
	require.Nil(t, p.videoPS,
		"videoPS must be cleared on give-up so its capacity is released")
}

// P-frames carry no SPS/PPS — appending them to videoPS only burns the cap
// without contributing to init detection. The keyframe filter MUST skip
// P-frames silently so a long-GOP source (NVENC default 10 s) doesn't
// latch videoPSGiveUp before the first IDR arrives.
//
// Regression guard for production failure: RTMP-loopback streams (test5)
// reconnected mid-GOP, accumulated 250+ P-frames at ~150 KB each (~37 MB)
// before the next IDR, hit the 4 MB cap, and stayed audio-only forever.
func TestAppendVideoPSLockedSkipsPFrames(t *testing.T) {
	t.Parallel()
	p := &dashFMP4Packager{}

	// Synthetic P-frame: NAL type 1 (non-IDR slice). 200 KB each.
	pframe := make([]byte, 200*1024)
	pframe[0], pframe[1], pframe[2], pframe[3] = 0x00, 0x00, 0x00, 0x01
	pframe[4] = 0x41 // NAL type 1 = P slice (with nal_ref_idc=2)

	// Push 250 P-frames (~50 MB total — way past the 4 MB cap if appended).
	for range 250 {
		require.False(t, p.appendVideoPSLocked(pframe),
			"P-frame must be skipped — no SPS/PPS to extract")
	}
	require.False(t, p.videoPSGiveUp,
		"videoPSGiveUp must NOT latch on P-frames — they were skipped, not appended")
	require.Empty(t, p.videoPS,
		"P-frames must not accumulate into videoPS")

	// First IDR after the long P-only run still works.
	require.True(t, p.appendVideoPSLocked(keyframeAnnexB(1024)),
		"first IDR after P-only run must still trigger init")
	require.Equal(t, 1024, len(p.videoPS))
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

// dashStreamTypeForCodec must map every codec the DASH packager actually
// processes (H264 / H265 / AAC). Anything else returns ok=false so the
// caller falls back to the raw-TS pipeline, which is the safe default.
// Regression guard for the AV bypass path: production was missing SPS/PPS
// in DASH because frames went through the TSMuxer→TSDemuxer round-trip
// (gomedia's TSDemuxer drops parameter-set NAL units for H264). Direct
// dispatch via this mapping skips the round-trip entirely.
func TestDashStreamTypeForCodec(t *testing.T) {
	t.Parallel()
	cases := []struct {
		codec  domain.AVCodec
		want   mpeg2.TS_STREAM_TYPE
		wantOK bool
	}{
		{domain.AVCodecH264, mpeg2.TS_STREAM_H264, true},
		{domain.AVCodecH265, mpeg2.TS_STREAM_H265, true},
		{domain.AVCodecAAC, mpeg2.TS_STREAM_AAC, true},

		// Codecs without a direct DASH path — must NOT be claimed by the
		// bypass branch (they'd skip the safe raw-TS fallback otherwise).
		{domain.AVCodecRawTSChunk, 0, false},
		{domain.AVCodecMP3, 0, false},
		{domain.AVCodecAC3, 0, false},
		{domain.AVCodecUnknown, 0, false},
	}
	for _, c := range cases {
		got, ok := dashStreamTypeForCodec(c.codec)
		require.Equal(t, c.wantOK, ok, "codec %v ok flag", c.codec)
		if c.wantOK {
			require.Equal(t, c.want, got, "codec %v stream type", c.codec)
		}
	}
}

// onTSFrame fed an H264 access unit with SPS+PPS+IDR (the wire shape the
// ingestor produces for keyframes after RTMP / RTSP pull) MUST produce
// videoInit. This is the contract the AV bypass relies on — direct calls
// from the producer goroutine skip the gomedia round-trip and depend on
// onTSFrame finding parameter sets in the bytes itself.
func TestOnTSFrameBuildsVideoInitFromAVAnnexBKeyframe(t *testing.T) {
	t.Parallel()

	// Real-world H264 NAL units captured from a 1080p Main@4.0 stream.
	// SPS encodes resolution + profile so mp4ff's ParseSPSNALUnit accepts
	// it; PPS is a typical Main-profile PPS payload.
	sps := []byte{0x67, 0x4d, 0x40, 0x28, 0xeb, 0x05, 0x07, 0x80, 0x44, 0x00, 0x00, 0x03, 0x00, 0x04, 0x00, 0x00, 0x03, 0x00, 0xf0, 0x3c, 0x60, 0xc6, 0x58}
	pps := []byte{0x68, 0xee, 0x3c, 0x80}
	// Synthetic IDR slice — NAL type 5, payload arbitrary. The DASH
	// packager doesn't decode the slice, only scans NAL types in the
	// stream for parameter-set extraction.
	idr := []byte{0x65, 0x88, 0x80, 0x40, 0x00, 0x00}

	startCode := []byte{0, 0, 0, 1}
	frame := make([]byte, 0, len(sps)+len(pps)+len(idr)+12)
	frame = append(frame, startCode...)
	frame = append(frame, sps...)
	frame = append(frame, startCode...)
	frame = append(frame, pps...)
	frame = append(frame, startCode...)
	frame = append(frame, idr...)

	// streamDir MUST be a tempdir — onTSFrame's success path writes the
	// init segment to disk via encodeInitToFile(streamDir/init_v.mp4).
	// Without this, an empty streamDir resolves to the working directory
	// and leaks "init_v.mp4" into internal/publisher/ alongside the test.
	p := &dashFMP4Packager{streamDir: t.TempDir()}
	require.Nil(t, p.videoInit, "video init starts unset")

	p.onTSFrame(mpeg2.TS_STREAM_H264, frame, 1000, 1000)

	require.NotNil(t, p.videoInit,
		"feeding an H264 access unit with SPS+PPS+IDR must produce video init — this is the contract the AV-bypass producer relies on")
	require.Nil(t, p.videoPS,
		"once init is built, videoPS is cleared — keeps the bypass loop allocation-free for the rest of the session")
	require.False(t, p.videoPSGiveUp, "give-up flag must NOT be latched on the success path")
}

func TestTimestampJumpFromLast(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		queue []uint64
		ts    uint64
		want  bool
	}{
		{name: "empty queue is never a jump", queue: nil, ts: 1000, want: false},
		{name: "tiny forward step within frame cadence", queue: []uint64{1000}, ts: 1040, want: false},
		{name: "tiny backward step within tolerance", queue: []uint64{1000}, ts: 960, want: false},
		{name: "exact threshold backward is not a jump", queue: []uint64{2000}, ts: 1000, want: false},
		{name: "exact threshold forward is not a jump", queue: []uint64{1000}, ts: 2000, want: false},
		{name: "1ms past threshold backward triggers", queue: []uint64{2001}, ts: 1000, want: true},
		{name: "1ms past threshold forward triggers", queue: []uint64{1000}, ts: 2001, want: true},
		// Real-world: bac_ninh CDN playlist resync skips ~200s ahead
		// of last queued DTS — pre-fix this baked +200s into tfdt.
		{name: "200s forward jump (CDN resync) triggers", queue: []uint64{50_000}, ts: 250_000, want: true},
		// Real-world: source failover where new source DTS resets
		// near zero. Pre-fix this also worked, kept as regression test.
		{name: "source-switch backward jump triggers", queue: []uint64{1_000_000}, ts: 5_000, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := timestampJumpFromLast(tc.queue, tc.ts)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestOnTSFrameForwardJumpFlushesQueue verifies the forward-jump guard
// added to onTSFrame: a single forward DTS jump >> dashSourceSwitchJumpMs
// must flush the in-flight segment so the jump becomes a segment boundary
// instead of one giant per-sample dur (which was baking +N seconds into
// the SegmentTimeline's tfdt and breaking DASH live edge math).
func TestOnTSFrameForwardJumpFlushesQueue(t *testing.T) {
	t.Parallel()

	// Synthetic non-keyframe NALU — onTSFrame doesn't decode the slice,
	// only inspects NAL types. We're exercising the queue-management path
	// (vDTS append, pre-append flush), not init-segment generation, so
	// no SPS/PPS is needed and videoInit can stay nil.
	nonKey := []byte{0x00, 0x00, 0x00, 0x01, 0x41, 0x88, 0x80, 0x40, 0x00, 0x00}

	p := &dashFMP4Packager{streamDir: t.TempDir()}

	// Frame 1: anchor.
	p.onTSFrame(mpeg2.TS_STREAM_H264, nonKey, 1000, 1000)
	require.Equal(t, []uint64{1000}, p.vDTS, "first frame seeds vDTS")

	// Frame 2: a frame-cadence step (40ms @ 25fps) — must NOT trigger
	// flush. vDTS grows, no spurious boundary.
	p.onTSFrame(mpeg2.TS_STREAM_H264, nonKey, 1040, 1040)
	require.Equal(t, []uint64{1000, 1040}, p.vDTS, "frame-cadence step appends without flush")

	// Frame 3: a 200s forward jump — must trigger flushSegmentLocked
	// before the new DTS is appended. flushSegmentLocked clears vDTS
	// (see line 804) regardless of whether segment writing succeeds, so
	// after the call vDTS holds ONLY the post-jump frame.
	p.onTSFrame(mpeg2.TS_STREAM_H264, nonKey, 201_040, 201_040)
	require.Equal(t, []uint64{201_040}, p.vDTS,
		"forward jump past dashSourceSwitchJumpMs must flush queue, leaving only the jumped frame")
}
