package transcoder

import (
	"strings"
	"testing"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// testBaseChain is the placeholder scale chain the watermark tests prepend
// to. Kept identical across cases so changes in chain wrapping show up in
// diff rather than via duplicated literals.
const testBaseChain = "scale=1280:720"

// noResizeScale is the frameScale value the legacy tests pass when they
// don't care about the reference-frame model — equivalent to "this profile
// IS the reference (largest) profile". With Resize=false anywhere in the
// config the scale value is ignored anyway.
const noResizeScale = 1.0

// Test fixtures shared across cases so the same literal can't drift.
const (
	testTmpLogo        = "/tmp/logo.png"
	testImgLogo        = "/img/logo.png"
	testScaleIWFactor  = "scale=iw*"
	test720pScaleChain = "scale=iw*0.6667:ih*0.6667"
)

// TestApplyImageWatermarkResizeBelowReference verifies that a smaller
// profile (frameScale<1) gets a `scale=iw*f:ih*f` filter inserted into the
// movie chain — that's the per-rendition shrink the reference-frame sizing
// model relies on.
func TestApplyImageWatermarkResizeBelowReference(t *testing.T) {
	base := testBaseChain + ",setsar=1"
	wm := &domain.WatermarkConfig{
		Enabled:   true,
		Type:      domain.WatermarkTypeImage,
		ImagePath: testTmpLogo,
		Position:  domain.WatermarkTopRight,
		Resize:    true,
	}
	got := applyWatermark(base, wm.Resolved(), false, 0.6667) // 720p relative to 1080p

	if !strings.Contains(got, test720pScaleChain) {
		t.Fatalf("expected scale=iw*0.6667:ih*0.6667 in movie chain, got: %s", got)
	}
}

// TestApplyImageWatermarkResizeReferenceProfile asserts that the largest
// profile (frameScale=1.0) skips the scale filter entirely — its watermark
// renders at the asset's native pixel size.
func TestApplyImageWatermarkResizeReferenceProfile(t *testing.T) {
	base := testBaseChain
	wm := &domain.WatermarkConfig{
		Enabled:   true,
		Type:      domain.WatermarkTypeImage,
		ImagePath: testTmpLogo,
		Position:  domain.WatermarkTopRight,
		Resize:    true,
	}
	got := applyWatermark(base, wm.Resolved(), false, 1.0)

	if strings.Contains(got, testScaleIWFactor) {
		t.Fatalf("largest profile (frameScale=1.0) must skip movie-chain scale, got: %s", got)
	}
}

// TestApplyImageWatermarkResizeDisabled confirms Resize=false leaves the
// movie chain untouched even on smaller profiles — operators who want a
// fixed-pixel watermark across the ladder keep that behaviour.
func TestApplyImageWatermarkResizeDisabled(t *testing.T) {
	base := testBaseChain
	wm := &domain.WatermarkConfig{
		Enabled:   true,
		Type:      domain.WatermarkTypeImage,
		ImagePath: testTmpLogo,
		Position:  domain.WatermarkTopRight,
		Resize:    false,
	}
	got := applyWatermark(base, wm.Resolved(), false, 0.5)

	if strings.Contains(got, testScaleIWFactor) {
		t.Fatalf("Resize=false must skip movie-chain scale regardless of frameScale, got: %s", got)
	}
}

// TestApplyTextWatermarkResizeScalesFontAndOffsets — text path: FontSize
// and offsets shrink by frameScale; relative on-screen ratio stays put.
func TestApplyTextWatermarkResizeScalesFontAndOffsets(t *testing.T) {
	base := testBaseChain
	wm := &domain.WatermarkConfig{
		Enabled:  true,
		Type:     domain.WatermarkTypeText,
		Text:     "LIVE",
		FontSize: 48,
		Position: domain.WatermarkTopRight,
		OffsetX:  20,
		OffsetY:  30,
		Resize:   true,
	}
	// 720p (frameScale ≈ 0.667) should produce fontsize=32, offset_x=13, offset_y=20.
	got := applyWatermark(base, wm.Resolved(), false, 0.6667)

	if !strings.Contains(got, "fontsize=32") {
		t.Fatalf("FontSize 48*0.6667 ≈ 32, got: %s", got)
	}
	if !strings.Contains(got, "x='w-tw-13'") {
		t.Fatalf("OffsetX 20*0.6667 ≈ 13, got: %s", got)
	}
	if !strings.Contains(got, "y='20'") {
		t.Fatalf("OffsetY 30*0.6667 ≈ 20, got: %s", got)
	}
}

// TestApplyTextWatermarkResizeReferenceProfile — frameScale=1.0 keeps the
// operator's chosen FontSize and offsets verbatim.
func TestApplyTextWatermarkResizeReferenceProfile(t *testing.T) {
	base := testBaseChain
	wm := &domain.WatermarkConfig{
		Enabled:  true,
		Type:     domain.WatermarkTypeText,
		Text:     "LIVE",
		FontSize: 24,
		Position: domain.WatermarkTopRight,
		OffsetX:  10,
		OffsetY:  10,
		Resize:   true,
	}
	got := applyWatermark(base, wm.Resolved(), false, 1.0)
	if !strings.Contains(got, "fontsize=24") {
		t.Fatalf("reference profile keeps native FontSize, got: %s", got)
	}
	if !strings.Contains(got, "x='w-tw-10'") {
		t.Fatalf("reference profile keeps native OffsetX, got: %s", got)
	}
}

func TestApplyWatermarkInactivePassthrough(t *testing.T) {
	base := "scale=1920:1080,setsar=1"
	cases := []*domain.WatermarkConfig{
		nil,
		{Enabled: false, Type: domain.WatermarkTypeText, Text: "x"},
		{Enabled: true, Type: domain.WatermarkTypeText, Text: "  "},
	}
	for i, wm := range cases {
		got := applyWatermark(base, wm, false, noResizeScale)
		if got != base {
			t.Errorf("case %d: chain mutated when watermark inactive: %s", i, got)
		}
	}
}

func TestApplyTextWatermarkCPU(t *testing.T) {
	base := "scale=1920:1080"
	wm := &domain.WatermarkConfig{
		Enabled:   true,
		Type:      domain.WatermarkTypeText,
		Text:      "LIVE",
		FontSize:  32,
		FontColor: "yellow",
		Opacity:   0.8,
		Position:  domain.WatermarkTopRight,
		OffsetX:   20, OffsetY: 30,
	}
	got := applyWatermark(base, wm, false, noResizeScale)

	for _, frag := range []string{
		"scale=1920:1080,",
		"drawtext=text='LIVE'",
		"fontsize=32",
		"fontcolor='yellow@0.80'",
		"x='w-tw-20'",
		"y='30'",
	} {
		if !strings.Contains(got, frag) {
			t.Errorf("missing fragment %q in %q", frag, got)
		}
	}
	if strings.Contains(got, "hwdownload") || strings.Contains(got, "hwupload_cuda") {
		t.Errorf("CPU pipeline should not insert HW shuffle: %s", got)
	}
}

func TestApplyTextWatermarkGPU(t *testing.T) {
	base := "scale_cuda=1280:720"
	wm := &domain.WatermarkConfig{
		Enabled: true, Type: domain.WatermarkTypeText, Text: "LIVE",
	}
	got := applyWatermark(base, wm, true, noResizeScale)

	for _, frag := range []string{
		"scale_cuda=1280:720,",
		"hwdownload",
		"format=nv12",
		"drawtext=",
		"hwupload_cuda",
	} {
		if !strings.Contains(got, frag) {
			t.Errorf("missing fragment %q in %q", frag, got)
		}
	}
	// The hwdownload must precede drawtext and hwupload_cuda must follow it.
	dl := strings.Index(got, "hwdownload")
	dt := strings.Index(got, "drawtext")
	up := strings.Index(got, "hwupload_cuda")
	if dl >= dt || dt >= up {
		t.Errorf("expected hwdownload < drawtext < hwupload_cuda, got %d %d %d", dl, dt, up)
	}
}

func TestApplyImageWatermarkCPU(t *testing.T) {
	base := "scale=1920:1080,setsar=1"
	wm := &domain.WatermarkConfig{
		Enabled:   true,
		Type:      domain.WatermarkTypeImage,
		ImagePath: "/var/lib/open-streamer/wm/logo.png",
		Opacity:   0.5,
		Position:  domain.WatermarkBottomLeft,
		OffsetX:   25, OffsetY: 25,
	}
	got := applyWatermark(base, wm, false, noResizeScale)

	// Three-segment graph: <base>[mid]; movie=...,format=rgba,colorchannelmixer[wm]; [mid][wm]overlay=...
	segs := strings.Split(got, ";")
	if len(segs) != 3 {
		t.Fatalf("expected 3 segments, got %d: %s", len(segs), got)
	}
	if !strings.HasSuffix(segs[0], "[mid]") {
		t.Errorf("seg0 missing [mid] label: %q", segs[0])
	}
	for _, frag := range []string{"movie='/var/lib/open-streamer/wm/logo.png'", "format=rgba", "colorchannelmixer=aa=0.50", "[wm]"} {
		if !strings.Contains(segs[1], frag) {
			t.Errorf("seg1 missing %q: %q", frag, segs[1])
		}
	}
	for _, frag := range []string{"[mid][wm]overlay=", "x='25'", "y='H-h-25'"} {
		if !strings.Contains(segs[2], frag) {
			t.Errorf("seg2 missing %q: %q", frag, segs[2])
		}
	}
}

func TestApplyImageWatermarkGPU(t *testing.T) {
	base := "scale_cuda=1920:1080"
	wm := &domain.WatermarkConfig{
		Enabled: true, Type: domain.WatermarkTypeImage,
		ImagePath: testImgLogo, Opacity: 1.0, Position: domain.WatermarkCenter,
	}
	got := applyWatermark(base, wm, true, noResizeScale)

	// hwdownload joins seg0 (before [mid]), hwupload_cuda joins seg2 (after overlay).
	if !strings.Contains(got, "hwdownload,format=nv12[mid]") {
		t.Errorf("seg0 should end with hwdownload,format=nv12[mid]: %q", got)
	}
	if !strings.HasSuffix(got, "hwupload_cuda") {
		t.Errorf("expected GPU graph to end with hwupload_cuda: %q", got)
	}
	// Opacity=1.0 → no colorchannelmixer needed (movie's alpha used as-is).
	if strings.Contains(got, "colorchannelmixer") {
		t.Errorf("opaque watermark should skip colorchannelmixer: %q", got)
	}
	// Center coords use W/w / H/h (overlay context), not w/h alone.
	if !strings.Contains(got, "x='(W-w)/2'") || !strings.Contains(got, "y='(H-h)/2'") {
		t.Errorf("center coords missing or wrong: %q", got)
	}
}

func TestApplyTextWatermarkCustomPosition(t *testing.T) {
	wm := &domain.WatermarkConfig{
		Enabled: true, Type: domain.WatermarkTypeText, Text: "x",
		Position: domain.WatermarkCustom,
		X:        "main_w-overlay_w-50", Y: "if(gt(t,5),10,-100)",
	}
	got := applyWatermark(testBaseChain, wm, false, noResizeScale)
	if !strings.Contains(got, "x='main_w-overlay_w-50'") {
		t.Errorf("custom x not forwarded: %q", got)
	}
	if !strings.Contains(got, "y='if(gt(t,5),10,-100)'") {
		t.Errorf("custom y not forwarded: %q", got)
	}
}

func TestApplyImageWatermarkCustomPosition(t *testing.T) {
	wm := &domain.WatermarkConfig{
		Enabled: true, Type: domain.WatermarkTypeImage, ImagePath: testImgLogo,
		Position: domain.WatermarkCustom, X: "100", Y: "200", Opacity: 1,
	}
	got := applyWatermark("scale=1920:1080", wm, false, noResizeScale)
	if !strings.Contains(got, "x='100'") || !strings.Contains(got, "y='200'") {
		t.Errorf("custom coords not in overlay: %q", got)
	}
}

func TestDefaultExprFallbackToZero(t *testing.T) {
	wm := &domain.WatermarkConfig{
		Enabled: true, Type: domain.WatermarkTypeText, Text: "x",
		Position: domain.WatermarkCustom,
		X:        "  ", // blank → "0"
	}
	got := applyWatermark("", wm, false, noResizeScale)
	if !strings.Contains(got, "x='0'") {
		t.Errorf("blank custom x should default to 0: %q", got)
	}
}

func TestDrawTextCoordsAllPositions(t *testing.T) {
	cases := map[domain.WatermarkPosition]struct {
		x, y string
	}{
		domain.WatermarkTopLeft:     {"10", "10"},
		domain.WatermarkTopRight:    {"w-tw-10", "10"},
		domain.WatermarkBottomLeft:  {"10", "h-th-10"},
		domain.WatermarkBottomRight: {"w-tw-10", "h-th-10"},
		domain.WatermarkCenter:      {"(w-tw)/2", "(h-th)/2"},
	}
	for pos, want := range cases {
		x, y := drawTextCoords(pos, 10, 10)
		if x != want.x || y != want.y {
			t.Errorf("%s: got (%s,%s), want (%s,%s)", pos, x, y, want.x, want.y)
		}
	}
}

func TestFFmpegQuoteEscapesSingleQuote(t *testing.T) {
	if got := ffmpegQuote("Bob's Show"); got != `'Bob'\''s Show'` {
		t.Errorf("got %q", got)
	}
	if got := ffmpegQuote("LIVE %{localtime:%H}"); got != `'LIVE %{localtime:%H}'` {
		t.Errorf("strftime body should pass through unescaped: %q", got)
	}
}

// TestApplyWatermarkEmptyBase covers the corner case where there is no
// scale chain (e.g. profile with width/height=0 and no resize): the output
// should still produce a valid filter graph using just the watermark.
func TestApplyWatermarkEmptyBase(t *testing.T) {
	wm := &domain.WatermarkConfig{
		Enabled: true, Type: domain.WatermarkTypeText, Text: "TAG",
	}
	got := applyWatermark("", wm, false, noResizeScale)
	if !strings.HasPrefix(got, "drawtext=") {
		t.Errorf("empty base should start with drawtext, got %q", got)
	}
}

// TestComputeWatermarkFrameScale covers reference-profile detection across
// the ladder: largest profile gets 1.0, smaller profiles get their proportional
// shrink, and degenerate cases (single profile, zero widths) fall back to 1.0.
func TestComputeWatermarkFrameScale(t *testing.T) {
	ladder := []domain.VideoProfile{
		{Width: 1920, Height: 1080},
		{Width: 1280, Height: 720},
		{Width: 854, Height: 480},
	}
	cases := map[string]struct {
		profileWidth int
		want         float64
	}{
		"largest":      {1920, 1.0},
		"720p ratio":   {1280, 1280.0 / 1920.0},
		"480p ratio":   {854, 854.0 / 1920.0},
		"unknown size": {0, 1.0},
	}
	for name, c := range cases {
		got := computeWatermarkFrameScale(c.profileWidth, ladder)
		if got != c.want {
			t.Errorf("%s: got %v, want %v", name, got, c.want)
		}
	}
}

// TestComputeWatermarkFrameScaleDegenerateLadders covers the edge cases
// where a sensible reference can't be picked.
func TestComputeWatermarkFrameScaleDegenerateLadders(t *testing.T) {
	cases := map[string]struct {
		ladder []domain.VideoProfile
		want   float64
	}{
		"empty":      {nil, 1.0},
		"all zero":   {[]domain.VideoProfile{{}, {}}, 1.0},
		"single fix": {[]domain.VideoProfile{{Width: 1280}}, 1.0},
	}
	for name, c := range cases {
		got := computeWatermarkFrameScale(1280, c.ladder)
		if got != c.want {
			t.Errorf("%s: got %v, want %v", name, got, c.want)
		}
	}
}

// TestBuildFFmpegArgsAppliesWatermark — end-to-end through buildFFmpegArgs:
// confirms the watermark survives composition with the rest of the args
// (single-output, legacy mode).
func TestBuildFFmpegArgsAppliesWatermark(t *testing.T) {
	tc := &domain.TranscoderConfig{
		Watermark: &domain.WatermarkConfig{
			Enabled: true, Type: domain.WatermarkTypeText, Text: "WM",
		},
	}
	args, err := buildFFmpegArgs([]Profile{{Width: 1280, Height: 720, Bitrate: "1500k"}}, tc, nil)
	if err != nil {
		t.Fatal(err)
	}
	vf, ok := readFlagValue(args, "-vf")
	if !ok {
		t.Fatal("no -vf in args")
	}
	if !strings.Contains(vf, "drawtext=text='WM'") {
		t.Errorf("watermark not in -vf: %s", vf)
	}
}

// TestBuildMultiOutputArgsAppliesWatermarkPerOutput — confirms each output
// in multi-output mode gets the watermark filter on its own -vf:v:0 chain.
func TestBuildMultiOutputArgsAppliesWatermarkPerOutput(t *testing.T) {
	tc := &domain.TranscoderConfig{
		Watermark: &domain.WatermarkConfig{
			Enabled: true, Type: domain.WatermarkTypeText, Text: "WM",
		},
	}
	args, err := buildMultiOutputArgs([]Profile{
		{Width: 1920, Height: 1080, Bitrate: "4500k"},
		{Width: 1280, Height: 720, Bitrate: "2500k"},
	}, tc, nil)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for i, a := range args {
		if a == "-vf:v:0" && i+1 < len(args) && strings.Contains(args[i+1], "drawtext=text='WM'") {
			count++
		}
	}
	if count != 2 {
		t.Errorf("expected watermark on 2 outputs, got %d", count)
	}
}

// TestBuildMultiOutputArgsResizesPerOutput verifies the reference-frame
// model wires correctly through both transcode modes: in multi-output mode,
// the smaller profile's -vf:v:0 chain must include scale=iw*<f>:ih*<f> while
// the largest profile's chain must NOT (it's the reference).
func TestBuildMultiOutputArgsResizesPerOutput(t *testing.T) {
	tc := &domain.TranscoderConfig{
		Watermark: &domain.WatermarkConfig{
			Enabled: true, Type: domain.WatermarkTypeImage,
			ImagePath: testImgLogo, Position: domain.WatermarkTopRight, Resize: true,
		},
		Video: domain.VideoTranscodeConfig{
			Profiles: []domain.VideoProfile{
				{Width: 1920, Height: 1080},
				{Width: 1280, Height: 720},
			},
		},
	}
	args, err := buildMultiOutputArgs([]Profile{
		{Width: 1920, Height: 1080, Bitrate: "4500k"},
		{Width: 1280, Height: 720, Bitrate: "2500k"},
	}, tc, nil)
	if err != nil {
		t.Fatal(err)
	}

	var refChain, smallerChain string
	seen := 0
	for i, a := range args {
		if a == "-vf:v:0" && i+1 < len(args) {
			if seen == 0 {
				refChain = args[i+1]
			} else {
				smallerChain = args[i+1]
			}
			seen++
		}
	}
	if seen != 2 {
		t.Fatalf("expected 2 -vf:v:0 chains, got %d", seen)
	}

	if strings.Contains(refChain, testScaleIWFactor) {
		t.Errorf("reference profile (1080p) must NOT carry movie-chain scale: %s", refChain)
	}
	if !strings.Contains(smallerChain, test720pScaleChain) {
		t.Errorf("smaller profile (720p) should carry scale=iw*0.6667:ih*0.6667, got: %s", smallerChain)
	}
}

// TestBuildFFmpegArgsResizesAgainstLadder mirrors the multi-output test
// but for legacy single-output mode: each ffmpeg subprocess gets a single
// profile but the frame-scale calculation should still consult the full
// ladder (tc.Video.Profiles) so the smaller profile shrinks correctly.
func TestBuildFFmpegArgsResizesAgainstLadder(t *testing.T) {
	tc := &domain.TranscoderConfig{
		Watermark: &domain.WatermarkConfig{
			Enabled: true, Type: domain.WatermarkTypeImage,
			ImagePath: testImgLogo, Position: domain.WatermarkTopRight, Resize: true,
		},
		Video: domain.VideoTranscodeConfig{
			Profiles: []domain.VideoProfile{
				{Width: 1920, Height: 1080},
				{Width: 1280, Height: 720},
			},
		},
	}
	// Building args for the SMALLER profile (single-output ffmpeg per profile).
	args, err := buildFFmpegArgs([]Profile{{Width: 1280, Height: 720, Bitrate: "2500k"}}, tc, nil)
	if err != nil {
		t.Fatal(err)
	}
	vf, ok := readFlagValue(args, "-vf")
	if !ok {
		t.Fatal("no -vf in args")
	}
	if !strings.Contains(vf, test720pScaleChain) {
		t.Errorf("legacy mode smaller profile should carry scale=iw*0.6667:ih*0.6667, got: %s", vf)
	}
}

// readFlagValue scans an FFmpeg argv for `flag <value>` and returns value.
func readFlagValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}
