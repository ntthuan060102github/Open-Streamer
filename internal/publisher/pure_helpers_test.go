package publisher

// pure_helpers_test.go covers the small pure functions sprinkled across
// fileutil.go, dash_fmp4.go and runtime.go. These are easy to unit test
// in isolation, and they accumulate enough statements that nailing them
// down moves the publisher coverage needle without standing up the full
// segmenter pipeline.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── fileutil ────────────────────────────────────────────────────────────────

func TestWindowTail(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   []string
		n    int
		want []string
	}{
		{"empty input", nil, 5, nil},
		{"n is zero", []string{"a", "b"}, 0, []string{"a", "b"}},
		{"n is negative", []string{"a", "b"}, -3, []string{"a", "b"}},
		{"len <= n returns all", []string{"a", "b"}, 5, []string{"a", "b"}},
		{"trims to last n", []string{"a", "b", "c", "d"}, 2, []string{"c", "d"}},
		{"n equals len", []string{"a", "b"}, 2, []string{"a", "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, windowTail(tc.in, tc.n))
		})
	}
}

func TestResetOutputDir_CreatesIfMissing(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "child", "nested")
	require.NoError(t, resetOutputDir(dir))
	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

func TestResetOutputDir_WipesExistingContents(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stale.ts"), []byte("x"), 0o644))

	require.NoError(t, resetOutputDir(dir))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "resetOutputDir must wipe leftover files")
}

func TestWriteFileAtomic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "out.bin")
	require.NoError(t, writeFileAtomic(target, []byte("payload")))

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, []byte("payload"), got)
}

// ─── dash_fmp4 pure helpers ──────────────────────────────────────────────────

func TestWindowTailUint64(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   []uint64
		n    int
		want []uint64
	}{
		{"empty", nil, 3, nil},
		{"n is zero", []uint64{1, 2}, 0, []uint64{1, 2}},
		{"len <= n", []uint64{1, 2}, 5, []uint64{1, 2}},
		{"trims to last n", []uint64{1, 2, 3, 4}, 2, []uint64{3, 4}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, windowTailUint64(tc.in, tc.n))
		})
	}
}

func TestTotalQueuedVideoDur90k(t *testing.T) {
	t.Parallel()
	assert.Equal(t, uint64(0), totalQueuedVideoDur90k(nil))
	assert.Equal(t, uint64(0), totalQueuedVideoDur90k([]uint64{42}))
	// First DTS = 1000, last = 5000 → diff = 4000 → ×90 = 360000.
	assert.Equal(t, uint64(360000), totalQueuedVideoDur90k([]uint64{1000, 3000, 5000}))
}

func TestParseDashSegNum(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		kind byte
		in   string
		want int
	}{
		{"video happy", 'v', "seg_v_00042.m4s", 42},
		{"audio happy", 'a', "seg_a_00012.m4s", 12},
		{"wrong kind returns 0", 'v', "seg_a_00007.m4s", 0},
		{"wrong suffix returns 0", 'v', "seg_v_00007.ts", 0},
		{"missing prefix returns 0", 'v', "00007.m4s", 0},
		{"non-numeric core returns 0", 'v', "seg_v_xxx.m4s", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, parseDashSegNum(tc.kind, tc.in))
		})
	}
}

func TestSplitAnnexBNALUs(t *testing.T) {
	t.Parallel()

	// 4-byte start code + NALU + 3-byte start code + NALU.
	stream := []byte{
		0x00, 0x00, 0x00, 0x01, // 4-byte SC
		0x67, 0x42, 0x00, 0x1e, // NALU 1
		0x00, 0x00, 0x01, // 3-byte SC
		0x68, 0xce, 0x3c, 0x80, // NALU 2
	}
	nalus := splitAnnexBNALUs(stream)
	require.Len(t, nalus, 2)
	assert.Equal(t, []byte{0x67, 0x42, 0x00, 0x1e}, nalus[0])
	assert.Equal(t, []byte{0x68, 0xce, 0x3c, 0x80}, nalus[1])

	// Empty input.
	assert.Nil(t, splitAnnexBNALUs(nil))
	// No start code → no NALUs (start stays -1, loop falls through).
	assert.Nil(t, splitAnnexBNALUs([]byte{0xab, 0xcd, 0xef}))
}

func TestH264AnnexBToAVCC(t *testing.T) {
	t.Parallel()

	// Empty input returns nil.
	assert.Nil(t, h264AnnexBToAVCC(nil))

	// Annex-B stream with one NALU. avc.ConvertByteStreamToNaluSample handles
	// this; the output is at least the NALU bytes plus a 4-byte length prefix.
	stream := []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x88, 0x84, 0x21}
	out := h264AnnexBToAVCC(stream)
	assert.NotEmpty(t, out)
}

func TestHEVCAnnexBToAVCC(t *testing.T) {
	t.Parallel()

	// Empty input returns nil.
	assert.Nil(t, hevcAnnexBToAVCC(nil))

	// Annex-B stream with two NALUs.
	stream := []byte{
		0x00, 0x00, 0x00, 0x01,
		0x40, 0x01, 0x0c, 0x01, // VPS-ish payload
		0x00, 0x00, 0x01,
		0x42, 0x01, 0x01,
	}
	out := hevcAnnexBToAVCC(stream)
	require.NotEmpty(t, out)
	// AVCC has a 4-byte length prefix per NALU; with two NALUs total length
	// must exceed the longer single NALU payload.
	assert.Greater(t, len(out), 4)
}

// ─── runtime ─────────────────────────────────────────────────────────────────

func TestPushStatusToInt(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 3, pushStatusToInt(PushStatusActive))
	assert.Equal(t, 2, pushStatusToInt(PushStatusStarting))
	assert.Equal(t, 1, pushStatusToInt(PushStatusReconnecting))
	assert.Equal(t, 0, pushStatusToInt(PushStatusFailed))
	assert.Equal(t, 0, pushStatusToInt(PushStatus("unknown")), "unknown values default to 0")
}
