package push

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// rtmpRouteKey reconstructs the URL path from `app` + `streamName`, strips
// any leading `live/`, and rejects bare single-segment paths (no `live/`
// prefix and no '/') so encoders can't accidentally hit a 1-segment stream
// via `rtmp://host/<code>`.
func TestRTMPRouteKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		app        string
		streamName string
		want       string
	}{
		{"canonical 1-segment via live app", "live", "foo", "foo"},
		{"2-segment app/streamName", "foo", "bar", "foo/bar"},
		{"3-segment app holds two", "foo/bar", "baz", "foo/bar/baz"},
		{"non-canonical live prefix on multi-segment", "live", "foo/bar", "foo/bar"},
		{"bare 1-segment via app only", "foo", "", ""},
		{"bare 1-segment via streamName only", "", "foo", ""},
		{"empty both", "", "", ""},
		{"whitespace trimmed", "  live  ", "  foo  ", "foo"},
		{"live alone rejected", "live", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, rtmpRouteKey(tc.app, tc.streamName))
		})
	}
}
