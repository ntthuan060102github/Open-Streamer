package dvr

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntthuan060102github/open-streamer/internal/domain"
)

// ---- loadIndex / saveIndex round-trip ----------------------------------------

func TestLoadIndex_Missing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	idx, err := loadIndex(dir)
	require.NoError(t, err)
	assert.Nil(t, idx)
}

func TestLoadIndex_Invalid(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, indexFileName), []byte("not json"), 0o644))
	_, err := loadIndex(dir)
	require.Error(t, err)
}

func TestSaveAndLoadIndex_RoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Millisecond)
	orig := &domain.DVRIndex{
		StreamCode:     "test-stream",
		StartedAt:      now,
		LastSegmentAt:  now.Add(10 * time.Second),
		SegmentCount:   5,
		TotalSizeBytes: 1024 * 1024,
		Gaps: []domain.DVRGap{
			{From: now.Add(2 * time.Second), To: now.Add(4 * time.Second), Duration: 2 * time.Second},
		},
	}
	require.NoError(t, saveIndex(dir, orig))

	got, err := loadIndex(dir)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, orig.StreamCode, got.StreamCode)
	assert.Equal(t, orig.SegmentCount, got.SegmentCount)
	assert.Equal(t, orig.TotalSizeBytes, got.TotalSizeBytes)
	require.Len(t, got.Gaps, 1)
	assert.Equal(t, orig.Gaps[0].Duration, got.Gaps[0].Duration)
}

func TestSaveIndex_Atomic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	idx := &domain.DVRIndex{StreamCode: "atomic-test", SegmentCount: 1}
	require.NoError(t, saveIndex(dir, idx))
	// Temp file must be gone.
	_, err := os.Stat(filepath.Join(dir, indexFileName+".tmp"))
	assert.True(t, os.IsNotExist(err), "tmp file should be gone after atomic write")
}

// ---- parsePlaylist -----------------------------------------------------------

func TestParsePlaylist_Missing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	segs, err := parsePlaylist(dir)
	require.NoError(t, err)
	assert.Nil(t, segs)
}

func TestParsePlaylist_Empty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "playlist.m3u8"), []byte("#EXTM3U\n"), 0o644))
	segs, err := parsePlaylist(dir)
	require.NoError(t, err)
	assert.Empty(t, segs)
}

func TestParsePlaylist_SingleSegment(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Create the segment file so Stat can return a size.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "000001.ts"), make([]byte, 512), 0o644))

	playlist := "#EXTM3U\n" +
		"#EXT-X-VERSION:3\n" +
		"#EXT-X-TARGETDURATION:5\n" +
		"#EXT-X-PROGRAM-DATE-TIME:2024-01-01T10:00:00.000Z\n" +
		"#EXTINF:4.000,\n" +
		"000001.ts\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "playlist.m3u8"), []byte(playlist), 0o644))

	segs, err := parsePlaylist(dir)
	require.NoError(t, err)
	require.Len(t, segs, 1)
	assert.Equal(t, 1, segs[0].index)
	assert.InDelta(t, 4.0, segs[0].duration.Seconds(), 0.001)
	assert.False(t, segs[0].discontinuity)
	assert.Equal(t, int64(512), segs[0].size)
}

func TestParsePlaylist_MultipleSegments(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for i := range 3 {
		require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("%06d.ts", i+1)), make([]byte, 100), 0o644))
	}

	playlist := "#EXTM3U\n" +
		"#EXT-X-VERSION:3\n" +
		"#EXT-X-PROGRAM-DATE-TIME:2024-01-01T10:00:00.000Z\n" +
		"#EXTINF:4.000,\n" +
		"000001.ts\n" +
		"#EXTINF:4.000,\n" +
		"000002.ts\n" +
		"#EXTINF:4.000,\n" +
		"000003.ts\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "playlist.m3u8"), []byte(playlist), 0o644))

	segs, err := parsePlaylist(dir)
	require.NoError(t, err)
	require.Len(t, segs, 3)
	for i, s := range segs {
		assert.Equal(t, i+1, s.index)
		assert.False(t, s.discontinuity)
	}
}

func TestParsePlaylist_Discontinuity(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "000001.ts"), []byte{}, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "000002.ts"), []byte{}, 0o644))

	playlist := "#EXTM3U\n" +
		"#EXT-X-PROGRAM-DATE-TIME:2024-01-01T10:00:00.000Z\n" +
		"#EXTINF:4.000,\n" +
		"000001.ts\n" +
		"#EXT-X-DISCONTINUITY\n" +
		"#EXT-X-PROGRAM-DATE-TIME:2024-01-01T10:01:00.000Z\n" +
		"#EXTINF:3.500,\n" +
		"000002.ts\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "playlist.m3u8"), []byte(playlist), 0o644))

	segs, err := parsePlaylist(dir)
	require.NoError(t, err)
	require.Len(t, segs, 2)
	assert.False(t, segs[0].discontinuity)
	assert.True(t, segs[1].discontinuity)
	assert.InDelta(t, 3.5, segs[1].duration.Seconds(), 0.001)
}

func TestParsePlaylist_SkipsNonNumericFilenames(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Only "000002.ts" has a numeric base.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "000002.ts"), []byte{}, 0o644))

	playlist := "#EXTM3U\n" +
		"#EXT-X-PROGRAM-DATE-TIME:2024-01-01T10:00:00.000Z\n" +
		"#EXTINF:4.000,\n" +
		"badname.ts\n" +
		"#EXTINF:4.000,\n" +
		"000002.ts\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "playlist.m3u8"), []byte(playlist), 0o644))

	segs, err := parsePlaylist(dir)
	require.NoError(t, err)
	require.Len(t, segs, 1)
	assert.Equal(t, 2, segs[0].index)
}
