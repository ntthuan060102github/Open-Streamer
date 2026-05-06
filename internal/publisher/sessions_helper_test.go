package publisher

// sessions_helper_test.go — guards the playSession bytes-credit semantics.
// The two paths must stay distinct:
//
//   - add() / close()           → RTMP, SRT, HLS where the publisher knows
//                                 the byte count at write time
//   - closeWithBytes(n)         → RTSP where the underlying library reports
//                                 the cumulative byte total only at session
//                                 end (gortsplib SessionStats.OutboundBytes)
//
// Mixing the two on the same session would double-count, hence the close*
// methods are mutually exclusive in usage. The fake closer below records
// the value handed to Close so the test can assert the right path was taken.

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// fakeCloser captures whatever Close is invoked with so the tests can assert
// (a) Close was called exactly once, (b) the byte total is the value the
// caller intended to credit. Implements sessions.Closer.
type fakeCloser struct {
	mu         sync.Mutex
	calls      int
	touchCalls int
	lastBytes  int64
	lastReas   domain.SessionCloseReason
}

func (c *fakeCloser) Close(reason domain.SessionCloseReason, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.lastBytes = bytes
	c.lastReas = reason
}

func (c *fakeCloser) Touch() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.touchCalls++
}

func TestPlaySessionCloseUsesAccumulatedBytes(t *testing.T) {
	t.Parallel()
	fc := &fakeCloser{}
	p := &playSession{closer: fc}

	p.add(1024)
	p.add(2048)
	p.close()

	require.Equal(t, 1, fc.calls)
	require.Equal(t, int64(3072), fc.lastBytes,
		"close() must credit the sum of add() calls")
	require.Equal(t, domain.SessionCloseClient, fc.lastReas)
}

// closeWithBytes is the RTSP path — overrides whatever was in the bytes
// counter (which RTSP never increments) with the externally-measured total
// from gortsplib SessionStats. Without this, every RTSP session record
// would show bytes=0.
func TestPlaySessionCloseWithBytesUsesExternalCount(t *testing.T) {
	t.Parallel()
	fc := &fakeCloser{}
	p := &playSession{closer: fc}

	// Even if add() was called for some reason, closeWithBytes ignores the
	// internal counter — the external value is the source of truth.
	p.add(999)
	p.closeWithBytes(5_242_880) // 5 MiB

	require.Equal(t, 1, fc.calls)
	require.Equal(t, int64(5_242_880), fc.lastBytes,
		"closeWithBytes must use the supplied total, not the internal counter")
}

// Negative byte totals are silently clamped to 0 — defensive guard against
// a future caller passing `int64(stats.OutboundBytes)` after a uint64
// underflow somewhere upstream. Better to record 0 than negative bandwidth.
func TestPlaySessionCloseWithBytesClampsNegative(t *testing.T) {
	t.Parallel()
	fc := &fakeCloser{}
	p := &playSession{closer: fc}

	p.closeWithBytes(-100)

	require.Equal(t, int64(0), fc.lastBytes)
}

// Disabled session (tracker not configured) must not call the closer at
// all — the no-op contract covers both close paths.
func TestPlaySessionCloseWithBytesNoopWhenDisabled(t *testing.T) {
	t.Parallel()
	fc := &fakeCloser{}
	p := &playSession{closer: fc, disable: true}

	p.closeWithBytes(1024)

	require.Equal(t, 0, fc.calls,
		"disabled session must not invoke closer.Close on either path")
}

// Nil closer must be a safe no-op — happens for short-lived sessions that
// never fully open against the tracker (e.g. RTSP DESCRIBE without PLAY).
func TestPlaySessionCloseWithBytesNoopWhenCloserNil(t *testing.T) {
	t.Parallel()
	p := &playSession{closer: nil}
	require.NotPanics(t, func() { p.closeWithBytes(1024) })
}

// add() must call Touch on the underlying closer so the idle reaper does
// not close a healthy long-running RTMP / SRT session as "idle". Without
// this, sessions disappear from the tracker after `idle_timeout_sec`
// (default 30s) even while bytes are flowing.
func TestPlaySessionAddTouchesCloser(t *testing.T) {
	t.Parallel()
	fc := &fakeCloser{}
	p := &playSession{closer: fc}

	p.add(1024)

	require.Equal(t, int64(1024), p.bytes.Load(), "bytes must accumulate")
	require.Equal(t, 1, fc.touchCalls, "first add() must touch")
}

// Touch is throttled — calling add() repeatedly within the throttle window
// must not hammer the tracker mutex. The throttle is what keeps the
// per-frame hot path (RTMP routinely emits hundreds of frames/sec) from
// contending the sessions map lock.
func TestPlaySessionTouchIsThrottled(t *testing.T) {
	t.Parallel()
	fc := &fakeCloser{}
	p := &playSession{closer: fc}

	// 100 add() calls in tight succession — only one should trigger Touch
	// because the throttle window is 5 s and the calls happen in microseconds.
	for i := 0; i < 100; i++ {
		p.add(64)
	}

	require.Equal(t, int64(64*100), p.bytes.Load(), "bytes must accumulate every call")
	require.Equal(t, 1, fc.touchCalls,
		"throttle must collapse 100 rapid touches into a single tracker call")
}

// Disabled / nil-closer session must not panic when add() is called from
// the hot path — the noop guard covers the byte counter AND the touch.
func TestPlaySessionAddNoopWhenDisabled(t *testing.T) {
	t.Parallel()
	fc := &fakeCloser{}
	p := &playSession{closer: fc, disable: true}

	p.add(1024)

	require.Equal(t, int64(0), p.bytes.Load())
	require.Equal(t, 0, fc.touchCalls)
}
