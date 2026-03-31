package ingestor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/open-streamer/open-streamer/internal/buffer"
	"github.com/open-streamer/open-streamer/internal/domain"
)

const (
	reconnectBaseDelay = time.Second
	reconnectMaxDelay  = 30 * time.Second
)

// runPullWorker reads from reader in a loop, writing each chunk to the buffer.
// It reconnects automatically after transient failures using exponential backoff.
// The loop exits cleanly when ctx is cancelled.
func runPullWorker(
	ctx context.Context,
	streamID domain.StreamID,
	input domain.Input,
	r Reader,
	buf *buffer.Service,
) {
	delay := reconnectBaseDelay

	for {
		if ctx.Err() != nil {
			return
		}

		if err := r.Open(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
		slog.Error("ingestor: open failed",
			"stream_id", streamID,
			"input_id", input.ID,
			"url", input.URL,
			"err", err,
		)
			if !waitBackoff(ctx, delay) {
				return
			}
			delay = min(delay*2, reconnectMaxDelay)
			continue
		}

		slog.Info("ingestor: source connected",
			"stream_id", streamID,
			"input_id", input.ID,
			"url", input.URL,
		)
		delay = reconnectBaseDelay // reset on successful open

		readErr := readLoop(ctx, streamID, input, r, buf)
		r.Close()

		if ctx.Err() != nil {
			return
		}

		if errors.Is(readErr, io.EOF) {
			// Graceful end of source (e.g. file finished).
			// For files configured to loop, the reader itself handles it.
			slog.Info("ingestor: source ended (EOF)",
				"stream_id", streamID,
				"input_id", input.ID,
			)
			return
		}

		slog.Warn("ingestor: read error, reconnecting",
			"stream_id", streamID,
			"input_id", input.ID,
			"err", readErr,
			"backoff", delay,
		)
		if !waitBackoff(ctx, delay) {
			return
		}
		delay = min(delay*2, reconnectMaxDelay)
	}
}

// readLoop reads from reader until error or ctx cancellation.
func readLoop(
	ctx context.Context,
	streamID domain.StreamID,
	input domain.Input,
	r Reader,
	buf *buffer.Service,
) error {
	for {
		pkt, err := r.Read(ctx)
		if err != nil {
			return err
		}
		if len(pkt) == 0 {
			continue
		}
		if writeErr := buf.Write(streamID, buffer.Packet(pkt)); writeErr != nil {
			slog.Error("ingestor: buffer write failed",
				"stream_id", streamID,
				"input_id", input.ID,
				"err", writeErr,
			)
			return writeErr
		}
	}
}

// waitBackoff sleeps for d, returning false if ctx is cancelled first.
func waitBackoff(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
