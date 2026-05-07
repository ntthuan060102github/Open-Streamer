package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/dvr"
	"github.com/ntt0601zcoder/open-streamer/internal/store"
)

// ─── Stubs ────────────────────────────────────────────────────────────────────

type fakeRecRepo struct {
	mu      sync.Mutex
	byID    map[domain.RecordingID]*domain.Recording
	byCode  map[domain.StreamCode][]*domain.Recording
	listErr error
	findErr error
}

func newFakeRecRepo() *fakeRecRepo {
	return &fakeRecRepo{
		byID:   make(map[domain.RecordingID]*domain.Recording),
		byCode: make(map[domain.StreamCode][]*domain.Recording),
	}
}

func (f *fakeRecRepo) seed(rec *domain.Recording) {
	f.mu.Lock()
	defer f.mu.Unlock()
	clone := *rec
	f.byID[rec.ID] = &clone
	f.byCode[rec.StreamCode] = append(f.byCode[rec.StreamCode], &clone)
}

func (f *fakeRecRepo) ListByStream(_ context.Context, code domain.StreamCode) ([]*domain.Recording, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.byCode[code], nil
}

func (f *fakeRecRepo) List(_ context.Context) ([]*domain.Recording, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]*domain.Recording, 0, len(f.byID))
	for _, r := range f.byID {
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeRecRepo) FindByID(_ context.Context, id domain.RecordingID) (*domain.Recording, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.findErr != nil {
		return nil, f.findErr
	}
	if r, ok := f.byID[id]; ok {
		clone := *r
		return &clone, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeRecRepo) Save(_ context.Context, r *domain.Recording) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	clone := *r
	f.byID[r.ID] = &clone
	return nil
}

func (f *fakeRecRepo) Delete(_ context.Context, id domain.RecordingID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.byID, id)
	return nil
}

// stubDVR implements dvrIndexReader with controllable returns. nil on a
// field means "use the natural disk-loader" — but tests override per
// case to avoid fs side-effects.
type stubDVR struct {
	indexByDir map[string]*domain.DVRIndex
	indexErr   error
	playByDir  map[string][]dvr.SegmentMeta
	playErr    error
}

func (s *stubDVR) LoadIndex(segDir string) (*domain.DVRIndex, error) {
	if s.indexErr != nil {
		return nil, s.indexErr
	}
	return s.indexByDir[segDir], nil
}

func (s *stubDVR) ParsePlaylist(segDir string) ([]dvr.SegmentMeta, error) {
	if s.playErr != nil {
		return nil, s.playErr
	}
	return s.playByDir[segDir], nil
}

// newRecHandlerForTest is the recording-handler analogue of
// newStreamHandlerForTest. Returns the handler + the fake repo + the stub
// DVR so tests can drive both.
func newRecHandlerForTest(t *testing.T) (*RecordingHandler, *fakeRecRepo, *stubDVR) {
	t.Helper()
	repo := newFakeRecRepo()
	d := &stubDVR{indexByDir: map[string]*domain.DVRIndex{}, playByDir: map[string][]dvr.SegmentMeta{}}
	h := &RecordingHandler{
		dvr:     d,
		recRepo: repo,
	}
	return h, repo, d
}

// ─── ListByStream ─────────────────────────────────────────────────────────────

func TestRecordingHandler_ListByStream(t *testing.T) {
	t.Parallel()
	h, repo, _ := newRecHandlerForTest(t)
	repo.seed(&domain.Recording{ID: "live", StreamCode: "live"})

	w := httptest.NewRecorder()
	h.ListByStream(w, chiReq(t, http.MethodGet, "/streams/live/recordings", nil, map[string]string{"code": "live"}))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"total":1`)
}

func TestRecordingHandler_ListByStream_RepoError(t *testing.T) {
	t.Parallel()
	h, repo, _ := newRecHandlerForTest(t)
	repo.listErr = errors.New("backend unreachable")
	w := httptest.NewRecorder()
	h.ListByStream(w, chiReq(t, http.MethodGet, "/streams/live/recordings", nil, map[string]string{"code": "live"}))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// ─── Get ─────────────────────────────────────────────────────────────────────

func TestRecordingHandler_Get_Found(t *testing.T) {
	t.Parallel()
	h, repo, _ := newRecHandlerForTest(t)
	repo.seed(&domain.Recording{ID: "live", StreamCode: "live", Status: domain.RecordingStatusRecording})

	w := httptest.NewRecorder()
	h.Get(w, chiReq(t, http.MethodGet, "/recordings/live", nil, map[string]string{"rid": "live"}))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"status":"recording"`)
}

