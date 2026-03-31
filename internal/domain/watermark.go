package domain

// WatermarkType determines whether the overlay is text or an image.
type WatermarkType string

const (
	WatermarkTypeText  WatermarkType = "text"
	WatermarkTypeImage WatermarkType = "image"
)

// WatermarkPosition controls where the overlay is placed in the frame.
type WatermarkPosition string

const (
	WatermarkTopLeft     WatermarkPosition = "top_left"
	WatermarkTopRight    WatermarkPosition = "top_right"
	WatermarkBottomLeft  WatermarkPosition = "bottom_left"
	WatermarkBottomRight WatermarkPosition = "bottom_right"
	WatermarkCenter      WatermarkPosition = "center"
)

// WatermarkConfig defines an overlay applied to the video before encoding.
// Applied via FFmpeg drawtext (text) or overlay (image) filter.
type WatermarkConfig struct {
	Enabled bool
	Type    WatermarkType

	// --- Text overlay ---

	// Text is the string to render. Supports strftime directives for live timestamps.
	// E.g. "LIVE %{localtime:%H:%M:%S}"
	Text string

	// FontFile is the path to a .ttf/.otf font file.
	// "" = FFmpeg default font.
	FontFile string

	// FontSize in pixels. Default: 24.
	FontSize int

	// FontColor in FFmpeg color syntax. E.g. "white", "#FFFFFF", "white@0.8".
	FontColor string

	// --- Image overlay ---

	// ImagePath is the path to the watermark image (PNG with alpha recommended).
	ImagePath string

	// --- Common ---

	// Opacity controls transparency: 0.0 = fully transparent, 1.0 = fully opaque.
	Opacity float64

	// Position is the corner/center anchor for the watermark.
	Position WatermarkPosition

	// OffsetX and OffsetY are pixel offsets from the chosen position edge.
	OffsetX int
	OffsetY int
}

// ThumbnailConfig controls periodic screenshot generation for stream preview.
// Thumbnails are written as JPEG files alongside HLS segments.
type ThumbnailConfig struct {
	Enabled bool

	// IntervalSec generates one thumbnail every N seconds.
	IntervalSec int

	// Width and Height of the output thumbnail in pixels.
	// 0 = match source resolution.
	Width  int
	Height int

	// Quality is the JPEG quality (1–31, lower = better). Default: 5.
	Quality int

	// OutputDir is relative to the publisher HLS directory.
	// E.g. "thumbnails" → written to {hls_dir}/{stream_id}/thumbnails/thumb.jpg
	OutputDir string
}
