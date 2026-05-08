package publisher

// abr_master_test.go covers the in-memory state machines + master-playlist
// writers used by the ABR HLS / DASH publishers. Both masters are
// goroutine-spawning state aggregators driven by per-rendition flushes;
// these tests poke them directly (no segmenters wired) to verify the
// debounced rewrites land deterministic output on disk.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── HLS ABR master ──────────────────────────────────────────────────────────

func TestHLSABRMaster_NewIsZeroState(t *testing.T) {
	t.Parallel()
	m := newHLSABRMaster("/tmp/index.m3u8", "live")
	assert.Equal(t, "/tmp/index.m3u8", m.rootPath)
	assert.Empty(t, m.reps)
}

// flushRoot is invoked directly so we don't have to wait for the 100ms
// debounce timer that onShardUpdated schedules.
func TestHLSABRMaster_FlushRoot_WritesMaster(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rootPath := filepath.Join(dir, "index.m3u8")

	m := newHLSABRMaster(rootPath, "live")
	m.onShardUpdated("720p", 2_500_000, 1280, 720, true)
	m.onShardUpdated("1080p", 5_000_000, 1920, 1080, true)
	m.flushRoot()

	body, err := os.ReadFile(rootPath)
	require.NoError(t, err)
	out := string(body)
	assert.True(t, strings.HasPrefix(out, "#EXTM3U\n"))
	assert.Contains(t, out, "BANDWIDTH=2500000")
	assert.Contains(t, out, "RESOLUTION=1280x720")
	assert.Contains(t, out, "BANDWIDTH=5000000")
	assert.Contains(t, out, "RESOLUTION=1920x1080")
	// Renditions are sorted by slug — 1080p sorts BEFORE 720p alphabetically,
	// so verify ordering.
	idx1080 := strings.Index(out, "1080p/index.m3u8")
	idx720 := strings.Index(out, "720p/index.m3u8")
	assert.Greater(t, idx1080, 0)
	assert.Greater(t, idx720, idx1080, "renditions must be sorted by slug")
}

func TestHLSABRMaster_FlushRoot_NoRepsIsNoOp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rootPath := filepath.Join(dir, "index.m3u8")
	m := newHLSABRMaster(rootPath, "live")

	// flushRoot with no shards must NOT write a file (caller relies on
	// "no master until at least one rendition has data").
	m.flushRoot()
	_, err := os.Stat(rootPath)
	assert.True(t, os.IsNotExist(err), "master should not be written when no reps have data")
}

func TestHLSABRMaster_SetRepOverride_WinsOverShardUpdates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rootPath := filepath.Join(dir, "index.m3u8")
	m := newHLSABRMaster(rootPath, "live")

	// First seed via onShardUpdated.
	m.onShardUpdated("720p", 2_500_000, 1280, 720, true)
	// Override with a higher bitrate / different resolution.
	m.SetRepOverride("720p", 9_999_999, 1920, 1080, false)
	// A subsequent shard update from the segmenter must NOT clobber the override.
	m.onShardUpdated("720p", 1_000_000, 640, 480, true)
	m.flushRoot()

	body, err := os.ReadFile(rootPath)
	require.NoError(t, err)
	assert.Contains(t, string(body), "BANDWIDTH=9999999")
	assert.Contains(t, string(body), "RESOLUTION=1920x1080")
}

func TestWriteHLSMasterPlaylist_FormatsStreamInf(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "index.m3u8")
	require.NoError(t, writeHLSMasterPlaylist(path, []hlsABRRep{
		{slug: "low", bwBps: 800_000, width: 640, height: 360, hasAudio: true},
	}))
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	out := string(body)
	assert.Contains(t, out, "#EXT-X-VERSION:3")
	assert.Contains(t, out, "BANDWIDTH=800000")
	assert.Contains(t, out, "RESOLUTION=640x360")
	assert.Contains(t, out, "low/index.m3u8")
}

// ─── DASH ABR master ─────────────────────────────────────────────────────────

func TestDashABRMaster_NewIsZeroState(t *testing.T) {
	t.Parallel()
	m := newDashABRMaster("/tmp/manifest.mpd", "live", 4, 5)
	assert.Equal(t, 4, m.segSec)
	assert.Equal(t, 5, m.window)
	assert.Empty(t, m.shards)
}

// onShardUpdated stores the packager and schedules a rewrite. We can't
// easily synchronise on the debounce timer in a unit test, so we exercise
// the storage path and verify stop() short-circuits subsequent updates.
func TestDashABRMaster_OnShardUpdatedTracksShard(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.mpd")
	m := newDashABRMaster(manifest, "live", 4, 5)

	// Track a shard. videoInit nil → flushRoot writes nothing (no
	// AdaptationSets), but the master must still record the reference.
	p := &dashFMP4Packager{abrSlug: "best"}
	m.onShardUpdated(p)

	m.mu.Lock()
	defer m.mu.Unlock()
	assert.Len(t, m.shards, 1)
	assert.Same(t, p, m.shards["best"])
}

