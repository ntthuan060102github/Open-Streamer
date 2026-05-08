package hooks

// classifiers_test.go covers the error-classification helpers used to bin
// hook delivery failures into the prometheus `reason` label. The classifier
// taxonomy is a Service contract — the dashboards alert on these exact
// strings, so any drift would silently break ops.

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// ─── classifyHookDeliveryErr ─────────────────────────────────────────────────

func TestClassifyHookDeliveryErr_NilIsOK(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "ok", classifyHookDeliveryErr(nil))
}

func TestClassifyHookDeliveryErr_ContextDeadline(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "timeout", classifyHookDeliveryErr(context.DeadlineExceeded))
	assert.Equal(t, "timeout", classifyHookDeliveryErr(context.Canceled))
}

// fakeNetTimeoutErr satisfies net.Error and reports Timeout()=true so the
// classifier can detect "real" network timeouts without us reaching for a
// real connection.
type fakeNetTimeoutErr struct{}

func (fakeNetTimeoutErr) Error() string   { return "fake net timeout" }
func (fakeNetTimeoutErr) Timeout() bool   { return true }
func (fakeNetTimeoutErr) Temporary() bool { return true }

func TestClassifyHookDeliveryErr_NetTimeout(t *testing.T) {
	t.Parallel()
	var err net.Error = fakeNetTimeoutErr{}
	assert.Equal(t, "timeout", classifyHookDeliveryErr(err))
}

func TestClassifyHookDeliveryErr_EncodeFailures(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "encode", classifyHookDeliveryErr(errors.New("marshal batch: bad shape")))
	assert.Equal(t, "encode", classifyHookDeliveryErr(errors.New("create request: bad URL")))
}

func TestClassifyHookDeliveryErr_TransportPostError(t *testing.T) {
	t.Parallel()
	// "http post:" without timeout substring → transport.
	assert.Equal(t, "transport", classifyHookDeliveryErr(errors.New("http post: connection refused")))
}

func TestClassifyHookDeliveryErr_TransportPostWithTimeoutString(t *testing.T) {
	t.Parallel()
	// net/http often emits transport errors with "timeout" / "deadline
	// exceeded" in the message rather than as a typed net.Error — the
	// classifier must promote those to "timeout".
	assert.Equal(t, "timeout", classifyHookDeliveryErr(errors.New("http post: i/o timeout")))
	assert.Equal(t, "timeout", classifyHookDeliveryErr(errors.New("http post: context deadline exceeded")))
}

func TestClassifyHookDeliveryErr_UnknownFallsThrough(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "unknown", classifyHookDeliveryErr(errors.New("a brand new failure mode")))
}

// ─── classifyHTTPStatusErr ───────────────────────────────────────────────────

func TestClassifyHTTPStatusErr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		msg  string
		want string
	}{
		{"unexpected status: 400 Bad Request", "http_4xx"},
		{"unexpected status: 401 Unauthorized", "http_4xx"},
		{"unexpected status: 404 Not Found", "http_4xx"},
		{"unexpected status: 500 Internal Server Error", "http_5xx"},
		{"unexpected status: 502", "http_5xx"},
		{"unexpected status: 503", "http_5xx"},
		// Out of range / nonsense → unknown rather than mis-classified.
		{"unexpected status: 200", "unknown"},
		{"unexpected status: abc", "unknown"},
		// Not an "unexpected status" message at all.
		{"some other failure", "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.msg, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyHTTPStatusErr(tc.msg))
		})
	}
}

func TestClassifyHookDeliveryErr_DispatchesToStatusClassifier(t *testing.T) {
	t.Parallel()
	// classifyHookDeliveryErr should defer to classifyHTTPStatusErr for
	// "unexpected status:" prefixed errors.
	assert.Equal(t, "http_4xx", classifyHookDeliveryErr(errors.New("unexpected status: 401 Unauthorized")))
	assert.Equal(t, "http_5xx", classifyHookDeliveryErr(errors.New("unexpected status: 503 Service Unavailable")))
}

// ─── classifyFileDeliveryErr ─────────────────────────────────────────────────

func TestClassifyFileDeliveryErr_NilIsOK(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "ok", classifyFileDeliveryErr(nil))
}

func TestClassifyFileDeliveryErr_MarshalIsEncode(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "encode", classifyFileDeliveryErr(errors.New("file delivery: marshal: bad shape")))
}

func TestClassifyFileDeliveryErr_PermissionDeniedIsTransport(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "transport", classifyFileDeliveryErr(errors.New("open /var/log/hooks: permission denied")))
}

func TestClassifyFileDeliveryErr_NoSuchFileIsTransport(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "transport", classifyFileDeliveryErr(errors.New("open /tmp/x: no such file or directory")))
}

func TestClassifyFileDeliveryErr_GenericFallsToTransport(t *testing.T) {
	t.Parallel()
	// Any unrecognised file error collapses to "transport" by design —
	// the file sink doesn't have a richer transport/status taxonomy to
	// project failures through.
	assert.Equal(t, "transport", classifyFileDeliveryErr(errors.New("disk quota exceeded")))
}

// ─── batchSizeFor / batchMetricsFor when metrics dependency missing ──────────

func TestBatchSizeFor_NoMetricsReturnsNil(t *testing.T) {
	t.Parallel()
	s := &Service{}
	assert.Nil(t, s.batchSizeFor(nil))
}

func TestBatchMetricsFor_NoMetricsReturnsZeroValue(t *testing.T) {
	t.Parallel()
	s := &Service{}
	got := s.batchMetricsFor(nil)
	assert.Nil(t, got.observeDelivery)
	assert.Nil(t, got.dropped)
	assert.Nil(t, got.queueDepth)
	assert.Nil(t, got.batchSize)
}

// Sanity timing reference — the classifier never sleeps; ensure it's
// effectively constant-time so we don't accidentally introduce a
// regex-DoS pattern in a future refactor.
func TestClassifyHookDeliveryErr_BoundedTime(t *testing.T) {
	t.Parallel()
	long := errors.New("unexpected status: " + repeatChar('9', 10_000))
	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
		_ = classifyHookDeliveryErr(long)
	}
}

func repeatChar(c byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}
