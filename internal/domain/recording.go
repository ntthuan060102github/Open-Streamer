package domain

import "time"

// RecordingID is the unique identifier for a DVR recording.
type RecordingID string

// RecordingStatus represents the lifecycle state of a recording.
type RecordingStatus string

const (
	RecordingStatusRecording RecordingStatus = "recording"
	RecordingStatusStopped   RecordingStatus = "stopped"
	RecordingStatusFailed    RecordingStatus = "failed"
)

// Segment is a single MPEG-TS chunk produced by the DVR.
type Segment struct {
	Index     int
	Path      string        // relative path under DVR root dir
	Duration  time.Duration
	Size      int64         // bytes
	CreatedAt time.Time
}

// Recording represents a DVR recording session for a stream.
type Recording struct {
	ID        RecordingID
	StreamID  StreamID
	StartedAt time.Time
	StoppedAt *time.Time
	Status    RecordingStatus
	Segments  []Segment

	// TotalSizeBytes is the sum of all segment sizes. Updated as segments are flushed.
	TotalSizeBytes int64
}

// StreamDVRConfig overrides the global DVR settings for a specific stream.
// All zero values mean "use the global default".
type StreamDVRConfig struct {
	Enabled bool

	// RetentionHours overrides the global retention window.
	// 0 = keep forever (or use global setting).
	RetentionHours int

	// SegmentDuration overrides the global segment length in seconds.
	// Must align with the transcoder KeyframeInterval.
	// 0 = use global default (typically 6s).
	SegmentDuration int

	// StoragePath overrides the global DVR root directory for this stream.
	// "" = use global DVR root dir.
	StoragePath string

	// MaxSizeGB caps the total disk usage for this stream's recordings.
	// When exceeded, the oldest segments are pruned. 0 = no limit.
	MaxSizeGB float64
}
