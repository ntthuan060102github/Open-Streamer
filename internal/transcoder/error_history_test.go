package transcoder

import (
	"testing"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/stretchr/testify/require"
)

// recordProfileErrorEntry contract — newest at index 0, capped at
// maxProfileErrorHistory. Same shape as recordInputError so frontend can read
// Errors[0] for the most recent crash.
func TestRecordProfileErrorEntry_OrderingAndCap(t *testing.T) {
	t.Parallel()
	pw := &profileWorker{}
	base := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 7; i++ {
		recordProfileErrorEntry(pw, profileErrMsg(i), base.Add(time.Duration(i)*time.Second))
	}

	require.Len(t, pw.errors, maxProfileErrorHistory)
	require.Equal(t, "crash-6", pw.errors[0].Message)
	require.Equal(t, "crash-2", pw.errors[maxProfileErrorHistory-1].Message)
}

// recordProfileError increments restartCount AND appends an error entry. The
// two are 1:1 — every recorded crash counts as one restart attempt.
func TestRecordProfileError_IncrementsAndRecords(t *testing.T) {
	t.Parallel()
	sw := &streamWorker{
		profiles: map[int]*profileWorker{0: {}},
	}
	s := &Service{
		workers: map[domain.StreamCode]*streamWorker{"live": sw},
	}

	s.recordProfileError("live", 0, "ffmpeg exit: status 234")
	s.recordProfileError("live", 0, "ffmpeg exit: status 1")

	require.Equal(t, 2, sw.profiles[0].restartCount)
	require.Len(t, sw.profiles[0].errors, 2)
	require.Equal(t, "ffmpeg exit: status 1", sw.profiles[0].errors[0].Message, "newest first")
	require.Equal(t, "ffmpeg exit: status 234", sw.profiles[0].errors[1].Message)
}

// recordProfileError is a no-op when stream/profile have been torn down — it
// runs from the retry loop which can fire after Stop().
func TestRecordProfileError_NoOpOnMissing(t *testing.T) {
	t.Parallel()
	s := &Service{workers: map[domain.StreamCode]*streamWorker{}}
	require.NotPanics(t, func() {
		s.recordProfileError("nope", 0, "boom")
	})
}

// RuntimeStatus exposes the per-profile state (track slug, restart count,
// defensively-copied error slice) sorted by index.
func TestRuntimeStatus_ShapeAndSort(t *testing.T) {
	t.Parallel()
	now := time.Now()
	sw := &streamWorker{
		profiles: map[int]*profileWorker{
			2: {restartCount: 3, errors: []domain.ErrorEntry{{Message: "z", At: now}}},
			0: {restartCount: 1, errors: []domain.ErrorEntry{{Message: "a", At: now}}},
			1: {restartCount: 0, errors: nil},
		},
	}
	s := &Service{workers: map[domain.StreamCode]*streamWorker{"live": sw}}

	rt, ok := s.RuntimeStatus("live")
	require.True(t, ok)
	require.Len(t, rt.Profiles, 3)
	// Sorted by index ascending — stable output across calls.
	require.Equal(t, 0, rt.Profiles[0].Index)
	require.Equal(t, 1, rt.Profiles[1].Index)
	require.Equal(t, 2, rt.Profiles[2].Index)
	require.Equal(t, buffer.VideoTrackSlug(0), rt.Profiles[0].Track)
	require.Equal(t, 1, rt.Profiles[0].RestartCount)
	require.Equal(t, 3, rt.Profiles[2].RestartCount)
	require.Empty(t, rt.Profiles[1].Errors, "no errors → omitted")
	require.Equal(t, "z", rt.Profiles[2].Errors[0].Message)

	// Mutate state after snapshot — snapshot must be unaffected.
	s.recordProfileError("live", 0, "after-snapshot")
	require.Equal(t, "a", rt.Profiles[0].Errors[0].Message, "snapshot is a defensive copy")
}

func TestRuntimeStatus_NotRunning(t *testing.T) {
	t.Parallel()
	s := &Service{workers: map[domain.StreamCode]*streamWorker{}}
	_, ok := s.RuntimeStatus("nope")
	require.False(t, ok)
}

func profileErrMsg(i int) string {
	return "crash-" + string(rune('0'+i))
}
