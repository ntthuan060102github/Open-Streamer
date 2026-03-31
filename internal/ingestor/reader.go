// Package ingestor handles raw stream ingestion.
//
// The ingestor accepts a plain URL from the user and automatically derives
// the protocol from the URL scheme — no protocol configuration is needed.
//
// Pull mode (server connects to remote source):
//   - UDP MPEG-TS  → native Go net.PacketConn
//   - HLS playlist → native Go HTTP client + M3U8 parser (grafov/m3u8)
//   - Local file   → native Go os.Read (with optional loop)
//   - HTTP stream  → native Go http.Client (raw MPEG-TS over HTTP/HTTPS)
//   - SRT pull     → native gosrt.Dial (SRT carries MPEG-TS natively)
//   - RTMP pull    → native yapingcat/gomedia RTMP client + TSMuxer
//   - RTSP pull    → native yapingcat/gomedia RTSP client + TSMuxer
//   - S3           → AWS SDK v2 GetObject (s3://bucket/key)
//
// Push mode (external encoder connects to our server):
//   - RTMP → RTMPServer (gomedia RTMP server handle, native FLV→MPEG-TS)
//   - SRT  → SRTServer  (gosrt native, MPEG-TS passthrough)
//
// Push mode is auto-detected: if the URL host is 0.0.0.0 / :: and the scheme
// is rtmp or srt, the ingestor starts/uses the shared push server instead of
// a pull worker.
package ingestor

import (
	"context"
	"fmt"

	"github.com/open-streamer/open-streamer/config"
	"github.com/open-streamer/open-streamer/internal/domain"
	"github.com/open-streamer/open-streamer/internal/ingestor/pull"
	"github.com/open-streamer/open-streamer/pkg/protocol"
)

// Reader is implemented by all pull-mode ingest sources.
// Each Read call returns one chunk of raw MPEG-TS data.
// Implementations must be called from a single goroutine only.
type Reader interface {
	// Open establishes the connection or opens the resource.
	// Must be called once before the first Read.
	Open(ctx context.Context) error

	// Read blocks until the next MPEG-TS chunk is available.
	// Returns io.EOF when the source ends gracefully.
	Read(ctx context.Context) ([]byte, error)

	// Close releases all resources held by the reader.
	// May be called more than once; subsequent calls are no-ops.
	Close() error
}

// NewReader constructs the appropriate Reader for the given Input.
// The protocol is derived from the URL scheme — no explicit protocol
// configuration is required on the caller's side.
//
// Returns an error when:
//   - The URL scheme is unrecognised
//   - The URL describes a push-listen address (handled by the push servers)
func NewReader(input domain.Input, cfg config.IngestorConfig) (Reader, error) {
	if protocol.IsPushListen(input.URL) {
		return nil, fmt.Errorf(
			"ingestor: %q is a push-listen address — handled by the push server, not a pull reader",
			input.URL,
		)
	}

	kind := protocol.Detect(input.URL)

	switch kind {
	case protocol.KindUDP:
		return pull.NewUDPReader(input), nil

	case protocol.KindHLS:
		return pull.NewHLSReader(input, cfg), nil

	case protocol.KindFile:
		return pull.NewFileReader(input), nil

	case protocol.KindHTTP:
		return pull.NewHTTPReader(input), nil

	case protocol.KindSRT:
		return pull.NewSRTReader(input), nil

	case protocol.KindRTMP:
		return pull.NewRTMPReader(input), nil

	case protocol.KindRTSP:
		return pull.NewRTSPReader(input), nil

	case protocol.KindS3:
		return pull.NewS3Reader(input), nil

	default:
		return nil, fmt.Errorf(
			"ingestor: cannot infer protocol from URL %q — unsupported scheme",
			input.URL,
		)
	}
}
