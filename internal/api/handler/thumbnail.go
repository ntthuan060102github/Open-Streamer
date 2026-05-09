package handler

import (
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/samber/do/v2"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/thumbnail"
)

// thumbnailService narrows *thumbnail.Service to the methods this
// handler uses, keeping tests free of subprocess machinery.
// *thumbnail.Service satisfies implicitly.
type thumbnailService interface {
	LatestPath(domain.StreamCode) string
}

// ThumbnailHandler serves the most recent JPEG snapshot for a stream
// at `GET /streams/{code}/thumbnail.jpg`. Independent from
// StreamHandler because it has its own narrow dep (thumbnail.Service)
// and a different cache policy (short-lived, not the full stream
// envelope).
type ThumbnailHandler struct {
	svc thumbnailService
}

// NewThumbnailHandler is the samber/do constructor.
func NewThumbnailHandler(i do.Injector) (*ThumbnailHandler, error) {
	return &ThumbnailHandler{
		svc: do.MustInvoke[*thumbnail.Service](i),
	}, nil
}

// Get serves the latest JPEG. Returns 404 when the stream isn't
// running with thumbnails enabled, OR when the worker has just started
// and FFmpeg hasn't produced its first frame yet (file still absent on
// disk). Cache-Control mirrors HLS segment headers — short-lived so
// the UI sees fresh frames without aggressive intermediate caching.
//
// @Summary     Get latest thumbnail
// @Tags        streams
// @Produce     image/jpeg
// @Param       code path string true "Stream code"
// @Success     200 {file} binary
// @Failure     404 {object} apidocs.ErrorBody
// @Router      /streams/{code}/thumbnail.jpg [get].
func (h *ThumbnailHandler) Get(w http.ResponseWriter, r *http.Request) {
	code := domain.StreamCode(strings.TrimSpace(chi.URLParam(r, "code")))
	if code == "" {
		writeError(w, http.StatusBadRequest, "INVALID_CODE", "stream code is required")
		return
	}

	path := h.svc.LatestPath(code)
	if path == "" {
		writeError(w, http.StatusNotFound, "NO_THUMBNAIL",
			"thumbnail not enabled for this stream or worker not running")
		return
	}
	if _, err := os.Stat(path); err != nil {
		writeError(w, http.StatusNotFound, "NO_THUMBNAIL",
			"thumbnail file not yet produced (worker just started)")
		return
	}

	w.Header().Set("Content-Type", "image/jpeg")
	// Don't cache aggressively — the file is overwritten on every
	// IntervalSec by the worker. UI clients want fresh frames; an
	// intermediate cache of even 5 s defeats the purpose of a live
	// preview. `no-cache` (with revalidation) lets browsers re-use the
	// HTTP/1.1 conditional GET if the server later adds Last-Modified.
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, path)
}
