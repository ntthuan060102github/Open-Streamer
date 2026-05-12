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

// Tests for the dash-specific helpers (windowTailUint64,
// totalQueuedVideoDur90k, parseDashSegNum, splitAnnexBNALUs,
// h264AnnexBToAVCC, hevcAnnexBToAVCC) used to live here; those
// helpers moved to internal/publisher/dash/ as part of the DASH
// rewrite and are now covered by tests inside that package.

// ─── runtime ─────────────────────────────────────────────────────────────────

func TestPushStatusToInt(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 3, pushStatusToInt(PushStatusActive))
	assert.Equal(t, 2, pushStatusToInt(PushStatusStarting))
	assert.Equal(t, 1, pushStatusToInt(PushStatusReconnecting))
	assert.Equal(t, 0, pushStatusToInt(PushStatusFailed))
	assert.Equal(t, 0, pushStatusToInt(PushStatus("unknown")), "unknown values default to 0")
}
