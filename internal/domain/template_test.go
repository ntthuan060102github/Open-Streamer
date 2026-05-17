package domain

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateTemplateCode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		wantErr string
	}{
		{"empty", "", "required"},
		{"whitespace only", "   ", "required"},
		{"alphanumeric ok", "profile_1", ""},
		{"dash ok", "profile-A", ""},
		{"slash rejected", "region/north", "must contain only"},
		{"space rejected", "profile A", "must contain only"},
		{"too long", strings.Repeat("a", MaxTemplateCodeLen+1), "exceeds max length"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTemplateCode(tc.in)
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			}
		})
	}
}

// ResolveStream: a stream with no Template reference must come back unchanged
// (same pointer) so callers can rely on identity to avoid unnecessary copies.
func TestResolveStream_NoTemplateReturnsSamePointer(t *testing.T) {
	t.Parallel()
	s := &Stream{Code: "live"}
	got := ResolveStream(s, &Template{Code: "profile-a", Transcoder: &TranscoderConfig{}})
	assert.Same(t, s, got, "stream with no Template reference must not be merged")
}

// Templates are nil → nothing to merge. Callers (handler, coordinator) rely on
// this so a missing/deleted template falls through to the raw stream.
func TestResolveStream_NilTemplateReturnsSamePointer(t *testing.T) {
	t.Parallel()
	code := TemplateCode("profile-a")
	s := &Stream{Code: "live", Template: &code}
	assert.Same(t, s, ResolveStream(s, nil))
}

// Pointer fields (Transcoder / DVR / Watermark / Thumbnail) follow the
// "nil = inherit" rule.
func TestResolveStream_InheritsPointerFieldsWhenNil(t *testing.T) {
	t.Parallel()
	code := TemplateCode("profile-a")
	tpl := &Template{
		Code:       "profile-a",
		Transcoder: &TranscoderConfig{Mode: TranscoderModeMulti},
		DVR:        &StreamDVRConfig{},
		Watermark:  &WatermarkConfig{},
		Thumbnail:  &ThumbnailConfig{},
	}
	s := &Stream{Code: "live", Template: &code}
	out := ResolveStream(s, tpl)

	require.NotSame(t, s, out, "merge must produce a copy so the persisted stream is not mutated")
	assert.Same(t, tpl.Transcoder, out.Transcoder)
	assert.Same(t, tpl.DVR, out.DVR)
	assert.Same(t, tpl.Watermark, out.Watermark)
	assert.Same(t, tpl.Thumbnail, out.Thumbnail)
	assert.Nil(t, s.Transcoder, "original stream must stay untouched")
}

// Non-nil stream values override the template even when the template would
// fill them in. The override rule is field-level: setting Transcoder on the
// stream does NOT cause DVR to also be taken from the stream.
func TestResolveStream_StreamPointerOverridesTemplate(t *testing.T) {
	t.Parallel()
	code := TemplateCode("profile-a")
	tplTc := &TranscoderConfig{Mode: TranscoderModeMulti}
	streamTc := &TranscoderConfig{Mode: TranscoderModePerProfile}
	tpl := &Template{
		Code:       "profile-a",
		Transcoder: tplTc,
		DVR:        &StreamDVRConfig{},
	}
	s := &Stream{Code: "live", Template: &code, Transcoder: streamTc}

	out := ResolveStream(s, tpl)
	assert.Same(t, streamTc, out.Transcoder, "stream's Transcoder wins")
	assert.Same(t, tpl.DVR, out.DVR, "untouched fields still inherit")
}

// Slice fields (Push) use len == 0 as the inherit signal.
func TestResolveStream_InheritsEmptySlices(t *testing.T) {
	t.Parallel()
	code := TemplateCode("profile-a")
	tpl := &Template{
		Code: "profile-a",
		Push: []PushDestination{{URL: "rtmp://example/a"}},
	}
	s := &Stream{Code: "live", Template: &code}
	out := ResolveStream(s, tpl)
	require.Len(t, out.Push, 1)
	assert.Equal(t, "rtmp://example/a", out.Push[0].URL)
}

func TestResolveStream_NonEmptyStreamSliceOverridesTemplate(t *testing.T) {
	t.Parallel()
	code := TemplateCode("profile-a")
	tpl := &Template{
		Code: "profile-a",
		Push: []PushDestination{{URL: "rtmp://example/a"}},
	}
	s := &Stream{
		Code:     "live",
		Template: &code,
		Push:     []PushDestination{{URL: "rtmp://example/b"}},
	}
	out := ResolveStream(s, tpl)
	require.Len(t, out.Push, 1)
	assert.Equal(t, "rtmp://example/b", out.Push[0].URL)
}

// Protocols is a struct, not a pointer; the inherit signal is the all-zero
// value. Any one bit set on the stream takes the whole struct.
func TestResolveStream_InheritsZeroProtocols(t *testing.T) {
	t.Parallel()
	code := TemplateCode("profile-a")
	tpl := &Template{Code: "profile-a", Protocols: OutputProtocols{HLS: true, DASH: true}}
	s := &Stream{Code: "live", Template: &code}
	out := ResolveStream(s, tpl)
	assert.Equal(t, OutputProtocols{HLS: true, DASH: true}, out.Protocols)
}

func TestResolveStream_PartialProtocolsOverrideTemplate(t *testing.T) {
	t.Parallel()
	code := TemplateCode("profile-a")
	tpl := &Template{Code: "profile-a", Protocols: OutputProtocols{HLS: true, DASH: true}}
	s := &Stream{
		Code:      "live",
		Template:  &code,
		Protocols: OutputProtocols{RTMP: true},
	}
	out := ResolveStream(s, tpl)
	assert.Equal(t, OutputProtocols{RTMP: true}, out.Protocols,
		"any flag set on the stream replaces the entire protocols struct")
}

// Identity fields (Code, Name, Description, Tags, StreamKey, Inputs,
// Disabled) live on the stream and must never be touched even when the
// template has a Name/Description of its own.
func TestResolveStream_IdentityFieldsAreNeverInherited(t *testing.T) {
	t.Parallel()
	code := TemplateCode("profile-a")
	tpl := &Template{
		Code:        "profile-a",
		Name:        "Template Name",
		Description: "Template Description",
	}
	s := &Stream{
		Code:        "live",
		Template:    &code,
		Name:        "Stream Name",
		Description: "Stream Description",
		Inputs:      []Input{{URL: "publish://"}},
	}
	out := ResolveStream(s, tpl)
	assert.Equal(t, "Stream Name", out.Name)
	assert.Equal(t, "Stream Description", out.Description)
	require.Len(t, out.Inputs, 1)
	assert.Equal(t, "publish://", out.Inputs[0].URL)
}
