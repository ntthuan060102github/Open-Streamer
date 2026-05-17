// Package mediaserve serves HLS/DASH files from disk for a parsed stream
// code + filename. Routing is owned by the API server (which parses the
// catch-all path); this package handles content-type, cache headers, and
// the directory lookup.
package mediaserve

import (
	"net/http"
	"path/filepath"
	"strings"
)

// Header keys + media MIMEs used across the file handler. Centralised so
// the API server and any test wrapper agree on the wire shape.
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

// Roots resolves <code>/<filename> requests against the configured HLS
// and DASH publish directories.
type Roots struct {
	HLSDir  string
	DASHDir string
}

// ServeFile writes the file <root>/<code>/<filename> with the correct
// content-type and cache headers, choosing the root by extension.
// Returns 404 for empty inputs, traversal attempts, or unsupported
// extensions. The caller (api server dispatcher) is responsible for
// having already validated the stream code.
func (s Roots) ServeFile(w http.ResponseWriter, r *http.Request, code, filename string) {
	if code == "" || filename == "" {
		http.NotFound(w, r)
		return
	}
	rel := filepath.Clean(filename)
	if rel == "." || strings.HasPrefix(rel, "..") {
		http.NotFound(w, r)
		return
	}
	ext := strings.ToLower(filepath.Ext(filepath.Base(rel)))
	var baseDir string
	switch ext {
	case ".ts", ".m3u8":
		baseDir = s.HLSDir
	case ".m4s", ".mpd", ".mp4":
		baseDir = s.DASHDir
	default:
		http.NotFound(w, r)
		return
	}
	// Force the correct media MIME for each extension. Without this,
	// http.ServeFile falls back to mime.TypeByExtension which on most
	// Linux distros maps `.ts` to `text/vnd.trolltech.linguist` (the
	// historic Qt Linguist Translation type in /etc/mime.types) — HLS
	// players that strict-check Content-Type then refuse the segment.
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
