package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ntt0601zcoder/open-streamer/config"
)

// transcoderRequiresRestart is the gate that decides whether toggling a
// transcoder field forces every running stream to bounce. The intent: only
// fields that change in-flight FFmpeg behaviour cost a restart.
//
// MultiOutput flips the spawn shape (per-profile vs single multi-output
// process) so it requires a restart. FFmpegPath swap also requires one
// (we won't run a stale binary mid-stream). Future hot-swappable fields
// should be added to this test alongside the field itself.
func TestTranscoderRequiresRestart(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		old  *config.TranscoderConfig
		new  *config.TranscoderConfig
		want bool
	}{
		{
			name: "both nil → no restart",
			old:  nil,
			new:  nil,
			want: false,
		},
		{
			name: "MultiOutput off → on requires restart",
			old:  &config.TranscoderConfig{MultiOutput: false},
			new:  &config.TranscoderConfig{MultiOutput: true},
			want: true,
		},
		{
			name: "MultiOutput on → off requires restart",
			old:  &config.TranscoderConfig{MultiOutput: true},
			new:  &config.TranscoderConfig{MultiOutput: false},
			want: true,
		},
		{
			name: "MultiOutput unchanged → no restart",
			old:  &config.TranscoderConfig{MultiOutput: true, FFmpegPath: "/usr/bin/ffmpeg"},
			new:  &config.TranscoderConfig{MultiOutput: true, FFmpegPath: "/usr/bin/ffmpeg"},
			want: false,
		},
		{
			name: "FFmpegPath swap requires restart",
			old:  &config.TranscoderConfig{FFmpegPath: "/usr/bin/ffmpeg"},
			new:  &config.TranscoderConfig{FFmpegPath: "/opt/ffmpeg/bin/ffmpeg"},
			want: true,
		},
		{
			name: "nil → set defaults treated as off→off",
			old:  nil,
			new:  &config.TranscoderConfig{}, // zero values
			want: false,
		},
		{
			name: "nil → set with MultiOutput=true requires restart",
			old:  nil,
			new:  &config.TranscoderConfig{MultiOutput: true},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, transcoderRequiresRestart(tc.old, tc.new))
		})
	}
}
