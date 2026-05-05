package publisher

// serve_rtsp.go — RTSP play server (publisher-side).
//
// Architecture:
//
//	Buffer Hub → subscriber → tsmux.FeedWirePacket → tsBuffer → mpeg2.TSDemuxer
//	                                                                   │
//	                                                   onTSFrame (H.264 Annex-B, AAC ADTS)
//	                                                                   │
//	                                                  rtspSession (codec detection + RTP encoding)
//	                                                                   │
//	                                              gortsplib.ServerStream.WritePacketRTP
//	                                                                   │
//	                                                             RTSP clients
//
// Stream registration lifecycle:
//  1. serveRTSP waits for the gortsplib server to start, then subscribes to the Buffer Hub.
//  2. Frames are collected until SPS/PPS (H.264) and AAC AudioSpecificConfig are known
//     (or up to maxRTSPPendingFrames video frames if audio never arrives).
//  3. A ServerStream is created and mounted at /live/<stream_code>; the handler starts
//     returning it to DESCRIBE/SETUP requests.
//  4. On stream stop, the mount is removed and the ServerStream is closed.
//
// Client URL: rtsp://host:port/live/<stream_code> (port from listeners.rtsp.port)

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpmpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	mpeg2 "github.com/yapingcat/gomedia/go-mpeg2"

	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/tsmux"
)

// maxRTSPPendingFrames is the maximum number of video frames buffered while
// waiting for AAC codec info. After this limit, the stream is initialised with
// video-only (no audio track in SDP).
const maxRTSPPendingFrames = 90

func rtspLiveMountPath(code domain.StreamCode) string {
	return "live/" + string(code)
}

// RunRTSPPlayServer starts the RTSP play listener.
// Returns nil immediately when listeners.rtsp is disabled.
func (s *Service) RunRTSPPlayServer(ctx context.Context) error {
	rtsp := s.currentListeners().RTSP
	if !rtsp.Enabled || rtsp.Port == 0 {
		close(s.rtspSrvReady)
		return nil
	}
	host := rtsp.ListenHost
	if host == "" {
		host = domain.DefaultListenHost
	}
	addr := fmt.Sprintf("%s:%d", host, rtsp.Port)

	srv := &gortsplib.Server{
		RTSPAddress: addr,
		Handler:     &rtspHandler{svc: s},
		// gortsplib's default WriteQueueSize is 256 packets — too tight
		// for HD bitrates. At 1080p ~70 RTP packets/frame × 25fps ≈
		// 1750 packets/s, 256 ≈ 146 ms of slack; any client jitter >
		// 146 ms triggers "write queue is full" + "RTP packets lost".
		// 1024 (~600 ms) absorbs realistic jitter on home/office WAN
		// without ballooning memory (≈1.5 MB per session). Must be a
		// power of two — gortsplib rejects other values at Start().
		WriteQueueSize: 1024,
	}
	if err := srv.Start(); err != nil {
		return fmt.Errorf("publisher rtsp: listen %q: %w", addr, err)
	}

	s.mu.Lock()
	s.rtspSrv = srv
	s.mu.Unlock()
	close(s.rtspSrvReady)

	slog.Info("publisher: RTSP play server listening", "addr", addr)

	<-ctx.Done()
	srv.Close()
	_ = srv.Wait()
	return nil
}

// ─── handler ─────────────────────────────────────────────────────────────────

type rtspHandler struct{ svc *Service }

// rtspPathStreamCode extracts the StreamCode from an RTSP request path.
// Accepts "live/<code>", "/live/<code>", or bare "<code>". Path may also
// contain a track suffix (e.g. /live/foo/trackID=0) — we trim that.
func rtspPathStreamCode(path string) domain.StreamCode {
	p := strings.TrimPrefix(path, "/")
	p = strings.TrimPrefix(p, "live/")
	if i := strings.IndexByte(p, '/'); i > 0 {
		p = p[:i]
	}
	return domain.StreamCode(strings.TrimSpace(p))
}

