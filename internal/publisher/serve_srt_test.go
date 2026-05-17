package publisher

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// srtStreamCode strips the `live/` prefix for single-segment codes and
// passes multi-segment streamids through unchanged. A bare single-segment
// streamid (no `live/`, no `/`) is rejected — returns "" — so a half-typed
// SRT URL can't accidentally hit a single-segment stream.
func TestSRTStreamCode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		streamid string
		want     string
	}{
		{"single segment with live prefix", "live/foo", "foo"},
		{"multi-segment direct", "foo/bar", "foo/bar"},
		{"multi-segment with non-canonical live prefix", "live/foo/bar", "foo/bar"},
		{"deep multi-segment", "a/b/c/d", "a/b/c/d"},
		{"bare single segment rejected", "foo", ""},
		{"empty streamid rejected", "", ""},
		{"whitespace trimmed", "  live/foo  ", "foo"},
		{"live alone rejected (no code after prefix)", "live/", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, srtStreamCode(tc.streamid))
		})
	}
}
