// Package logger initialises the application-wide slog.Logger.
// One logger is created at startup and passed via DI.
// Never call slog.SetDefault in tests — use the injected logger instead.
package logger

import (
	"io"
	"log/slog"
	"os"

	"github.com/open-streamer/open-streamer/config"
)

// New creates a slog.Logger from the given LogConfig.
// Format "json" produces structured JSON; anything else produces human-readable text.
func New(cfg config.LogConfig) *slog.Logger {
	var level slog.Level
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	if cfg.Format == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}

	return slog.New(handler)
}

// NewWithWriter creates a logger that writes to the provided writer (useful for tests).
func NewWithWriter(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}
