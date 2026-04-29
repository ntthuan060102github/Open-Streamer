package pull

// rtmp.go — RTMP pull ingestor using nareix/joy4/format/rtmp.
//
// Connects to a remote RTMP server in play mode, probes codec data (H.264 SPS/PPS,
// AAC AudioSpecificConfig) from the stream header, then enters a ReadPacket loop.
//
// Codec conversions applied before emitting domain.AVPacket:
//   H.264 : AVCC length-prefixed → Annex-B start-codes;
//           SPS+PPS (from codec config) prepended on every IDR frame.
//   AAC   : bare MPEG-4 Audio frame → 7-byte ADTS header prepended when needed.
//
// Package-level helpers in this file are also used by rtsp.go:
//   filterSupportedRTMPStreams, h264ForTSMuxer, h264AccessUnitForTS,
//   isH264AVCCAccessUnit, avccAccessUnitToAnnexB, aacFrameToADTS.

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	joyav "github.com/nareix/joy4/av"
	"github.com/nareix/joy4/codec/aacparser"
	joyh264 "github.com/nareix/joy4/codec/h264parser"
	joyrtmp "github.com/nareix/joy4/format/rtmp"

	gocodec "github.com/yapingcat/gomedia/go-codec"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

const rtmpChanSize = 16384

// RTMPReader connects to a remote RTMP server in play (pull) mode and emits domain.AVPacket.
//
// Life cycle: NewRTMPReader → Open (blocks on TCP+RTMP handshake) → ReadPackets loop → Close.
// The underlying read goroutine exits when the server closes the connection or Close is called;
// both paths close pkts so ReadPackets returns io.EOF.
type RTMPReader struct {
	input domain.Input

	mu   sync.Mutex
	conn *joyrtmp.Conn
	pkts chan domain.AVPacket
	done chan struct{}

	filtered            []joyav.CodecData
	idxMap              map[int]int // source stream index → index in filtered
	h264KeyPrefixAnnexB [][]byte    // per filtered index: 4-byte SC + SPS + 4-byte SC + PPS
	aacCfg              *aacparser.MPEG4AudioConfig
}

// NewRTMPReader constructs an RTMPReader without opening a connection.
func NewRTMPReader(input domain.Input) *RTMPReader {
	return &RTMPReader{input: input}
}

// Open dials the RTMP source, negotiates codec data, and starts the background read loop.
// Blocks until the TCP connection + RTMP handshake + codec probing complete.
func (r *RTMPReader) Open(ctx context.Context) error {
	_ = ctx // joy4 dial exposes a timeout but no context API

	r.mu.Lock()
	if r.conn != nil {
		r.mu.Unlock()
		return fmt.Errorf("rtmp reader: already open")
	}
	r.pkts = make(chan domain.AVPacket, rtmpChanSize)
	r.done = make(chan struct{})
	r.mu.Unlock()

	connectTimeout := time.Duration(r.input.Net.TimeoutSec) * time.Second
	if connectTimeout == 0 {
		connectTimeout = time.Duration(domain.DefaultRTMPTimeoutSec) * time.Second
	}

	conn, err := joyrtmp.DialTimeout(r.input.URL, connectTimeout)
	if err != nil {
		r.abortOpen()
		return fmt.Errorf("rtmp reader: dial %q: %w", r.input.URL, err)
	}

	streams, err := conn.Streams()
	if err != nil {
		_ = conn.Close()
		r.abortOpen()
		return fmt.Errorf("rtmp reader: streams %q: %w", r.input.URL, err)
	}

	r.scanStreams(streams)

	filtered, idxMap := filterSupportedRTMPStreams(streams)
	if len(filtered) == 0 {
		_ = conn.Close()
		r.abortOpen()
		return fmt.Errorf("rtmp reader: no supported streams in %q (need H264 or AAC)", r.input.URL)
	}

	h264Prefix := r.buildH264Prefixes(filtered)

	r.mu.Lock()
	r.conn = conn
	r.filtered = filtered
	r.idxMap = idxMap
	r.h264KeyPrefixAnnexB = h264Prefix
	r.mu.Unlock()

	go r.readLoop()
	return nil
}

// scanStreams logs discovered tracks and captures the AAC config for ADTS wrapping.
func (r *RTMPReader) scanStreams(streams []joyav.CodecData) {
	for i, s := range streams {
		supported := s.Type() == joyav.H264 || s.Type() == joyav.AAC
		slog.Info("rtmp reader: source stream discovered",
			"url", r.input.URL,
			"source_stream_idx", i,
			"codec", s.Type().String(),
			"supported", supported,
		)
		if s.Type() == joyav.AAC {
			if cd, ok := s.(aacparser.CodecData); ok {
				cfg := cd.Config
				r.aacCfg = &cfg
			}
		}
	}
}

