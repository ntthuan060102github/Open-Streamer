// Package thumbnail produces periodic JPEG snapshots of a live stream so the
// console (and any operator dashboard) can show a "what's on screen now"
// preview without having to bring up a full media player. Each enabled
// stream gets one FFmpeg subprocess that reads MPEG-TS bytes from the
// stream's playback buffer (best rendition for ABR, raw / single-track
// otherwise) and writes a single JPEG file that's atomically renamed
// into place — so an HTTP client never observes a half-written image.
//
// Lifecycle is owned by the coordinator: Start when a stream's pipeline
// comes up with `Stream.Thumbnail.Enabled=true`, Update when the config
// changes, Stop when the pipeline tears down. The service is otherwise
// passive — no background goroutines outside the per-stream worker.
package thumbnail

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/samber/do/v2"

	"github.com/ntt0601zcoder/open-streamer/config"
	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// Service manages per-stream thumbnail generation.
type Service struct {
	tcCfg  config.TranscoderConfig
	pubCfg config.PublisherConfig
	buf    *buffer.Service

	mu      sync.Mutex
	workers map[domain.StreamCode]*worker
}

// worker is the per-stream state. cancel tears the FFmpeg subprocess +
// pump goroutine down; outputPath is the absolute path to the latest
// JPEG (returned to API handlers as-is).
type worker struct {
	cancel     context.CancelFunc
	done       chan struct{}
	outputPath string
}

// New is the do/v2 constructor. Registered in cmd/server/main.go.
func New(i do.Injector) (*Service, error) {
	tcCfg := do.MustInvoke[config.TranscoderConfig](i)
	pubCfg := do.MustInvoke[config.PublisherConfig](i)
	buf := do.MustInvoke[*buffer.Service](i)
	return &Service{
		tcCfg:   tcCfg,
		pubCfg:  pubCfg,
		buf:     buf,
		workers: make(map[domain.StreamCode]*worker),
	}, nil
}

// SetConfig updates the cached configs (FFmpeg path can change at
// runtime; HLS dir doesn't change post-boot but we accept both for
// symmetry with other Service.SetConfig signatures). Existing workers
// keep their original FFmpeg path until restart — same convention as
// transcoder.Service.SetConfig.
func (s *Service) SetConfig(tcCfg config.TranscoderConfig, pubCfg config.PublisherConfig) {
	s.mu.Lock()
	s.tcCfg = tcCfg
	s.pubCfg = pubCfg
	s.mu.Unlock()
}

// Start spawns a thumbnail worker for streamID reading from bufferID.
// No-op (with nil error) when cfg is nil or disabled. Returns an error
// only on argument validation; subprocess crashes are recovered in the
// pump goroutine.
//
// bufferID is the playback-buffer ID: caller resolves it via
// `buffer.PlaybackBufferID(stream.Code, stream.Transcoder)` so ABR
// streams pick the best rendition automatically.
func (s *Service) Start(
	ctx context.Context,
	streamID domain.StreamCode,
	bufferID domain.StreamCode,
	cfg *domain.ThumbnailConfig,
) error {
	if cfg == nil || !cfg.Enabled {
		return nil
	}
	hlsDir := s.pubCfg.HLS.Dir
	if hlsDir == "" {
		return fmt.Errorf("thumbnail: cannot start %q — publisher.hls.dir is unset", streamID)
	}

	outDir, output := resolveOutputPath(hlsDir, streamID, cfg.OutputDir)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("thumbnail: create output dir %q: %w", outDir, err)
	}

	// Stop any existing worker before swapping in the new one — covers
	// the Update path where the operator changed interval / size live.
	s.Stop(streamID)

	workerCtx, cancel := context.WithCancel(ctx)
	w := &worker{
		cancel:     cancel,
		done:       make(chan struct{}),
		outputPath: output,
	}

	s.mu.Lock()
	s.workers[streamID] = w
	ffmpegPath := s.tcCfg.FFmpegPath
	if ffmpegPath == "" {
		ffmpegPath = domain.DefaultFFmpegPath
	}
	s.mu.Unlock()

	sub, err := s.buf.Subscribe(bufferID)
	if err != nil {
		s.mu.Lock()
		delete(s.workers, streamID)
		s.mu.Unlock()
		cancel()
		return fmt.Errorf("thumbnail: subscribe buffer %q: %w", bufferID, err)
	}

	go func() {
		defer close(w.done)
		defer s.buf.Unsubscribe(bufferID, sub)
		s.pump(workerCtx, streamID, sub, ffmpegPath, output, cfg)
	}()

	slog.Info("thumbnail: worker started",
		"stream_code", streamID, "buffer_id", bufferID,
		"output", output, "interval_sec", effectiveInterval(cfg),
	)
	return nil
}

