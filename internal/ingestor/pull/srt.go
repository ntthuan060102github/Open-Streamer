package pull

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"time"

	srt "github.com/datarhei/gosrt"
	"github.com/open-streamer/open-streamer/internal/domain"
)

const srtReadChunk = 188 * 7 // one SRT datagram = 1316 bytes = 7 TS packets

// SRTReader dials an SRT server in caller mode and reads raw MPEG-TS.
// SRT natively carries MPEG-TS so no codec conversion is needed.
// URL format: srt://host:port?streamid=key&latency=120
type SRTReader struct {
	input domain.Input
	conn  srt.Conn
	buf   []byte
}

// NewSRTReader constructs an SRTReader.
func NewSRTReader(input domain.Input) *SRTReader {
	return &SRTReader{input: input, buf: make([]byte, srtReadChunk)}
}

func (r *SRTReader) Open(ctx context.Context) error {
	u, err := url.Parse(r.input.URL)
	if err != nil {
		return fmt.Errorf("srt reader: parse url: %w", err)
	}

	cfg := srt.DefaultConfig()
	cfg.StreamId = u.Query().Get("streamid")
	if lat := u.Query().Get("latency"); lat != "" {
		var ms int
		if _, err := fmt.Sscanf(lat, "%d", &ms); err == nil {
			cfg.Latency = time.Duration(ms) * time.Millisecond
		}
	}

	// Strip query from the address used for dialing.
	host := u.Host
	conn, err := srt.Dial("srt", host, cfg)
	if err != nil {
		return fmt.Errorf("srt reader: dial %q: %w", host, err)
	}
	r.conn = conn
	return nil
}

func (r *SRTReader) Read(ctx context.Context) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	n, err := r.conn.Read(r.buf)
	if n > 0 {
		out := make([]byte, n)
		copy(out, r.buf[:n])
		return out, nil
	}
	if err == io.EOF {
		return nil, io.EOF
	}
	if err != nil {
		return nil, fmt.Errorf("srt reader: read: %w", err)
	}
	return nil, nil
}

func (r *SRTReader) Close() error {
	if r.conn != nil {
		return r.conn.Close()
	}
	return nil
}
