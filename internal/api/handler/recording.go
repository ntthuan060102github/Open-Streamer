package handler

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/open-streamer/open-streamer/internal/domain"
	"github.com/open-streamer/open-streamer/internal/dvr"
	"github.com/open-streamer/open-streamer/internal/store"
	"github.com/samber/do/v2"
)

// RecordingHandler handles DVR recording and playback REST endpoints.
type RecordingHandler struct {
	dvr        *dvr.Service
	recRepo    store.RecordingRepository
	streamRepo store.StreamRepository
}

// NewRecordingHandler creates a RecordingHandler and registers it with the DI injector.
func NewRecordingHandler(i do.Injector) (*RecordingHandler, error) {
	return &RecordingHandler{
		dvr:        do.MustInvoke[*dvr.Service](i),
		recRepo:    do.MustInvoke[store.RecordingRepository](i),
		streamRepo: do.MustInvoke[store.StreamRepository](i),
	}, nil
}

func (h *RecordingHandler) Start(w http.ResponseWriter, r *http.Request) {
	streamID := domain.StreamID(chi.URLParam(r, "id"))

	// Resolve the stream's per-stream DVR config to get the segment duration.
	var segmentDuration time.Duration
	if stream, err := h.streamRepo.FindByID(r.Context(), streamID); err == nil && stream.DVR != nil {
		segmentDuration = time.Duration(stream.DVR.SegmentDuration) * time.Second
	}

	rec, err := h.dvr.StartRecording(r.Context(), streamID, segmentDuration)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "RECORDING_START_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"data": rec})
}

func (h *RecordingHandler) Stop(w http.ResponseWriter, r *http.Request) {
	streamID := domain.StreamID(chi.URLParam(r, "id"))
	if err := h.dvr.StopRecording(r.Context(), streamID); err != nil {
		writeError(w, http.StatusInternalServerError, "RECORDING_STOP_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]string{"status": "stopped"}})
}

func (h *RecordingHandler) ListByStream(w http.ResponseWriter, r *http.Request) {
	streamID := domain.StreamID(chi.URLParam(r, "id"))
	recs, err := h.recRepo.ListByStream(r.Context(), streamID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", "failed to list recordings")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": recs, "total": len(recs)})
}

func (h *RecordingHandler) Get(w http.ResponseWriter, r *http.Request) {
	rid := domain.RecordingID(chi.URLParam(r, "rid"))
	rec, err := h.recRepo.FindByID(r.Context(), rid)
	if err != nil {
		writeError(w, http.StatusNotFound, "RECORDING_NOT_FOUND", "recording not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": rec})
}

func (h *RecordingHandler) Delete(w http.ResponseWriter, r *http.Request) {
	rid := domain.RecordingID(chi.URLParam(r, "rid"))
	if err := h.recRepo.Delete(r.Context(), rid); err != nil {
		writeError(w, http.StatusInternalServerError, "DELETE_FAILED", "failed to delete recording")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *RecordingHandler) Playlist(w http.ResponseWriter, r *http.Request) {
	// TODO: serve M3U8 VOD playlist for the recording
	writeError(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "playlist serving not implemented")
}