func TestDashABRMaster_StopWhenAlreadyStopped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.mpd")
	m := newDashABRMaster(manifest, "live", 4, 5)

	// Stopped flag short-circuits subsequent onShardUpdated calls so a
	// segmenter that finishes a final flush after teardown doesn't race
	// with shutdown.
	m.stop()
	m.onShardUpdated(&dashFMP4Packager{abrSlug: "late"})
	// No new shard should be tracked.
	m.mu.Lock()
	defer m.mu.Unlock()
	assert.Empty(t, m.shards, "onShardUpdated after stop must be a no-op")
}

func TestDashABRMaster_FlushRoot_NoShardsIsNoOp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.mpd")
	m := newDashABRMaster(manifest, "live", 4, 5)

	m.flushRoot()
	_, err := os.Stat(manifest)
	assert.True(t, os.IsNotExist(err))
}

// ─── writeDASHABRRootMPD (pure writer) ───────────────────────────────────────

func TestWriteDASHABRRootMPD_EmptySnapsSkipsWrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	manifest := filepath.Join(dir, "root.mpd")

	// Empty snaps → no representations → no AdaptationSets → no MPD on disk.
	require.NoError(t, writeDASHABRRootMPD(manifest, 4, 5, nil))
	_, err := os.Stat(manifest)
	assert.True(t, os.IsNotExist(err))
}

func TestWriteDASHABRRootMPD_VideoOnlyMaster(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	manifest := filepath.Join(dir, "root.mpd")

	snaps := []dashABRRep{{
		slug:       "1080p",
		videoBW:    5_000_000,
		videoCodec: "avc1.4d401f",
		width:      1920,
		height:     1080,
		hasVideo:   true,
		vSegs:      []string{"seg_v_00001.m4s", "seg_v_00002.m4s"},
		vDurs:      []uint64{180000, 180000},
		vStarts:    []uint64{0},
		segSec:     4,
	}}
	require.NoError(t, writeDASHABRRootMPD(manifest, 4, 5, snaps))

	body, err := os.ReadFile(manifest)
	require.NoError(t, err)
	out := string(body)
	assert.Contains(t, out, "<MPD")
	assert.Contains(t, out, "contentType=\"video\"")
	assert.Contains(t, out, "1080p/")
	assert.Contains(t, out, "bandwidth=\"5000000\"")
	assert.NotContains(t, out, "contentType=\"audio\"")
}

func TestWriteDASHABRRootMPD_VideoAndAudioMaster(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	manifest := filepath.Join(dir, "root.mpd")

	snaps := []dashABRRep{{
		slug:       "best",
		videoBW:    5_000_000,
		videoCodec: "avc1.4d401f",
		width:      1920,
		height:     1080,
		hasVideo:   true,
		hasAudio:   true,
		audioSR:    48000,
		audioCodec: "mp4a.40.2",
		vSegs:      []string{"seg_v_00001.m4s"},
		vDurs:      []uint64{180000},
		vStarts:    []uint64{0},
		aSegs:      []string{"seg_a_00001.m4s"},
		aDurs:      []uint64{96000},
		aStarts:    []uint64{0},
		segSec:     4,
	}}
	require.NoError(t, writeDASHABRRootMPD(manifest, 4, 5, snaps))

	body, err := os.ReadFile(manifest)
	require.NoError(t, err)
	out := string(body)
	assert.Contains(t, out, "contentType=\"video\"")
	assert.Contains(t, out, "contentType=\"audio\"")
	assert.Contains(t, out, "mp4a.40.2")
}

// ─── dashFMP4Packager method receivers ───────────────────────────────────────

func TestDashFMP4Packager_AudioFramesPerSegment(t *testing.T) {
	t.Parallel()
	// SR unset → safe default for 48 kHz × 2 s.
	p := &dashFMP4Packager{segSec: 2}
	assert.Equal(t, 94, p.audioFramesPerSegment())

	// 48 kHz × 4 s = 192000 / 1024 = 187.5 → ceil = 188.
	p.audioSR = 48000
	p.segSec = 4
	assert.Equal(t, 188, p.audioFramesPerSegment())
}

func TestDashFMP4Packager_EffectiveVideoBW(t *testing.T) {
	t.Parallel()
	p := &dashFMP4Packager{}
	assert.Equal(t, 5_000_000, p.effectiveVideoBW(), "default when videoBW unset")

	p.videoBW = 12_345_000
	assert.Equal(t, 12_345_000, p.effectiveVideoBW())
}

