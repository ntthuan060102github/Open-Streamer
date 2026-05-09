package thumbnail

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

func TestResolveOutputPath_Default(t *testing.T) {
	t.Parallel()
	dir, file := resolveOutputPath("/var/hls", "stream1", "")
	if dir != "/var/hls/stream1/thumbnails" {
		t.Fatalf("dir: got %q, want /var/hls/stream1/thumbnails", dir)
	}
	if file != "/var/hls/stream1/thumbnails/thumb.jpg" {
		t.Fatalf("file: got %q, want /var/hls/stream1/thumbnails/thumb.jpg", file)
	}
}

func TestResolveOutputPath_CustomSubdir(t *testing.T) {
	t.Parallel()
	// Operator-specified OutputDir must override the default "thumbnails".
	// Path joining is filesystem-aware so trailing separators don't double.
	dir, file := resolveOutputPath("/var/hls", "stream1", "previews")
	if dir != "/var/hls/stream1/previews" {
		t.Fatalf("dir: got %q", dir)
	}
	if file != "/var/hls/stream1/previews/thumb.jpg" {
		t.Fatalf("file: got %q", file)
	}
}

func TestEffectiveInterval_DefaultsWhenZero(t *testing.T) {
	t.Parallel()
	cfg := &domain.ThumbnailConfig{}
	if got := effectiveInterval(cfg); got != domain.DefaultThumbnailIntervalSec {
		t.Fatalf("zero IntervalSec must fall back to default %d, got %d",
			domain.DefaultThumbnailIntervalSec, got)
	}
}

func TestEffectiveInterval_RespectsExplicitValue(t *testing.T) {
	t.Parallel()
	cfg := &domain.ThumbnailConfig{IntervalSec: 12}
	if got := effectiveInterval(cfg); got != 12 {
		t.Fatalf("explicit IntervalSec=12 not honoured: got %d", got)
	}
}

func TestEffectiveQuality_ClampsToValidRange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want int
	}{
		{0, domain.DefaultThumbnailQuality},  // 0 → default
		{-3, domain.DefaultThumbnailQuality}, // negative → default
		{1, 1},                               // valid lower bound
		{31, 31},                             // valid upper bound
		{50, 31},                             // above max clamped
	}
	for _, c := range cases {
		got := effectiveQuality(&domain.ThumbnailConfig{Quality: c.in})
		if got != c.want {
			t.Fatalf("Quality=%d: got %d, want %d", c.in, got, c.want)
		}
	}
}

func TestBuildFFmpegArgs_DefaultIntervalAndQuality(t *testing.T) {
	t.Parallel()
	args := buildFFmpegArgs("/tmp/out.jpg", &domain.ThumbnailConfig{})
	joined := strings.Join(args, " ")

	// Interval falls back to DefaultThumbnailIntervalSec — verify the
	// resulting fps filter spelling is the FFmpeg-canonical form.
	if !strings.Contains(joined, "-vf fps=1/5") {
		t.Fatalf("default interval (5 s) must produce -vf fps=1/5; got %q", joined)
	}
	// Quality default → -q:v 5.
	if !strings.Contains(joined, "-q:v 5") {
		t.Fatalf("default quality must produce -q:v 5; got %q", joined)
	}
	// Output path is the LAST arg.
	if args[len(args)-1] != "/tmp/out.jpg" {
		t.Fatalf("output path must be the final arg; got %q", args[len(args)-1])
	}
	// `-update 1 -y` ensures FFmpeg overwrites the file in place rather
	// than emitting a numbered sequence (image2 default).
	if !strings.Contains(joined, "-update 1 -y") {
		t.Fatalf("must include -update 1 -y to keep overwriting same file; got %q", joined)
	}
	// Stdin TS feed is the input — stays before -vf so the order is
	// (-fflags) (-i pipe:0) (-vf …) (-q:v) (output). FFmpeg accepts other
	// orderings but a stable order makes diff-on-args tests pin behaviour.
	pipeIdx := indexOf(args, "pipe:0")
	vfIdx := indexOf(args, "-vf")
	if pipeIdx < 0 || vfIdx < 0 || pipeIdx > vfIdx {
		t.Fatalf("expected -i pipe:0 to precede -vf; got %v", args)
	}
}

func TestBuildFFmpegArgs_WidthOnlyAddsScaleFilter(t *testing.T) {
	t.Parallel()
	// scale=W:-2 keeps aspect ratio while forcing even output width
	// (mjpeg encoder accepts odd, but downstream display pipelines
	// occasionally choke on odd-pixel JPEGs).
	args := buildFFmpegArgs("/tmp/o.jpg", &domain.ThumbnailConfig{Width: 320})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, ",scale=320:-2") {
		t.Fatalf("width-only must produce scale=320:-2; got %q", joined)
	}
}