// OnDescribe handles DESCRIBE — returns the ServerStream for the requested path.
func (h *rtspHandler) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	stream := h.lookupStream(ctx.Path)
	if stream == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, stream, nil
}

// OnSetup handles SETUP — associates the client session with the ServerStream.
func (h *rtspHandler) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	stream := h.lookupStream(ctx.Path)
	if stream == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, stream, nil
}

// OnPlay opens a tracker session for one RTSP client. Bytes are not credited
// (gortsplib writes the wire format internally and we have no per-subscriber
// hook); the open / close events still record the viewer's IP, UA, duration.
func (h *rtspHandler) OnPlay(ctx *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	code := rtspPathStreamCode(ctx.Path)
	if code == "" {
		return &base.Response{StatusCode: base.StatusBadRequest}, nil
	}
	ua := ""
	if ctx.Request != nil {
		if v, ok := ctx.Request.Header["User-Agent"]; ok && len(v) > 0 {
			ua = v[0]
		}
	}
	sess := openRTSPSession(context.Background(), h.svc.tracker, code, ctx.Conn.NetConn().RemoteAddr(), ua)
	if sess != nil {
		h.svc.rtspSessionsMu.Lock()
		h.svc.rtspSessions[ctx.Session] = sess
		h.svc.rtspSessionsMu.Unlock()
	}
	return &base.Response{StatusCode: base.StatusOK}, nil
}

// OnSessionClose closes the tracker session that mirrored this RTSP one.
// Idempotent on missing entries (a session that never reached PLAY).
func (h *rtspHandler) OnSessionClose(ctx *gortsplib.ServerHandlerOnSessionCloseCtx) {
	h.svc.rtspSessionsMu.Lock()
	sess, ok := h.svc.rtspSessions[ctx.Session]
	if ok {
		delete(h.svc.rtspSessions, ctx.Session)
	}
	h.svc.rtspSessionsMu.Unlock()
	if ok {
		sess.close()
	}
}

// lookupStream matches a request path to a registered ServerStream.
// The SETUP path may have a track suffix (e.g. /live/foo/trackID=0), so we
// match by exact path or by prefix followed by "/".
func (h *rtspHandler) lookupStream(path string) *gortsplib.ServerStream {
	path = strings.TrimPrefix(path, "/")
	h.svc.mu.Lock()
	defer h.svc.mu.Unlock()
	for mountPath, stream := range h.svc.rtspMounts {
		if path == mountPath || strings.HasPrefix(path, mountPath+"/") {
			return stream
		}
	}
	return nil
}

// ─── per-stream pipeline ─────────────────────────────────────────────────────

// serveRTSP is spawned by spawnOutputs and runs for the lifetime of the stream.
func (s *Service) serveRTSP(ctx context.Context, streamCode, bufID domain.StreamCode) {
	// Wait for RTSP server to start (or be disabled).
	select {
	case <-ctx.Done():
		return
	case <-s.rtspSrvReady:
	}

	s.mu.Lock()
	srv := s.rtspSrv
	s.mu.Unlock()
	if srv == nil {
		return // server disabled (port = 0)
	}

	sub, err := s.buf.Subscribe(bufID)
	if err != nil {
		slog.Warn("publisher: RTSP subscribe failed", "stream_code", streamCode, "err", err)
		return
	}
	defer s.buf.Unsubscribe(bufID, sub)

	slog.Info("publisher: RTSP session started", "stream_code", streamCode)
	defer slog.Info("publisher: RTSP session ended", "stream_code", streamCode)

	runRTSPPipeline(ctx, streamCode, srv, sub, s)
}

