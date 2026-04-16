package domain

// HookID is the unique identifier for a registered hook.
type HookID string

// HookType is the delivery mechanism for a hook.
type HookType string

// HookType values name supported hook transports.
const (
	HookTypeHTTP  HookType = "http"
	HookTypeKafka HookType = "kafka"
)

// StreamCodeFilter defines include/exclude rules for stream code matching.
// Only and Except are mutually exclusive; Only takes precedence when both are set.
type StreamCodeFilter struct {
	// Only delivers events only for streams in this list.
	Only []StreamCode `json:"only,omitempty" yaml:"only,omitempty"`
	// Except delivers events for all streams except those in this list.
	Except []StreamCode `json:"except,omitempty" yaml:"except,omitempty"`
}

// Matches reports whether the given stream code passes the filter.
func (f *StreamCodeFilter) Matches(code StreamCode) bool {
	if f == nil {
		return true
	}
	if len(f.Only) > 0 {
		for _, c := range f.Only {
			if c == code {
				return true
			}
		}
		return false
	}
	for _, c := range f.Except {
		if c == code {
			return false
		}
	}
	return true
}

// Hook is a registered external integration that receives domain events.
type Hook struct {
	ID     HookID   `json:"id" yaml:"id"`
	Name   string   `json:"name" yaml:"name"`
	Type   HookType `json:"type" yaml:"type"`
	Target string   `json:"target" yaml:"target"` // HTTP URL or Kafka topic
	Secret string   `json:"secret" yaml:"secret"` // HMAC-SHA256 signing secret (HTTP only)

	// EventTypes filters which events trigger delivery. Empty = all events.
	EventTypes []EventType `json:"event_types,omitempty" yaml:"event_types,omitempty"`

	// StreamCodes filters delivery by stream code.
	// Only and Except are mutually exclusive; Only takes precedence when both are set.
	// Omitting the field (nil) means all streams are included.
	StreamCodes *StreamCodeFilter `json:"stream_codes,omitempty" yaml:"stream_codes,omitempty"`

	// Metadata holds user-defined key-value pairs merged into every event payload
	// delivered by this hook. Useful for tagging events with custom context
	// (e.g. environment, tenant ID, region) without modifying the server config.
	Metadata map[string]string `json:"metadata,omitempty" yaml:"metadata,omitempty"`

	Enabled bool `json:"enabled" yaml:"enabled"`

	// MaxRetries is the number of delivery attempts before giving up.
	// 0 means use the server default (3).
	MaxRetries int `json:"max_retries" yaml:"max_retries"`

	// TimeoutSec is the per-attempt delivery timeout in seconds.
	// 0 means use the server default (10s).
	TimeoutSec int `json:"timeout_sec" yaml:"timeout_sec"`
}