func TestDashFMP4Packager_QueuedVideoStartsWithIDR_EmptyIsFalse(t *testing.T) {
	t.Parallel()
	p := &dashFMP4Packager{}
	assert.False(t, p.queuedVideoStartsWithIDRLocked())
}

// ─── buildSegTimeline / buildMPD ─────────────────────────────────────────────

func TestBuildSegTimeline_EmptyReturnsNil(t *testing.T) {
	t.Parallel()
	assert.Nil(t, buildSegTimeline(nil, nil, nil))
}

func TestBuildSegTimeline_FirstEntryGetsExplicitT(t *testing.T) {
	t.Parallel()
	tl := buildSegTimeline(
		[]string{"seg_v_00001.m4s", "seg_v_00002.m4s"},
		[]uint64{180000, 180000},
		[]uint64{1000},
	)
	require.NotNil(t, tl)
	require.Len(t, tl.S, 2)
	require.NotNil(t, tl.S[0].T)
	assert.Equal(t, uint64(1000), *tl.S[0].T)
	assert.Equal(t, uint64(180000), tl.S[0].D)
	assert.Nil(t, tl.S[1].T, "subsequent entries must have no @t (implied by predecessor)")
}

// ─── buildMPD ────────────────────────────────────────────────────────────────

func TestBuildMPD_NoSegmentsReturnsNil(t *testing.T) {
	t.Parallel()
	doc := buildMPD(time.Now(), 4, 5,
		nil, "", 0, 0, 0,
		nil, nil, nil,
		nil, "", 0,
		nil, nil, nil,
		"")
	assert.Nil(t, doc)
}

func TestBuildMPD_VideoOnly(t *testing.T) {
	t.Parallel()
	init := mp4.CreateEmptyInit()
	doc := buildMPD(time.Date(2026, 5, 8, 0, 0, 0, 0, time.UTC), 4, 5,
		init, "avc1.4d401f", 5_000_000, 1920, 1080,
		[]string{"seg_v_00001.m4s", "seg_v_00002.m4s"},
		[]uint64{180000, 180000},
		[]uint64{0},
		nil, "", 0,
		nil, nil, nil,
		"")
	require.NotNil(t, doc)
	require.Len(t, doc.Periods, 1)
	require.Len(t, doc.Periods[0].AdaptationSets, 1)
	vAS := doc.Periods[0].AdaptationSets[0]
	assert.Equal(t, "video", vAS.ContentType)
	require.Len(t, vAS.Representations, 1)
	assert.Equal(t, 5_000_000, vAS.Representations[0].Bandwidth)
	assert.Equal(t, 1920, vAS.Representations[0].Width)
	assert.Equal(t, 1080, vAS.Representations[0].Height)
}

func TestBuildMPD_VideoAndAudio(t *testing.T) {
	t.Parallel()
	vInit := mp4.CreateEmptyInit()
	aInit := mp4.CreateEmptyInit()
	doc := buildMPD(time.Time{}, 4, 5, // zero availStart skips the attribute
		vInit, "avc1.4d401f", 0, 1920, 1080, // BW=0 still emits the field as 0
		[]string{"seg_v_00001.m4s"}, []uint64{180000}, []uint64{0},
		aInit, "mp4a.40.2", 48000,
		[]string{"seg_a_00001.m4s"}, []uint64{96000}, []uint64{0},
		"")
	require.NotNil(t, doc)
	require.Len(t, doc.Periods[0].AdaptationSets, 2)
	assert.Equal(t, "video", doc.Periods[0].AdaptationSets[0].ContentType)
	assert.Equal(t, "audio", doc.Periods[0].AdaptationSets[1].ContentType)
}

func TestBuildMPD_BaseURLPropagatedToTemplates(t *testing.T) {
	t.Parallel()
	init := mp4.CreateEmptyInit()
	doc := buildMPD(time.Now(), 4, 5,
		init, "avc1.4d401f", 5_000_000, 1920, 1080,
		[]string{"seg_v_00001.m4s"}, []uint64{180000}, []uint64{0},
		nil, "", 0,
		nil, nil, nil,
		"shard1/")
	require.NotNil(t, doc)
	tmpl := doc.Periods[0].AdaptationSets[0].Representations[0].SegmentTemplate
	assert.True(t, strings.HasPrefix(tmpl.Initialization, "shard1/"))
	assert.True(t, strings.HasPrefix(tmpl.Media, "shard1/"))
}