// runRTSPPipeline feeds TS from sub through a TSDemuxer and writes H.264/AAC RTP.
func runRTSPPipeline(
	ctx context.Context,
	streamCode domain.StreamCode,
	srv *gortsplib.Server,
	sub *buffer.Subscriber,
	svc *Service,
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
					tsmux.DrainTS188Aligned(&tsCarry, b, func(aligned []byte) {
						_, _ = tb.Write(aligned)
					})
				})
			}
		}
	}()

	sess := &rtspSession{
		streamCode: streamCode,
		srv:        srv,
		svc:        svc,
		mountPath:  rtspLiveMountPath(streamCode),
		firstVideo: true,
	}

	demux := mpeg2.NewTSDemuxer()
	demux.OnFrame = sess.onTSFrame

	demuxDone := make(chan error, 1)
	go func() { demuxDone <- demux.Input(tb) }()

	select {
	case <-ctx.Done():
		tb.Close()
		<-demuxDone
	case err := <-demuxDone:
		if err != nil {
			slog.Debug("publisher: RTSP demux ended",
				"stream_code", streamCode, "err", err)
		}
	}
	sess.closeStream()
}

// ─── rtspSession ─────────────────────────────────────────────────────────────

type rtspSession struct {
	streamCode domain.StreamCode
	srv        *gortsplib.Server
	svc        *Service
	mountPath  string

	// codec detection
	sps    []byte
	pps    []byte
	aacCfg *mpeg4audio.AudioSpecificConfig

	// pending frames buffered before the stream is initialised
	pendingVideo []rtspPendingVideo
	pendingAudio []rtspPendingAudio

	// serving state (nil until initialised)
	stream     *gortsplib.ServerStream
	videoMedia *description.Media
	audioMedia *description.Media
	videoEnc   *rtph264.Encoder
	audioEnc   *rtpmpeg4audio.Encoder

	// timing (MPEG-TS 90 kHz)
	baseDTS      uint64
	baseDTSSet   bool
	baseAudio    uint64
	baseAudioSet bool
	firstVideo   bool
}

type rtspPendingVideo struct {
	frame    []byte
	pts, dts uint64
}

type rtspPendingAudio struct {
	frame []byte
	dts   uint64
}

func (sess *rtspSession) onTSFrame(cid mpeg2.TS_STREAM_TYPE, frame []byte, pts, dts uint64) {
	if len(frame) == 0 {
		return
	}
	switch cid {
	case mpeg2.TS_STREAM_H264:
		sess.handleVideo(frame, pts, dts)
	case mpeg2.TS_STREAM_AAC:
		sess.handleAudio(frame, dts)
	case mpeg2.TS_STREAM_H265,
		mpeg2.TS_STREAM_AUDIO_MPEG1,
		mpeg2.TS_STREAM_AUDIO_MPEG2:
	default:
	}
}

func (sess *rtspSession) handleVideo(frame []byte, pts, dts uint64) {
	// Drop until the first IDR so decoders can initialise.
	if sess.firstVideo {
		if !tsmux.KeyFrameH264(frame) {
			return
		}
		sess.firstVideo = false
	}

	// Extract SPS/PPS on keyframes.
	if tsmux.KeyFrameH264(frame) {
		if sps, pps := extractSPSPPS(frame); len(sps) > 0 && len(pps) > 0 {
			sess.sps = sps
			sess.pps = pps
		}
	}

	if sess.stream == nil {
		// Queue frame while waiting for codec info.
		if len(sess.sps) > 0 && len(sess.pps) > 0 {
			cp := make([]byte, len(frame))
			copy(cp, frame)
			sess.pendingVideo = append(sess.pendingVideo, rtspPendingVideo{frame: cp, pts: pts, dts: dts})

			// Init if we also have audio config, or if we've waited long enough.
			if sess.aacCfg != nil || len(sess.pendingVideo) >= maxRTSPPendingFrames {
				if err := sess.initStream(); err != nil {
					slog.Warn("publisher: RTSP init stream failed",
						"stream_code", sess.streamCode, "err", err)
					sess.pendingVideo = sess.pendingVideo[:0]
					sess.pendingAudio = sess.pendingAudio[:0]
				}
			}
		}
		return
	}

	sess.writeVideoRTP(frame, pts, dts)
}

