package transcoder

// pure_helpers_test.go covers stdlib-only pure helpers used by the
// service / probe / worker code paths. Each function is small and
// self-contained — exercising them keeps regressions from sneaking in
// during refactors of the surrounding goroutine-heavy logic.

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// ─── probe.trimOutput ────────────────────────────────────────────────────────

func TestTrimOutput_ShortPassesThrough(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "ffmpeg version 7.0", trimOutput([]byte("ffmpeg version 7.0\n")))
}

func TestTrimOutput_TrimsLeadingAndTrailingWhitespace(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "hello", trimOutput([]byte("  \t\nhello\n  \t")))
}

func TestTrimOutput_TruncatesLongInput(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 1024)
	got := trimOutput([]byte(long))
	assert.True(t, strings.HasSuffix(got, "… (truncated)"))
	// Original maxLen=512; trimmed body keeps 512 bytes plus the truncation hint.
	assert.Equal(t, 512+len("… (truncated)"), len(got))
}

func TestTrimOutput_EmptyInput(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "", trimOutput(nil))
	assert.Equal(t, "", trimOutput([]byte("")))
	assert.Equal(t, "", trimOutput([]byte("    \n\t")))
}

// ─── worker_run.minDuration ──────────────────────────────────────────────────

func TestMinDuration(t *testing.T) {
	t.Parallel()
	assert.Equal(t, time.Second, minDuration(time.Second, 2*time.Second))
	assert.Equal(t, time.Second, minDuration(2*time.Second, time.Second))
	assert.Equal(t, time.Duration(0), minDuration(0, 0))
	// Equal values: returns either (operator chose `<` so equality keeps b).
	assert.Equal(t, time.Second, minDuration(time.Second, time.Second))
	// Negative inputs (degenerate but mathematically defined).
	assert.Equal(t, -time.Second, minDuration(-time.Second, time.Second))
}

// ─── pipe_size_other.setFFmpegStdinPipeSize ──────────────────────────────────
//
// On non-Linux platforms (macOS/BSD developer builds) the helper is a no-op
// — the build tag ensures only this stub compiles. The test asserts the
// helper accepts an io.WriteCloser without panicking and leaves it usable.
//
// On Linux the test builds against pipe_size_linux.go (real fcntl call); we
// keep the assertion intentionally weak so it works on either platform —
// the real-thing exercise is left to integration tests that spawn ffmpeg.

type nopWriteCloser struct{ bytes.Buffer }

func (nopWriteCloser) Close() error { return nil }

func TestSetFFmpegStdinPipeSize_HandlesArbitraryWriter(t *testing.T) {
	t.Parallel()
	// Must not panic on a non-pipe writer; both the macOS no-op and the
	// Linux fcntl path silently swallow errors when the fd isn't a pipe.
	w := &nopWriteCloser{}
	setFFmpegStdinPipeSize(w)
	// The writer must remain usable afterwards.
	_, err := w.Write([]byte("ok"))
	assert.NoError(t, err)
}
