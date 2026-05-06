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

// newRouterWithDVRTracker mimics the production /recordings/{rid}/{file}
// route shape so DVRHTTPMiddleware can extract the rid via chi.URLParam.
func newRouterWithDVRTracker(t *testing.T, tr Tracker) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Route("/recordings/{rid}", func(r chi.Router) {
		r.Group(func(rr chi.Router) {
			rr.Use(DVRHTTPMiddleware(tr))
			rr.Get("/{file}", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("0123456789ABCDEF"))
			})
		})
	})
	return r
}

// A successful hit on /recordings/{rid}/{file}.m3u8 must record a session
// tagged DVR=true, with StreamCode = rid and Protocol = HLS.
func TestDVRHTTPMiddlewareTagsSessionAsDVR(t *testing.T) {
	s := newTestService(t)
	h := newRouterWithDVRTracker(t, s)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/recordings/bac_ninh/playlist.m3u8?from=2026-05-01T10:00:00Z", nil)
	req.RemoteAddr = "1.2.3.4:1234"
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
	if !got.DVR {
		t.Errorf("DVR flag not set on recording-route session")
	}
	if got.StreamCode != "bac_ninh" {
		t.Errorf("stream_code=%s, want bac_ninh", got.StreamCode)
	}
	if got.Protocol != domain.SessionProtoHLS {
		t.Errorf("proto=%s, want hls", got.Protocol)
	}
}

// Same client (stream + ip + ua) hitting BOTH the live mediaserve mount
// and the DVR / recording mount must yield two distinct session records,
// not one merged record. Regression guard for the fingerprint key — the
// dvr bool is part of the hash so live and timeshift never collide.
func TestDVRHitDoesNotCollideWithLiveSession(t *testing.T) {
	s := newTestService(t)

	live := chi.NewRouter()
	live.Use(HTTPMiddleware(s))
	live.Get("/{code}/seg0.ts", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("0123456789ABCDEF"))
	})
	dvr := newRouterWithDVRTracker(t, s)

	const ua = "vlc"
	const ip = "1.2.3.4:1"

	liveReq := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/bac_ninh/seg0.ts", nil)
	liveReq.RemoteAddr = ip
	liveReq.Header.Set("User-Agent", ua)
	live.ServeHTTP(httptest.NewRecorder(), liveReq)

	dvrReq := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/recordings/bac_ninh/playlist.m3u8", nil)
	dvrReq.RemoteAddr = ip
	dvrReq.Header.Set("User-Agent", ua)
	dvr.ServeHTTP(httptest.NewRecorder(), dvrReq)

	all := s.List(Filter{})
	if len(all) != 2 {
		t.Fatalf("got %d sessions, want 2 (live + dvr distinct)", len(all))
	}
	var liveSeen, dvrSeen bool
	for _, sess := range all {
		if sess.DVR {
			dvrSeen = true
		} else {
			liveSeen = true
		}
	}
	if !liveSeen || !dvrSeen {
		t.Errorf("expected both live and dvr sessions, got liveSeen=%v dvrSeen=%v", liveSeen, dvrSeen)
	}
}

// JSON metadata routes (/recordings/{rid}, /recordings/{rid}/info) sit
// outside the DVR middleware group and must NOT create sessions even if
// hit. This test pokes the same tracker through a router that has the
// middleware on /{file} only — the /info call should pass through
// untracked, the .ts call should track.
func TestDVRHTTPMiddlewareSkipsInfoRoutes(t *testing.T) {
	s := newTestService(t)
	r := chi.NewRouter()
	r.Route("/recordings/{rid}", func(r chi.Router) {
		r.Get("/info", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"x":1}`))
		})
		r.Group(func(rr chi.Router) {
			rr.Use(DVRHTTPMiddleware(s))
			rr.Get("/{file}", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("0123456789ABCDEF"))
			})
		})
	})

	infoReq := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/recordings/bac_ninh/info", nil)
	infoReq.RemoteAddr = "1.2.3.4:1"
	r.ServeHTTP(httptest.NewRecorder(), infoReq)
	if got := s.Stats().Active; got != 0 {
		t.Fatalf("info route created %d sessions, want 0", got)
	}

	tsReq := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/recordings/bac_ninh/000001.ts", nil)
	tsReq.RemoteAddr = "1.2.3.4:1"
	r.ServeHTTP(httptest.NewRecorder(), tsReq)
	if got := s.Stats().Active; got != 1 {
		t.Errorf("segment route did not create a session: Active=%d", got)
	}
}
