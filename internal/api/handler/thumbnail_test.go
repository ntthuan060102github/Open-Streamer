package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// stubThumbnailSvc lets tests dictate what LatestPath returns without
// pulling in the real thumbnail.Service (which would spawn FFmpeg).
type stubThumbnailSvc struct {
	paths map[domain.StreamCode]string
}

func (s *stubThumbnailSvc) LatestPath(code domain.StreamCode) string {
	return s.paths[code]
}

func newThumbHandlerRouter(svc thumbnailService) http.Handler {
	r := chi.NewRouter()
	h := &ThumbnailHandler{svc: svc}
	r.Get("/streams/{code}/thumbnail.jpg", h.Get)
	return r
}

func TestThumbnailHandler_404WhenNoWorker(t *testing.T) {
	t.Parallel()
	// LatestPath returns "" → handler must respond 404 with the
	// documented error code so the UI can distinguish "stream doesn't
	// have thumbnails enabled" from "transient missing file".
	r := newThumbHandlerRouter(&stubThumbnailSvc{paths: map[domain.StreamCode]string{}})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/streams/abc/thumbnail.jpg", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rec.Code)
	}
}

func TestThumbnailHandler_404WhenFileMissing(t *testing.T) {
	t.Parallel()
	// LatestPath returns a non-empty path but the file doesn't exist
	// (worker just started, FFmpeg hasn't produced its first frame).
	// Handler must surface 404 instead of letting http.ServeFile emit
	// its own 404 — a wrapped writeError keeps the JSON envelope shape
	// consistent with the rest of the API.
	tmp := filepath.Join(t.TempDir(), "thumb.jpg") // doesn't exist
	r := newThumbHandlerRouter(&stubThumbnailSvc{
		paths: map[domain.StreamCode]string{"abc": tmp},
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/streams/abc/thumbnail.jpg", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rec.Code)
	}
}

func TestThumbnailHandler_ServesExistingJPEG(t *testing.T) {
	t.Parallel()
	// File-on-disk happy path. We write a tiny 3-byte fake "JPEG" — the
	// handler is content-agnostic, just sets Content-Type and streams.
	dir := t.TempDir()
	tmp := filepath.Join(dir, "thumb.jpg")
	if err := os.WriteFile(tmp, []byte{0xff, 0xd8, 0xff}, 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	r := newThumbHandlerRouter(&stubThumbnailSvc{
		paths: map[domain.StreamCode]string{"abc": tmp},
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/streams/abc/thumbnail.jpg", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	// Content-Type must declare image/jpeg explicitly; http.ServeFile
	// will sniff and override only if we don't set it ourselves —
	// pinning here protects against future header-stripping middleware.
	if got := rec.Header().Get("Content-Type"); got != "image/jpeg" {
		t.Fatalf("Content-Type: got %q, want image/jpeg", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control: got %q, want no-cache", got)
	}
	if rec.Body.Len() != 3 {
		t.Fatalf("body bytes: got %d, want 3", rec.Body.Len())
	}
}
