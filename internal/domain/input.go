package domain

// Input is a single ingest source for a stream.
// Multiple inputs can be configured; the Stream Manager selects the active one
// based on Priority and runtime health (Alive flag).
//
// The only required field is URL.  The ingestor derives the ingest protocol
// and connection mode (pull vs push-listen) automatically from the URL scheme
// and host — no additional protocol configuration is needed.
//
// Supported URL formats:
//
//	Pull (server connects to remote source):
//	  rtmp://server.com/live/stream_key       RTMP pull from remote
//	  rtsp://camera.local:554/stream          RTSP pull (IP camera)
//	  http://cdn.example.com/live.ts          HTTP MPEG-TS stream
//	  https://cdn.example.com/playlist.m3u8   HLS pull (grafov m3u8 parser)
//	  udp://239.1.1.1:5000                    UDP multicast MPEG-TS
//	  srt://relay.example.com:9999            SRT pull (caller mode)
//	  file:///recordings/source.ts            local file (loops if ?loop=true)
//
//	Push (external encoder connects to our server):
//	  rtmp://0.0.0.0:1935/live/stream_key     RTMP push — our RTMP server listens
//	  srt://0.0.0.0:9999?streamid=stream_key  SRT push  — our SRT server listens
//
// Push mode is detected automatically when the URL host is a wildcard address
// (0.0.0.0, ::, empty) and the scheme is rtmp or srt.
type Input struct {
	// URL is the source endpoint. See the package doc for supported formats.
	URL string `json:"url" yaml:"url"`

	// Priority determines failover order. Lower value = higher priority.
	// The Stream Manager always prefers the lowest-priority alive input.
	Priority int `json:"priority" yaml:"priority"`

	// Headers are arbitrary HTTP headers sent with every request for HTTP/HLS inputs.
	// Common uses:
	//   "Authorization": "Bearer <token>"
	//   "Authorization": "Basic <base64(user:pass)>"
	//   "X-Custom-Token": "secret"
	Headers map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`

	// Params are extra URL query parameters merged into the source URL before
	// connecting. Used for protocols that carry credentials or options in the
	// query string (SRT ?passphrase=, S3 ?access_key= / ?secret_key=, etc.).
	//   "passphrase": "my-srt-passphrase"
	//   "access_key": "AKID..."   (S3)
	//   "secret_key": "wJal..."   (S3)
	Params map[string]string `json:"params,omitempty" yaml:"params,omitempty"`

	// Net controls reconnect and timeout behaviour.
	Net InputNetConfig `json:"net,omitempty" yaml:"net,omitempty"`

	// Alive is a runtime-only field updated by the Stream Manager health checker.
	// Not persisted to storage.
	Alive bool `json:"-" yaml:"-"`
}

// InputNetConfig controls per-input network behaviour.
//
// Reconnect / silence-detection knobs were removed because they were
// declared but never consumed by any reader: pull workers use a hardcoded
// exponential backoff on transient errors (worker.go), and stream-level
// liveness is the manager's job (manager.input_packet_timeout_sec).
// Reintroduce specific knobs only when a reader actually wires them.
type InputNetConfig struct {
	// TimeoutSec is the per-protocol operation budget the reader applies
	// when this input is opened. Semantics differ by protocol:
	//
	//   - HLS:  HTTP request timeout (entire round-trip incl. body) for
	//           the playlist GET. Segment GETs derive from this — typically
	//           4× the playlist budget, floored at the segment default.
	//   - RTMP: TCP dial timeout (handshake budget).
	//   - RTSP: dial + initial read timeout (until first packet).
	//   - SRT:  connection / handshake timeout.
	//
	// Zero uses the reader's per-protocol default
	// (DefaultHLSPlaylistTimeoutSec for HLS; DefaultRTMPTimeoutSec /
	// DefaultRTSPTimeoutSec for the rest).
	TimeoutSec int `json:"timeout_sec,omitempty" yaml:"timeout_sec,omitempty"`

	// InsecureTLS disables TLS certificate verification for HTTPS pulls
	// (HLS playlist + segment GETs). Default false — leave secure-by-default
	// for production. Use only when the source uses a self-signed,
	// expired, or otherwise-invalid certificate that you trust at the
	// network level (private VLAN, fixed IP allowlist).
	InsecureTLS bool `json:"insecure_tls,omitempty" yaml:"insecure_tls,omitempty"`
}
