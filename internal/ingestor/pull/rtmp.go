package pull

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"sync"

	gocodec "github.com/yapingcat/gomedia/go-codec"
	gompeg2 "github.com/yapingcat/gomedia/go-mpeg2"
	gortmp "github.com/yapingcat/gomedia/go-rtmp"

	"github.com/open-streamer/open-streamer/internal/domain"
)

const rtmpChanSize = 512

// RTMPReader connects to a remote RTMP server in play (pull) mode and outputs
// raw MPEG-TS chunks.  The FLV → MPEG-TS conversion is done natively via
// yapingcat/gomedia without spawning any external process.
type RTMPReader struct {
	input  domain.Input
	conn   net.Conn
	client *gortmp.RtmpClient
	pkts   chan []byte
	once   sync.Once
}

// NewRTMPReader constructs an RTMPReader.
func NewRTMPReader(input domain.Input) *RTMPReader {
	return &RTMPReader{input: input, pkts: make(chan []byte, rtmpChanSize)}
}

func (r *RTMPReader) Open(ctx context.Context) error {
	u, err := url.Parse(r.input.URL)
	if err != nil {
		return fmt.Errorf("rtmp reader: parse url: %w", err)
	}
	host := u.Host
	if u.Port() == "" {
		host += ":1935"
	}

	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp4", host)
	if err != nil {
		return fmt.Errorf("rtmp reader: dial %q: %w", host, err)
	}
	r.conn = conn

	mux := gompeg2.NewTSMuxer()
	var videoPid, audioPid uint16
	var hasVideo, hasAudio bool

	mux.OnPacket = func(pkg []byte) {
		out := make([]byte, len(pkg))
		copy(out, pkg)
		select {
		case r.pkts <- out:
		default:
		}
	}

	client := gortmp.NewRtmpClient(gortmp.WithChunkSize(60000), gortmp.WithComplexHandshake())
	client.OnFrame(func(cid gocodec.CodecID, pts, dts uint32, frame []byte) {
		switch cid {
		case gocodec.CODECID_VIDEO_H264:
			r.once.Do(func() {
				videoPid = mux.AddStream(gompeg2.TS_STREAM_H264)
				hasVideo = true
			})
			if hasVideo {
				_ = mux.Write(videoPid, frame, uint64(pts), uint64(dts))
			}
		case gocodec.CODECID_VIDEO_H265:
			r.once.Do(func() {
				videoPid = mux.AddStream(gompeg2.TS_STREAM_H265)
				hasVideo = true
			})
			if hasVideo {
				_ = mux.Write(videoPid, frame, uint64(pts), uint64(dts))
			}
		case gocodec.CODECID_AUDIO_AAC:
			if !hasAudio {
				audioPid = mux.AddStream(gompeg2.TS_STREAM_AAC)
				hasAudio = true
			}
			if hasAudio {
				_ = mux.Write(audioPid, frame, uint64(pts), uint64(pts))
			}
		}
	})
	client.SetOutput(func(b []byte) error {
		_, err := r.conn.Write(b)
		return err
	})
	r.client = client

	client.Start(r.input.URL)

	go r.readLoop()
	return nil
}

func (r *RTMPReader) readLoop() {
	buf := make([]byte, 65536)
	for {
		n, err := r.conn.Read(buf)
		if err != nil {
			close(r.pkts)
			return
		}
		if err := r.client.Input(buf[:n]); err != nil {
			close(r.pkts)
			return
		}
	}
}

func (r *RTMPReader) Read(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case pkt, ok := <-r.pkts:
		if !ok {
			return nil, fmt.Errorf("rtmp reader: connection closed")
		}
		return pkt, nil
	}
}

func (r *RTMPReader) Close() error {
	if r.conn != nil {
		return r.conn.Close()
	}
	return nil
}
