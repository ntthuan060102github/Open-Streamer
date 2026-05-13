package transcoder

import (
	"testing"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sarBSFArg picks the encoder-appropriate BSF when present.
func TestSarBSFArg_PicksRightBSFPerEncoder(t *testing.T) {
	t.Parallel()
	all := map[string]bool{"h264_metadata": true, "hevc_metadata": true}

	tests := []struct {
		name       string
		encoder    string
		wantPrefix string
	}{
		{"h264_nvenc routes to h264_metadata", "h264_nvenc", "h264_metadata="},
		{"libx264 routes to h264_metadata", "libx264", "h264_metadata="},
		{"hevc_nvenc routes to hevc_metadata", "hevc_nvenc", "hevc_metadata="},
		{"libx265 routes to hevc_metadata", "libx265", "hevc_metadata="},
		{"h265_videotoolbox routes to hevc_metadata", "h265_videotoolbox", "hevc_metadata="},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := sarBSFArg("1:1", tc.encoder, all)
			require.True(t, ok)
			assert.Contains(t, got, tc.wantPrefix)
			assert.Contains(t, got, "sample_aspect_ratio=1/1", "translates W:H to W/N")
		})
	}
}

// Without the relevant BSF flag the helper returns ok=false so the
// caller falls back to a setsar entry in the -vf chain.
func TestSarBSFArg_FallsBackWhenBSFMissing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		encoder string
		bsfs    map[string]bool
	}{
		{"H.264 encoder, no h264_metadata", "h264_nvenc", nil},
		{"H.264 encoder, only hevc_metadata", "h264_nvenc", map[string]bool{"hevc_metadata": true}},
		{"H.265 encoder, no hevc_metadata", "libx265", map[string]bool{"h264_metadata": true}},
		{"non H.264/265 encoder", "libvpx-vp9", map[string]bool{"h264_metadata": true, "hevc_metadata": true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := sarBSFArg("1:1", tc.encoder, tc.bsfs)
			assert.False(t, ok)
		})
	}
}

// Empty SAR is always a no-op regardless of probe state.
func TestSarBSFArg_EmptySAR(t *testing.T) {
	t.Parallel()
	_, ok := sarBSFArg("", "h264_nvenc", map[string]bool{"h264_metadata": true})
	assert.False(t, ok)
}

// Multi-component SAR ("8:9", "32:27") gets translated correctly to
// h264_metadata's W/N syntax.
func TestSarBSFArg_NonSquareRatios(t *testing.T) {
	t.Parallel()
	got, ok := sarBSFArg("8:9", "h264_nvenc", map[string]bool{"h264_metadata": true})
	require.True(t, ok)
	assert.Equal(t, "h264_metadata=sample_aspect_ratio=8/9", got)
}

// When the BSF path is enabled, buildVideoFilter does NOT add setsar to
// the -vf chain — that's what saves the GPU↔CPU round-trip. The caller
// emits -bsf:v separately.
func TestBuildVideoFilter_OmitsSetsarWhenBSFAvailable(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNVENC}}
	p := Profile{Width: 1280, Height: 720, SAR: "1:1"}

	bsfsEnabled := map[string]bool{"h264_metadata": true}
	vf := buildVideoFilter(p, tc, "h264_nvenc", bsfsEnabled)
	assert.NotContains(t, vf, "setsar=", "BSF path should suppress setsar in -vf")
}

// Without the relevant BSF, buildVideoFilter falls back to the legacy
// setsar entry so SAR is still applied (just at higher GPU cost).
func TestBuildVideoFilter_KeepsSetsarFallbackWhenBSFMissing(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNVENC}}
	p := Profile{Width: 1280, Height: 720, SAR: "1:1"}

	vf := buildVideoFilter(p, tc, "h264_nvenc", nil)
	assert.Contains(t, vf, "setsar=1:1", "fallback path keeps setsar so SAR still applies")
}

// buildFFmpegArgs and buildMultiOutputArgs emit -bsf:v when the relevant
// BSF is in the probe set. The flag goes after -c:v / -c:v:0 so FFmpeg
// associates it with the correct stream.
func TestBuildFFmpegArgs_EmitsBSFFlagWhenAvailable(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNVENC}}
	prof := Profile{Width: 1280, Height: 720, Bitrate: "1500k", Codec: "h264", SAR: "1:1"}

	args, err := buildFFmpegArgs([]Profile{prof}, tc, map[string]bool{"h264_metadata": true})
	require.NoError(t, err)

	require.Contains(t, args, "-bsf:v")
	bsfVal, ok := readFlagValue(args, "-bsf:v")
	require.True(t, ok)
	assert.Contains(t, bsfVal, "h264_metadata=sample_aspect_ratio=1/1")
}

func TestBuildMultiOutputArgs_EmitsBSFFlagPerOutput(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNVENC}}
	profiles := []Profile{
		{Width: 1920, Height: 1080, Bitrate: "4500k", Codec: "h264", SAR: "1:1"},
		{Width: 1280, Height: 720, Bitrate: "2500k", Codec: "h264", SAR: "1:1"},
	}

	args, err := buildMultiOutputArgs(profiles, tc, map[string]bool{"h264_metadata": true})
	require.NoError(t, err)

	count := 0
	for i, a := range args {
		if a == "-bsf:v:0" && i+1 < len(args) && a != "" {
			if got := args[i+1]; got == "h264_metadata=sample_aspect_ratio=1/1" {
				count++
			}
		}
	}
	assert.Equal(t, 2, count, "expected -bsf:v:0 with SAR on each of the 2 outputs")
}

// bsfMapFromProbe defensively copies; mutating the returned map must not
// reach back into the Service's own state.
func TestBsfMapFromProbe_DefensiveCopy(t *testing.T) {
	t.Parallel()
	src := &ProbeResult{BSFs: map[string]bool{"h264_metadata": true}}
	out := bsfMapFromProbe(src)
	require.True(t, out["h264_metadata"])

	out["h264_metadata"] = false
	out["fake"] = true
	assert.True(t, src.BSFs["h264_metadata"], "source map must not see caller mutation")
	assert.False(t, src.BSFs["fake"])
}

// nil ProbeResult yields an empty but non-nil map so callers can index
// without nil-checks.
func TestBsfMapFromProbe_NilProbe(t *testing.T) {
	t.Parallel()
	out := bsfMapFromProbe(nil)
	require.NotNil(t, out)
	assert.Empty(t, out)
	assert.False(t, out["h264_metadata"], "missing keys read false from nil-input map")
}
