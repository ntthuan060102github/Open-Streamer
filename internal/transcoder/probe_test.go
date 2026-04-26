package transcoder

import (
	"context"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// Pure parser tests — no FFmpeg invocation needed. They pin the line
// shape we expect from `ffmpeg -version` / `-encoders` / `-muxers`
// across versions (4.x, 5.x, 6.x, 7.x, 8.x — banner format unchanged).

func TestParseFFmpegVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"ubuntu apt build", "ffmpeg version 4.4.2-0ubuntu0.22.04.1 Copyright …", "4.4.2-0ubuntu0.22.04.1"},
		{"static homebrew", "ffmpeg version 6.1.1 Copyright (c) 2000-2023 …", "6.1.1"},
		{"git build tag", "ffmpeg version n7.0-12-gabc123 Copyright …", "n7.0-12-gabc123"},
		{"v8 release", "ffmpeg version 8.0.1 Copyright (c) 2000-2025 …", "8.0.1"},
		{"unrelated banner", "FFprobe version 6.1.1", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, parseFFmpegVersion(tc.in))
		})
	}
}

// `ffmpeg -encoders` lists each encoder on its own line, prefixed with
// a 6-char flag column. Parser must match the NAME field exactly so
// "libx264" doesn't accidentally match "libx264rgb".
func TestEncoderPresent(t *testing.T) {
	t.Parallel()
	const sample = `Encoders:
 V..... = Video
 A..... = Audio
 ------
 V..... libsvtav1            SVT-AV1 (codec av1)
 V....D libx264              libx264 H.264 / AVC (codec h264)
 V....D libx264rgb           libx264 H.264 RGB (codec h264)
 V....D libx265              libx265 H.265 / HEVC (codec hevc)
 V..... h264_nvenc           NVIDIA NVENC H.264 encoder (codec h264)
 A....D aac                  AAC (Advanced Audio Coding)
 A..... libopus              libopus Opus (codec opus)
`
	assert.True(t, encoderPresent(sample, "libx264"))
	assert.True(t, encoderPresent(sample, "libx264rgb"))
	assert.True(t, encoderPresent(sample, "h264_nvenc"))
	assert.True(t, encoderPresent(sample, "aac"))
	assert.True(t, encoderPresent(sample, "libsvtav1"))

	// Must not partial-match: searching "libx" should not match libx264.
	assert.False(t, encoderPresent(sample, "libx"))
	// Must not match unlisted.
	assert.False(t, encoderPresent(sample, "libvpx-vp9"))
	assert.False(t, encoderPresent(sample, "hevc_nvenc"))
	// Header lines (no flag column with V/A/S) must be skipped.
	assert.False(t, encoderPresent(sample, "Encoders:"))
}

func TestMuxerPresent(t *testing.T) {
	t.Parallel()
	const sample = `Muxers:
 D. = Demuxing supported
 .E = Muxing supported
 --
  E dash            DASH Muxer
  E hls             Apple HTTP Live Streaming
  E mpegts          MPEG-TS (MPEG-2 Transport Stream)
  E rtp_mpegts      RTP/mpegts output format
`
	assert.True(t, muxerPresent(sample, "mpegts"))
	assert.True(t, muxerPresent(sample, "hls"))
	assert.True(t, muxerPresent(sample, "dash"))
	assert.True(t, muxerPresent(sample, "rtp_mpegts"))
	assert.False(t, muxerPresent(sample, "webm"))
	// Header lines must skip.
	assert.False(t, muxerPresent(sample, "Muxers:"))
}

// optionalEncodersFor: HW-specific filter — must return ONLY encoders
// relevant to that backend + the HW-independent audio set. Without this
// the probe response would force the UI to client-filter (the original
// pain point that motivated the hw parameter).
func TestOptionalEncodersFor_PerHW(t *testing.T) {
	t.Parallel()
	cases := []struct {
		hw          domain.HWAccel
		mustInclude []string
		mustExclude []string
	}{
		{
			hw:          domain.HWAccelNVENC,
			mustInclude: []string{"h264_nvenc", "hevc_nvenc", "libopus", "libmp3lame", "ac3"},
			mustExclude: []string{"h264_qsv", "h264_vaapi", "libx265", "libvpx-vp9"},
		},
		{
			hw:          domain.HWAccelNone,
			mustInclude: []string{"libx265", "libvpx-vp9", "libsvtav1", "libopus"},
			mustExclude: []string{"h264_nvenc", "h264_qsv", "h264_vaapi", "h264_videotoolbox"},
		},
		{
			hw:          domain.HWAccelQSV,
			mustInclude: []string{"h264_qsv", "hevc_qsv", "libopus"},
			mustExclude: []string{"h264_nvenc", "h264_vaapi", "libx265"},
		},
		{
			hw:          domain.HWAccelVAAPI,
			mustInclude: []string{"h264_vaapi", "hevc_vaapi"},
			mustExclude: []string{"h264_nvenc", "h264_qsv"},
		},
		{
			hw:          domain.HWAccelVideoToolbox,
			mustInclude: []string{"h264_videotoolbox", "hevc_videotoolbox"},
			mustExclude: []string{"h264_nvenc", "h264_qsv"},
		},
	}
	for _, tc := range cases {
		t.Run(string(tc.hw), func(t *testing.T) {
			t.Parallel()
			got := optionalEncodersFor(tc.hw)
			for _, want := range tc.mustInclude {
				assert.Contains(t, got, want, "%s set must include %s", tc.hw, want)
			}
			for _, unwanted := range tc.mustExclude {
				assert.NotContains(t, got, unwanted, "%s set must NOT include %s", tc.hw, unwanted)
			}
		})
	}
}

