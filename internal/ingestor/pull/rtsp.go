package pull

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"sync"

	gortsp "github.com/yapingcat/gomedia/go-rtsp"
	"github.com/yapingcat/gomedia/go-rtsp/sdp"
	gompeg2 "github.com/yapingcat/gomedia/go-mpeg2"

	"github.com/open-streamer/open-streamer/internal/domain"
)

const rtspChanSize = 512

// RTSPReader pulls an RTSP stream in play mode and outputs raw MPEG-TS chunks.
// RTP → MPEG-TS conversion is done natively via yapingcat/gomedia.
// Only TCP-interleaved transport is used (no separate UDP RTP sockets).
//
// Supported codecs: H264, H265, AAC.
type RTSPReader struct {
	input domain.Input
	conn  net.Conn
	pkts  chan []byte
}

// NewRTSPReader constructs an RTSPReader.
func NewRTSPReader(input domain.Input) *RTSPReader {
	return &RTSPReader{input: input, pkts: make(chan []byte, rtspChanSize)}
}

func (r *RTSPReader) Open(ctx context.Context) error {
	u, err := url.Parse(r.input.URL)
	if err != nil {
		return fmt.Errorf("rtsp reader: parse url: %w", err)
	}
	host := u.Host
	if u.Port() == "" {
		host += ":554"
	}

	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", host)
	if err != nil {
		return fmt.Errorf("rtsp reader: dial %q: %w", host, err)
	}
	r.conn = conn

	mux := gompeg2.NewTSMuxer()
	mux.OnPacket = func(pkg []byte) {
		out := make([]byte, len(pkg))
		copy(out, pkg)
		select {
		case r.pkts <- out:
		default:
		}
	}

	handler := &rtspClientHandler{mux: mux}
	client, err := gortsp.NewRtspClient(r.input.URL, handler)
	if err != nil {
		conn.Close()
		return fmt.Errorf("rtsp reader: build client: %w", err)
	}
	client.SetOutput(func(b []byte) error {
		_, err := r.conn.Write(b)
		return err
	})

	if err := client.Start(); err != nil {
		conn.Close()
		return fmt.Errorf("rtsp reader: start: %w", err)
	}

	go r.readLoop(client)
	return nil
}

func (r *RTSPReader) readLoop(client *gortsp.RtspClient) {
	buf := make([]byte, 8192)
	for {
		n, err := r.conn.Read(buf)
		if err != nil {
			close(r.pkts)
			return
		}
		if err := client.Input(buf[:n]); err != nil {
			close(r.pkts)
			return
		}
	}
}

func (r *RTSPReader) Read(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case pkt, ok := <-r.pkts:
		if !ok {
			return nil, fmt.Errorf("rtsp reader: connection closed")
		}
		return pkt, nil
	}
}

func (r *RTSPReader) Close() error {
	if r.conn != nil {
		return r.conn.Close()
	}
	return nil
}

// rtspClientHandler implements gortsp.ClientHandle and feeds decoded samples
// into a TSMuxer.
type rtspClientHandler struct {
	mux      *gompeg2.TSMuxer
	mu       sync.Mutex
	videoPid uint16
	audioPid uint16
	hasVideo bool
	hasAudio bool
}

func (h *rtspClientHandler) HandleOption(_ *gortsp.RtspClient, _ gortsp.RtspResponse, _ []string) error {
	return nil
}

func (h *rtspClientHandler) HandleDescribe(_ *gortsp.RtspClient, _ gortsp.RtspResponse, _ *sdp.Sdp, tracks map[string]*gortsp.RtspTrack) error {
	for name, track := range tracks {
		track := track // capture loop variable
		sampleRate := track.Codec.SampleRate
		if sampleRate == 0 {
			sampleRate = 90000
		}

		switch track.Codec.Cid {
		case gortsp.RTSP_CODEC_H264:
			h.mu.Lock()
			h.videoPid = h.mux.AddStream(gompeg2.TS_STREAM_H264)
			h.hasVideo = true
			pid := h.videoPid
			h.mu.Unlock()

			track.OnSample(func(sample gortsp.RtspSample) {
				ms := uint64(sample.Timestamp) * 1000 / uint64(sampleRate)
				h.mux.Write(pid, sample.Sample, ms, ms) //nolint:errcheck
			})

		case gortsp.RTSP_CODEC_H265:
			h.mu.Lock()
			h.videoPid = h.mux.AddStream(gompeg2.TS_STREAM_H265)
			h.hasVideo = true
			pid := h.videoPid
			h.mu.Unlock()

			track.OnSample(func(sample gortsp.RtspSample) {
				ms := uint64(sample.Timestamp) * 1000 / uint64(sampleRate)
				h.mux.Write(pid, sample.Sample, ms, ms) //nolint:errcheck
			})

		case gortsp.RTSP_CODEC_AAC:
			h.mu.Lock()
			h.audioPid = h.mux.AddStream(gompeg2.TS_STREAM_AAC)
			h.hasAudio = true
			pid := h.audioPid
			h.mu.Unlock()

			track.OnSample(func(sample gortsp.RtspSample) {
				ms := uint64(sample.Timestamp) * 1000 / uint64(sampleRate)
				h.mux.Write(pid, sample.Sample, ms, ms) //nolint:errcheck
			})

		default:
			_ = name
		}
	}
	return nil
}

func (h *rtspClientHandler) HandleSetup(_ *gortsp.RtspClient, _ gortsp.RtspResponse, _ *gortsp.RtspTrack, _ map[string]*gortsp.RtspTrack, _ string, _ int) error {
	return nil
}
func (h *rtspClientHandler) HandleAnnounce(_ *gortsp.RtspClient, _ gortsp.RtspResponse) error {
	return nil
}
func (h *rtspClientHandler) HandlePlay(_ *gortsp.RtspClient, _ gortsp.RtspResponse, _ *gortsp.RangeTime, _ *gortsp.RtpInfo) error {
	return nil
}
func (h *rtspClientHandler) HandlePause(_ *gortsp.RtspClient, _ gortsp.RtspResponse) error {
	return nil
}
func (h *rtspClientHandler) HandleTeardown(_ *gortsp.RtspClient, _ gortsp.RtspResponse) error {
	return nil
}
func (h *rtspClientHandler) HandleGetParameter(_ *gortsp.RtspClient, _ gortsp.RtspResponse) error {
	return nil
}
func (h *rtspClientHandler) HandleSetParameter(_ *gortsp.RtspClient, _ gortsp.RtspResponse) error {
	return nil
}
func (h *rtspClientHandler) HandleRedirect(_ *gortsp.RtspClient, _ gortsp.RtspRequest, _ string, _ *gortsp.RangeTime) error {
	return nil
}
func (h *rtspClientHandler) HandleRecord(_ *gortsp.RtspClient, _ gortsp.RtspResponse, _ *gortsp.RangeTime, _ *gortsp.RtpInfo) error {
	return nil
}
func (h *rtspClientHandler) HandleRequest(_ *gortsp.RtspClient, _ gortsp.RtspRequest) error {
	return nil
}
