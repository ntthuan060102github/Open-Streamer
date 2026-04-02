package pull

// file.go — pull reader for local media files.
//
// Three container formats are supported without FFmpeg:
//
//	MPEG-TS (.ts .mts .m2ts)  — raw passthrough; bytes emitted as-is at
//	                             read speed (no pacing, no re-mux).
//	MP4     (.mp4 .m4v .mov)  — demux via gomedia/go-mp4 → re-mux to TS;
//	                             real-time paced using packet DTS.
//	FLV     (.flv)             — demux via gomedia/go-flv → re-mux to TS;
//	                             real-time paced using frame DTS.
//
// URL formats accepted:
//
//	file:///absolute/path/to/source.ts
//	file:///absolute/path/to/source.ts?loop=true
//	/absolute/path/to/source.ts          (bare POSIX path)
//	relative/path/to/source.ts           (relative to CWD)
//
// When loop=true the file is rewound and replayed after EOF, simulating
// a continuous live source.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	gocodec "github.com/yapingcat/gomedia/go-codec"
	goflv "github.com/yapingcat/gomedia/go-flv"
	gomp4 "github.com/yapingcat/gomedia/go-mp4"
	gompeg2 "github.com/yapingcat/gomedia/go-mpeg2"

	"github.com/ntthuan060102github/open-streamer/internal/domain"
)

const (
	fileTSChunk   = 188 * 56    // ~10 KB; aligned to TS packet boundary
	flvReadBuf    = 32 * 1024   // 32 KB read buffer for FLV chunk feeding
)

// fileHandler is the internal strategy selected based on the file extension.
// Each implementation is single-goroutine — the caller must not call read
// and close concurrently.
type fileHandler interface {
	read(ctx context.Context) ([]byte, error)
	close() error
}

// FileReader is a pull source for local media files.
// It implements TSChunkReader and is intended to wrap with NewTSDemuxPacketReader.
type FileReader struct {
	path    string
	loop    bool
	handler fileHandler
}

// NewFileReader constructs a FileReader without opening the file.
// Accepts file:// URLs and bare filesystem paths.
func NewFileReader(input domain.Input) *FileReader {
	path, loop := parseFileURL(input.URL)
	return &FileReader{path: path, loop: loop}
}

// Open opens the file and selects the appropriate handler based on extension.
// Returns an error if the path does not exist or is a directory.
func (r *FileReader) Open(_ context.Context) error {
	if r.handler != nil {
		return nil // idempotent
	}

	st, err := os.Stat(r.path)
	if err != nil {
		return fmt.Errorf("file reader: stat %q: %w", r.path, err)
	}
	if st.IsDir() {
		return fmt.Errorf("file reader: %q is a directory", r.path)
	}

	switch strings.ToLower(filepath.Ext(r.path)) {
	case ".mp4", ".m4v", ".mov":
		h, err := newMP4Handler(r.path, r.loop)
		if err != nil {
			return fmt.Errorf("file reader: mp4 init %q: %w", r.path, err)
		}
		r.handler = h

	case ".flv":
		h, err := newFLVHandler(r.path, r.loop)
		if err != nil {
			return fmt.Errorf("file reader: flv init %q: %w", r.path, err)
		}
		r.handler = h

	default:
		// .ts, .mts, .m2ts or unknown → raw passthrough
		h, err := newTSHandler(r.path, r.loop)
		if err != nil {
			return fmt.Errorf("file reader: open %q: %w", r.path, err)
		}
		r.handler = h
	}

	return nil
}

// Read returns the next raw MPEG-TS chunk.
// Returns io.EOF when the file is exhausted (and loop is not set).
func (r *FileReader) Read(ctx context.Context) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if r.handler == nil {
		return nil, fmt.Errorf("file reader: not opened")
	}
	return r.handler.read(ctx)
}

// Close releases the open file handle.
// Calling Close before Open, or calling it twice, is safe.
func (r *FileReader) Close() error {
	if r.handler == nil {
		return nil
	}
	err := r.handler.close()
	r.handler = nil
	return err
}

// ─── URL helpers ─────────────────────────────────────────────────────────────

// parseFileURL returns the filesystem path and loop flag from an input URL.
// Accepts file:///path?loop=true and bare POSIX / relative paths.
func parseFileURL(rawURL string) (path string, loop bool) {
	if strings.HasPrefix(rawURL, "file://") {
		if u, err := url.Parse(rawURL); err == nil {
			return u.Path, u.Query().Get("loop") == "true"
		}
	}
	return rawURL, false
}

// ─── MPEG-TS passthrough ─────────────────────────────────────────────────────

type tsHandler struct {
	path string
	loop bool
	f    *os.File
	buf  []byte
}

