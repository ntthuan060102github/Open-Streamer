package mediaserve

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func setupRoots(t *testing.T) Roots {
	t.Helper()
	root := t.TempDir()
	hlsDir := filepath.Join(root, "hls")
	dashDir := filepath.Join(root, "dash")

	type entry struct {
		dir, code, file, body string
	}
	files := []entry{
		{hlsDir, "live", "index.m3u8", "#EXTM3U\n"},
		{hlsDir, "live", "seg0.ts", "TSDATA"},
		{dashDir, "live", "index.mpd", "<MPD/>"},
		{dashDir, "live", "init.m4s", "M4S"},
		{dashDir, "live", "init.mp4", "MP4"},
		// Nested-code dir to exercise codes containing '/'.
		{hlsDir, "region/north", "index.m3u8", "#EXTM3U\nNORTH\n"},
	}
	for _, f := range files {
		if err := os.MkdirAll(filepath.Join(f.dir, f.code), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.dir, f.code, f.file), []byte(f.body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return Roots{HLSDir: hlsDir, DASHDir: dashDir}
}

func serveFile(t *testing.T, roots Roots, code, filename string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/"+code+"/"+filename, nil)
	w := httptest.NewRecorder()
	roots.ServeFile(w, req, code, filename)
	return w
}

func TestServeFile_HLSManifest(t *testing.T) {
	roots := setupRoots(t)
	w := serveFile(t, roots, "live", "index.m3u8")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "application/vnd.apple.mpegurl" {
		t.Errorf("content-type=%s", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("cache-control=%s", got)
	}
	if got := w.Header().Get("Pragma"); got != "no-cache" {
		t.Errorf("pragma=%s", got)
	}
}

func TestServeFile_DASHManifest(t *testing.T) {
	roots := setupRoots(t)
	w := serveFile(t, roots, "live", "index.mpd")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "application/dash+xml" {
		t.Errorf("content-type=%s", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store, max-age=0, must-revalidate" {
		t.Errorf("cache-control=%s", got)
	}
}

func TestServeFile_TSSegment(t *testing.T) {
	roots := setupRoots(t)
	w := serveFile(t, roots, "live", "seg0.ts")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "video/mp2t" {
		t.Errorf("content-type=%s", got)
	}
	if w.Body.String() != "TSDATA" {
		t.Errorf("body=%q", w.Body.String())
	}
}

func TestServeFile_M4SSegment(t *testing.T) {
	roots := setupRoots(t)
	w := serveFile(t, roots, "live", "init.m4s")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	if w.Body.String() != "M4S" {
		t.Errorf("body=%q", w.Body.String())
	}
}

func TestServeFile_MP4Segment(t *testing.T) {
	roots := setupRoots(t)
	w := serveFile(t, roots, "live", "init.mp4")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestServeFile_UnsupportedExtension(t *testing.T) {
	roots := setupRoots(t)
	w := serveFile(t, roots, "live", "file.bin")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown ext, got %d", w.Code)
	}
}

func TestServeFile_RejectsTraversal(t *testing.T) {
	roots := setupRoots(t)
	w := serveFile(t, roots, "live", "../../etc/passwd")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for traversal, got %d", w.Code)
	}
}

func TestServeFile_MissingFile(t *testing.T) {
	roots := setupRoots(t)
	w := serveFile(t, roots, "live", "nope.ts")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestServeFile_EmptyInputs(t *testing.T) {
	roots := setupRoots(t)
	if w := serveFile(t, roots, "", "index.m3u8"); w.Code != http.StatusNotFound {
		t.Errorf("empty code: want 404, got %d", w.Code)
	}
	if w := serveFile(t, roots, "live", ""); w.Code != http.StatusNotFound {
		t.Errorf("empty filename: want 404, got %d", w.Code)
	}
}

// Codes containing '/' (e.g. region/north) resolve into the corresponding
// nested directory on disk — the API server passes the full prefix as
// `code` and only the trailing filename as `filename`.
func TestServeFile_NestedCode(t *testing.T) {
	roots := setupRoots(t)
	w := serveFile(t, roots, "region/north", "index.m3u8")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != "#EXTM3U\nNORTH\n" {
		t.Errorf("body=%q", got)
	}
}
