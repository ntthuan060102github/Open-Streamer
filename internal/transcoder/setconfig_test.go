package transcoder

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ntt0601zcoder/open-streamer/config"
)

// SetConfig must be safe to call concurrently with Config (and with the
// internal Start/Stop paths that take s.mu). The atomic round-trip below
// is a smoke check; -race catches any torn read.
func TestService_SetConfig_RoundTrip(t *testing.T) {
	t.Parallel()
	s := &Service{cfg: config.TranscoderConfig{FFmpegPath: "old", MultiOutput: false}}

	s.SetConfig(config.TranscoderConfig{FFmpegPath: "new", MultiOutput: true})

	got := s.Config()
	assert.Equal(t, "new", got.FFmpegPath)
	assert.True(t, got.MultiOutput)
}

// Concurrent writers / readers must not race. This catches a regression if
// SetConfig forgets to take s.mu (or a future caller bypasses it).
func TestService_SetConfig_RaceSafe(t *testing.T) {
	t.Parallel()
	s := &Service{cfg: config.TranscoderConfig{}}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			s.SetConfig(config.TranscoderConfig{MultiOutput: i%2 == 0})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			_ = s.Config()
		}
	}()
	wg.Wait()
}
