package pull

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/grafov/m3u8"
	"github.com/open-streamer/open-streamer/config"
	"github.com/open-streamer/open-streamer/internal/domain"
)

// HLSReader polls an HLS playlist and downloads new segments in order.
// Supports both live (sliding window) and VOD playlists.
// Expected URL format: http[s]://host/path/to/playlist.m3u8.
type HLSReader struct {
	input  domain.Input
	cfg    config.IngestorConfig
	client *http.Client

	// segments is an internal channel fed by the polling goroutine.
	segments chan []byte
	// errCh carries a terminal error from the polling goroutine.
	errCh chan error

	once   sync.Once
	cancel context.CancelFunc
}

// NewHLSReader constructs an HLSReader for the given input.
func NewHLSReader(input domain.Input, cfg config.IngestorConfig) *HLSReader {
	maxBuf := cfg.HLSMaxSegmentBuffer
	if maxBuf <= 0 {
		maxBuf = 8
	}
	return &HLSReader{
		input:    input,
		cfg:      cfg,
		segments: make(chan []byte, maxBuf),
		errCh:    make(chan error, 1),
	}
}

// Open starts background playlist polling until Close or ctx ends.
func (r *HLSReader) Open(ctx context.Context) error {
	connectTimeout := time.Duration(r.input.Net.ConnectTimeoutSec) * time.Second
	if connectTimeout == 0 {
		connectTimeout = 10 * time.Second
	}
	r.client = &http.Client{Timeout: connectTimeout}

	pollCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel

	go r.poll(pollCtx)
	return nil
}

func (r *HLSReader) Read(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case err := <-r.errCh:
		return nil, err
	case seg := <-r.segments:
		return seg, nil
	}
}

// Close stops the polling goroutine.
func (r *HLSReader) Close() error {
	r.once.Do(func() {
		if r.cancel != nil {
			r.cancel()
		}
	})
	return nil
}

// poll continuously fetches and parses the playlist, queuing new segments.
func (r *HLSReader) poll(ctx context.Context) {
	seen := make(map[string]struct{})
	var mediaSeqBase uint64

	for {
		if ctx.Err() != nil {
			return
		}

		pl, targetDuration, isVOD, err := r.fetchPlaylist(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			r.errCh <- fmt.Errorf("hls reader: fetch playlist: %w", err)
			return
		}

		if targetDuration == 0 {
			targetDuration = 2
		}

		for i, seg := range pl {
			key := segmentKey(seg, mediaSeqBase+uint64(i))
			if _, already := seen[key]; already {
				continue
			}
			seen[key] = struct{}{}

			data, err := r.fetchSegment(ctx, seg)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				slog.Warn("hls reader: segment fetch failed",
					"url", seg,
					"err", err,
				)
				continue
			}

			select {
			case r.segments <- data:
			case <-ctx.Done():
				return
			}
		}

		if isVOD {
			// VOD playlist is complete; signal EOF.
			r.errCh <- io.EOF
			return
		}

		// Wait ~half the target duration before re-polling.
		waitDur := time.Duration(targetDuration/2) * time.Second
		if waitDur < 500*time.Millisecond {
			waitDur = 500 * time.Millisecond
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(waitDur):
		}
	}
}

// fetchPlaylist downloads and parses the M3U8 at r.input.URL.
// Returns the ordered list of absolute segment URLs, the target duration,
// a flag indicating it is a finished VOD, and any error.
func (r *HLSReader) fetchPlaylist(ctx context.Context) (segs []string, targetDuration float64, isVOD bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.input.URL, nil)
	if err != nil {
		return nil, 0, false, err
	}
	for k, v := range r.input.Headers {
		req.Header.Set(k, v)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, 0, false, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, 0, false, fmt.Errorf("http %d", resp.StatusCode)
	}

	pl, listType, err := m3u8.DecodeFrom(resp.Body, true)
	if err != nil {
		return nil, 0, false, fmt.Errorf("m3u8 decode: %w", err)
	}

	base, _ := url.Parse(r.input.URL)

	switch listType {
	case m3u8.ListType(0):
		return nil, 0, false, fmt.Errorf("hls reader: undefined playlist type")
	case m3u8.MEDIA:
		media := pl.(*m3u8.MediaPlaylist)
		for _, seg := range media.Segments {
			if seg == nil {
				continue
			}
			abs := resolveURL(base, seg.URI)
			segs = append(segs, abs)
		}
		if media.Closed {
			isVOD = true
		}
		return segs, media.TargetDuration, isVOD, nil

	case m3u8.MASTER:
		// If we received a master playlist, pick the first (or highest bandwidth) variant.
		master := pl.(*m3u8.MasterPlaylist)
		best := pickBestVariant(master)
		if best == "" {
			return nil, 0, false, fmt.Errorf("hls reader: master playlist has no variants")
		}
		// Update URL and recurse once.
		r.input.URL = resolveURL(base, best)
		return r.fetchPlaylist(ctx)

	default:
		return nil, 0, false, fmt.Errorf("hls reader: unknown playlist type")
	}
}

// fetchSegment downloads a segment and returns its bytes.
func (r *HLSReader) fetchSegment(ctx context.Context, segURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, segURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d for segment %s", resp.StatusCode, segURL)
	}

	return io.ReadAll(resp.Body)
}

// resolveURL resolves ref against base, returning an absolute URL string.
func resolveURL(base *url.URL, ref string) string {
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return ref
	}
	refURL, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return base.ResolveReference(refURL).String()
}

// pickBestVariant returns the URI of the variant with the highest bandwidth.
func pickBestVariant(master *m3u8.MasterPlaylist) string {
	var best string
	var bestBW uint32
	for _, v := range master.Variants {
		if v == nil {
			continue
		}
		if v.Bandwidth >= bestBW {
			bestBW = v.Bandwidth
			best = v.URI
		}
	}
	return best
}

// segmentKey builds a stable dedup key for a segment.
func segmentKey(segURL string, seq uint64) string {
	return fmt.Sprintf("%s#%d", segURL, seq)
}