// buildH264Prefixes constructs the Annex-B SPS+PPS prefix for each H.264 filtered track.
func (r *RTMPReader) buildH264Prefixes(filtered []joyav.CodecData) [][]byte {
	out := make([][]byte, len(filtered))
	for i, s := range filtered {
		if s.Type() != joyav.H264 {
			continue
		}
		cd, ok := s.(joyh264.CodecData)
		if !ok {
			slog.Warn("rtmp reader: H264 track has no h264parser.CodecData — SPS/PPS injection disabled",
				"url", r.input.URL, "es_stream_idx", i)
			continue
		}
		sps, pps := cd.SPS(), cd.PPS()
		if len(sps) == 0 || len(pps) == 0 {
			slog.Warn("rtmp reader: H264 codec data missing SPS or PPS",
				"url", r.input.URL, "es_stream_idx", i)
			continue
		}
		var b []byte
		b = append(b, 0, 0, 0, 1)
		b = append(b, sps...)
		b = append(b, 0, 0, 0, 1)
		b = append(b, pps...)
		out[i] = b
	}
	return out
}

func (r *RTMPReader) abortOpen() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pkts != nil {
		close(r.pkts)
	}
	r.pkts = nil
	r.done = nil
	r.conn = nil
}

func (r *RTMPReader) readLoop() {
	defer r.teardownAfterReadLoop()
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("rtmp reader: panic in joy4, closing connection",
				"url", r.input.URL, "panic", rec)
		}
	}()

	for {
		r.mu.Lock()
		c := r.conn
		r.mu.Unlock()
		if c == nil {
			return
		}

		pkt, err := c.ReadPacket()
		if err != nil {
			slog.Warn("rtmp reader: read packet ended",
				"url", r.input.URL, "err", err)
			return
		}

		if r.dispatch(pkt) {
			return
		}
	}
}

// dispatch routes one joy4 packet to the appropriate emit method.
// Returns true when the caller should stop the read loop.
func (r *RTMPReader) dispatch(pkt joyav.Packet) bool {
	mapped, ok := r.idxMap[int(pkt.Idx)]
	if !ok || mapped < 0 || mapped >= len(r.filtered) {
		return false
	}

	r.mu.Lock()
	outCh := r.pkts
	doneCh := r.done
	r.mu.Unlock()
	if outCh == nil {
		return true
	}

	dtsMS := uint64(pkt.Time / time.Millisecond)
	ptsMS := dtsMS + uint64(pkt.CompositionTime/time.Millisecond)

	switch r.filtered[mapped].Type() {
	case joyav.H264:
		return r.emitH264(outCh, doneCh, pkt, mapped, ptsMS, dtsMS)
	case joyav.AAC:
		return r.emitAAC(outCh, doneCh, pkt, dtsMS)
	}
	return false
}

func (r *RTMPReader) emitH264(
	outCh chan domain.AVPacket,
	doneCh chan struct{},
	pkt joyav.Packet,
	mapped int,
	ptsMS, dtsMS uint64,
) bool {
	annexB := h264ForTSMuxer(pkt.Data)
	data := h264AccessUnitForTS(pkt.IsKeyFrame, r.h264KeyPrefixAnnexB[mapped], annexB)
	if len(data) == 0 {
		return false
	}
	p := domain.AVPacket{
		Codec:    domain.AVCodecH264,
		Data:     data,
		PTSms:    ptsMS,
		DTSms:    dtsMS,
		KeyFrame: gocodec.IsH264IDRFrame(data),
	}
	select {
	case outCh <- p:
	case <-doneCh:
		return true
	}
	return false
}

func (r *RTMPReader) emitAAC(
	outCh chan domain.AVPacket,
	doneCh chan struct{},
	pkt joyav.Packet,
	dtsMS uint64,
) bool {
	data := aacFrameToADTS(r.aacCfg, pkt.Data)
	if len(data) == 0 {
		return false
	}
	p := domain.AVPacket{
		Codec: domain.AVCodecAAC,
		Data:  data,
		PTSms: dtsMS,
		DTSms: dtsMS,
	}
	select {
	case outCh <- p:
	case <-doneCh:
		return true
	}
	return false
}

func (r *RTMPReader) teardownAfterReadLoop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn != nil {
		_ = r.conn.Close()
		r.conn = nil
	}
	if r.done != nil {
		close(r.done)
		r.done = nil
	}
	if r.pkts != nil {
		close(r.pkts)
		r.pkts = nil
	}
}

