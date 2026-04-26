package transcoder

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Regression: a stream config with preset="veryfast" + HW=nvenc routed
// codec="" → encoder="h264_nvenc". Without normalization FFmpeg
// rejected `-preset veryfast` and the stream went down with 0 packets.
// Pin the exact crashing input here so it can never regress silently.
func TestNormalizePreset_VeryfastOnNVENC(t *testing.T) {
	t.Parallel()
	got := normalizePreset("veryfast", "h264_nvenc")
	assert.Equal(t, "p2", got, "libx264 'veryfast' must translate to NVENC 'p2'")
}

// libx264-style → NVENC translation matrix. Mapping is encoded in
// x264ToNvencPreset; this test pins the contract so future edits to
// the map don't silently change operator-visible semantics.
//
// Excludes "fast" / "medium" / "slow" — those overlap with NVENC's
// legacy aliases (still accepted natively), so normalizePreset returns
// them unchanged instead of translating to p-series. See the dedicated
// overlap test below.
func TestNormalizePreset_X264ToNVENC(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"ultrafast": "p1",
		"superfast": "p1",
		"veryfast":  "p2",
		"faster":    "p3",
		"slower":    "p6",
		"veryslow":  "p7",
		"placebo":   "p7",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, want, normalizePreset(in, "h264_nvenc"))
			assert.Equal(t, want, normalizePreset(in, "hevc_nvenc"),
				"hevc_nvenc shares the same preset namespace as h264_nvenc")
		})
	}
}

// fast/medium/slow are valid in BOTH libx264 and NVENC namespaces
// (NVENC keeps them as legacy aliases). They must pass through
// unchanged on either encoder — translating "fast" → "p3" on NVENC
// would silently change behaviour for operators who explicitly typed
// the legacy alias expecting native NVENC handling.
func TestNormalizePreset_OverlapNamesPassThrough(t *testing.T) {
	t.Parallel()
	for _, p := range []string{"fast", "medium", "slow"} {
		t.Run(p+"/nvenc", func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, p, normalizePreset(p, "h264_nvenc"))
		})
		t.Run(p+"/libx264", func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, p, normalizePreset(p, "libx264"))
		})
	}
}

// NVENC-native presets must pass through unchanged on NVENC encoders.
// Includes both the modern p-series and legacy aliases (some operators
// still use "hq" / "ll" / "fast").
func TestNormalizePreset_NVENCNative(t *testing.T) {
	t.Parallel()
	natives := []string{
		"p1", "p2", "p3", "p4", "p5", "p6", "p7",
		"hq", "hp", "ll", "llhq", "llhp", "lossless", "losslesshp",
		"slow", "medium", "fast",
	}
	for _, p := range natives {
		t.Run(p, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, p, normalizePreset(p, "h264_nvenc"))
		})
	}
}

// libx264 / libx265 / QSV all share the same preset namespace. Native
// values pass through unchanged.
func TestNormalizePreset_LibX264FamilyNative(t *testing.T) {
	t.Parallel()
	encoders := []string{"libx264", "libx265", "h264_qsv", "hevc_qsv"}
	natives := []string{"ultrafast", "veryfast", "fast", "medium", "slow", "veryslow"}
	for _, enc := range encoders {
		for _, p := range natives {
			t.Run(enc+"/"+p, func(t *testing.T) {
				t.Parallel()
				assert.Equal(t, p, normalizePreset(p, enc))
			})
		}
	}
}

// Reverse direction: operator types "p4" but encoder routed to libx264
// (HW=none, codec=""). Must translate so libx264 doesn't reject.
func TestNormalizePreset_NVENCToX264(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"p1": "ultrafast",
		"p2": "veryfast",
		"p3": "fast",
		"p4": "medium",
		"p5": "slow",
		"p6": "slower",
		"p7": "veryslow",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, want, normalizePreset(in, "libx264"))
			assert.Equal(t, want, normalizePreset(in, "libx265"))
			assert.Equal(t, want, normalizePreset(in, "h264_qsv"),
				"QSV inherits libx264 preset namespace")
		})
	}
}

// VAAPI uses `-compression_level`, not `-preset`. VideoToolbox has no
// preset analog. Both must drop the value (return "") so caller skips
// the `-preset` flag — emitting it would let FFmpeg reject the
// invocation just like the original NVENC/veryfast crash.
func TestNormalizePreset_BackendsWithoutPreset(t *testing.T) {
	t.Parallel()
	encoders := []string{
		"h264_vaapi", "hevc_vaapi",
		"h264_videotoolbox", "hevc_videotoolbox",
	}
	for _, enc := range encoders {
		// Both libx264 and NVENC style inputs must drop.
		for _, p := range []string{"medium", "veryfast", "p4", "p1"} {
			t.Run(enc+"/"+p, func(t *testing.T) {
				t.Parallel()
				assert.Empty(t, normalizePreset(p, enc),
					"%s must return empty preset (no -preset flag mechanism)", enc)
			})
		}
	}
}

// Empty preset must short-circuit to empty regardless of encoder —
// caller relies on this to know whether to emit the `-preset` flag.
func TestNormalizePreset_EmptyInputAlwaysSkips(t *testing.T) {
	t.Parallel()
	for _, enc := range []string{
		"h264_nvenc", "libx264", "h264_qsv", "h264_vaapi",
		"h264_videotoolbox", "unknown_encoder",
	} {
		assert.Empty(t, normalizePreset("", enc))
		assert.Empty(t, normalizePreset("   ", enc), "whitespace-only must also skip")
	}
}

// Garbage preset on a known encoder must drop — not pass through —
// because the encoder will reject and crash the stream.
func TestNormalizePreset_GarbageDropped(t *testing.T) {
	t.Parallel()
	cases := []struct {
		preset, encoder string
	}{
		{"not-a-real-preset", "h264_nvenc"},
		{"foobar", "libx264"},
		{"slowest", "h264_qsv"},
	}
	for _, tc := range cases {
		t.Run(tc.encoder+"/"+tc.preset, func(t *testing.T) {
			t.Parallel()
			assert.Empty(t, normalizePreset(tc.preset, tc.encoder),
				"unknown preset must be dropped, not passed through (would crash encoder)")
		})
	}
}

// Unknown encoder family (custom build, future codec) — pass through
// rather than silently drop the operator's value. Encoder will reject
// if invalid; we don't second-guess what we don't recognise.
func TestNormalizePreset_UnknownEncoderPassthrough(t *testing.T) {
	t.Parallel()
	got := normalizePreset("custom-preset", "experimental_codec")
	assert.Equal(t, "custom-preset", got,
		"unknown encoder must pass preset through unchanged")
}

// Case insensitivity: operators may type "VeryFast" or "P4" — must
// normalize to lowercase before lookup so casing doesn't crash the
// stream. ("MEDIUM" stays as native "medium" on NVENC because of the
// legacy alias overlap — see TestNormalizePreset_OverlapNamesPassThrough.)
func TestNormalizePreset_CaseInsensitive(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "p2", normalizePreset("VeryFast", "h264_nvenc"))
	assert.Equal(t, "p1", normalizePreset("ULTRAFAST", "h264_nvenc"))
	assert.Equal(t, "medium", normalizePreset("P4", "libx264"))
	assert.Equal(t, "veryfast", normalizePreset("P2", "libx264"))
}