// Stop tears down the per-stream worker. Idempotent.
func (s *Service) Stop(streamID domain.StreamCode) {
	s.mu.Lock()
	w, ok := s.workers[streamID]
	if ok {
		delete(s.workers, streamID)
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	w.cancel()
	// Wait for the pump goroutine to exit so the FFmpeg subprocess is
	// reaped before the caller starts a replacement (Update path) or
	// the test harness checks for goroutine leaks.
	<-w.done
	slog.Info("thumbnail: worker stopped", "stream_code", streamID)
}

// LatestPath returns the absolute filesystem path to the most recent
// JPEG for streamID, or "" when no worker is running. The file may not
// yet exist (worker has just started and FFmpeg hasn't produced its
// first frame) — handlers should serve 404 in that case.
func (s *Service) LatestPath(streamID domain.StreamCode) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.workers[streamID]
	if !ok {
		return ""
	}
	return w.outputPath
}

// pump is the per-stream worker goroutine. It launches an FFmpeg child
// reading from a stdin pipe, copies buffer.Subscriber chunks into that
// pipe, and watches for ffmpeg exit / context cancel. Atomic JPEG
// writes are achieved by directing FFmpeg to a `.tmp` path and renaming
// after each frame completion (see watchAndRename below).
func (s *Service) pump(
	ctx context.Context,
	streamID domain.StreamCode,
	sub *buffer.Subscriber,
	ffmpegPath, output string,
	cfg *domain.ThumbnailConfig,
) {
	tmpPath := output + ".tmp"
	args := buildFFmpegArgs(tmpPath, cfg)

	cmd := exec.CommandContext(ctx, ffmpegPath, args...) //nolint:gosec // ffmpegPath is operator-trusted
	stdin, err := cmd.StdinPipe()
	if err != nil {
		slog.Warn("thumbnail: stdin pipe", "stream_code", streamID, "err", err)
		return
	}
	cmd.Stderr = io.Discard // Thumbnail FFmpeg is verbose with "frame=" lines; silence to keep logs readable
	if err := cmd.Start(); err != nil {
		slog.Warn("thumbnail: ffmpeg start failed",
			"stream_code", streamID, "ffmpeg_path", ffmpegPath, "err", err)
		_ = stdin.Close()
		return
	}

	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		feedSubscriber(stdin, sub)
		_ = stdin.Close()
	}()

	// Watch tmpPath for size changes; on every "ffmpeg finished writing"
	// signal, atomic-rename to output. Polling is fine here — interval
	// is seconds, fsnotify dependency would be heavy for the use case.
	renameCtx, renameCancel := context.WithCancel(ctx)
	renameDone := make(chan struct{})
	go func() {
		defer close(renameDone)
		watchAndRename(renameCtx, tmpPath, output)
	}()

	if err := cmd.Wait(); err != nil && ctx.Err() == nil {
		slog.Debug("thumbnail: ffmpeg exited", "stream_code", streamID, "err", err)
	}
	renameCancel()
	<-pumpDone
	<-renameDone
	_ = os.Remove(tmpPath) // best-effort cleanup; harmless if absent
}

// feedSubscriber copies every TS chunk from the buffer subscriber into
// FFmpeg's stdin until the channel closes (stream torn down) or stdin
// stops accepting writes (ffmpeg crashed). Skips empty / non-TS packets
// — thumbnail only makes sense for raw-TS playback buffers.
func feedSubscriber(stdin io.WriteCloser, sub *buffer.Subscriber) {
	for pkt := range sub.Recv() {
		if len(pkt.TS) == 0 {
			continue
		}
		if _, err := stdin.Write(pkt.TS); err != nil {
			return
		}
	}
}