func TestBuildSegTimeline_SkipsZeroDuration(t *testing.T) {
	t.Parallel()
	tl := buildSegTimeline(
		[]string{"seg_v_00001.m4s", "seg_v_00002.m4s"},
		[]uint64{0, 180000}, // first dur is zero — skipped
		[]uint64{1000},
	)
	require.NotNil(t, tl)
	require.Len(t, tl.S, 1)
	assert.Equal(t, uint64(180000), tl.S[0].D)
}

// ─── hlsSegmenter trim/flush behaviour ───────────────────────────────────────

func TestHLSSegmenter_TrimDisk_RespectsEphemeralFlag(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Pre-create three segment files; trim should leave them untouched
	// because ephemeral=false (DVR-style retention).
	for _, n := range []string{"seg_000001.ts", "seg_000002.ts", "seg_000003.ts"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644))
	}
	p := &hlsSegmenter{
		streamDir: dir,
		window:    1,
		history:   0,
		ephemeral: false,
		onDisk: []hlsSegEntry{
			{name: "seg_000001.ts"},
			{name: "seg_000002.ts"},
			{name: "seg_000003.ts"},
		},
	}
	p.trimDiskLocked()
	// onDisk slice untouched + files still on disk.
	assert.Len(t, p.onDisk, 3)
	for _, n := range []string{"seg_000001.ts", "seg_000002.ts", "seg_000003.ts"} {
		_, err := os.Stat(filepath.Join(dir, n))
		assert.NoError(t, err)
	}
}

func TestHLSSegmenter_TrimDisk_EphemeralRemovesOldest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, n := range []string{"seg_000001.ts", "seg_000002.ts", "seg_000003.ts", "seg_000004.ts"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644))
	}
	p := &hlsSegmenter{
		streamDir: dir,
		window:    2,
		history:   0,
		ephemeral: true,
		onDisk: []hlsSegEntry{
			{name: "seg_000001.ts"},
			{name: "seg_000002.ts"},
			{name: "seg_000003.ts"},
			{name: "seg_000004.ts"},
		},
	}
	p.trimDiskLocked()
	// Window=2, history=0 → keep last two; first two get unlinked.
	assert.Len(t, p.onDisk, 2)
	assert.Equal(t, "seg_000003.ts", p.onDisk[0].name)

	_, err := os.Stat(filepath.Join(dir, "seg_000001.ts"))
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(dir, "seg_000002.ts"))
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(dir, "seg_000003.ts"))
	assert.NoError(t, err)
}

func TestHLSSegmenter_TrimDisk_HistoryExtendsWindow(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, n := range []string{"seg_000001.ts", "seg_000002.ts", "seg_000003.ts", "seg_000004.ts"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644))
	}
	p := &hlsSegmenter{
		streamDir: dir,
		window:    1,
		history:   2,
		ephemeral: true,
		onDisk: []hlsSegEntry{
			{name: "seg_000001.ts"},
			{name: "seg_000002.ts"},
			{name: "seg_000003.ts"},
			{name: "seg_000004.ts"},
		},
	}
	p.trimDiskLocked()
	// window=1 + history=2 → keep last 3, drop the first.
	assert.Len(t, p.onDisk, 3)
	_, err := os.Stat(filepath.Join(dir, "seg_000001.ts"))
	assert.True(t, os.IsNotExist(err))
}

func TestHLSSegmenter_DoFlush_NoOpWhenBufferEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := &hlsSegmenter{
		streamID:     "test",
		streamDir:    dir,
		manifestPath: filepath.Join(dir, "index.m3u8"),
		segSec:       2,
		window:       3,
		failoverGen:  func() uint64 { return 0 },
	}
	// No segBuf → doFlush is a no-op (no manifest written, no segments).
	p.doFlush()
	_, err := os.Stat(filepath.Join(dir, "index.m3u8"))
	assert.True(t, os.IsNotExist(err))
}

func TestHLSSegmenter_FlushLocked_WritesSegmentAndManifest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := &hlsSegmenter{
		streamID:     "test",
		streamDir:    dir,
		manifestPath: filepath.Join(dir, "index.m3u8"),
		segSec:       2,
		window:       3,
		ephemeral:    true,
		failoverGen:  func() uint64 { return 0 },
		segBuf:       []byte("ts-payload"),
		segStart:     time.Now().Add(-2 * time.Second),
	}
	p.flushLocked()

	// Segment 1 must exist with the buffered bytes.
	got, err := os.ReadFile(filepath.Join(dir, "seg_000001.ts"))
	require.NoError(t, err)
	assert.Equal(t, []byte("ts-payload"), got)

	// Manifest must list it.
	manifest, err := os.ReadFile(filepath.Join(dir, "index.m3u8"))
	require.NoError(t, err)
	assert.Contains(t, string(manifest), "seg_000001.ts")
	assert.Equal(t, uint64(1), p.segN)
}