func (sess *rtspSession) handleAudio(frame []byte, dts uint64) {
	if sess.firstVideo {
		return // hold audio until first video keyframe
	}

	// Extract AAC config from ADTS on first audio frame.
	if sess.aacCfg == nil {
		var pkts mpeg4audio.ADTSPackets
		if err := pkts.Unmarshal(frame); err == nil && len(pkts) > 0 {
			p := pkts[0]
			sess.aacCfg = &mpeg4audio.AudioSpecificConfig{
				Type:          p.Type,
				SampleRate:    p.SampleRate,
				ChannelConfig: p.ChannelConfig,
			}
		}
	}

	if sess.stream == nil {
		// Queue frame while waiting for stream init.
		cp := make([]byte, len(frame))
		copy(cp, frame)
		sess.pendingAudio = append(sess.pendingAudio, rtspPendingAudio{frame: cp, dts: dts})

		// If video codec is ready, trigger init now that we have audio config.
		if sess.aacCfg != nil && len(sess.sps) > 0 && len(sess.pps) > 0 {
			if err := sess.initStream(); err != nil {
				slog.Warn("publisher: RTSP init stream failed",
					"stream_code", sess.streamCode, "err", err)
				sess.pendingVideo = sess.pendingVideo[:0]
				sess.pendingAudio = sess.pendingAudio[:0]
			}
		}
		return
	}

	sess.writeAudioRTP(frame, dts)
}

func (sess *rtspSession) initStream() error {
	h264Format := &format.H264{
		PayloadTyp:        96,
		SPS:               append([]byte(nil), sess.sps...),
		PPS:               append([]byte(nil), sess.pps...),
		PacketizationMode: 1,
	}
	videoMedia := &description.Media{
		Type:    description.MediaTypeVideo,
		Formats: []format.Format{h264Format},
	}

	medias := []*description.Media{videoMedia}

	var aacFormat *format.MPEG4Audio
	var audioMedia *description.Media
	if sess.aacCfg != nil {
		// SizeLength / IndexLength / IndexDeltaLength / ProfileLevelID are
		// part of the MPEG4-GENERIC RTP packetisation (RFC 3640) and
		// surface in SDP as `a=fmtp:... sizelength=13;...`. gortsplib only
		// emits each field when its value is non-zero, so leaving them at
		// the zero default produces an SDP without `sizelength`, which
		// strict clients (gortsplib v4 / v5 pull, ffmpeg) reject with
		// `invalid SDP: media N is invalid: sizelength is missing`.
		// 13 / 3 / 3 are the universal AAC-hbr Mode 1 values (matches
		// FFmpeg's RTP muxer, Wowza, Flussonic).
		aacFormat = &format.MPEG4Audio{
			PayloadTyp:       97,
			Config:           sess.aacCfg,
			SizeLength:       13,
			IndexLength:      3,
			IndexDeltaLength: 3,
			ProfileLevelID:   1,
		}
		audioMedia = &description.Media{
			Type:    description.MediaTypeAudio,
			Formats: []format.Format{aacFormat},
		}
		medias = append(medias, audioMedia)
	}

	stream := &gortsplib.ServerStream{
		Server: sess.srv,
		Desc:   &description.Session{Medias: medias},
	}
	if err := stream.Initialize(); err != nil {
		return fmt.Errorf("initialize server stream: %w", err)
	}

	videoEnc, err := h264Format.CreateEncoder()
	if err != nil {
		stream.Close()
		return fmt.Errorf("create H264 encoder: %w", err)
	}
	if err := videoEnc.Init(); err != nil {
		stream.Close()
		return fmt.Errorf("init H264 encoder: %w", err)
	}

	var audioEnc *rtpmpeg4audio.Encoder
	if aacFormat != nil {
		audioEnc, err = aacFormat.CreateEncoder()
		if err != nil {
			stream.Close()
			return fmt.Errorf("create AAC encoder: %w", err)
		}
		if err := audioEnc.Init(); err != nil {
			stream.Close()
			return fmt.Errorf("init AAC encoder: %w", err)
		}
	}

	sess.stream = stream
	sess.videoMedia = videoMedia
	sess.audioMedia = audioMedia
	sess.videoEnc = videoEnc
	sess.audioEnc = audioEnc

	// Register mount so the handler can serve DESCRIBE requests.
	sess.svc.mu.Lock()
	sess.svc.rtspMounts[sess.mountPath] = stream
	sess.svc.mu.Unlock()

	slog.Info("publisher: RTSP stream mounted",
		"stream_code", sess.streamCode,
		"path", sess.mountPath,
		"audio", audioMedia != nil,
	)

	// Replay buffered frames.
	for _, v := range sess.pendingVideo {
		sess.writeVideoRTP(v.frame, v.pts, v.dts)
	}
	for _, a := range sess.pendingAudio {
		sess.writeAudioRTP(a.frame, a.dts)
	}
	sess.pendingVideo = nil
	sess.pendingAudio = nil

	return nil
}