// watchAndRename polls tmpPath every renamePollInterval. When the file
// exists with non-zero size AND its mtime changed since the last rename,
// rename it onto outputPath atomically (cross-fs renames don't apply
// here — both paths share a directory).
func watchAndRename(ctx context.Context, tmpPath, outputPath string) {
	var lastMtime time.Time
	tick := time.NewTicker(renamePollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			info, err := os.Stat(tmpPath)
			if err != nil || info.Size() == 0 {
				continue
			}
			if info.ModTime().Equal(lastMtime) {
				continue
			}
			if err := os.Rename(tmpPath, outputPath); err != nil {
				slog.Debug("thumbnail: rename failed", "tmp", tmpPath, "err", err)
				continue
			}
			lastMtime = info.ModTime()
		}
	}
}

// renamePollInterval is how often we look at tmpPath for changes. Keeps
// the rename lag bounded at about half a second regardless of the
// caller's IntervalSec — that's well under the slowest reasonable
// operator polling rate.
const renamePollInterval = 500 * time.Millisecond

// resolveOutputPath returns (dir, file) where dir is the directory to
// MkdirAll and file is the absolute path FFmpeg writes to. Defaults
// match the docstring on ThumbnailConfig: `{hls_dir}/{streamCode}/thumbnails/thumb.jpg`.
func resolveOutputPath(hlsDir string, streamID domain.StreamCode, customSubdir string) (dir, file string) {
	subdir := customSubdir
	if subdir == "" {
		subdir = "thumbnails"
	}
	dir = filepath.Join(hlsDir, string(streamID), subdir)
	file = filepath.Join(dir, "thumb.jpg")
	return dir, file
}

// effectiveInterval clamps user-supplied IntervalSec to a positive
// minimum so we never end up with `fps=1/0` (DivByZero in ffmpeg →
// process exit immediately).
func effectiveInterval(cfg *domain.ThumbnailConfig) int {
	v := cfg.IntervalSec
	if v <= 0 {
		v = domain.DefaultThumbnailIntervalSec
	}
	return v
}

// effectiveQuality clamps Quality to FFmpeg's valid mjpeg q:v range
// (1 best, 31 worst). Default 5 matches docs.
func effectiveQuality(cfg *domain.ThumbnailConfig) int {
	q := cfg.Quality
	if q <= 0 {
		q = domain.DefaultThumbnailQuality
	}
	if q < 1 {
		q = 1
	}
	if q > 31 {
		q = 31
	}
	return q
}

// buildFFmpegArgs constructs the argv list for one thumbnail worker.
// Pure function — no I/O — so unit tests can pin the exact command line
// without exec.Command machinery.
//
//	-fflags +genpts+discardcorrupt   tolerate noisy upstream PTS
//	-i pipe:0                         read TS from stdin
//	-vf fps=1/N[,scale=W:H]           one frame per N seconds, optional resize
//	-q:v Q                            mjpeg quality
//	-f image2 -update 1               continuously overwrite the output file
//	-y outputPath                     atomic-friendly via the .tmp + rename wrapper
func buildFFmpegArgs(outputPath string, cfg *domain.ThumbnailConfig) []string {
	interval := effectiveInterval(cfg)
	quality := effectiveQuality(cfg)

	vf := "fps=1/" + strconv.Itoa(interval)
	if cfg.Width > 0 || cfg.Height > 0 {
		// scale's -1 keeps the input dimension when only one of W/H is
		// set; -2 forces evenness (mjpeg encoder is fine with odd, but
		// downstream consumers occasionally choke).
		w := "-2"
		h := "-2"
		if cfg.Width > 0 {
			w = strconv.Itoa(cfg.Width)
		}
		if cfg.Height > 0 {
			h = strconv.Itoa(cfg.Height)
		}
		vf += ",scale=" + w + ":" + h
	}

	return []string{
		"-hide_banner",
		"-loglevel", "error",
		"-fflags", "+genpts+discardcorrupt",
		"-f", "mpegts",
		"-i", "pipe:0",
		"-vf", vf,
		"-q:v", strconv.Itoa(quality),
		"-f", "image2",
		"-update", "1",
		"-y",
		outputPath,
	}
}
