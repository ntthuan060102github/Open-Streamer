package pull_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-streamer/open-streamer/internal/domain"
	"github.com/open-streamer/open-streamer/internal/ingestor/pull"
)

func TestHTTPReader_ReadsBody(t *testing.T) {
	t.Parallel()

	want := []byte("raw-mpeg-ts-bytes-from-http-stream")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(want) //nolint:errcheck
	}))
	defer srv.Close()

	r := pull.NewHTTPReader(domain.Input{URL: srv.URL})
	require.NoError(t, r.Open(context.Background()))
	defer r.Close()

	got := readAll(t, r)
	assert.Equal(t, want, got)
}

func TestHTTPReader_BasicAuth(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "alice" || pass != "secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Write([]byte("authenticated")) //nolint:errcheck
	}))
	defer srv.Close()

	// Credentials are passed via Headers using a standard Basic auth value.
	// base64("alice:secret") = "YWxpY2U6c2VjcmV0"
	r := pull.NewHTTPReader(domain.Input{
		URL: srv.URL,
		Headers: map[string]string{
			"Authorization": "Basic YWxpY2U6c2VjcmV0",
		},
	})
	require.NoError(t, r.Open(context.Background()))
	defer r.Close()

	got := readAll(t, r)
	assert.Equal(t, []byte("authenticated"), got)
}

func TestHTTPReader_Non200_ReturnsError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	r := pull.NewHTTPReader(domain.Input{URL: srv.URL})
	err := r.Open(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "404")
}

func TestHTTPReader_ConnectFails_ReturnsError(t *testing.T) {
	t.Parallel()

	r := pull.NewHTTPReader(domain.Input{URL: "http://127.0.0.1:19999/unreachable"})
	err := r.Open(context.Background())
	require.Error(t, err)
}

func TestHTTPReader_ContextCancelledBeforeOpen(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("data")) //nolint:errcheck
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r := pull.NewHTTPReader(domain.Input{URL: srv.URL})
	err := r.Open(ctx)
	require.Error(t, err)
}

func TestHTTPReader_ReturnsEOFAtEnd(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("small")) //nolint:errcheck
	}))
	defer srv.Close()

	r := pull.NewHTTPReader(domain.Input{URL: srv.URL})
	require.NoError(t, r.Open(context.Background()))
	defer r.Close()

	// Drain until EOF.
	var lastErr error
	for {
		_, err := r.Read(context.Background())
		if err != nil {
			lastErr = err
			break
		}
	}
	assert.ErrorIs(t, lastErr, io.EOF)
}