func (sess *rtspSession) writeVideoRTP(frame []byte, pts, dts uint64) {
	if !sess.baseDTSSet {
		sess.baseDTS = dts
		sess.baseDTSSet = true
	}
	// gomedia's mpeg2 TSDemuxer divides the PES PTS/DTS by 90 before
	// invoking OnFrame (ts-demuxer.go line 211/213), so the values that
	// reach this callback are MILLISECONDS — not 90 kHz ticks. H.264
	// RTP runs at 90 kHz; we have to multiply ms by 90 to land in the
	// right clock domain.
	//
	// Earlier code used `pts - baseDTS` directly, treating ms as 90 kHz.
	// The result was an RTP timeline 90× slower than reality: the
	// receiver decoded the first IDR (timestamps still small) and then
	// hung once subsequent frames arrived with timestamps barely
	// advancing — Flussonic's player froze after ~1 s, OpenStreamer's
	// HLS pull saw the audio stream emit non-monotonic DTS in ffprobe
	// (because the same scale bug afflicted the audio path).
	//
	// Per RFC 6184 §7.1 the RTP timestamp is the presentation time and
	// must be IDENTICAL for every RTP packet that fragments one access
	// unit (e.g. all FU-A pieces). We use PTS in 90 kHz; the receiver
	// derives presentation order from PTS alone and handles reordering.
	rtpTS := uint32((pts - sess.baseDTS) * 90)

	nalus := annexBToNALUs(frame)
	if len(nalus) == 0 {
		return
	}

	pkts, err := sess.videoEnc.Encode(nalus)
	if err != nil {
		slog.Debug("publisher: RTSP H264 encode error", "stream_code", sess.streamCode, "err", err)
		return
	}
	for _, pkt := range pkts {
		pkt.Timestamp = rtpTS
		if err := sess.stream.WritePacketRTP(sess.videoMedia, pkt); err != nil {
			slog.Debug("publisher: RTSP write video RTP", "stream_code", sess.streamCode, "err", err)
			return
		}
	}
}

