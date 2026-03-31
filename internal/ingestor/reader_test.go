package ingestor_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-streamer/open-streamer/config"
	"github.com/open-streamer/open-streamer/internal/domain"
	"github.com/open-streamer/open-streamer/internal/ingestor"
)

var testCfg = config.IngestorConfig{
	HLSMaxSegmentBuffer: 8,
}

func TestNewReader_PushListenURL_ReturnsError(t *testing.T) {
	t.Parallel()

	pushURLs := []string{
		"rtmp://0.0.0.0:1935/live/key",
		"rtmp://127.0.0.1:1935/live/key",
		"rtmp://[::]:1935/live/key",
		"srt://0.0.0.0:9999?streamid=key",
	}

	for _, u := range pushURLs {
		t.Run(u, func(t *testing.T) {
			t.Parallel()
			_, err := ingestor.NewReader(domain.Input{URL: u}, testCfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "push-listen")
		})
	}
}

func TestNewReader_UnknownScheme_ReturnsError(t *testing.T) {
	t.Parallel()

	unknownURLs := []string{
		"ftp://server/file",
		"ws://server/live",
		"",
		"relative/path",
	}

	for _, u := range unknownURLs {
		t.Run(u, func(t *testing.T) {
			t.Parallel()
			_, err := ingestor.NewReader(domain.Input{URL: u}, testCfg)
			require.Error(t, err)
		})
	}
}

func TestNewReader_ValidPullURLs_ReturnsReader(t *testing.T) {
	t.Parallel()

	// These URLs trigger reader construction only — no actual network I/O.
	// We verify the reader is non-nil and no error is returned.
	validURLs := []string{
		"udp://239.1.1.1:5000",
		"http://cdn.example.com/live.ts",
		"https://cdn.example.com/live.ts",
		"http://cdn.example.com/playlist.m3u8",
		"https://cdn.example.com/playlist.m3u8",
		"srt://relay.example.com:9999",
		"rtmp://server.example.com/live/key",
		"rtsp://camera.local:554/stream",
		"s3://my-bucket/live.ts",
		"file:///tmp/source.ts",
		"/tmp/source.ts",
	}

	for _, u := range validURLs {
		t.Run(u, func(t *testing.T) {
			t.Parallel()
			r, err := ingestor.NewReader(domain.Input{URL: u}, testCfg)
			require.NoError(t, err)
			assert.NotNil(t, r)
		})
	}
}
