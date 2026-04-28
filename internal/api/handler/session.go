package handler

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/samber/do/v2"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/sessions"
)

// SessionHandler exposes the play-sessions tracker over HTTP.
//
// Routes (registered by api.Server):
//
//	GET    /streams/{code}/sessions       — sessions for one stream
//	GET    /sessions                      — all active sessions across streams
//	GET    /sessions/{id}                 — single session
//	DELETE /sessions/{id}                 — force-close ("kick") a session
type SessionHandler struct {
	tracker sessions.Tracker
}

// NewSessionHandler returns a handler reading the tracker from DI. The
// tracker is registered as *sessions.Service in main.go's wireServices.
// Returning an error from this constructor is reserved for future config
// validation; today it always succeeds.
func NewSessionHandler(i do.Injector) (*SessionHandler, error) {
	return &SessionHandler{
		tracker: do.MustInvoke[*sessions.Service](i),
	}, nil
}

// sessionListResponse mirrors Flussonic's `{ sessions, total_count }` shape.
// Total_count is exact (not estimated) since the tracker stays in memory.
type sessionListResponse struct {
	Sessions   []*domain.PlaySession `json:"sessions"`
	TotalCount int                   `json:"total_count"`
	Stats      sessions.Stats        `json:"stats"`
}

// List returns every active session. Optional filters: ?proto=hls|dash|...,
// ?status=active|closed, ?limit=N (1..1000).
//
// @Summary List play sessions
// @Tags sessions
// @Produce json
// @Param proto query string false "Filter by protocol (hls|dash|rtmp|srt|rtsp)"
// @Param status query string false "Filter by status (active|closed)"
// @Param limit query int false "Max sessions to return (default 0 = no cap)"
// @Success 200 {object} apidocs.SessionList
// @Router /sessions [get].
func (h *SessionHandler) List(w http.ResponseWriter, r *http.Request) {
	filter, err := parseSessionFilter(r, "")
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_QUERY", err.Error())
		return
	}
	resp := sessionListResponse{
		Sessions:   h.tracker.List(filter),
		Stats:      h.tracker.Stats(),
		TotalCount: 0,
	}
	resp.TotalCount = len(resp.Sessions)
	writeJSON(w, http.StatusOK, resp)
}

// ListByStream returns sessions for a single stream — the same payload shape
// as List, but scoped to chi.URLParam("code").
//
// @Summary List sessions for a stream
// @Tags sessions
// @Produce json
// @Param code path string true "Stream code"
// @Param proto query string false "Filter by protocol"
// @Param status query string false "Filter by status"
// @Param limit query int false "Max sessions to return"
// @Success 200 {object} apidocs.SessionList
// @Failure 400 {object} apidocs.ErrorBody
// @Router /streams/{code}/sessions [get].
func (h *SessionHandler) ListByStream(w http.ResponseWriter, r *http.Request) {
	code := domain.StreamCode(chi.URLParam(r, "code"))
	if err := domain.ValidateStreamCode(string(code)); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_STREAM_CODE", err.Error())
		return
	}
	filter, err := parseSessionFilter(r, code)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_QUERY", err.Error())
		return
	}
	resp := sessionListResponse{
		Sessions: h.tracker.List(filter),
		Stats:    h.tracker.Stats(),
	}
	resp.TotalCount = len(resp.Sessions)
	writeJSON(w, http.StatusOK, resp)
}

// Get returns one session by ID.
//
// @Summary Get a play session by ID
// @Tags sessions
// @Produce json
// @Param id path string true "Session ID"
// @Success 200 {object} domain.PlaySession
// @Failure 404 {object} apidocs.ErrorBody
// @Router /sessions/{id} [get].
func (h *SessionHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sess, ok := h.tracker.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "session not found or already closed")
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

// Delete force-closes ("kicks") a session. Idempotent — returns 204 even when
// the session was already gone, so ops scripts can fire-and-forget.
//
// @Summary Kick a play session
// @Tags sessions
// @Param id path string true "Session ID"
// @Success 204 "kicked"
// @Failure 404 {object} apidocs.ErrorBody
// @Router /sessions/{id} [delete].
func (h *SessionHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !h.tracker.Kick(id) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "session not found or already closed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseSessionFilter validates query params and builds a sessions.Filter.
// streamScope, when non-empty, locks StreamCode regardless of any ?stream=…
// param so route-derived scope is the source of truth.
func parseSessionFilter(r *http.Request, streamScope domain.StreamCode) (sessions.Filter, error) {
	q := r.URL.Query()
	f := sessions.Filter{StreamCode: streamScope}
	if proto := q.Get("proto"); proto != "" {
		switch domain.SessionProto(proto) {
		case domain.SessionProtoHLS, domain.SessionProtoDASH,
			domain.SessionProtoRTMP, domain.SessionProtoSRT, domain.SessionProtoRTSP:
			f.Protocol = domain.SessionProto(proto)
		default:
			return f, errInvalidProto
		}
	}
	switch q.Get("status") {
	case "":
		// FilterStatusAny — but our tracker only retains active sessions in
		// memory, so this defaults to "active" anyway. Leave as Any so that
		// future history-backed filters keep working.
	case string(sessions.FilterStatusActive):
		f.Status = sessions.FilterStatusActive
	case string(sessions.FilterStatusClosed):
		f.Status = sessions.FilterStatusClosed
	default:
		return f, errInvalidStatus
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return f, errInvalidLimit
		}
		if n > sessionListMaxLimit {
			n = sessionListMaxLimit
		}
		f.Limit = n
	}
	return f, nil
}

const sessionListMaxLimit = 1000

var (
	errInvalidProto  = sessionFilterErr("proto must be hls|dash|rtmp|srt|rtsp")
	errInvalidStatus = sessionFilterErr("status must be active|closed")
	errInvalidLimit  = sessionFilterErr("limit must be a non-negative integer")
)

type sessionFilterErr string

func (e sessionFilterErr) Error() string { return string(e) }
