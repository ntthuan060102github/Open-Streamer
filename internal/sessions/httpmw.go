package sessions

import (
	"net/http"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// HTTPMiddleware returns a net/http middleware that records a session hit
// for every successful HLS / DASH manifest or segment request served from
// /<stream_code>/…  (the URL layout owned by mediaserve.Mount).
//
// Behaviour:
//   - Stream code is parsed from the URL path's first segment. We deliberately
//     don't rely on chi.URLParam here because chi populates the routing
//     context AFTER the middleware chain — the middleware sees an empty
//     param. Manual parsing matches mediaserve's "/<code>/<file>" layout.
//   - Only file extensions known to mediaserve are counted: .m3u8 / .ts for
//     HLS; .mpd / .m4s / .mp4 for DASH. Other extensions pass through
//     untouched.
//   - Bytes counted are exactly what was written to the wire — we wrap the
//     ResponseWriter and read its byte counter.
//   - Non-2xx responses still record a hit (so a broken segment URL still
//     reveals the viewer in the dashboard) but credit zero bytes.
func HTTPMiddleware(t Tracker) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			proto := protocolForPath(r.URL.Path)
			if proto == "" {
				next.ServeHTTP(w, r)
				return
			}
			code := streamCodeFromPath(r.URL.Path)
			if code == "" {
				next.ServeHTTP(w, r)
				return
			}

			cw := &countingWriter{ResponseWriter: w}
			next.ServeHTTP(cw, r)

			// Only credit bytes when the response succeeded — error pages
			// don't represent media a player rendered.
			var delta int64
			if cw.status >= 200 && cw.status < 300 {
				delta = cw.written
			}
			t.TrackHTTP(r.Context(), NewHTTPHit(r, code, proto, delta))
		})
	}
}

// DVRHTTPMiddleware returns a net/http middleware for the DVR / timeshift
// route group (/recordings/{rid}/...). Mirrors HTTPMiddleware behaviour but:
//
//   - The stream code is taken from the chi `rid` URL param (which the
//     recording handler treats as the recording ID == stream code).
//     Unlike the live mediaserve mount, the recording sub-router runs the
//     middleware AFTER chi populates the routing context, so chi.URLParam
//     works here.
//   - DVR is hardcoded true on every hit, so the session record surfaces
//     "watching from the archive" in the dashboard. This holds for both
//     full-recording playback (no query) and timeshift slices (with
//     ?from=... or ?offset_sec=... — the handler distinguishes them
//     internally for content selection, but both are still "from disk,
//     not live edge" from the viewer's perspective).
//   - Protocol is forced to SessionProtoHLS — recording delivery is HLS
//     only (.ts segments + .m3u8 playlist); the .mpd / .m4s / .mp4 set
//     never appears under this route.
//
// Hits to non-media files (e.g. /recordings/{rid}/info → JSON metadata)
// are skipped via the same extension filter.
func DVRHTTPMiddleware(t Tracker) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			proto := protocolForPath(r.URL.Path)
			if proto != domain.SessionProtoHLS {
				// Recording delivery is HLS-only; skip the rest (DASH ext
				// would never be seen here anyway, but be explicit so a
				// future operator who serves DASH from /recordings doesn't
				// silently accumulate live-vs-DVR collisions).
				next.ServeHTTP(w, r)
				return
			}
			rid := strings.TrimSpace(chi.URLParam(r, "rid"))
			if rid == "" {
				next.ServeHTTP(w, r)
				return
			}

			cw := &countingWriter{ResponseWriter: w}
			next.ServeHTTP(cw, r)

			var delta int64
			if cw.status >= 200 && cw.status < 300 {
				delta = cw.written
			}
			hit := NewHTTPHit(r, domain.StreamCode(rid), proto, delta)
			hit.DVR = true
			t.TrackHTTP(r.Context(), hit)
		})
	}
}

// streamCodeFromPath returns the first non-empty path segment of the URL,
// which is the stream code under mediaserve's layout. Returns "" when the
// path has no segment (e.g. "/" or "/foo" with no trailing file).
func streamCodeFromPath(p string) domain.StreamCode {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return ""
	}
	if i := strings.IndexByte(p, '/'); i > 0 {
		return domain.StreamCode(p[:i])
	}
	return ""
}

// protocolForPath maps a request path's extension to its SessionProto.
// Returns "" for extensions outside the HLS/DASH delivery set.
func protocolForPath(p string) domain.SessionProto {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".m3u8", ".ts":
		return domain.SessionProtoHLS
	case ".mpd", ".m4s", ".mp4":
		return domain.SessionProtoDASH
	default:
		return ""
	}
}

// countingWriter is a minimal http.ResponseWriter wrapper that tallies bytes
// written and remembers the status code. We avoid go-chi's middleware.WrapResponseWriter
// here because it allocates a couple of small structs per request; on a busy
// HLS edge that adds up. Net/http guarantees Write is called serially per
// request so the counters need no locks.
type countingWriter struct {
	http.ResponseWriter
	status  int
	written int64
}

// WriteHeader records the status before delegating so the middleware can
// distinguish 2xx vs error responses without re-reading from the wire.
func (c *countingWriter) WriteHeader(code int) {
	c.status = code
	c.ResponseWriter.WriteHeader(code)
}

func (c *countingWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	n, err := c.ResponseWriter.Write(b)
	c.written += int64(n)
	return n, err
}
