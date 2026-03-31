package domain

// HookID is the unique identifier for a registered hook.
type HookID string

// HookType is the delivery mechanism for a hook.
type HookType string

const (
	HookTypeHTTP  HookType = "http"
	HookTypeNATS  HookType = "nats"
	HookTypeKafka HookType = "kafka"
)

// Hook is a registered external integration that receives domain events.
type Hook struct {
	ID     HookID
	Name   string
	Type   HookType
	Target string // HTTP URL, NATS subject, or Kafka topic
	Secret string // HMAC-SHA256 signing secret (HTTP only)

	// EventTypes filters which events trigger delivery. nil = all events.
	EventTypes []EventType

	Enabled bool

	// MaxRetries is the number of delivery attempts before giving up.
	// 0 means use the server default (3).
	MaxRetries int

	// TimeoutSec is the per-attempt delivery timeout in seconds.
	// 0 means use the server default (10s).
	TimeoutSec int
}
