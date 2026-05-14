package transcoder

import (
	"bufio"
	"strings"
	"testing"
)

// TestParseFFmpegFrameCount covers the lines logStderr uses to drive
// transcoder_frames_total: real FFmpeg progress output, padding
// variations FFmpeg uses for column alignment, and lookalike lines
// (errors that mention "frame=" but aren't progress) which must NOT
// be parsed.
func TestParseFFmpegFrameCount(t *testing.T) {
	cases := []struct {
		name   string
		line   string
		wantN  uint64
		wantOk bool
	}{
		{
			name:   "tight progress line",
			line:   "frame=1234 fps=25 q=27.0 size=1234kB time=00:00:49.36 bitrate=200.6kbits/s speed=1.01x",
			wantN:  1234,
			wantOk: true,
		},
		{
			name:   "padded progress line (FFmpeg column-aligns numbers)",
			line:   "frame=  1234 fps= 25 q=27.0 size=    1234kB time=00:00:49.36 bitrate= 200.6kbits/s speed=1.01x",
			wantN:  1234,
			wantOk: true,
		},
		{
			name:   "leading whitespace + progress",
			line:   "  frame= 42 fps= 30 q=-1.0 size=  N/A time=N/A bitrate=N/A speed=N/A",
			wantN:  42,
			wantOk: true,
		},
		{
			name:   "very large frame count (long-running stream)",
			line:   "frame=1234567890 fps=25 q=27.0 size=N/A time=N/A bitrate=N/A speed=N/A",
			wantN:  1234567890,
			wantOk: true,
		},
		// Lookalike lines that must NOT match.
		{
			name:   "error message mentioning frame=",
			line:   "[h264 @ 0xdeadbeef] error processing frame=12 reference picture missing",
			wantOk: false,
		},
		{
			name:   "filter chain reference",
			line:   "Filtergraph 'frame=10' invalid",
			wantOk: false,
		},
		{
			name:   "missing fps tag (not a progress line)",
			line:   "frame=999 dur=00:00:10",
			wantOk: false,
		},
		{
			name:   "frame= with non-numeric value",
			line:   "frame=N/A fps=N/A",
			wantOk: false,
		},
		{
			name:   "empty",
			line:   "",
			wantOk: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotN, gotOk := parseFFmpegFrameCount(tc.line)
			if gotOk != tc.wantOk {
				t.Fatalf("ok mismatch: got=%v want=%v (line=%q)", gotOk, tc.wantOk, tc.line)
			}
			if gotOk && gotN != tc.wantN {
				t.Errorf("frame count: got=%d want=%d (line=%q)", gotN, tc.wantN, tc.line)
			}
		})
	}
}

// TestScanLinesOrCR ensures the SplitFunc breaks on both '\n' and '\r'.
// Critical because FFmpeg progress lines use '\r' and the default
// bufio.ScanLines silently buffers them (root cause of frame-counter
// metric staying at 0 in production before the fix).
func TestScanLinesOrCR(t *testing.T) {
	// Mix of LF-, CR-, and tail-without-terminator content. A real
	// FFmpeg stream interleaves '\r'-terminated progress with
	// '\n'-terminated info/error lines.
	input := "info line\nframe= 1 fps= 25\rframe= 2 fps= 25\rfinal\n"
	sc := bufio.NewScanner(strings.NewReader(input))
	sc.Split(scanLinesOrCR)

	var got []string
	for sc.Scan() {
		got = append(got, sc.Text())
	}
	want := []string{"info line", "frame= 1 fps= 25", "frame= 2 fps= 25", "final"}
	if len(got) != len(want) {
		t.Fatalf("token count: got=%d want=%d (got=%v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("token[%d]: got=%q want=%q", i, got[i], want[i])
		}
	}
}
