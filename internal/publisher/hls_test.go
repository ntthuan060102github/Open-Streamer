package publisher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntthuan060102github/open-streamer/internal/buffer"
	"github.com/ntthuan060102github/open-streamer/internal/domain"
)

// ---- windowTailEntries -------------------------------------------------------

func TestWindowTailEntries_Empty(t *testing.T) {
	t.Parallel()
	assert.Empty(t, windowTailEntries(nil, 5))
	assert.Empty(t, windowTailEntries([]hlsSegEntry{}, 5))
}

func TestWindowTailEntries_ZeroWindow(t *testing.T) {
	t.Parallel()
	entries := []hlsSegEntry{{name: "a"}, {name: "b"}}
	// n=0 returns all entries unchanged.
	got := windowTailEntries(entries, 0)
	assert.Equal(t, entries, got)
}

func TestWindowTailEntries_LargerThanSlice(t *testing.T) {
	t.Parallel()
	entries := []hlsSegEntry{{name: "a"}, {name: "b"}}
	got := windowTailEntries(entries, 10)
	assert.Equal(t, entries, got)
}

func TestWindowTailEntries_ExactWindow(t *testing.T) {
	t.Parallel()
	entries := []hlsSegEntry{{name: "a"}, {name: "b"}, {name: "c"}}
	got := windowTailEntries(entries, 3)
	assert.Equal(t, entries, got)
}

func TestWindowTailEntries_TruncatesOldest(t *testing.T) {
	t.Parallel()
	entries := []hlsSegEntry{{name: "a"}, {name: "b"}, {name: "c"}, {name: "d"}}
	got := windowTailEntries(entries, 2)
	require.Len(t, got, 2)
	assert.Equal(t, "c", got[0].name)
	assert.Equal(t, "d", got[1].name)
}

// ---- hlsCodecString ----------------------------------------------------------

func TestHLSCodecString(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "avc1.640028", hlsCodecString(1920, 1080))
	assert.Equal(t, "avc1.640028", hlsCodecString(3840, 2160))
	assert.Equal(t, "avc1.4d401f", hlsCodecString(1280, 720))
	assert.Equal(t, "avc1.42e01e", hlsCodecString(854, 480))
	assert.Equal(t, "avc1.42e01e", hlsCodecString(0, 0))
}

// ---- hlsSegmenter.writeManifestLocked / flushLocked -------------------------

func TestHLSSegmenter_WriteManifest_EmptyOnDisk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := &hlsSegmenter{
		streamID:     "test",
		streamDir:    dir,
		manifestPath: filepath.Join(dir, "index.m3u8"),
		segSec:       2,
		window:       5,
		failoverGen:  func() uint64 { return 0 },
	}
	// No segments — manifest should still write without error.
	err := p.writeManifestLocked()
	require.NoError(t, err)
	data, err := os.ReadFile(filepath.Join(dir, "index.m3u8"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "#EXTM3U")
	assert.Contains(t, string(data), "#EXT-X-VERSION:3")
}

func TestHLSSegmenter_WriteManifest_DiscontinuityTag(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := &hlsSegmenter{
		streamID:     "test",
		streamDir:    dir,
		manifestPath: filepath.Join(dir, "index.m3u8"),
		segSec:       2,
		window:       5,
		failoverGen:  func() uint64 { return 0 },
		onDisk: []hlsSegEntry{
			{name: "seg_000001.ts", dur: 2.0, disc: false},
			{name: "seg_000002.ts", dur: 2.0, disc: true},
			{name: "seg_000003.ts", dur: 2.0, disc: false},
		},
	}
	require.NoError(t, p.writeManifestLocked())
	data, err := os.ReadFile(filepath.Join(dir, "index.m3u8"))
	require.NoError(t, err)
	content := string(data)
	assert.Contains(t, content, "#EXT-X-DISCONTINUITY")
	// Only one DISCONTINUITY tag (for seg_000002).
	assert.Equal(t, 1, strings.Count(content, "#EXT-X-DISCONTINUITY"))
}

func TestHLSSegmenter_WriteManifest_TargetDuration(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := &hlsSegmenter{
		streamID:     "test",
		streamDir:    dir,
		manifestPath: filepath.Join(dir, "index.m3u8"),
		segSec:       2,
		window:       5,
		failoverGen:  func() uint64 { return 0 },
		onDisk: []hlsSegEntry{
			{name: "seg_000001.ts", dur: 5.7},
		},
	}
	require.NoError(t, p.writeManifestLocked())
	data, err := os.ReadFile(filepath.Join(dir, "index.m3u8"))
	require.NoError(t, err)
	// int(5.7)+1 = 6
	assert.Contains(t, string(data), "#EXT-X-TARGETDURATION:6")
}

func TestHLSSegmenter_WriteManifest_MediaSequence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := &hlsSegmenter{
		streamID:     "test",
		streamDir:    dir,
		manifestPath: filepath.Join(dir, "index.m3u8"),
		segSec:       2,
		window:       3,
		failoverGen:  func() uint64 { return 0 },
		segN:         10,
		onDisk: []hlsSegEntry{
			{name: "seg_000008.ts", dur: 2.0},
			{name: "seg_000009.ts", dur: 2.0},
			{name: "seg_000010.ts", dur: 2.0},
		},
	}
	require.NoError(t, p.writeManifestLocked())
	data, err := os.ReadFile(filepath.Join(dir, "index.m3u8"))
	require.NoError(t, err)
	// segN=10, window=3 → mediaSeq=7
	assert.Contains(t, string(data), "#EXT-X-MEDIA-SEQUENCE:7")
}

func TestHLSSegmenter_WriteManifest_NoPathNoABR(t *testing.T) {
	t.Parallel()
	// manifestPath="" and abrMaster=nil → should be a no-op.
	p := &hlsSegmenter{
		manifestPath: "",
		abrMaster:    nil,
		failoverGen:  func() uint64 { return 0 },
	}
	err := p.writeManifestLocked()
	require.NoError(t, err)
}

// ---- runHLSSegmenter integration (AV packets → segment files) ---------------

func TestRunHLSSegmenter_FlushesOnContextCancel(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	buf := buffer.NewServiceForTesting(64)
	bufID := domain.StreamCode("seg-test")
	buf.Create(bufID)

	sub, err := buf.Subscribe(bufID)
	require.NoError(t, err)
	defer buf.Unsubscribe(bufID, sub)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		runHLSSegmenter(ctx, bufID, sub, dir, filepath.Join(dir, "index.m3u8"), 2, 5, 0, false, nil)
	}()

	// Write a few TS sync bytes to give the segmenter something to buffer.
	pkt := make([]byte, 188)
	pkt[0] = 0x47
	for i := range 3 {
		_ = i
		_ = buf.Write(bufID, buffer.TSPacket(pkt))
	}

	// Give segmenter time to receive packets, then cancel.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("segmenter did not exit after context cancel")
	}
}
