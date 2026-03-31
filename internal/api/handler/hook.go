package handler

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/open-streamer/open-streamer/internal/domain"
	"github.com/open-streamer/open-streamer/internal/store"
	"github.com/samber/do/v2"
)

// HookHandler handles webhook/hook management REST endpoints.
type HookHandler struct {
	hookRepo store.HookRepository
}

// NewHookHandler creates a HookHandler and registers it with the DI injector.
func NewHookHandler(i do.Injector) (*HookHandler, error) {
	return &HookHandler{
		hookRepo: do.MustInvoke[store.HookRepository](i),
	}, nil
}

func (h *HookHandler) List(w http.ResponseWriter, r *http.Request) {
	hooks, err := h.hookRepo.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", "failed to list hooks")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": hooks, "total": len(hooks)})
}

func (h *HookHandler) Create(w http.ResponseWriter, r *http.Request) {
	var hook domain.Hook
	if err := json.NewDecoder(r.Body).Decode(&hook); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}
	if err := h.hookRepo.Save(r.Context(), &hook); err != nil {
		writeError(w, http.StatusInternalServerError, "SAVE_FAILED", "failed to save hook")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"data": hook})
}

func (h *HookHandler) Get(w http.ResponseWriter, r *http.Request) {
	hid := domain.HookID(chi.URLParam(r, "hid"))
	hook, err := h.hookRepo.FindByID(r.Context(), hid)
	if err != nil {
		writeError(w, http.StatusNotFound, "HOOK_NOT_FOUND", "hook not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": hook})
}

func (h *HookHandler) Update(w http.ResponseWriter, r *http.Request) {
	// TODO: update hook fields
	writeError(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "not implemented")
}

func (h *HookHandler) Delete(w http.ResponseWriter, r *http.Request) {
	hid := domain.HookID(chi.URLParam(r, "hid"))
	if err := h.hookRepo.Delete(r.Context(), hid); err != nil {
		writeError(w, http.StatusInternalServerError, "DELETE_FAILED", "failed to delete hook")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HookHandler) Test(w http.ResponseWriter, r *http.Request) {
	// TODO: send a test event to this hook
	writeError(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "not implemented")
}
