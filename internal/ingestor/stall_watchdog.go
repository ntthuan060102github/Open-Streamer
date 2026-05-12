package ingestor

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// stallWatchdog watches the time-since-last-write on a worker's
// readLoop and emits a SessionStartStallRecovery marker when the gap
// exceeds stallThreshold. The marker fronts the next packet written
// (buffer.Service auto-stamps SessionStart=true on the next write
// after SetSession), so downstream consumers (DASH packager's
// onSessionBoundary, HLS segmenter, RTSP/RTMP re-stream) flush their
// accumulated state at a clean boundary instead of accreting a giant
// per-sample dur or an A/V skew.
//
// The watchdog fires AT MOST ONCE per stall event — stallSignaled
// latches when a marker is emitted and clears as soon as a fresh
// packet write resets the lastWriteAt timestamp. This avoids a flood
// of session boundaries during a long outage.
//
// Scope: applies to all reader paths (raw-TS / AV). Raw-TS streams
// (HLS pull / SRT / UDP / file) benefit most because they bypass the
// Normaliser's MaxBehindMs re-anchor; AV-path streams already get a
// per-track re-anchor via the Normaliser but still benefit from the
// downstream session-boundary signal.
const (
	// stallThreshold is the gap-since-last-write that the watchdog
	// treats as a source stall. 3 s balances "real stall worth signalling"
	// against "transient jitter we should ignore" — sources at 4 Mbps
	// pause for 200–500 ms regularly during HLS chunk fetches; 3 s is
	// well beyond that.
	stallThreshold = 3 * time.Second
	// stallCheckInterval is how often the watchdog ticks. Smaller than
	// stallThreshold so the detection latency is bounded.
	stallCheckInterval = 1 * time.Second
)

// runStallWatchdog blocks until ctx is cancelled, emitting a session
// boundary on `buf` for `streamID` whenever the gap since lastWriteAt
// exceeds stallThreshold. Safe to run as a goroutine alongside the
// worker's read loop; the readLoop bumps lastWriteAt on every
// successful buffer.Write.
func runStallWatchdog(
	ctx context.Context,
	streamID, bufferWriteID domain.StreamCode,
	buf *buffer.Service,
	lastWriteAt *atomic.Int64,
) {
	ticker := time.NewTicker(stallCheckInterval)
	defer ticker.Stop()

	stallSignaled := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			last := time.Unix(0, lastWriteAt.Load())
			gap := time.Since(last)
			switch {
			case gap >= stallThreshold && !stallSignaled:
				slog.Info("ingestor: source stall detected, signalling session boundary",
					"stream_code", streamID,
					"gap_ms", gap.Milliseconds(),
					"threshold_ms", stallThreshold.Milliseconds(),
				)
				buf.SetSession(bufferWriteID, domain.SessionStartStallRecovery, nil, nil)
				stallSignaled = true
			case gap < stallThreshold && stallSignaled:
				// A fresh write reset the clock — clear the latch so a
				// later stall on the same readLoop can fire again.
				stallSignaled = false
			}
		}
	}
}
