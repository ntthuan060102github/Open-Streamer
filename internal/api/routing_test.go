package api

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestSplitTailAction(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		suffix     string
		wantHead   string
		wantAction string
	}{
		{"single segment, no action", "live", "live", ""},
		{"two segments, action match", "live/restart", "live", "restart"},
		{"two segments, no action match", "live/seg.ts", "live/seg.ts", ""},
		{"namespaced code + action", "region/north/live/switch", "region/north/live", "switch"},
		{"namespaced code, no action", "region/north/live", "region/north/live", ""},
		{"action segment in middle stays in code", "live/restart/seg.ts", "live/restart/seg.ts", ""},
		{"empty input", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			head, action := splitTailAction(c.suffix, streamActionRestart, streamActionSwitch)
			if head != c.wantHead {
				t.Errorf("head: got %q, want %q", head, c.wantHead)
			}
			if action != c.wantAction {
				t.Errorf("action: got %q, want %q", action, c.wantAction)
			}
		})
	}
}

func TestSplitCodeAndFile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		suffix   string
		wantCode string
		wantFile string
		wantOK   bool
	}{
		{"plain code + file", "live/index.m3u8", "live", "index.m3u8", true},
		{"plain code + segment", "live/000001.ts", "live", "000001.ts", true},
		{"namespaced code + file", "region/north/live/index.mpd", "region/north/live", "index.mpd", true},
		{"namespaced code + recording_status", "region/north/live/recording_status.json", "region/north/live", "recording_status.json", true},
		{"no file (single segment)", "live", "", "", false},
		{"empty", "", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			code, file, ok := splitCodeAndFile(c.suffix)
			if ok != c.wantOK {
				t.Fatalf("ok: got %v, want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if code != c.wantCode {
				t.Errorf("code: got %q, want %q", code, c.wantCode)
			}
			if file != c.wantFile {
				t.Errorf("file: got %q, want %q", file, c.wantFile)
			}
		})
	}
}

func TestSanitizeSubpath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{"plain", "live/index.m3u8", "live/index.m3u8", true},
		{"leading slash stripped", "/live/index.m3u8", "live/index.m3u8", true},
		{"empty", "", "", false},
		{"just slash", "/", "", false},
		{"traversal explicit", "live/../etc/passwd", "", false},
		{"traversal nested", "a/b/../c", "", false},
		{"dot segment alone", ".", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, ok := sanitizeSubpath(c.in)
			if ok != c.wantOK {
				t.Fatalf("ok: got %v, want %v", ok, c.wantOK)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestSetURLParam(t *testing.T) {
	t.Parallel()
	// Simulate a request that hit the catch-all dispatcher — chi has put a
	// RouteContext on it but no `code` param yet.
	req := httptest.NewRequestWithContext(t.Context(), "GET", "/streams/region/north/live/restart", nil)
	rctx := chi.NewRouteContext()
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	req = setURLParam(req, "code", "region/north/live")
	if got := chi.URLParam(req, "code"); got != "region/north/live" {
		t.Fatalf("URLParam(code): got %q, want %q", got, "region/north/live")
	}

	// Second call to a different key should not clobber the first.
	req = setURLParam(req, "rid", "region/north/live")
	if got := chi.URLParam(req, "code"); got != "region/north/live" {
		t.Errorf("URLParam(code) after second setURLParam: got %q", got)
	}
	if got := chi.URLParam(req, "rid"); got != "region/north/live" {
		t.Errorf("URLParam(rid): got %q", got)
	}
}

func TestSetURLParam_NoExistingContext(t *testing.T) {
	t.Parallel()
	// Bare httptest request has no chi route context at all — helper must
	// inject one rather than panic.
	req := httptest.NewRequestWithContext(t.Context(), "GET", "/anything", nil)
	req = setURLParam(req, "code", "live")
	if got := chi.URLParam(req, "code"); got != "live" {
		t.Fatalf("URLParam(code): got %q, want %q", got, "live")
	}
}
