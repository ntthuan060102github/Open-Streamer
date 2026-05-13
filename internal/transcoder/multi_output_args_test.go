package transcoder

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// Multi-output mode emits one -f mpegts pipe:N per profile (N starts at 3
// because fd 0/1/2 are stdin/stdout/stderr). Verifies the full ladder
// produces the expected pipe count + correct numbering.
func TestBuildMultiOutputArgs_PipeNumbering(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Audio:  domain.AudioTranscodeConfig{Copy: true},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNVENC},
	}
	profiles := []Profile{
		{Width: 1280, Height: 720, Bitrate: "2500k", Codec: "h264"},
		{Width: 854, Height: 480, Bitrate: "1200k", Codec: "h264"},
		{Width: 640, Height: 360, Bitrate: "800k", Codec: "h264"},
	}
	args, err := buildMultiOutputArgs(profiles, tc, nil)
	require.NoError(t, err)

	joined := strings.Join(args, " ")
	assert.Contains(t, joined, "pipe:3", "first output pipe")
	assert.Contains(t, joined, "pipe:4", "second output pipe")
	assert.Contains(t, joined, "pipe:5", "third output pipe")
	assert.NotContains(t, joined, "pipe:6", "no extra pipe for 3-profile ladder")

	// Single shared input on stdin.
	pipe0Count := strings.Count(joined, "-i pipe:0")
	assert.Equal(t, 1, pipe0Count, "exactly one -i (shared decode)")

	// One -f mpegts per output.
	mpegtsOutputs := strings.Count(joined, "-f mpegts")
	// One for the input (-f mpegts -i pipe:0) + one per output pipe.
	assert.Equal(t, 1+len(profiles), mpegtsOutputs)
}

// Per-profile bitrate / scale must appear with the matching output. We
// assert the bitrates are present and ordered correctly relative to the
// output pipes (FFmpeg applies un-suffixed flags to the next-defined
// output, so order matters).
func TestBuildMultiOutputArgs_PerProfileBitrate(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Audio:  domain.AudioTranscodeConfig{Copy: true},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNVENC},
	}
	profiles := []Profile{
		{Width: 1280, Height: 720, Bitrate: "2500k", Codec: "h264"},
		{Width: 854, Height: 480, Bitrate: "1200k", Codec: "h264"},
	}
	args, err := buildMultiOutputArgs(profiles, tc, nil)
	require.NoError(t, err)

	joined := strings.Join(args, " ")
	// 2500k must appear before pipe:3, 1200k must appear before pipe:4.
	idx2500 := strings.Index(joined, "2500k")
	idxPipe3 := strings.Index(joined, "pipe:3")
	idx1200 := strings.Index(joined, "1200k")
	idxPipe4 := strings.Index(joined, "pipe:4")
	require.Greater(t, idx2500, -1)
	require.Greater(t, idxPipe3, -1)
	require.Greater(t, idx1200, -1)
	require.Greater(t, idxPipe4, -1)
	assert.Less(t, idx2500, idxPipe3, "first profile bitrate must precede first output target")
	assert.Less(t, idx1200, idxPipe4, "second profile bitrate must precede second output target")
	assert.Less(t, idxPipe3, idx1200, "first output target must precede second profile bitrate")
}

// Multi-output requires per-profile scale to land on a pure-GPU filter
// chain when nvenc is selected (no CPU round-trip).
func TestBuildMultiOutputArgs_NVENCUsesScaleCuda(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Audio:  domain.AudioTranscodeConfig{Copy: true},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNVENC},
	}
	profiles := []Profile{
		{Width: 1280, Height: 720, Bitrate: "2500k", Codec: "h264"},
	}
	args, err := buildMultiOutputArgs(profiles, tc, nil)
	require.NoError(t, err)

	joined := strings.Join(args, " ")
	assert.Contains(t, joined, "scale_cuda=1280:720", "must use GPU scaler")
	assert.NotContains(t, joined, "hwdownload", "no CPU round-trip allowed")
	assert.Contains(t, joined, "h264_nvenc")
	assert.Contains(t, joined, "-hwaccel cuda")
}

func TestBuildMultiOutputArgs_RejectVideoCopy(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video: domain.VideoTranscodeConfig{Copy: true},
	}
	profiles := []Profile{{Width: 1280, Height: 720, Bitrate: "2500k"}}
	_, err := buildMultiOutputArgs(profiles, tc, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "video.copy=true")
}

func TestBuildMultiOutputArgs_RejectEmpty(t *testing.T) {
	t.Parallel()
	_, err := buildMultiOutputArgs(nil, &domain.TranscoderConfig{}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no video profiles")
}

// Single-profile case under multi-output mode produces the same shape as
// multi-profile (same arg order, just one output) — caller is free to use
// multi-output for any N≥1 without special-casing.
func TestBuildMultiOutputArgs_SingleProfileWorks(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Audio:  domain.AudioTranscodeConfig{Copy: true},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNVENC},
	}
	profiles := []Profile{
		{Width: 1920, Height: 1080, Bitrate: "5000k", Codec: "h264"},
	}
	args, err := buildMultiOutputArgs(profiles, tc, nil)
	require.NoError(t, err)

	joined := strings.Join(args, " ")
	assert.Contains(t, joined, "pipe:3")
	assert.NotContains(t, joined, "pipe:4")
	assert.Contains(t, joined, "5000k")
	assert.Contains(t, joined, "scale_cuda=1920:1080")
}