// ReadPackets blocks until at least one AVPacket is available or the source ends.
func (r *RTMPReader) ReadPackets(ctx context.Context) ([]domain.AVPacket, error) {
	r.mu.Lock()
	ch := r.pkts
	r.mu.Unlock()
	if ch == nil {
		return nil, io.EOF
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case p, ok := <-ch:
		if !ok {
			return nil, io.EOF
		}
		batch := []domain.AVPacket{p}
		for len(batch) < 256 {
			select {
			case p2, ok2 := <-ch:
				if !ok2 {
					return batch, nil
				}
				batch = append(batch, p2)
			default:
				return batch, nil
			}
		}
		return batch, nil
	}
}

// Close closes the RTMP connection; the readLoop then closes the packet channels.
func (r *RTMPReader) Close() error {
	r.mu.Lock()
	c := r.conn
	r.conn = nil
	r.mu.Unlock()
	if c != nil {
		return c.Close()
	}
	return nil
}

// ─── Package-level codec helpers (shared by rtsp.go) ────────────────────────

// filterSupportedRTMPStreams returns only H.264 and AAC tracks and a source→filtered index map.
func filterSupportedRTMPStreams(streams []joyav.CodecData) ([]joyav.CodecData, map[int]int) {
	filtered := make([]joyav.CodecData, 0, len(streams))
	idxMap := make(map[int]int, len(streams))
	for i, s := range streams {
		switch s.Type() {
		case joyav.H264, joyav.AAC:
			idxMap[i] = len(filtered)
			filtered = append(filtered, s)
		}
	}
	return filtered, idxMap
}

// h264ForTSMuxer converts AVCC length-prefixed data to Annex-B start-code format.
// Returns the input unchanged when it is already Annex-B.
func h264ForTSMuxer(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	if isH264AVCCAccessUnit(b) {
		return avccAccessUnitToAnnexB(b)
	}
	return b
}

// h264AccessUnitForTS prepends SPS+PPS on IDR frames so downstream decoders
// can initialise without out-of-band codec data.
func h264AccessUnitForTS(isKeyFrame bool, psPrefixAnnexB, annexBAU []byte) []byte {
	if len(annexBAU) == 0 {
		return nil
	}
	if len(psPrefixAnnexB) == 0 || !isKeyFrame {
		return annexBAU
	}
	out := make([]byte, 0, len(psPrefixAnnexB)+len(annexBAU))
	out = append(out, psPrefixAnnexB...)
	out = append(out, annexBAU...)
	return out
}

// aacFrameToADTS prepends a 7-byte ADTS header to a bare MPEG-4 AAC frame.
// Returns the input unchanged when it already carries an ADTS header
// (0xFF 0xF? sync word) or when no MPEG4AudioConfig is available.
func aacFrameToADTS(cfg *aacparser.MPEG4AudioConfig, raw []byte) []byte {
	if len(raw) >= 2 && raw[0] == 0xff && raw[1]&0xf0 == 0xf0 {
		return raw // already ADTS
	}
	if cfg == nil {
		return raw
	}
	hdr := make([]byte, aacparser.ADTSHeaderLength)
	aacparser.FillADTSHeader(hdr, *cfg, 1024, len(raw))
	out := make([]byte, 0, len(hdr)+len(raw))
	out = append(out, hdr...)
	out = append(out, raw...)
	return out
}

// maxH264NALSize is the sanity cap used to distinguish AVCC from Annex-B payloads.
const maxH264NALSize = 8 * 1024 * 1024

// isH264AVCCAccessUnit returns true when b looks like a valid sequence of
// length-prefixed (4-byte big-endian) NAL units.
func isH264AVCCAccessUnit(b []byte) bool {
	i := 0
	for i+4 <= len(b) {
		n := int(binary.BigEndian.Uint32(b[i : i+4]))
		if n <= 0 || n > maxH264NALSize || i+4+n > len(b) {
			return false
		}
		i += 4 + n
	}
	return i == len(b)
}

// avccAccessUnitToAnnexB converts AVCC length-prefixed NALUs to Annex-B start codes.
func avccAccessUnitToAnnexB(src []byte) []byte {
	var out []byte
	i := 0
	for i+4 <= len(src) {
		n := int(binary.BigEndian.Uint32(src[i : i+4]))
		i += 4
		if n <= 0 || i+n > len(src) {
			return src
		}
		out = append(out, 0, 0, 0, 1)
		out = append(out, src[i:i+n]...)
		i += n
	}
	if len(out) == 0 {
		return src
	}
	return out
}
