// Package handler contains HTTP request handlers for the API server.
package handler

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/open-streamer/open-streamer/internal/domain"
	"github.com/open-streamer/open-streamer/internal/manager"
	"github.com/open-streamer/open-streamer/internal/store"
	"github.com/samber/do/v2"
)

// StreamHandler handles stream lifecycle REST endpoints.
type StreamHandler struct {
	streamRepo store.StreamRepository
	manager    *manager.Service
}

// NewStreamHandler creates a StreamHandler and registers it with the DI injector.
func NewStreamHandler(i do.Injector) (*StreamHandler, error) {
	return &StreamHandler{
		streamRepo: do.MustInvoke[store.StreamRepository](i),
		manager:    do.MustInvoke[*manager.Service](i),
	}, nil
}

func (h *StreamHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID          string                   `json:"stream_id"`
		Name        string                   `json:"name"`
		Description string                   `json:"description"`
		Tags        []string                 `json:"tags"`
		Inputs      []domain.Input           `json:"inputs"`
		Transcoder  domain.TranscoderConfig  `json:"transcoder"`
		Protocols   domain.OutputProtocols   `json:"protocols"`
		Push        []domain.PushDestination  `json:"push"`
		DVR         *domain.StreamDVRConfig  `json:"dvr"`
		Watermark   *domain.WatermarkConfig  `json:"watermark"`
		Thumbnail   *domain.ThumbnailConfig  `json:"thumbnail"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}

	stream := &domain.Stream{
		ID:          domain.StreamID(req.ID),
		Name:        req.Name,
		Description: req.Description,
		Tags:        req.Tags,
		Status:      domain.StatusIdle,
		Inputs:      req.Inputs,
		Transcoder:  req.Transcoder,
		Protocols:   req.Protocols,
		Push:        req.Push,
		DVR:         req.DVR,
		Watermark:   req.Watermark,
		Thumbnail:   req.Thumbnail,
	}
	if err := h.streamRepo.Save(r.Context(), stream); err != nil {
		writeError(w, http.StatusInternalServerError, "SAVE_FAILED", "failed to save stream")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"data": stream})
}

func (h *StreamHandler) List(w http.ResponseWriter, r *http.Request) {
	streams, err := h.streamRepo.List(r.Context(), store.StreamFilter{})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", "failed to list streams")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": streams, "total": len(streams)})
}

func (h *StreamHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := domain.StreamID(chi.URLParam(r, "id"))
	stream, err := h.streamRepo.FindByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "STREAM_NOT_FOUND", "stream not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": stream})
}

func (h *StreamHandler) Update(w http.ResponseWriter, r *http.Request) {
	// TODO: implement update stream config
	writeError(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "not implemented")
}

func (h *StreamHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := domain.StreamID(chi.URLParam(r, "id"))
	if err := h.streamRepo.Delete(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "DELETE_FAILED", "failed to delete stream")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *StreamHandler) Start(w http.ResponseWriter, r *http.Request) {
	id := domain.StreamID(chi.URLParam(r, "id"))
	stream, err := h.streamRepo.FindByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "STREAM_NOT_FOUND", "stream not found")
		return
	}
	if err := h.manager.Register(r.Context(), stream); err != nil {
		writeError(w, http.StatusInternalServerError, "START_FAILED", "failed to start stream")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]string{"status": "started"}})
}

func (h *StreamHandler) Stop(w http.ResponseWriter, r *http.Request) {
	id := domain.StreamID(chi.URLParam(r, "id"))
	h.manager.Unregister(id)
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]string{"status": "stopped"}})
}

func (h *StreamHandler) Status(w http.ResponseWriter, r *http.Request) {
	// TODO: return live runtime status from manager
	writeError(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "not implemented")
}

func (h *StreamHandler) ListInputs(w http.ResponseWriter, r *http.Request) {
	id := domain.StreamID(chi.URLParam(r, "id"))
	stream, err := h.streamRepo.FindByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "STREAM_NOT_FOUND", "stream not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": stream.Inputs})
}

func (h *StreamHandler) UpdateInput(w http.ResponseWriter, r *http.Request) {
	// TODO: update a specific input by ID
	writeError(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "not implemented")
}
