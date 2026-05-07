package handler

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/samber/do/v2"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/events"
	"github.com/ntt0601zcoder/open-streamer/internal/watermarks"
)

// watermarkUploadMaxBytes caps a single watermark upload. Picked at 16 MiB
// so common high-DPI logos (vector exports rendered at 4K) fit comfortably
// while still bounding the multipart-parser temp footprint. The service
// layer additionally enforces domain.MaxWatermarkAssetBytes (8 MiB) — the
// HTTP cap is bigger so the service emits a clean "too large" error
// instead of the multipart parser bailing out mid-upload.
const (
	watermarkUploadMaxBytes    = 16 << 20 // 16 MiB request body cap
	watermarkUploadMemoryBytes = 4 << 20  // 4 MiB before spilling to temp
)

// WatermarkHandler exposes the asset library over REST. Mirrors the VOD
// handler shape (list / upload / get / raw / delete) so the UI's file
// management widgets can be reused with minimal adaptation.
type WatermarkHandler struct {
	svc *watermarks.Service
	bus events.Bus
}

// NewWatermarkHandler is the samber/do constructor.
func NewWatermarkHandler(i do.Injector) (*WatermarkHandler, error) {
	return &WatermarkHandler{
		svc: do.MustInvoke[*watermarks.Service](i),
		bus: do.MustInvoke[events.Bus](i),
	}, nil
}

// List returns every asset, newest first.
//
// @Summary     List watermark assets
// @Tags        watermarks
// @Produce     json
// @Success     200 {object} apidocs.WatermarkAssetList
// @Failure     500 {object} apidocs.ErrorBody
// @Router      /watermarks [get].
func (h *WatermarkHandler) List(w http.ResponseWriter, _ *http.Request) {
	assets := h.svc.List()
	writeJSON(w, http.StatusOK, map[string]any{
		"data":  assets,
		"total": len(assets),
		"dir":   h.svc.Dir(),
	})
}

// Get returns a single asset's metadata.
//
// @Summary     Get watermark metadata
// @Tags        watermarks
// @Produce     json
// @Param       id path string true "Asset ID"
// @Success     200 {object} apidocs.WatermarkAssetData
// @Failure     400 {object} apidocs.ErrorBody
// @Failure     404 {object} apidocs.ErrorBody
// @Router      /watermarks/{id} [get].
func (h *WatermarkHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := domain.WatermarkAssetID(chi.URLParam(r, "id"))
	if err := domain.ValidateWatermarkAssetID(string(id)); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ID", err.Error())
		return
	}
	asset, err := h.svc.Get(id)
	if err != nil {
		if errors.Is(err, watermarks.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
			return
		}
		serverError(w, r, "GET_FAILED", "get watermark asset", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": asset})
}

// Raw streams the binary image bytes. Used by the UI to preview an asset
// before applying it to a stream and by player-side overlays that need
// to fetch the same bitmap as ffmpeg.
//
// @Summary     Download watermark image
// @Tags        watermarks
// @Produce     image/png
// @Param       id path string true "Asset ID"
// @Success     200 {file} binary
// @Failure     404 {object} apidocs.ErrorBody
// @Router      /watermarks/{id}/raw [get].
func (h *WatermarkHandler) Raw(w http.ResponseWriter, r *http.Request) {
	id := domain.WatermarkAssetID(chi.URLParam(r, "id"))
	if err := domain.ValidateWatermarkAssetID(string(id)); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ID", err.Error())
		return
	}
	asset, err := h.svc.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "asset not found")
		return
	}
	abs, err := h.svc.ResolvePath(id)
	if err != nil {
		serverError(w, r, "RESOLVE_FAILED", "resolve watermark path", err)
		return
	}
	if asset.ContentType != "" {
		w.Header().Set("Content-Type", asset.ContentType)
	}
	// Long cache — assets are immutable after upload (no in-place edits;
	// renaming changes the ID). Cuts re-fetches on UI reloads.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, abs)
}

// Upload accepts a multipart "file" field. Optional ?name= sets the display
// name; absent uses the upload's basename.
//
// @Summary     Upload a watermark image
// @Tags        watermarks
// @Accept      multipart/form-data
// @Produce     json
// @Param       file formData file true "Image file (PNG / JPG / GIF, ≤ 8 MiB)"
// @Param       name query string false "Display name (defaults to filename)"
// @Success     201 {object} apidocs.WatermarkAssetData
// @Failure     400 {object} apidocs.ErrorBody
// @Failure     413 {object} apidocs.ErrorBody
// @Failure     415 {object} apidocs.ErrorBody
// @Failure     500 {object} apidocs.ErrorBody
// @Router      /watermarks [post].
func (h *WatermarkHandler) Upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, watermarkUploadMaxBytes)
	if err := r.ParseMultipartForm(watermarkUploadMemoryBytes); err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "UPLOAD_TOO_LARGE", err.Error())
		return
	}

	src, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "missing 'file' field: "+err.Error())
		return
	}
	defer func() { _ = src.Close() }()

	filename := filepath.Base(strings.TrimSpace(header.Filename))
	if filename == "" || filename == "." || filename == string(filepath.Separator) {
		writeError(w, http.StatusBadRequest, "INVALID_FILENAME", "missing filename")
		return
	}
	displayName := r.URL.Query().Get("name")

	asset, err := h.svc.Save(displayName, filename, src)
	switch {
	case err == nil:
		// Audit event for asset library mutations — small payload (id +
		// display name) so hooks can sync inventory without re-fetching
		// the full asset list. Nil-safe for tests building the handler
		// without DI.
		if h.bus != nil {
			h.bus.Publish(r.Context(), domain.Event{
				Type: domain.EventWatermarkAssetCreated,
				Payload: map[string]any{
					"asset_id": string(asset.ID),
					"name":     asset.Name,
				},
			})
		}
		writeJSON(w, http.StatusCreated, map[string]any{"data": asset})
	case errors.Is(err, watermarks.ErrInvalidContent):
		writeError(w, http.StatusUnsupportedMediaType, "INVALID_IMAGE", err.Error())
	case errors.Is(err, watermarks.ErrTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "UPLOAD_TOO_LARGE", err.Error())
	case errors.Is(err, os.ErrPermission):
		serverError(w, r, "PERMISSION_DENIED", "write watermark asset", err)
	default:
		serverError(w, r, "UPLOAD_FAILED", "save watermark asset", err)
	}
}

// Delete removes an asset. Idempotent — repeat deletes after success
// return 404, but never 500.
//
// @Summary     Delete a watermark asset
// @Tags        watermarks
// @Param       id path string true "Asset ID"
// @Success     204 "No Content"
// @Failure     400 {object} apidocs.ErrorBody
// @Failure     404 {object} apidocs.ErrorBody
// @Failure     500 {object} apidocs.ErrorBody
// @Router      /watermarks/{id} [delete].
func (h *WatermarkHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := domain.WatermarkAssetID(chi.URLParam(r, "id"))
	if err := domain.ValidateWatermarkAssetID(string(id)); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ID", err.Error())
		return
	}
	if err := h.svc.Delete(id); err != nil {
		if errors.Is(err, watermarks.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
			return
		}
		serverError(w, r, "DELETE_FAILED", "delete watermark asset", err)
		return
	}
	if h.bus != nil {
		h.bus.Publish(r.Context(), domain.Event{
			Type:    domain.EventWatermarkAssetDeleted,
			Payload: map[string]any{"asset_id": string(id)},
		})
	}
	w.WriteHeader(http.StatusNoContent)
}
