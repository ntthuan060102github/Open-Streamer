package domain

// PushStatus is the runtime state of a push destination.
type PushStatus string

const (
	PushStatusIdle        PushStatus = "idle"
	PushStatusConnecting  PushStatus = "connecting"
	PushStatusActive      PushStatus = "active"
	PushStatusRetrying    PushStatus = "retrying"
	PushStatusFailed      PushStatus = "failed"
	PushStatusDisabled    PushStatus = "disabled"
)

// PushDestination is an external endpoint the server actively pushes the stream to.
type PushDestination struct {
	// URL is the destination ingest endpoint.
	// E.g. rtmp://a.rtmp.youtube.com/live2/{stream_key}
	URL string `json:"url"`

	// Enabled controls whether this destination is active.
	Enabled bool `json:"enabled"`

	// TimeoutSec is the connection/write timeout in seconds.
	TimeoutSec int `json:"timeout_sec"`

	// RetryTimeoutSec is the delay between retry attempts in seconds.
	RetryTimeoutSec int `json:"retry_timeout_sec"`

	// Limit is the maximum number of retry attempts. 0 = unlimited.
	Limit int `json:"limit"`

	// Comment is a human-readable note for this destination.
	Comment string `json:"comment"`

	// Status is a runtime-only field updated by the publisher.
	// Not persisted to storage.
	Status PushStatus `json:"status,omitempty"`
}