func (sess *rtspSession) writeAudioRTP(frame []byte, dts uint64) {
	if sess.audioMedia == nil || sess.audioEnc == nil || sess.aacCfg == nil {
		return
	}

	if !sess.baseAudioSet {
		sess.baseAudio = dts
		sess.baseAudioSet = true
	}
	// gomedia's TSDemuxer hands us DTS in milliseconds (see comment in
	// writeVideoRTP). AAC RTP runs at the audio sample rate (48 kHz
	// here), so the conversion is `ms × sampleRate / 1000`.
	//
	// The previous formula divided by 90000 instead of 1000, treating
	// the input as 90 kHz ticks. That produced packet timestamps 90×
	// smaller than expected; consecutive RTP packets ended up only
	// ~91 ticks apart (vs. the correct 1024 per AAC AU), so within a
	// single ADTS frame containing N AUs the encoder's internal AU
	// spacing (1024) overshot the next packet's base, and ffmpeg saw
	// "non-monotonic DTS" — which froze playback after a couple of
	// frames worth of audio.
	relMs := dts - sess.baseAudio
	rtpTS := uint32(relMs * uint64(sess.aacCfg.SampleRate) / 1000)

	var adtsPkts mpeg4audio.ADTSPackets
	if err := adtsPkts.Unmarshal(frame); err != nil || len(adtsPkts) == 0 {
		return
	}
	aus := make([][]byte, 0, len(adtsPkts))
	for _, p := range adtsPkts {
		if len(p.AU) > 0 {
			aus = append(aus, p.AU)
		}
	}
	if len(aus) == 0 {
		return
	}

	pkts, err := sess.audioEnc.Encode(aus)
	if err != nil {
		slog.Debug("publisher: RTSP AAC encode error", "stream_code", sess.streamCode, "err", err)
		return
	}
	// Encoder.Encode resets its internal timestamp to 0 on every call and
	// increments by 1024 (samples per AAC AU) for each subsequent AU in
	// the batch. So pkts come back with relative timestamps 0, 1024,
	// 2048, ... — we ADD our base (rtpTS) instead of overwriting, so
	// the second / third / Nth AU in a multi-AU ADTS frame keeps the
	// correct +1024 spacing relative to the first.
	//
	// The previous behaviour (`pkt.Timestamp = rtpTS`) collapsed every
	// AU in a batch to the same timestamp; ffmpeg / Flussonic rebuilt
	// the implicit AU timeline (TS, TS+1024, TS+2048…) and saw
	// "non-monotonically increasing dts" once a later frame's base TS
	// fell below an earlier frame's last implied AU time, freezing
	// playback after ~1s.
	for _, pkt := range pkts {
		pkt.Timestamp += rtpTS
		if err := sess.stream.WritePacketRTP(sess.audioMedia, pkt); err != nil {
			slog.Debug("publisher: RTSP write audio RTP", "stream_code", sess.streamCode, "err", err)
			return
		}
	}
}

func (sess *rtspSession) closeStream() {
	if sess.stream == nil {
		return
	}
	sess.svc.mu.Lock()
	delete(sess.svc.rtspMounts, sess.mountPath)
	sess.svc.mu.Unlock()
	sess.stream.Close()
	slog.Info("publisher: RTSP stream unmounted", "stream_code", sess.streamCode)
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// extractSPSPPS returns the raw SPS (type 7) and PPS (type 8) NAL units from
// an Annex-B H.264 byte stream. Returns nil slices if not found.
func extractSPSPPS(annexB []byte) (sps, pps []byte) {
	for _, nalu := range annexBToNALUs(annexB) {
		if len(nalu) == 0 {
			continue
		}
		switch nalu[0] & 0x1f {
		case 7:
			sps = append([]byte(nil), nalu...)
		case 8:
			pps = append([]byte(nil), nalu...)
		}
	}
	return sps, pps
}

// annexBToNALUs splits an Annex-B H.264 byte stream into individual raw NAL units
// (without start codes). Handles both 3-byte and 4-byte start codes.
func annexBToNALUs(annexB []byte) [][]byte {
	var result [][]byte
	start := -1
	i := 0
	for i < len(annexB) {
		if i+4 <= len(annexB) && annexB[i] == 0 && annexB[i+1] == 0 && annexB[i+2] == 0 && annexB[i+3] == 1 {
			if start >= 0 && i > start {
				result = append(result, annexB[start:i])
			}
			i += 4
			start = i
			continue
		}
		if i+3 <= len(annexB) && annexB[i] == 0 && annexB[i+1] == 0 && annexB[i+2] == 1 {
			if start >= 0 && i > start {
				result = append(result, annexB[start:i])
			}
			i += 3
			start = i
			continue
		}
		i++
	}
	if start >= 0 && start < len(annexB) {
		result = append(result, annexB[start:])
	}
	// Filter empty NALUs.
	out := result[:0]
	for _, n := range result {
		if len(n) > 0 {
			out = append(out, n)
		}
	}
	return out
}
