package publisher

// serve_rtmp.go — RTMP play server (publisher-side).
//
// Allows external clients (VLC, OBS, ffplay, …) to pull any active stream
// via the RTMP protocol.
//
// Config: publisher.rtmp.listen_host + publisher.rtmp.port (default 0 = disabled).
// Client URL: rtmp://host:port/live/<stream_code>
//
// Data flow per client connection:
//
//	Buffer Hub → subscriber → tsmux.FeedWirePacket → tsBuffer → mpeg2.TSDemuxer
//	                                                                    │
//	                                                       onTSFrame (H.264 Annex-B, AAC ADTS)
//	                                                                    │
//	                                   gortmp.RtmpServerHandle.WriteFrame → RTMP client
//
// gomedia's WriteFrame accepts Annex-B for H.264 and ADTS for AAC — both are the
// native output of mpeg2.TSDemuxer, so no conversion is needed.
//
// The server blocks in RunRTMPPlayServer; call it as a goroutine from main.go.

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"

	gocodec "github.com/yapingcat/gomedia/go-codec"
	mpeg2 "github.com/yapingcat/gomedia/go-mpeg2"
	gortmp "github.com/yapingcat/gomedia/go-rtmp"

	"github.com/ntthuan060102github/open-streamer/internal/buffer"
	"github.com/ntthuan060102github/open-streamer/internal/domain"
	"github.com/ntthuan060102github/open-streamer/internal/tsmux"
)

// RunRTMPPlayServer starts the RTMP play listener and blocks until ctx is cancelled.
// Returns nil if Port is not configured (server is disabled).
func (s *Service) RunRTMPPlayServer(ctx context.Context) error {
	port := s.cfg.RTMP.Port
	if port == 0 {
		return nil // disabled
	}
	host := s.cfg.RTMP.ListenHost
	if host == "" {
		host = "0.0.0.0"
	}
	addr := fmt.Sprintf("%s:%d", host, port)

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("publisher rtmp: listen %q: %w", addr, err)
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	slog.Info("publisher: RTMP play server listening", "addr", addr)

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return fmt.Errorf("publisher rtmp: accept: %w", err)
			}
		}
		go s.handleRTMPPlayConn(ctx, conn)
	}
}

// handleRTMPPlayConn drives one RTMP client connection.
func (s *Service) handleRTMPPlayConn(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()

	handle := gortmp.NewRtmpServerHandle()
	handle.SetOutput(func(b []byte) error {
		_, err := conn.Write(b)
		return err
	})

	var (
		sessMu   sync.Mutex
		sess     *rtmpPlaySession
		sessDone chan struct{}
	)

	handle.OnPlay(func(app, key string, start, duration float64, reset bool) gortmp.StatusCode {
		bufID := s.mediaBufferFor(domain.StreamCode(key))
		sub, err := s.buf.Subscribe(bufID)
		if err != nil {
			slog.Warn("publisher: RTMP play stream not found", "key", key)
			return gortmp.NETSTREAM_PLAY_NOTFOUND
		}

		done := make(chan struct{})
		ps := &rtmpPlaySession{
			streamCode: domain.StreamCode(key),
			handle:     handle,
			sub:        sub,
			done:       done,
		}
		sessMu.Lock()
		sess = ps
		sessDone = done
		sessMu.Unlock()

		go s.runRTMPPlaySession(ctx, ps)

		slog.Info("publisher: RTMP play client connected", "key", key)
		return gortmp.NETSTREAM_PLAY_START
	})

	buf := make([]byte, 65536)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			break
		}
		if err := handle.Input(buf[:n]); err != nil {
			slog.Debug("publisher: RTMP play handle input error", "err", err)
			break
		}
	}

	// Stop the play session when the client disconnects.
	sessMu.Lock()
	if sess != nil {
		s.buf.Unsubscribe(s.mediaBufferFor(sess.streamCode), sess.sub)
		close(sess.done)
	}
	d := sessDone
	sessMu.Unlock()
	if d != nil {
		<-d // wait for runRTMPPlaySession to exit
	}
}

// rtmpPlaySession holds per-client state for one RTMP play connection.
type rtmpPlaySession struct {
	streamCode domain.StreamCode
	handle     *gortmp.RtmpServerHandle
	sub        *buffer.Subscriber
	done       chan struct{}

	// Timestamp anchoring — makes timestamps start near 0 for the client.
	baseDTS     uint64
	baseDTSSet  bool
	baseADTS    uint64
	baseADTSSet bool

	// firstVideo: skip frames until the first H.264 IDR so the decoder can init.
	firstVideo bool
}

