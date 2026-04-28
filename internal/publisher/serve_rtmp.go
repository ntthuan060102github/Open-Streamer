package publisher

// serve_rtmp.go — RTMP play handler (publisher-side).
//
// The RTMP listener is owned by the ingestor and bound on the shared port
// (listeners.rtmp). When an external client issues a "play" command, the
// ingestor's RTMP server delegates to HandleRTMPPlay below. Wired in
// runtime.Manager via ing.SetRTMPPlayHandler(pub.HandleRTMPPlay).
//
// Client URL: rtmp://host:port/live/<stream_code>
//
// Data flow per session:
//
//	Buffer Hub → subscriber → tsmux.FeedWirePacket → tsBuffer → mpeg2.TSDemuxer
//	                                                                   │
//	                                                    onTSFrame (H.264 Annex-B, AAC ADTS)
//	                                                                   │
//	                                                  writeFrame callback → RTMP client

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"

	gocodec "github.com/yapingcat/gomedia/go-codec"
	mpeg2 "github.com/yapingcat/gomedia/go-mpeg2"

	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/ingestor/push"
	"github.com/ntt0601zcoder/open-streamer/internal/tsmux"
)

// HandleRTMPPlay is the play handler registered with the ingestor's RTMP server.
// It subscribes to the Buffer Hub for the given stream key, demuxes the TS stream,
// and calls writeFrame for each H.264/AAC frame until ctx is cancelled.
// Returns an error when the stream is not active (caller closes the connection).
//
// Signature matches push.PlayFunc — the info argument carries remote-peer
// metadata captured by the RTMP server at OnPlay handshake time and is
// forwarded into the play-sessions tracker for the API.
func (s *Service) HandleRTMPPlay(
	ctx context.Context,
	key string,
	info push.PlayInfo,
	writeFrame func(cid gocodec.CodecID, data []byte, pts, dts uint32) error,
) error {
	code := domain.StreamCode(key)

	bufID, ok := s.mediaBufferFor(code)
	if !ok {
		return fmt.Errorf("stream %q not active", key)
	}

	sub, err := s.buf.Subscribe(bufID)
	if err != nil {
		return fmt.Errorf("subscribe %q: %w", key, err)
	}
	defer s.buf.Unsubscribe(bufID, sub)

	slog.Info("publisher: RTMP play session started", "stream_code", code, "remote", info.RemoteAddr)
	defer slog.Info("publisher: RTMP play session ended", "stream_code", code)

	rtmpSess := openRTMPSession(ctx, s.tracker, code, info.RemoteAddr, info.FlashVer)
	defer rtmpSess.close()

	runRTMPPlayPipeline(ctx, code, sub, rtmpSess, writeFrame)
	return nil
}

// ─── pipeline ────────────────────────────────────────────────────────────────

// runRTMPPlayPipeline feeds TS from sub into a TSDemuxer and writes H.264/AAC
// frames via writeFrame until ctx is cancelled or sub closes. The sess argument
// is credited with each frame's payload bytes for the play-sessions API.
func runRTMPPlayPipeline(
	ctx context.Context,
	streamCode domain.StreamCode,
	sub *buffer.Subscriber,
	sess *playSession,
	writeFrame func(cid gocodec.CodecID, data []byte, pts, dts uint32) error,
) {
	tb := newTSBuffer()

	go func() {
		defer tb.Close()
		var tsCarry []byte
		var avMux *tsmux.FromAV

		for {
			select {
			case <-ctx.Done():
				return
			case pkt, ok := <-sub.Recv():
				if !ok {
					return
				}
				if pkt.AV != nil && pkt.AV.Discontinuity {
					avMux = nil
					tsCarry = nil
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

	// Wrap writeFrame so each successful payload increments the tracker's
	// per-session byte counter. Approximation: we count the H.264 / AAC
	// payload size, ignoring the few-byte RTMP chunk header — bytes total
	// is for analytics, not billing.
	trackedWrite := func(cid gocodec.CodecID, data []byte, pts, dts uint32) error {
		if err := writeFrame(cid, data, pts, dts); err != nil {
			return err
		}
		sess.add(int64(len(data)))
		return nil
	}

	ps := &rtmpPlaySession{
		streamCode: streamCode,
		writeFrame: trackedWrite,
		firstVideo: true,
	}

	demux := mpeg2.NewTSDemuxer()
	demux.OnFrame = ps.onTSFrame

	demuxDone := make(chan error, 1)
	go func() { demuxDone <- demux.Input(tb) }()

	select {
	case <-ctx.Done():
		tb.Close()
		<-demuxDone
	case err := <-demuxDone:
		if err != nil {
			slog.Debug("publisher: RTMP play demux ended",
				"stream_code", streamCode, "err", err)
		}
	}
}

// ─── rtmpPlaySession ─────────────────────────────────────────────────────────

type rtmpPlaySession struct {
	streamCode  domain.StreamCode
	writeFrame  func(cid gocodec.CodecID, data []byte, pts, dts uint32) error
	baseDTS     uint64
	baseDTSSet  bool
	baseADTS    uint64
	baseADTSSet bool
	firstVideo  bool
}

func (ps *rtmpPlaySession) onTSFrame(cid mpeg2.TS_STREAM_TYPE, frame []byte, pts, dts uint64) {
	if len(frame) == 0 {
		return
	}
	switch cid {
	case mpeg2.TS_STREAM_H264:
		ps.writeVideo(frame, pts, dts)
	case mpeg2.TS_STREAM_AAC:
		ps.writeAudio(frame, dts)
	case mpeg2.TS_STREAM_H265,
		mpeg2.TS_STREAM_AUDIO_MPEG1,
		mpeg2.TS_STREAM_AUDIO_MPEG2:
		return // not supported in standard RTMP
	default:
		return
	}
}

func (ps *rtmpPlaySession) writeVideo(frame []byte, pts, dts uint64) {
	if ps.firstVideo {
		if !tsmux.KeyFrameH264(frame) {
			return
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
	if err := ps.writeFrame(gocodec.CODECID_VIDEO_H264, frame, relPTS, relDTS); err != nil {
		slog.Debug("publisher: RTMP play write video error",
			"stream_code", ps.streamCode, "err", err)
	}
}

func (ps *rtmpPlaySession) writeAudio(frame []byte, dts uint64) {
	if ps.firstVideo {
		return // hold audio until first video keyframe
	}
	if !ps.baseADTSSet {
		ps.baseADTS = dts
		ps.baseADTSSet = true
	}
	relDTS := uint32(dts - ps.baseADTS)
	if err := ps.writeFrame(gocodec.CODECID_AUDIO_AAC, frame, relDTS, relDTS); err != nil {
		slog.Debug("publisher: RTMP play write audio error",
			"stream_code", ps.streamCode, "err", err)
	}
}
