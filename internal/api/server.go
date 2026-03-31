// Package api implements the HTTP API server.
// All routes follow the REST conventions defined in api-conventions.mdc.
package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/open-streamer/open-streamer/config"
	"github.com/open-streamer/open-streamer/internal/api/handler"
	"github.com/samber/do/v2"
)

// Server is the HTTP API server.
type Server struct {
	cfg    config.ServerConfig
	router *chi.Mux
	http   *http.Server
}

// New creates a Server and registers it with the DI injector.
func New(i do.Injector) (*Server, error) {
	cfg := do.MustInvoke[*config.Config](i)
	streamHandler := do.MustInvoke[*handler.StreamHandler](i)
	recordingHandler := do.MustInvoke[*handler.RecordingHandler](i)
	hookHandler := do.MustInvoke[*handler.HookHandler](i)

	s := &Server{cfg: cfg.Server}
	s.router = s.buildRouter(streamHandler, recordingHandler, hookHandler)
	s.http = &http.Server{
		Addr:    cfg.Server.HTTPAddr,
		Handler: s.router,
	}
	return s, nil
}

func (s *Server) buildRouter(
	stream *handler.StreamHandler,
	recording *handler.RecordingHandler,
	hook *handler.HookHandler,
) *chi.Mux {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Logger)

	r.Get("/healthz", healthz)
	r.Get("/readyz", readyz)

	r.Route("/streams", func(r chi.Router) {
		r.Post("/", stream.Create)
		r.Get("/", stream.List)
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", stream.Get)
			r.Put("/", stream.Update)
			r.Delete("/", stream.Delete)
			r.Post("/start", stream.Start)
			r.Post("/stop", stream.Stop)
			r.Get("/status", stream.Status)
			r.Get("/inputs", stream.ListInputs)
			r.Put("/inputs/{iid}", stream.UpdateInput)

			r.Post("/recordings/start", recording.Start)
			r.Post("/recordings/stop", recording.Stop)
			r.Get("/recordings", recording.ListByStream)
		})
	})

	r.Route("/recordings/{rid}", func(r chi.Router) {
		r.Get("/", recording.Get)
		r.Delete("/", recording.Delete)
		r.Get("/playlist.m3u8", recording.Playlist)
	})

	r.Route("/hooks", func(r chi.Router) {
		r.Get("/", hook.List)
		r.Post("/", hook.Create)
		r.Route("/{hid}", func(r chi.Router) {
			r.Get("/", hook.Get)
			r.Put("/", hook.Update)
			r.Delete("/", hook.Delete)
			r.Post("/test", hook.Test)
		})
	})

	return r
}

// Start begins accepting HTTP connections. Blocks until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.http.Shutdown(shutCtx)
	}()

	slog.Info("api: HTTP server listening", "addr", s.cfg.HTTPAddr)
	if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("api: server: %w", err)
	}
	return nil
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func readyz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
