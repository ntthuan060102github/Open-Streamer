package transcoder

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/ntt0601zcoder/open-streamer/config"
	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// TestRunOnce_CleanExitWithCtxAliveIsCrashed — the production-bug
// reproducer. A child that exits code 0 while the worker's context is
// still alive must be reported as a crash so the surrounding loop
// restarts. Before this fix, an upstream stall (raw-ingest subscriber
// chan blip, source URL briefly 404) let FFmpeg drain its stdin pipe,
// exit cleanly, and the worker treated that as a graceful shutdown —
// retiring the transcoder for the rest of the stream's lifetime even
// after ingest resumed. Cascade: every downstream consumer of the
// rendition buffer (publisher, ABR-copy tap, SRT/RTMP/RTSP republish
// listeners) blocked indefinitely on the now-orphaned buffer.
func TestRunOnce_CleanExitWithCtxAliveIsCrashed(t *testing.T) {
	const truePath = "/usr/bin/true"
	if _, err := exec.LookPath(truePath); err != nil {
		t.Skipf("%s not available: %v", truePath, err)
	}

	buf := buffer.NewServiceForTesting(8)
	streamID := domain.StreamCode("test-clean-exit")
	buf.Create(streamID)

	sub, err := buf.Subscribe(streamID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	// Unsubscribe immediately so the stdin-writer goroutine inside
	// runOnce sees a closed channel on its first Recv and returns; the
	// test would otherwise hang on stdinWG.Wait() because /usr/bin/true
	// never reads stdin and never makes the writer's Write fail.
	buf.Unsubscribe(streamID, sub)

	s := &Service{
		cfg: config.TranscoderConfig{FFmpegPath: truePath},
		buf: buf,
	}

	done := make(chan struct {
		crashed bool
		err     error
	}, 1)
	go func() {
		c, e := s.runOnce(context.Background(), streamID, streamID, "track_0", sub, nil)
		done <- struct {
			crashed bool
			err     error
		}{c, e}
	}()

	select {
	case res := <-done:
		if !res.crashed {
			t.Errorf("expected crashed=true when child exits code 0 with ctx still alive, got crashed=false (err=%v)", res.err)
		}
		if res.err == nil {
			t.Error("expected a non-nil error describing the unexpected clean exit")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runOnce did not return within 3s — stdin-writer or stdout-reader goroutine is stuck")
	}
}

// TestRunOnce_CtxCancelReturnsGraceful — cancelling the worker's ctx
// must make runOnce return crashed=false regardless of how the child
// process exited. The SIGKILL that exec.CommandContext delivers on
// cancellation produces a non-nil cmd.Wait() error; that error is the
// CONSEQUENCE of our cancellation, not a real fault, so the loop must
// not interpret it as a crash worthy of restart.
func TestRunOnce_CtxCancelReturnsGraceful(t *testing.T) {
	// Use /bin/sleep directly — NOT /bin/sh -c "sleep N", because
	// exec.CommandContext only SIGKILLs the direct child. With a shell
	// wrapper the grandchild sleep inherits stdout/stderr fds, keeps the
	// pipe open after sh dies, and stdout.Read inside runOnce blocks
	// indefinitely (observed as a CI hang on ubuntu-latest where the
	// 3 s timeout fired before the orphan sleep's pipe was reaped).
	const sleepPath = "/bin/sleep"
	if _, err := exec.LookPath(sleepPath); err != nil {
		t.Skipf("%s not available: %v", sleepPath, err)
	}

	buf := buffer.NewServiceForTesting(8)
	streamID := domain.StreamCode("test-ctx-cancel")
	buf.Create(streamID)
	sub, err := buf.Subscribe(streamID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	buf.Unsubscribe(streamID, sub) // unblock the stdin writer

	s := &Service{
		cfg: config.TranscoderConfig{FFmpegPath: sleepPath},
		buf: buf,
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct {
		crashed bool
		err     error
	}, 1)
	go func() {
		c, e := s.runOnce(ctx, streamID, streamID, "track_0", sub, []string{"30"})
		done <- struct {
			crashed bool
			err     error
		}{c, e}
	}()

	// Give the child a brief window to spin up before we cancel — that
	// way exec.CommandContext actually has a live process to SIGKILL,
	// exercising the path where cmd.Wait() returns a non-nil error.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case res := <-done:
		if res.crashed {
			t.Errorf("expected crashed=false when ctx is cancelled, got crashed=true (err=%v)", res.err)
		}
		if res.err != nil {
			t.Errorf("expected nil error on graceful ctx-cancel exit, got: %v", res.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runOnce did not return within 5s after ctx cancellation")
	}
	// Sanity-check the assumed precondition: ctx really was cancelled.
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Errorf("expected ctx.Err() == context.Canceled, got %v", ctx.Err())
	}
}