func newTSHandler(path string, loop bool) (*tsHandler, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &tsHandler{
		path: path,
		loop: loop,
		f:    f,
		buf:  make([]byte, fileTSChunk),
	}, nil
}

func (h *tsHandler) read(ctx context.Context) ([]byte, error) {
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		n, err := h.f.Read(h.buf)
		if n > 0 {
			out := make([]byte, n)
			copy(out, h.buf[:n])
			return out, nil
		}

		if !errors.Is(err, io.EOF) {
			return nil, err
		}

		if !h.loop {
			return nil, io.EOF
		}
		if _, seekErr := h.f.Seek(0, io.SeekStart); seekErr != nil {
			return nil, fmt.Errorf("file reader: ts loop seek: %w", seekErr)
		}
	}
}

func (h *tsHandler) close() error {
	if h.f == nil {
		return nil
	}
	err := h.f.Close()
	h.f = nil
	return err
}

// ─── MP4 → MPEG-TS ──────────────────────────────────────────────────────────

type mp4Handler struct {
	path  string
	loop  bool
	f     *os.File
	demux *gomp4.MovDemuxer
	mux   *gompeg2.TSMuxer
	queue [][]byte

	vpid uint16
	apid uint16
	vset bool
	aset bool

	// real-time pacing: emit at original media rate
	paceOnce bool
	paceRef  uint64    // DTS of first packet (ms)
	paceAt   time.Time // wall-clock of first packet
}

func newMP4Handler(path string, loop bool) (*mp4Handler, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	h := &mp4Handler{path: path, loop: loop, f: f}
	h.mux = gompeg2.NewTSMuxer()
	h.mux.OnPacket = func(pkg []byte) {
		out := make([]byte, len(pkg))
		copy(out, pkg)
		h.queue = append(h.queue, out)
	}

	if err := h.reset(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return h, nil
}

func (h *mp4Handler) reset() error {
	if _, err := h.f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("mp4 seek: %w", err)
	}
	dmx := gomp4.CreateMp4Demuxer(h.f)
	if _, err := dmx.ReadHead(); err != nil {
		return fmt.Errorf("mp4 read head: %w", err)
	}
	h.demux = dmx
	h.queue = h.queue[:0]
	h.vpid, h.apid = 0, 0
	h.vset, h.aset = false, false
	h.paceOnce = false
	return nil
}

func (h *mp4Handler) read(ctx context.Context) ([]byte, error) {
	for {
		if len(h.queue) > 0 {
			out := h.queue[0]
			h.queue = h.queue[1:]
			return out, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err := h.feedNextPacket(ctx); err != nil {
			return nil, err
		}
	}
}

func (h *mp4Handler) feedNextPacket(ctx context.Context) error {
	pkt, err := h.demux.ReadPacket()
	if errors.Is(err, io.EOF) {
		if !h.loop {
			return io.EOF
		}
		return h.reset()
	}
	if err != nil {
		return fmt.Errorf("mp4 read packet: %w", err)
	}

	h.pace(ctx, pkt.Dts)
	h.muxPacket(pkt)
	return nil
}

func (h *mp4Handler) muxPacket(pkt *gomp4.AVPacket) {
	switch pkt.Cid {
	case gomp4.MP4_CODEC_H264:
		if !h.vset {
			h.vpid = h.mux.AddStream(gompeg2.TS_STREAM_H264)
			h.vset = true
		}
		_ = h.mux.Write(h.vpid, pkt.Data, pkt.Pts, pkt.Dts)

	case gomp4.MP4_CODEC_H265:
		if !h.vset {
			h.vpid = h.mux.AddStream(gompeg2.TS_STREAM_H265)
			h.vset = true
		}
		_ = h.mux.Write(h.vpid, pkt.Data, pkt.Pts, pkt.Dts)

	case gomp4.MP4_CODEC_AAC:
		if !h.aset {
			h.apid = h.mux.AddStream(gompeg2.TS_STREAM_AAC)
			h.aset = true
		}
		_ = h.mux.Write(h.apid, pkt.Data, pkt.Pts, pkt.Dts)
	}
}

func (h *mp4Handler) pace(ctx context.Context, dtsMS uint64) {
	if !h.paceOnce {
		h.paceOnce = true
		h.paceRef = dtsMS
		h.paceAt = time.Now()
		return
	}
	target := h.paceAt.Add(time.Duration(dtsMS-h.paceRef) * time.Millisecond)
	if wait := time.Until(target); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
	}
}

func (h *mp4Handler) close() error {
	if h.f == nil {
		return nil
	}
	err := h.f.Close()
	h.f = nil
	return err
}

