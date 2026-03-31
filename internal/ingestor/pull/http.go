package pull

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/open-streamer/open-streamer/internal/domain"
)

const httpReadChunk = 188 * 56

// HTTPReader pulls a raw MPEG-TS byte-stream from an HTTP or HTTPS URL.
// This handles live streams published over plain HTTP (e.g. MPEG-TS over HTTP,
// MPEG-TS over HTTPS from a CDN origin).  For HLS playlists (.m3u8) use
// HLSReader instead; this reader expects the response body to be a continuous
// byte stream, not a playlist.
type HTTPReader struct {
	input  domain.Input
	client *http.Client
	body   io.ReadCloser
	buf    []byte
}

// NewHTTPReader constructs an HTTPReader for the given input.
func NewHTTPReader(input domain.Input) *HTTPReader {
	return &HTTPReader{
		input: input,
		buf:   make([]byte, httpReadChunk),
	}
}

func (r *HTTPReader) Open(ctx context.Context) error {
	r.client = &http.Client{Timeout: 0} // streaming — no global timeout

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.input.URL, nil)
	if err != nil {
		return fmt.Errorf("http reader: build request: %w", err)
	}
	for k, v := range r.input.Headers {
		req.Header.Set(k, v)
	}

	// Set a connect-level timeout using a transport.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 15 * time.Second
	r.client.Transport = transport

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("http reader: connect to %q: %w", r.input.URL, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return fmt.Errorf("http reader: server returned %d for %q", resp.StatusCode, r.input.URL)
	}

	r.body = resp.Body
	return nil
}

func (r *HTTPReader) Read(ctx context.Context) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	n, err := r.body.Read(r.buf)
	if n > 0 {
		out := make([]byte, n)
		copy(out, r.buf[:n])
		return out, nil
	}
	if err == io.EOF {
		return nil, io.EOF
	}
	return nil, fmt.Errorf("http reader: read body: %w", err)
}

func (r *HTTPReader) Close() error {
	if r.body != nil {
		return r.body.Close()
	}
	return nil
}

