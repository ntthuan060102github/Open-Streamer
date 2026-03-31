package domain

import "time"

// StreamID is the unique identifier for a stream.
type StreamID string

// StreamStatus represents the lifecycle state of a stream.
type StreamStatus string

const (
	StatusIdle      StreamStatus = "idle"
	StatusActive    StreamStatus = "active"
	StatusDegraded  StreamStatus = "degraded"
	StatusStopped   StreamStatus = "stopped"
)

// Stream is the central domain entity.
// It describes everything needed to ingest, process, and deliver a live stream.
type Stream struct {
	ID          StreamID
	Name        string
	Description string
	Tags        []string

	// StreamKey is used to authenticate RTMP/SRT push ingest.
	StreamKey string

	// Status is the runtime lifecycle state (not persisted between restarts).
	Status StreamStatus

	// Inputs are the available ingest sources ordered by Priority.
	// The Stream Manager monitors health and switches between them on failure.
	Inputs []Input

	// Transcoder controls encoding behaviour: passthrough, remux, or full re-encode.
	Transcoder TranscoderConfig

	// Protocols defines which delivery protocols are opened for this stream.
	// The server opens a listener/packager for each enabled protocol.
	// Protocol-level config (ports, segment duration, CDN URL) lives in server config.
	Protocols OutputProtocols

	// Push is the list of external destinations the server actively pushes to.
	// Each entry defines one push target (YouTube, Facebook, Twitch, CDN relay, etc.).
	Push []PushDestination

	// DVR overrides the global DVR settings for this specific stream.
	// If nil, the global config is used (when DVR is enabled globally).
	DVR *StreamDVRConfig

	// Watermark is an optional text or image overlay applied before encoding.
	Watermark *WatermarkConfig

	// Thumbnail controls periodic screenshot generation for preview images.
	Thumbnail *ThumbnailConfig

	CreatedAt time.Time
	UpdatedAt time.Time
}
