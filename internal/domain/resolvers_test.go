package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// ResolveVideoEncoder must mirror transcoder.normalizeVideoEncoder
// exactly — any divergence makes FFmpeg use a different encoder than the
// transcoder's own logging / preview shows. Cases below pin every routing
// branch.
func TestResolveVideoEncoder(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		codec VideoCodec
		hw    HWAccel
		want  string
	}{
		{"empty + nvenc → h264_nvenc", "", HWAccelNVENC, "h264_nvenc"},
		{"empty + cpu → libx264", "", HWAccelNone, "libx264"},
		{"empty + vaapi → libx264 (no implicit routing)", "", HWAccelVAAPI, "libx264"},
		{"h264 + nvenc → h264_nvenc", "h264", HWAccelNVENC, "h264_nvenc"},
		{"avc + cpu → libx264", "avc", HWAccelNone, "libx264"},
		{"h265 + nvenc → hevc_nvenc", "h265", HWAccelNVENC, "hevc_nvenc"},
		{"hevc + cpu → libx265", "hevc", HWAccelNone, "libx265"},
		{"vp9 → libvpx-vp9", "vp9", HWAccelNone, "libvpx-vp9"},
		{"av1 → libsvtav1", "av1", HWAccelNone, "libsvtav1"},
		{"explicit h264_nvenc preserved", "h264_nvenc", HWAccelNone, "h264_nvenc"},
		{"explicit h264_qsv preserved", "h264_qsv", HWAccelNone, "h264_qsv"},
		{"unknown garbage → libx264 fallback", "garbage", HWAccelNone, "libx264"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, ResolveVideoEncoder(tt.codec, tt.hw))
		})
	}
}

func TestResolveAudioEncoder(t *testing.T) {
	t.Parallel()
	cases := []struct {
		codec AudioCodec
		want  string
	}{
		{"", "aac"},
		{AudioCodecCopy, "aac"},
		{AudioCodecAAC, "aac"},
		{AudioCodecMP2, "mp2"},
		{AudioCodecMP3, "libmp3lame"},
		{AudioCodecOpus, "libopus"},
		{AudioCodecAC3, "ac3"},
		{"unknown", "aac"},
	}
	for _, tt := range cases {
		t.Run(string(tt.codec), func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, ResolveAudioEncoder(tt.codec))
		})
	}
}

func TestResolveResizeMode(t *testing.T) {
	t.Parallel()
	assert.Equal(t, ResizeModePad, ResolveResizeMode(""))
	assert.Equal(t, ResizeModePad, ResolveResizeMode("garbage"))
	assert.Equal(t, ResizeModeCrop, ResolveResizeMode("crop"))
	assert.Equal(t, ResizeModeCrop, ResolveResizeMode("CROP"))
	assert.Equal(t, ResizeModeStretch, ResolveResizeMode("stretch"))
	assert.Equal(t, ResizeModeFit, ResolveResizeMode("fit"))
	assert.Equal(t, ResizeModePad, ResolveResizeMode("pad"))
}
