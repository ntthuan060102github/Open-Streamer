// Package mediaserve serves HLS/DASH files from disk under /{code}/… — same URL layout as the API server.
package mediaserve

import (
	"net/http"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"
)

// Header keys + media MIMEs used across the manifest and segment handlers.
// Centralised so we don't drift between routes (and so static analysers
// don't complain about duplicate string literals).
const (
	headerContentType = "Content-Type"
	headerCacheCtrl   = "Cache-Control"
	headerPragma      = "Pragma"

	mimeM3U8 = "application/vnd.apple.mpegurl"
	mimeMPD  = "application/dash+xml"
	mimeTS   = "video/mp2t"
	mimeM4S  = "video/iso.segment"
	mimeMP4  = "video/mp4"

	// cacheNoStore is for DASH manifests — they're rewritten constantly
	// and a CDN/proxy must not cache them at all.
	cacheNoStore = "no-store, max-age=0, must-revalidate"
	// cacheNoCache is for HLS manifests — they need revalidation but can
	// be cached briefly while the segment list is unchanged.
	cacheNoCache = "no-cache"
)

// Mount registers GET /{code}/index.m3u8, GET /{code}/index.mpd, and GET /{code}/* on r.
func Mount(r chi.Router, hlsDir, dashDir string) {
	s := &roots{hlsDir: hlsDir, dashDir: dashDir}
	r.Get("/{code}/index.m3u8", s.serveManifest("index.m3u8", mimeM3U8, s.hlsDir))
	r.Get("/{code}/index.mpd", s.serveManifest("index.mpd", mimeMPD, s.dashDir))
	r.Get("/{code}/*", s.serveStreamNested())
}

// NewHandler returns a standalone handler with the same routes as Mount (for tests or embedding).
func NewHandler(hlsDir, dashDir string) http.Handler {
	r := chi.NewRouter()
	Mount(r, hlsDir, dashDir)
	return r
}

type roots struct {
	hlsDir  string
	dashDir string
}

func (s *roots) serveManifest(filename, contentType, rootDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		code := chi.URLParam(r, "code")
		if code == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set(headerContentType, contentType)
		switch filename {
		case "index.mpd":
			w.Header().Set(headerCacheCtrl, cacheNoStore)
		case "index.m3u8":
			w.Header().Set(headerCacheCtrl, cacheNoCache)
			w.Header().Set(headerPragma, cacheNoCache)
		}
		http.ServeFile(w, r, filepath.Join(rootDir, code, filename))
	}
}

func (s *roots) serveStreamNested() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		code := chi.URLParam(r, "code")
		suffix := strings.TrimPrefix(chi.URLParam(r, "*"), "/")
		if code == "" || suffix == "" {
			http.NotFound(w, r)
			return
		}
		rel := filepath.Clean(suffix)
		if rel == "." || strings.HasPrefix(rel, "..") {
			http.NotFound(w, r)
			return
		}
		baseName := filepath.Base(rel)
		ext := strings.ToLower(filepath.Ext(baseName))
		baseDir := ""
		switch ext {
		case ".ts", ".m3u8":
			baseDir = s.hlsDir
		case ".m4s", ".mpd", ".mp4":
			baseDir = s.dashDir
		default:
			http.NotFound(w, r)
			return
		}
		// Force the correct media MIME for each extension. Without this,
		// http.ServeFile falls back to mime.TypeByExtension which on most
		// Linux distros maps `.ts` to `text/vnd.trolltech.linguist` (the
		// historic Qt Linguist Translation type in /etc/mime.types) — HLS
		// players that strict-check Content-Type then refuse the segment.
		// We set the header BEFORE ServeFile so it isn't overwritten.
		switch ext {
		case ".ts":
			w.Header().Set(headerContentType, mimeTS)
		case ".m3u8":
			w.Header().Set(headerContentType, mimeM3U8)
			w.Header().Set(headerCacheCtrl, cacheNoCache)
			w.Header().Set(headerPragma, cacheNoCache)
		case ".mpd":
			w.Header().Set(headerContentType, mimeMPD)
			w.Header().Set(headerCacheCtrl, cacheNoStore)
		case ".m4s":
			w.Header().Set(headerContentType, mimeM4S)
		case ".mp4":
			w.Header().Set(headerContentType, mimeMP4)
		}
		http.ServeFile(w, r, filepath.Join(baseDir, code, rel))
	}
}
