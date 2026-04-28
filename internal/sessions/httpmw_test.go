package sessions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ntt0601zcoder/open-streamer/config"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

func newRouterWithTracker(t *testing.T, tr Tracker) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Use(HTTPMiddleware(tr))
	r.Get("/{code}/index.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("#EXTM3U\n"))
	})
	r.Get("/{code}/seg0.ts", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "16")
		_, _ = w.Write([]byte("0123456789ABCDEF"))
	})
	r.Get("/{code}/index.mpd", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<MPD/>"))
	})
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	r.Get("/{code}/missing.ts", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusNotFound)
	})
	return r
}

func TestHTTPMiddlewareRecordsHLSHit(t *testing.T) {
	s := newTestService(t)
	h := newRouterWithTracker(t, s)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/abc/seg0.ts", nil)
	req.RemoteAddr = "1.2.3.4:9999"
	req.Header.Set("User-Agent", "vlc")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	all := s.List(Filter{})
	if len(all) != 1 {
		t.Fatalf("got %d sessions, want 1", len(all))
	}
	got := all[0]
	if got.Protocol != domain.SessionProtoHLS {
		t.Errorf("proto=%s, want hls", got.Protocol)
	}
	if got.IP != "1.2.3.4" {
		t.Errorf("ip=%s, want 1.2.3.4", got.IP)
	}
	if got.Bytes != 16 {
		t.Errorf("bytes=%d, want 16", got.Bytes)
	}
}

func TestHTTPMiddlewareDASHDetection(t *testing.T) {
	s := newTestService(t)
	h := newRouterWithTracker(t, s)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/abc/index.mpd", nil)
	req.RemoteAddr = "5.6.7.8:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	got := s.List(Filter{})
	if len(got) != 1 || got[0].Protocol != domain.SessionProtoDASH {
		t.Fatalf("expected one DASH session, got %+v", got)
	}
}

func TestHTTPMiddlewareSkipsNonMedia(t *testing.T) {
	s := newTestService(t)
	h := newRouterWithTracker(t, s)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := len(s.List(Filter{})); got != 0 {
		t.Errorf("non-media path created %d sessions, want 0", got)
	}
}

func TestHTTPMiddlewareCreditsZeroBytesOnError(t *testing.T) {
	s := newTestService(t)
	h := newRouterWithTracker(t, s)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/abc/missing.ts", nil)
	req.RemoteAddr = "9.9.9.9:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	got := s.List(Filter{})
	if len(got) != 1 {
		t.Fatalf("got %d sessions", len(got))
	}
	if got[0].Bytes != 0 {
		t.Errorf("Bytes=%d on 404 response, want 0", got[0].Bytes)
	}
}

func TestHTTPMiddlewareDisabledTrackerNoop(t *testing.T) {
	s := newService(config.SessionsConfig{Enabled: false}, nil, NullGeoIP{})
	h := newRouterWithTracker(t, s)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/abc/seg0.ts", nil)
	req.RemoteAddr = "1.2.3.4:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("middleware broke response when disabled: %d", rec.Code)
	}
	if got := s.Stats().Active; got != 0 {
		t.Errorf("Active=%d when disabled, want 0", got)
	}
}
