package domain

// OutputProtocols defines which delivery protocols are opened for a stream.
// Each enabled protocol starts a corresponding listener or packager.
// Protocol-level settings (ports, segment duration, CDN URL, etc.)
// are configured globally in the server config.
type OutputProtocols struct {
	// HLS enables Apple HTTP Live Streaming (m3u8 + segments over HTTP).
	// Compatible with browsers, iOS, Android, Smart TVs.
	HLS bool `json:"hls" yaml:"hls"`

	// DASH enables MPEG-DASH packaging over HTTP.
	// Required for Widevine/PlayReady DRM.
	DASH bool `json:"dash" yaml:"dash"`

	// RTSP opens an RTSP listener for pull clients (VLC, broadcast tools).
	RTSP bool `json:"rtsp" yaml:"rtsp"`

	// RTMP opens an RTMP publish endpoint for legacy players/CDNs.
	RTMP bool `json:"rtmp" yaml:"rtmp"`

	// SRT opens an SRT listener port for contribution-quality pull.
	SRT bool `json:"srt" yaml:"srt"`

	// MPEGTS exposes raw MPEG-TS over chunked HTTP at /<code>/mpegts —
	// the lowest-latency relay path between Open-Streamer instances (and
	// any HTTP client that can consume chunked TS, e.g. ffmpeg / VLC).
	// Latency is bounded only by network RTT and one buffer-hub chunk
	// (typically 50–200 ms vs 4–10 s for HLS / DASH).
	//
	// No goroutine is started per-stream; the endpoint subscribes to the
	// playback buffer on demand. Disabling the flag turns the endpoint
	// into a 404 for that stream so operators can opt out per-stream
	// without changing the global router.
	MPEGTS bool `json:"mpegts" yaml:"mpegts"`
}