// Empty hw → boot-check / show-all view: union of every backend's
// encoders + audio. Must never return empty (regression guard against
// "missing case" returning nil).
func TestOptionalEncodersFor_EmptyHWReturnsUnion(t *testing.T) {
	t.Parallel()
	got := optionalEncodersFor("")
	for _, want := range []string{
		"h264_nvenc", "h264_vaapi", "h264_qsv", "h264_videotoolbox",
		"libx265", "libvpx-vp9", "libsvtav1",
		"libopus", "libmp3lame", "ac3",
	} {
		assert.Contains(t, got, want, "empty hw must include %s in the union", want)
	}
}

// Unknown / typo'd HW value must fall back to a non-empty set rather
// than dropping warnings entirely. Picking the CPU set keeps audio +
// CPU video alternatives covered.
func TestOptionalEncodersFor_UnknownHWFallsBack(t *testing.T) {
	t.Parallel()
	got := optionalEncodersFor("not-a-real-backend")
	assert.NotEmpty(t, got, "unknown hw must fall back, not return empty")
	assert.Contains(t, got, "libopus", "audio set must always be present")
}

// End-to-end probe against the real FFmpeg binary on PATH. Skip when
// not installed so the suite still runs in minimal environments.
func TestProbe_RealFFmpeg(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH, skipping integration probe")
	}
	res, err := Probe(context.Background(), "", "")
	require.NoError(t, err, "probe must not error when ffmpeg exists on PATH")
	require.NotNil(t, res)
	assert.True(t, res.OK, "ffmpeg on PATH must satisfy required capabilities; errors=%v", res.Errors)
	assert.NotEmpty(t, res.Version, "version must be parsed from banner")

	// libx264 + aac + mpegts are the REQUIRED set we always expect from
	// any reasonable upstream/distro build (homebrew, apt, static, …).
	assert.True(t, res.Encoders["required"]["libx264"], "libx264 must be present in required set")
	assert.True(t, res.Encoders["required"]["aac"], "aac must be present in required set")
	assert.True(t, res.Muxers["mpegts"], "mpegts must be present")
}

// hw filter end-to-end: response's optional map must contain only the
// encoders relevant to the requested backend. Pin the contract the UI
// depends on (no client-side filtering needed).
func TestProbe_RealFFmpeg_FilterByHW(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH, skipping integration probe")
	}
	res, err := Probe(context.Background(), "", domain.HWAccelNone)
	require.NoError(t, err)

	opt := res.Encoders["optional"]
	// Must include the CPU-pipeline alternatives + audio.
	for _, want := range []string{"libx265", "libvpx-vp9", "libsvtav1", "libopus", "ac3"} {
		assert.Contains(t, opt, want, "hw=none must report %s as optional", want)
	}
	// Must NOT include other backends' encoders — that's the whole
	// point of the filter.
	for _, unwanted := range []string{"h264_nvenc", "hevc_nvenc", "h264_qsv", "h264_vaapi"} {
		assert.NotContains(t, opt, unwanted, "hw=none must NOT report %s", unwanted)
	}
}

// Probe against a non-existent binary path must surface error (not
// silently return OK). Caller distinguishes "binary missing" from "ran
// but capabilities incomplete" via err vs ProbeResult.Errors.
func TestProbe_BinaryMissing(t *testing.T) {
	t.Parallel()
	_, err := Probe(context.Background(), "/nonexistent/path/to/ffmpeg-impossible", "")
	require.Error(t, err)
}

// Probe against a real but non-FFmpeg binary must reject with a clear
// message — accepting anything that returns 0 would let operators point
// to /bin/true and break the server silently.
func TestProbe_NotFFmpegBinary(t *testing.T) {
	t.Parallel()
	echo, err := exec.LookPath("echo")
	if err != nil {
		t.Skip("echo not available")
	}
	_, perr := Probe(context.Background(), echo, "")
	require.Error(t, perr, "non-FFmpeg binary must be rejected")
	assert.Contains(t, perr.Error(), "not an FFmpeg binary")
}