// runRTMPPlaySession reads from the buffer subscriber, demuxes TS, and writes
// frames to the RTMP client via gomedia WriteFrame.
func (s *Service) runRTMPPlaySession(ctx context.Context, ps *rtmpPlaySession) {
	ps.firstVideo = true

	tb := newTSBuffer()

	// Producer: buffer hub → TS bytes.
	go func() {
		defer tb.Close()
		var tsCarry []byte
		var avMux *tsmux.FromAV

		for {
			select {
			case <-ctx.Done():
				return
			case <-ps.done:
				return
			case pkt, ok := <-ps.sub.Recv():
				if !ok {
					return
				}
				if pkt.AV != nil && pkt.AV.Discontinuity {
					tsCarry = tsCarry[:0]
					ps.firstVideo = true // re-wait for keyframe after source switch
				}
				tsmux.FeedWirePacket(pkt.TS, pkt.AV, &avMux, func(b []byte) {
					tsCarry = append(tsCarry, b...)
					for len(tsCarry) >= 188 {
						if tsCarry[0] != 0x47 {
							idx := bytes.IndexByte(tsCarry, 0x47)
							if idx < 0 {
								if len(tsCarry) > 187 {
									tsCarry = tsCarry[len(tsCarry)-187:]
								}
								return
							}
							tsCarry = tsCarry[idx:]
							if len(tsCarry) < 188 {
								return
							}
						}
						if len(tsCarry) >= 376 && tsCarry[188] != 0x47 {
							tsCarry = tsCarry[1:]
							continue
						}
						if _, err := tb.Write(tsCarry[:188]); err != nil {
							return
						}
						tsCarry = tsCarry[188:]
					}
				})
			}
		}
	}()

	// Demuxer: TS bytes → frames → WriteFrame.
	demux := mpeg2.NewTSDemuxer()
	demux.OnFrame = func(cid mpeg2.TS_STREAM_TYPE, frame []byte, pts, dts uint64) {
		ps.onTSFrame(cid, frame, pts, dts)
	}

	demuxDone := make(chan error, 1)
	go func() { demuxDone <- demux.Input(tb) }()

	select {
	case <-ctx.Done():
		tb.Close()
		<-demuxDone
	case <-ps.done:
		tb.Close()
		<-demuxDone
	case err := <-demuxDone:
		if err != nil {
			slog.Debug("publisher: RTMP play demux ended",
				"stream_code", ps.streamCode, "err", err)
		}
	}
}

// onTSFrame is the TSDemuxer callback for one RTMP play session.
func (ps *rtmpPlaySession) onTSFrame(cid mpeg2.TS_STREAM_TYPE, frame []byte, pts, dts uint64) {
	if len(frame) == 0 {
		return
	}

	switch cid {
	case mpeg2.TS_STREAM_H264:
		if ps.firstVideo {
			if !tsmux.KeyFrameH264(frame) {
				return // skip until IDR
			}
			ps.firstVideo = false
			ps.baseDTS = dts
			ps.baseDTSSet = true
		}
		if !ps.baseDTSSet {
			ps.baseDTS = dts
			ps.baseDTSSet = true
		}
		relDTS := uint32(dts - ps.baseDTS)
		relPTS := uint32(pts - ps.baseDTS)
		if err := ps.handle.WriteFrame(gocodec.CODECID_VIDEO_H264, frame, relPTS, relDTS); err != nil {
			slog.Debug("publisher: RTMP play write video error",
				"stream_code", ps.streamCode, "err", err)
		}

	case mpeg2.TS_STREAM_AAC:
		if ps.firstVideo {
			return // don't send audio before first video keyframe
		}
		if !ps.baseADTSSet {
			ps.baseADTS = dts
			ps.baseADTSSet = true
		}
		relDTS := uint32(dts - ps.baseADTS)
		if err := ps.handle.WriteFrame(gocodec.CODECID_AUDIO_AAC, frame, relDTS, relDTS); err != nil {
			slog.Debug("publisher: RTMP play write audio error",
				"stream_code", ps.streamCode, "err", err)
		}

	case mpeg2.TS_STREAM_H265,
		mpeg2.TS_STREAM_AUDIO_MPEG1,
		mpeg2.TS_STREAM_AUDIO_MPEG2:
		// Not supported in standard RTMP; skip silently.
		return

	default:
		return
	}
}