func TestRecordingHandler_Get_NotFound(t *testing.T) {
	t.Parallel()
	h, _, _ := newRecHandlerForTest(t)
	w := httptest.NewRecorder()
	h.Get(w, chiReq(t, http.MethodGet, "/recordings/missing", nil, map[string]string{"rid": "missing"}))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// ─── Info ────────────────────────────────────────────────────────────────────

func TestRecordingHandler_Info_NoSegmentDirReturns404(t *testing.T) {
	t.Parallel()
	h, repo, _ := newRecHandlerForTest(t)
	repo.seed(&domain.Recording{ID: "blank", StreamCode: "blank"}) // SegmentDir empty
	w := httptest.NewRecorder()
	h.Info(w, chiReq(t, http.MethodGet, "/recordings/blank/info", nil, map[string]string{"rid": "blank"}))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestRecordingHandler_Info_HappyPath(t *testing.T) {
	t.Parallel()
	h, repo, d := newRecHandlerForTest(t)
	dir := t.TempDir()
	repo.seed(&domain.Recording{ID: "live", StreamCode: "live", SegmentDir: dir})
	d.indexByDir[dir] = &domain.DVRIndex{
		StartedAt:      time.Now().Add(-time.Hour),
		LastSegmentAt:  time.Now().Add(-5 * time.Minute),
		SegmentCount:   42,
		TotalSizeBytes: 12345,
	}

	w := httptest.NewRecorder()
	h.Info(w, chiReq(t, http.MethodGet, "/recordings/live/info", nil, map[string]string{"rid": "live"}))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"segment_count":42`)
}

func TestRecordingHandler_Info_IndexErrorReturns500(t *testing.T) {
	t.Parallel()
	h, repo, d := newRecHandlerForTest(t)
	dir := t.TempDir()
	repo.seed(&domain.Recording{ID: "live", StreamCode: "live", SegmentDir: dir})
	d.indexErr = errors.New("disk gone")
	w := httptest.NewRecorder()
	h.Info(w, chiReq(t, http.MethodGet, "/recordings/live/info", nil, map[string]string{"rid": "live"}))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestRecordingHandler_Info_NilIndexReturns404(t *testing.T) {
	t.Parallel()
	h, repo, d := newRecHandlerForTest(t)
	dir := t.TempDir()
	repo.seed(&domain.Recording{ID: "live", StreamCode: "live", SegmentDir: dir})
	d.indexByDir[dir] = nil // explicit nil — handler must treat as "no data yet"
	w := httptest.NewRecorder()
	h.Info(w, chiReq(t, http.MethodGet, "/recordings/live/info", nil, map[string]string{"rid": "live"}))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// ─── ServeFile ──────────────────────────────────────────────────────────────

func TestRecordingHandler_ServeFile_PlaylistFromDisk(t *testing.T) {
	t.Parallel()
	h, repo, _ := newRecHandlerForTest(t)
	dir := t.TempDir()
	playlist := filepath.Join(dir, "playlist.m3u8")
	require.NoError(t, os.WriteFile(playlist, []byte("#EXTM3U\n#EXT-X-VERSION:3\n"), 0o644))
	repo.seed(&domain.Recording{ID: "live", StreamCode: "live", SegmentDir: dir})

	w := httptest.NewRecorder()
	h.ServeFile(w, chiReq(t, http.MethodGet, "/recordings/live/playlist.m3u8", nil, map[string]string{
		"rid": "live", "file": "playlist.m3u8",
	}))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/vnd.apple.mpegurl", w.Header().Get("Content-Type"))
	assert.Contains(t, w.Body.String(), "#EXTM3U")
}

func TestRecordingHandler_ServeFile_TSSegment(t *testing.T) {
	t.Parallel()
	h, repo, _ := newRecHandlerForTest(t)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "000001.ts"), []byte("\x47\x00"), 0o644))
	repo.seed(&domain.Recording{ID: "live", StreamCode: "live", SegmentDir: dir})

	w := httptest.NewRecorder()
	h.ServeFile(w, chiReq(t, http.MethodGet, "/recordings/live/000001.ts", nil, map[string]string{
		"rid": "live", "file": "000001.ts",
	}))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "video/mp2t", w.Header().Get("Content-Type"))
}

func TestRecordingHandler_ServeFile_UnsupportedExtensionReturns404(t *testing.T) {
	t.Parallel()
	h, repo, _ := newRecHandlerForTest(t)
	repo.seed(&domain.Recording{ID: "live", StreamCode: "live", SegmentDir: t.TempDir()})

	w := httptest.NewRecorder()
	h.ServeFile(w, chiReq(t, http.MethodGet, "/recordings/live/index.json", nil, map[string]string{
		"rid": "live", "file": "index.json",
	}))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestRecordingHandler_ServeFile_NoSegmentDirReturns404(t *testing.T) {
	t.Parallel()
	h, repo, _ := newRecHandlerForTest(t)
	repo.seed(&domain.Recording{ID: "live", StreamCode: "live"})
	w := httptest.NewRecorder()
	h.ServeFile(w, chiReq(t, http.MethodGet, "/recordings/live/playlist.m3u8", nil, map[string]string{
		"rid": "live", "file": "playlist.m3u8",
	}))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// ─── serveTimeshift dispatch via ServeFile ──────────────────────────────────

func TestRecordingHandler_ServeFile_TimeshiftFromOffset(t *testing.T) {
	t.Parallel()
	h, repo, d := newRecHandlerForTest(t)
	dir := t.TempDir()
	repo.seed(&domain.Recording{ID: "live", StreamCode: "live", SegmentDir: dir})

	// Stub two parsed segments — handler builds a sliced VOD playlist.
	base := time.Now().Add(-time.Hour)
	d.playByDir[dir] = []dvr.SegmentMeta{
		{Index: 1, WallTime: base, Duration: 4 * time.Second},
		{Index: 2, WallTime: base.Add(4 * time.Second), Duration: 4 * time.Second},
	}

	r := chiReq(t, http.MethodGet, "/recordings/live/playlist.m3u8?offset_sec=0", nil,
		map[string]string{"rid": "live", "file": "playlist.m3u8"})
	w := httptest.NewRecorder()
	h.ServeFile(w, r)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "#EXT-X-PLAYLIST-TYPE:VOD")
	assert.Contains(t, w.Body.String(), "#EXT-X-ENDLIST")
}

func TestRecordingHandler_ServeFile_TimeshiftInvalidFromReturns400(t *testing.T) {
	t.Parallel()
	h, repo, d := newRecHandlerForTest(t)
	dir := t.TempDir()
	repo.seed(&domain.Recording{ID: "live", StreamCode: "live", SegmentDir: dir})
	d.playByDir[dir] = []dvr.SegmentMeta{{Index: 1, WallTime: time.Now(), Duration: time.Second}}

	r := chiReq(t, http.MethodGet, "/recordings/live/playlist.m3u8?from=garbage", nil,
		map[string]string{"rid": "live", "file": "playlist.m3u8"})
	w := httptest.NewRecorder()
	h.ServeFile(w, r)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRecordingHandler_ServeFile_TimeshiftEmptyPlaylistReturns404(t *testing.T) {
	t.Parallel()
	h, repo, d := newRecHandlerForTest(t)
	dir := t.TempDir()
	repo.seed(&domain.Recording{ID: "live", StreamCode: "live", SegmentDir: dir})
	d.playByDir[dir] = nil // no segments yet

	r := chiReq(t, http.MethodGet, "/recordings/live/playlist.m3u8?offset_sec=0", nil,
		map[string]string{"rid": "live", "file": "playlist.m3u8"})
	w := httptest.NewRecorder()
	h.ServeFile(w, r)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestRecordingHandler_ServeFile_TimeshiftWindowOutOfRange(t *testing.T) {
	t.Parallel()
	h, repo, d := newRecHandlerForTest(t)
	dir := t.TempDir()
	repo.seed(&domain.Recording{ID: "live", StreamCode: "live", SegmentDir: dir})
	d.playByDir[dir] = []dvr.SegmentMeta{
		{Index: 1, WallTime: time.Now().Add(-time.Hour), Duration: 4 * time.Second},
	}

	// offset_sec far past every segment → window is empty → 404.
	r := chiReq(t, http.MethodGet, "/recordings/live/playlist.m3u8?offset_sec=86400", nil,
		map[string]string{"rid": "live", "file": "playlist.m3u8"})
	w := httptest.NewRecorder()
	h.ServeFile(w, r)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestRecordingHandler_ServeFile_TimeshiftInvalidDurationReturns400(t *testing.T) {
	t.Parallel()
	h, repo, d := newRecHandlerForTest(t)
	dir := t.TempDir()
	repo.seed(&domain.Recording{ID: "live", StreamCode: "live", SegmentDir: dir})
	d.playByDir[dir] = []dvr.SegmentMeta{{Index: 1, WallTime: time.Now(), Duration: time.Second}}

	r := chiReq(t, http.MethodGet, "/recordings/live/playlist.m3u8?offset_sec=0&duration=-1", nil,
		map[string]string{"rid": "live", "file": "playlist.m3u8"})
	w := httptest.NewRecorder()
	h.ServeFile(w, r)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}