func TestBuildFFmpegArgs_BothDimensionsOverrideAspect(t *testing.T) {
	t.Parallel()
	// When both W and H are set, operator explicitly chose to override
	// aspect ratio — the function must emit literal W:H, not the -2 form.
	args := buildFFmpegArgs("/tmp/o.jpg", &domain.ThumbnailConfig{Width: 320, Height: 180})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, ",scale=320:180") {
		t.Fatalf("explicit W+H must produce scale=320:180 (no -2); got %q", joined)
	}
}

func TestStart_NoOpWhenDisabled(t *testing.T) {
	t.Parallel()
	// Enabled=false (or nil cfg) must short-circuit cleanly — no
	// subscription, no subprocess, no temp directory creation.
	s := &Service{workers: make(map[domain.StreamCode]*worker)}
	if err := s.Start(context.Background(), "x", "x", nil); err != nil {
		t.Fatalf("nil cfg must be no-op, got err: %v", err)
	}
	if err := s.Start(context.Background(), "x", "x",
		&domain.ThumbnailConfig{Enabled: false}); err != nil {
		t.Fatalf("Enabled=false must be no-op, got err: %v", err)
	}
	if got := s.LatestPath("x"); got != "" {
		t.Fatalf("LatestPath must be empty when no worker started; got %q", got)
	}
}

func TestLatestPath_ReturnsEmptyForUnknownStream(t *testing.T) {
	t.Parallel()
	s := &Service{workers: make(map[domain.StreamCode]*worker)}
	if got := s.LatestPath("does-not-exist"); got != "" {
		t.Fatalf("unknown stream must return empty path; got %q", got)
	}
}

func TestLatestPath_TracksWorkerOutput(t *testing.T) {
	t.Parallel()
	// Inject a worker entry directly so the test stays subprocess-free.
	// Production code only writes to workers via Start; tests use the
	// internal map for fixture setup.
	s := &Service{workers: make(map[domain.StreamCode]*worker)}
	w := &worker{outputPath: "/var/hls/abc/thumbnails/thumb.jpg"}
	s.workers["abc"] = w
	if got := s.LatestPath("abc"); got != w.outputPath {
		t.Fatalf("LatestPath: got %q, want %q", got, w.outputPath)
	}
}

func TestStop_NoOpWhenStreamNotTracked(t *testing.T) {
	t.Parallel()
	// Stop must be safely idempotent — coordinator may call Stop on
	// teardown of streams that never had Thumbnail enabled.
	s := &Service{workers: make(map[domain.StreamCode]*worker)}
	s.Stop("nope") // must not panic, deadlock, or race with anything
}

func TestStop_TearsDownWorker(t *testing.T) {
	t.Parallel()
	// Drive the cancel path without spawning a real subprocess: the
	// goroutine here just waits for ctx and closes done. Mirrors the
	// shape of the real pump goroutine.
	s := &Service{workers: make(map[domain.StreamCode]*worker)}
	ctx, cancel := context.WithCancel(context.Background())
	w := &worker{
		cancel:     cancel,
		done:       make(chan struct{}),
		outputPath: "/tmp/x",
	}
	s.workers["s1"] = w
	go func() {
		<-ctx.Done()
		close(w.done)
	}()

	doneCh := make(chan struct{})
	go func() {
		s.Stop("s1")
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop blocked on done channel — cancel not propagated")
	}
	if got := s.LatestPath("s1"); got != "" {
		t.Fatalf("after Stop, LatestPath must be empty; got %q", got)
	}
}

func TestStart_ErrorWhenHLSDirEmpty(t *testing.T) {
	t.Parallel()
	// Service constructed with empty HLS dir must reject Start —
	// otherwise we'd silently MkdirAll(""+"/code/thumbnails") which
	// happens to succeed but stashes thumbs in CWD on disk.
	s := &Service{
		workers: make(map[domain.StreamCode]*worker),
		buf:     buffer.NewServiceForTesting(8),
	}
	cfg := &domain.ThumbnailConfig{Enabled: true}
	err := s.Start(context.Background(), "code", "code", cfg)
	if err == nil {
		t.Fatal("Start with empty HLS dir must return error")
	}
	if !strings.Contains(err.Error(), "publisher.hls.dir") {
		t.Fatalf("error should mention the missing config key; got %q", err)
	}
}

// indexOf is a small slice-search helper kept private so it can be
// inlined into individual tests without burdening the public package.
func indexOf(haystack []string, needle string) int {
	for i, v := range haystack {
		if v == needle {
			return i
		}
	}
	return -1
}

// guardrails so dead-code analysers don't strip filepath import when
// tests are otherwise minimal.
var _ = filepath.Join
