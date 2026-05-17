package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// MaxTemplateCodeLen is the maximum length of a template code.
const MaxTemplateCodeLen = 128

// templateCodePattern allows alphanumerics, dash, and underscore. Unlike
// stream codes, templates have no `/` namespacing — they live in a flat
// namespace because they are referenced by streams (Stream.Template), not
// mounted on a media URL path.
var templateCodePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// TemplateCode is the user-assigned primary key for a template.
// Allowed characters: a-z, A-Z, 0-9, underscore, dash.
type TemplateCode string

// ValidateTemplateCode reports whether s is a non-empty valid template code.
func ValidateTemplateCode(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return errors.New("template code is required")
	}
	if len(s) > MaxTemplateCodeLen {
		return fmt.Errorf("template code exceeds max length %d", MaxTemplateCodeLen)
	}
	if !templateCodePattern.MatchString(s) {
		return errors.New("template code must contain only A-Z, a-z, 0-9, '_', and '-'")
	}
	return nil
}

// Template is a reusable bundle of stream configuration that can be
// inherited by multiple streams. Only "config-like" fields live here —
// per-stream identity (Code, Name, Description, Tags, StreamKey, Inputs,
// Disabled) always stays on the stream itself. A stream references at
// most one template via Stream.Template; for every config-like field
// the stream's own value wins when set, otherwise the template's value
// is used (see ResolveStream).
type Template struct {
	// Code is the unique key chosen by the operator.
	Code TemplateCode `json:"code" yaml:"code"`

	// Name and Description are template-level metadata. They describe the
	// template to operators and are NOT propagated into streams that
	// inherit from it (those keep their own Name / Description fields).
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description" yaml:"description"`

	// Transcoder controls encoding/decoding settings. nil means no
	// transcoding for streams inheriting this template (unless they set
	// their own Transcoder).
	Transcoder *TranscoderConfig `json:"transcoder,omitempty" yaml:"transcoder,omitempty"`

	// Protocols defines which delivery protocols are opened.
	Protocols OutputProtocols `json:"protocols" yaml:"protocols"`

	// Push is the list of external destinations the server actively pushes to.
	Push []PushDestination `json:"push" yaml:"push"`

	// DVR overrides the global DVR settings. Streams that inherit this
	// template inherit the DVR configuration unless they set their own.
	DVR *StreamDVRConfig `json:"dvr,omitempty" yaml:"dvr,omitempty"`

	// Watermark is an optional text or image overlay applied before encoding.
	Watermark *WatermarkConfig `json:"watermark,omitempty" yaml:"watermark,omitempty"`

	// Thumbnail controls periodic screenshot generation.
	Thumbnail *ThumbnailConfig `json:"thumbnail,omitempty" yaml:"thumbnail,omitempty"`
}

// ResolveStream returns the effective configuration of a stream after applying
// its template. Per-stream identity fields (Code, Name, Description, Tags,
// StreamKey, Inputs, Disabled) always come from the stream. Config-like
// fields use the stream's value when non-zero and fall back to the template's
// value otherwise:
//
//   - Pointer fields (Transcoder, DVR, Watermark, Thumbnail): nil = inherit.
//   - Slice fields (Push): len == 0 = inherit.
//   - Struct fields (Protocols): all-zero = inherit.
//
// If the stream has no Template reference, or the template lookup returns
// nil, the stream is returned unchanged (caller may safely pass nil for
// tpl).
//
// The returned pointer is always a copy when a template merge happens — the
// caller is free to mutate it without affecting the persisted stream. When
// no merge is performed the original pointer is returned.
func ResolveStream(s *Stream, tpl *Template) *Stream {
	if s == nil || s.Template == nil || tpl == nil {
		return s
	}
	out := *s
	if out.Transcoder == nil && tpl.Transcoder != nil {
		out.Transcoder = tpl.Transcoder
	}
	if out.Protocols == (OutputProtocols{}) {
		out.Protocols = tpl.Protocols
	}
	if len(out.Push) == 0 && len(tpl.Push) > 0 {
		out.Push = tpl.Push
	}
	if out.DVR == nil && tpl.DVR != nil {
		out.DVR = tpl.DVR
	}
	if out.Watermark == nil && tpl.Watermark != nil {
		out.Watermark = tpl.Watermark
	}
	if out.Thumbnail == nil && tpl.Thumbnail != nil {
		out.Thumbnail = tpl.Thumbnail
	}
	return &out
}
