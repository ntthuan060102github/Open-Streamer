package handler

import (
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/dvr"
	"github.com/ntt0601zcoder/open-streamer/internal/store"
	"github.com/samber/do/v2"
)

// dvrIndexReader narrows *dvr.Service to the on-disk-read methods this
// handler uses, so tests can stub out the DVR service without wiring a
// full buffer + bus + metrics graph behind it. *dvr.Service satisfies
// implicitly.
type dvrIndexReader interface {
	LoadIndex(segDir string) (*domain.DVRIndex, error)
	ParsePlaylist(segDir string) ([]dvr.SegmentMeta, error)
}

// RecordingHandler handles DVR recording and playback REST endpoints.
type RecordingHandler struct {
	dvr     dvrIndexReader
	recRepo store.RecordingRepository
}

// NewRecordingHandler creates a RecordingHandler and registers it with the DI injector.
func NewRecordingHandler(i do.Injector) (*RecordingHandler, error) {
	return &RecordingHandler{
		dvr:     do.MustInvoke[*dvr.Service](i),
		recRepo: do.MustInvoke[store.RecordingRepository](i),
	}, nil
}

// ListByStream lists recording metadata for a stream.
// @Summary List recordings by stream
// @Tags recordings
// @Produce json
// @Param code path string true "Stream code"
// @Success 200 {object} apidocs.RecordingList
// @Failure 500 {object} apidocs.ErrorBody
// @Router /streams/{code}/recordings [get].
func (h *RecordingHandler) ListByStream(w http.ResponseWriter, r *http.Request) {
	streamCode := domain.StreamCode(chi.URLParam(r, "code"))
	recs, err := h.recRepo.ListByStream(r.Context(), streamCode)
	if err != nil {
		serverError(w, r, "LIST_FAILED", "list recordings", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": recs, "total": len(recs)})
}

// Get returns one recording's lifecycle metadata by id.
// @Summary Get recording
// @Tags recordings
// @Produce json
// @Param rid path string true "Recording ID (= stream code)"
// @Success 200 {object} apidocs.RecordingData
// @Failure 404 {object} apidocs.ErrorBody
// @Router /recordings/{rid} [get].
func (h *RecordingHandler) Get(w http.ResponseWriter, r *http.Request) {
	rid := domain.RecordingID(chi.URLParam(r, "rid"))
	rec, err := h.recRepo.FindByID(r.Context(), rid)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": rec})
}

// Info returns full DVR metadata for a recording: dvr_range, gaps, segment count, total size.
// Data is read from index.json on disk, so it is always up-to-date.
// @Summary DVR recording info
// @Tags recordings
// @Produce json
// @Param rid path string true "Recording ID (= stream code)"
// @Success 200 {object} map[string]any
// @Failure 404 {object} apidocs.ErrorBody
// @Router /recordings/{rid}/info [get].
func (h *RecordingHandler) Info(w http.ResponseWriter, r *http.Request) {
	rid := domain.RecordingID(chi.URLParam(r, "rid"))
	rec, err := h.recRepo.FindByID(r.Context(), rid)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if rec.SegmentDir == "" {
		writeError(w, http.StatusNotFound, "NO_DATA", "recording has no data yet")
		return
	}

	idx, err := h.dvr.LoadIndex(rec.SegmentDir)
	if err != nil {
		serverError(w, r, "INDEX_READ_FAILED", "load dvr index", err)
		return
	}
	if idx == nil {
		writeError(w, http.StatusNotFound, "NO_DATA", "recording has no data yet")
		return
	}

	type dvrRange struct {
		StartedAt     time.Time `json:"started_at"`
		LastSegmentAt time.Time `json:"last_segment_at,omitempty"`
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"data": map[string]any{
			"stream_code": rec.StreamCode,
			"status":      rec.Status,
			"dvr_range": dvrRange{
				StartedAt:     idx.StartedAt,
				LastSegmentAt: idx.LastSegmentAt,
			},
			"gaps":             idx.Gaps,
			"segment_count":    idx.SegmentCount,
			"total_size_bytes": idx.TotalSizeBytes,
		},
	})
}

// ServeFile serves files from a DVR recording's SegmentDir. It is the
// single endpoint behind /recordings/{rid}/{file} — the legacy
// /playlist.m3u8 and /timeshift.m3u8 routes were collapsed in favour of a
// uniform dispatcher because everything ultimately resolves to a file in
// SegmentDir, and the only behavioural fork is "static playlist file" vs
// "dynamic VOD slice over a time window".
//
// Behaviour by request shape:
//
//   - file=*.ts                                  — serve the segment as-is
//   - file=*.m3u8 (no `from`/`offset_sec` query) — serve playlist.m3u8 from disk
//   - file=*.m3u8 with `from` or `offset_sec`    — build a dynamic VOD slice
//     (timeshift); see serveTimeshift for query param semantics
//
// Other extensions return 404 — DVR only writes .ts + .m3u8 + index.json.
//
// @Summary  Serve recording file (segment or playlist; timeshift via query)
// @Tags     recordings
// @Produce  octet-stream
// @Param    rid        path  string true  "Recording ID (= stream code)"
// @Param    file       path  string true  "Segment filename (e.g. 000000.ts) or playlist.m3u8"
// @Param    from       query string false "Timeshift: absolute start (RFC3339). Implies M3U8 dispatch"
// @Param    offset_sec query int    false "Timeshift: relative start (seconds from recording start)"
// @Param    duration   query int    false "Timeshift: window length in seconds (default: all remaining)"
// @Success  200 {file} binary
// @Failure  404 {object} apidocs.ErrorBody
// @Router   /recordings/{rid}/{file} [get].
func (h *RecordingHandler) ServeFile(w http.ResponseWriter, r *http.Request) {
	rid := domain.RecordingID(chi.URLParam(r, "rid"))
	file := filepath.Base(chi.URLParam(r, "file")) // strip path traversal

	rec, err := h.recRepo.FindByID(r.Context(), rid)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if rec.SegmentDir == "" {
		writeError(w, http.StatusNotFound, "NO_DATA", "recording has no data yet")
		return
	}

	switch strings.ToLower(filepath.Ext(file)) {
	case ".m3u8":
		// Timeshift dispatch: presence of either query param flips the
		// route to the dynamic slice builder, regardless of which file
		// name the client used (some players append cache-busters to the
		// URL — keep the trigger keyed on params, not on path).
		q := r.URL.Query()
		if q.Has("from") || q.Has("offset_sec") {
			h.serveTimeshift(w, r, rec)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, filepath.Join(rec.SegmentDir, "playlist.m3u8"))
	case ".ts":
		w.Header().Set("Content-Type", "video/mp2t")
		http.ServeFile(w, r, filepath.Join(rec.SegmentDir, file))
	default:
		writeError(w, http.StatusNotFound, "FILE_NOT_FOUND", "unsupported file type for recording")
	}
}

// serveTimeshift builds a dynamic VOD M3U8 sliced over a wall-clock window.
//
// Query parameters (mutually exclusive for start point):
//   - from=<RFC3339>   absolute start time, e.g. 2026-04-06T14:30:00Z
//   - offset_sec=<N>   seconds from recording start (relative)
//
// Optional:
//   - duration=<N>     window length in seconds (default: all remaining)
func (h *RecordingHandler) serveTimeshift(w http.ResponseWriter, r *http.Request, rec *domain.Recording) {
	// Parse all segments from playlist.m3u8.
	segments, err := h.dvr.ParsePlaylist(rec.SegmentDir)
	if err != nil || len(segments) == 0 {
		writeError(w, http.StatusNotFound, "NO_PLAYLIST", "playlist not yet available")
		return
	}

	// Determine start time.
	var startTime time.Time
	if raw := r.URL.Query().Get("from"); raw != "" {
		startTime, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_FROM", "from must be RFC3339, e.g. 2026-04-06T14:30:00Z")
			return
		}
	} else if raw := r.URL.Query().Get("offset_sec"); raw != "" {
		offsetSec, err := strconv.ParseFloat(raw, 64)
		if err != nil || offsetSec < 0 {
			writeError(w, http.StatusBadRequest, "INVALID_OFFSET", "offset_sec must be a non-negative number")
			return
		}
		// Anchor to first segment's wall time + offset.
		startTime = segments[0].WallTime.Add(time.Duration(offsetSec * float64(time.Second)))
	} else {
		// No start → return from beginning (equivalent to full playlist).
		startTime = segments[0].WallTime
	}

	// Determine window duration (0 = unlimited).
	var windowDur time.Duration
	if raw := r.URL.Query().Get("duration"); raw != "" {
		durSec, err := strconv.ParseFloat(raw, 64)
		if err != nil || durSec <= 0 {
			writeError(w, http.StatusBadRequest, "INVALID_DURATION", "duration must be a positive number of seconds")
			return
		}
		windowDur = time.Duration(durSec * float64(time.Second))
	}

	// Slice the segment list to the requested window.
	// A segment is included if its wall_time >= startTime and it falls within windowDur.
	endTime := startTime.Add(windowDur)
	window := make([]dvr.SegmentMeta, 0, len(segments))
	for _, seg := range segments {
		if seg.WallTime.IsZero() {
			continue
		}
		segEnd := seg.WallTime.Add(seg.Duration)
		// Include if segment overlaps [startTime, endTime).
		if segEnd.Before(startTime) || segEnd.Equal(startTime) {
			continue
		}
		if windowDur > 0 && seg.WallTime.After(endTime) {
			break
		}
		window = append(window, seg)
	}

	if len(window) == 0 {
		writeError(w, http.StatusNotFound, "NO_SEGMENTS_IN_RANGE", "no segments found for the requested time range")
		return
	}

	// Build VOD M3U8.
	maxSec := 1.0
	for _, seg := range window {
		if d := seg.Duration.Seconds(); d > maxSec {
			maxSec = d
		}
	}

	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", int(math.Ceil(maxSec)))
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	b.WriteByte('\n')

	needDateTime := true
	for _, seg := range window {
		if seg.Discontinuity {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
			needDateTime = true
		}
		if needDateTime && !seg.WallTime.IsZero() {
			fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n",
				seg.WallTime.UTC().Format("2006-01-02T15:04:05.000Z"))
			needDateTime = false
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n", seg.Duration.Seconds())
		fmt.Fprintf(&b, "%06d.ts\n", seg.Index)
	}
	b.WriteString("#EXT-X-ENDLIST\n")

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}