// ─── FLV → MPEG-TS ──────────────────────────────────────────────────────────

type flvHandler struct {
	path   string
	loop   bool
	f      *os.File
	reader *goflv.FlvReader
	mux    *gompeg2.TSMuxer
	queue  [][]byte
	buf    []byte

	vpid uint16
	apid uint16
	vset bool
	aset bool

	// real-time pacing — ctx stored during read so the OnFrame callback can use it.
	// Safe because read is always called from a single goroutine and OnFrame fires
	// synchronously inside reader.Input (no concurrency).
	paceCtx  context.Context
	paceOnce bool
	paceRef  uint32
	paceAt   time.Time
}

func newFLVHandler(path string, loop bool) (*flvHandler, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	h := &flvHandler{
		path: path,
		loop: loop,
		f:    f,
		buf:  make([]byte, flvReadBuf),
	}
	h.mux = gompeg2.NewTSMuxer()
	h.mux.OnPacket = func(pkg []byte) {
		out := make([]byte, len(pkg))
		copy(out, pkg)
		h.queue = append(h.queue, out)
	}
	h.buildReader()
	return h, nil
}

// buildReader creates (or recreates) the stateful FlvReader with all callbacks wired.
func (h *flvHandler) buildReader() {
	r := goflv.CreateFlvReader()
	r.OnFrame = func(cid gocodec.CodecID, frame []byte, pts uint32, dts uint32) {
		ctx := h.paceCtx
		if ctx == nil {
			ctx = context.Background()
		}
		h.pace(ctx, dts)
		h.muxFrame(cid, frame, pts, dts)
	}
	h.reader = r
}

// muxFrame routes a decoded FLV frame to the MPEG-TS muxer, adding the stream
// track on first use (lazy registration matches the MP4 handler pattern).
func (h *flvHandler) muxFrame(cid gocodec.CodecID, frame []byte, pts uint32, dts uint32) {
	switch cid {
	case gocodec.CODECID_VIDEO_H264:
		if !h.vset {
			h.vpid = h.mux.AddStream(gompeg2.TS_STREAM_H264)
			h.vset = true
		}
		_ = h.mux.Write(h.vpid, frame, uint64(pts), uint64(dts))

	case gocodec.CODECID_VIDEO_H265:
		if !h.vset {
			h.vpid = h.mux.AddStream(gompeg2.TS_STREAM_H265)
			h.vset = true
		}
		_ = h.mux.Write(h.vpid, frame, uint64(pts), uint64(dts))

	case gocodec.CODECID_AUDIO_AAC:
		if !h.aset {
			h.apid = h.mux.AddStream(gompeg2.TS_STREAM_AAC)
			h.aset = true
		}
		_ = h.mux.Write(h.apid, frame, uint64(pts), uint64(dts))
	}
}

func (h *flvHandler) reset() error {
	if _, err := h.f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("flv loop seek: %w", err)
	}
	h.queue = h.queue[:0]
	h.vpid, h.apid = 0, 0
	h.vset, h.aset = false, false
	h.paceOnce = false
	h.buildReader()
	return nil
}

func (h *flvHandler) read(ctx context.Context) ([]byte, error) {
	h.paceCtx = ctx
	defer func() { h.paceCtx = nil }()

	for {
		if len(h.queue) > 0 {
			out := h.queue[0]
			h.queue = h.queue[1:]
			return out, nil
		}

		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		if err := h.feedChunk(); err != nil {
			return nil, err
		}
	}
}

// feedChunk reads one buffer of raw FLV bytes and feeds it to the parser.
// Returns io.EOF when the file is exhausted (after looping if enabled).
func (h *flvHandler) feedChunk() error {
	n, err := h.f.Read(h.buf)
	if n > 0 {
		if ferr := h.reader.Input(h.buf[:n]); ferr != nil {
			return fmt.Errorf("flv parse: %w", ferr)
		}
	}
	if err == nil {
		return nil
	}
	if !errors.Is(err, io.EOF) {
		return fmt.Errorf("flv read: %w", err)
	}
	if !h.loop {
		return io.EOF
	}
	return h.reset()
}

func (h *flvHandler) pace(ctx context.Context, dtsMS uint32) {
	if !h.paceOnce {
		h.paceOnce = true
		h.paceRef = dtsMS
		h.paceAt = time.Now()
		return
	}

	elapsed := dtsMS - h.paceRef
	if elapsed == 0 {
		return
	}
	target := h.paceAt.Add(time.Duration(elapsed) * time.Millisecond)
	if wait := time.Until(target); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
	}
}

func (h *flvHandler) close() error {
	if h.f == nil {
		return nil
	}
	err := h.f.Close()
	h.f = nil
	return err
}
