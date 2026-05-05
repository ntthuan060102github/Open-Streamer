package push

// rtmp_writer.go — bridges the publisher-side TS-demux pipeline to a lal
// `*rtmp.ServerSession` for serving external RTMP play clients.
//
// The publisher (internal/publisher/serve_rtmp.go) reads from the Buffer
// Hub, demuxes the MPEG-TS bytes via gomedia's TSDemuxer, and emits one
// callback per H.264 / AAC access unit. Per-access-unit data is in:
//
//   - H.264: Annex-B start codes, SPS/PPS prepended on IDR (rtmp.go's
//     handling guarantees this on the ingest side; the same shape is
//     preserved through the buffer hub).
//   - AAC:   ADTS-prefixed bytes (7-byte header + raw frame).
//
// To send those to the lal session, we wrap each access unit into the
// FLV tag format the RTMP wire protocol expects (the same format lal's
// own demuxer parses on the receive side):
//
//   - First H.264 frame: derive SPS/PPS from Annex-B, build an AVC
//     sequence header tag (FLV PacketType=0 = AVCDecoderConfigurationRecord),
//     send.
//   - Subsequent H.264: convert Annex-B → AVCC, build a NALU tag (PacketType=1).
//   - First AAC frame:   parse ADTS header → AscContext, build an AAC
//     sequence header tag (PacketType=0 = AudioSpecificConfig), send.
//   - Subsequent AAC: strip ADTS header, build raw frame tag (PacketType=1).
//
// Sequence headers must precede NALU / raw audio frames otherwise the
// receiving player can't initialise its decoder. The writer enforces
// this by latching `seqSent` flags on first frame.
//
// Strict players (Flussonic, JW Player, ffplay -err_detect) also expect:
//
//   - An `onMetaData` AMF0 script tag with codec IDs *before* any AV data.
//     Without it they refuse to play with "unknown stream" — even if all
//     subsequent video / audio tags are valid. Lenient players (LAL pull,
//     OBS preview) tolerate the omission, which is why the bug went
//     unnoticed against open-streamer ↔ open-streamer.
//   - Sequence headers at timestamp 0, not at the first frame's DTS.
//     The wire timestamp on the seq header is informational; players
//     timestamp-sort the GOP and a non-zero seq-header timestamp pushes
//     it past the first NALU, breaking decoder init.
//
// To satisfy them, we always emit:
//
//   1. onMetaData (CSID 5, timestamp 0) on first WriteFrame call.
//   2. AVC seq header (CSID 7, timestamp 0) on first H.264 frame.
//   3. AAC seq header (CSID 6, timestamp 0) on first AAC frame.
//   4. NALU / raw audio (CSIDs 7/6) at their actual DTS.

import (
	"fmt"

	"github.com/q191201771/lal/pkg/aac"
	"github.com/q191201771/lal/pkg/avc"
	"github.com/q191201771/lal/pkg/base"
	"github.com/q191201771/lal/pkg/rtmp"
)

// FrameKind classifies an access unit so the writer doesn't depend on
// the source TS demuxer's codec enum.
type FrameKind int

// FrameKind values.
const (
	FrameKindUnknown FrameKind = iota
	FrameKindH264
	FrameKindAAC
)

// adtsHeaderLength is the standard 7-byte ADTS header (no CRC). LAL
// emits this length on the ingest side; the demuxer downstream of buffer
// hub does too. Frames shorter than this are malformed and dropped.
const adtsHeaderLength = 7

// RTMPFrameWriter sends one access unit at a time to a lal ServerSession,
// emitting onMetaData and AVC / AAC sequence headers automatically on
// the first frame of each codec.
type RTMPFrameWriter struct {
	session *rtmp.ServerSession

	metaSent   bool
	avcSeqSent bool
	aacSeqSent bool
}

// NewRTMPFrameWriter wraps session for per-frame writes.
func NewRTMPFrameWriter(session *rtmp.ServerSession) *RTMPFrameWriter {
	return &RTMPFrameWriter{session: session}
}

// WriteFrame sends one access unit. dts/pts are RTMP wire timestamps in
// milliseconds. Returns an error if the underlying TCP write fails.
func (w *RTMPFrameWriter) WriteFrame(kind FrameKind, data []byte, pts, dts uint32) error {
	if len(data) == 0 {
		return nil
	}
	if !w.metaSent {
		if err := w.sendMetadata(); err != nil {
			return err
		}
		w.metaSent = true
	}
	switch kind {
	case FrameKindH264:
		return w.writeH264(data, pts, dts)
	case FrameKindAAC:
		return w.writeAAC(data, dts)
	case FrameKindUnknown:
		return nil
	}
	return nil
}

// sendMetadata emits the onMetaData AMF0 script tag at timestamp 0.
// Open Streamer's RTMP play path only supports H.264 + AAC so the codec
// IDs are hardcoded — strict players (Flussonic) refuse to play without
// this tag even if every subsequent AV message is well-formed.
func (w *RTMPFrameWriter) sendMetadata() error {
	meta, err := rtmp.BuildMetadata(-1, -1, int(base.RtmpSoundFormatAac), int(base.RtmpCodecIdAvc))
	if err != nil {
		return fmt.Errorf("rtmp writer: build metadata: %w", err)
	}
	header := base.RtmpHeader{
		Csid:         rtmp.CsidAmf,
		MsgTypeId:    base.RtmpTypeIdMetadata,
		MsgStreamId:  rtmp.Msid1,
		MsgLen:       uint32(len(meta)),
		TimestampAbs: 0,
	}
	chunks := rtmp.Message2Chunks(meta, &header)
	return w.session.Write(chunks)
}

// writeH264 sends a single H.264 access unit (Annex-B with optional
// SPS/PPS prefix on IDR). Emits the AVC sequence header tag once at
// timestamp 0 before the first NALU tag.
func (w *RTMPFrameWriter) writeH264(annexB []byte, pts, dts uint32) error {
	if !w.avcSeqSent {
		sps, pps, ok := extractSpsPpsFromAnnexB(annexB)
		if !ok {
			// Drop frames until we see one carrying SPS+PPS — the player
			// can't decode without them anyway.
			return nil
		}
		seqRecord, err := avc.BuildSeqHeaderFromSpsPps(sps, pps)
		if err != nil {
			return fmt.Errorf("rtmp writer: build avc seq header: %w", err)
		}
		seqTag := buildFLVAvcTag(true, 0, 0, seqRecord)
		if err := w.send(base.RtmpTypeIdVideo, 0, seqTag); err != nil {
			return err
		}
		w.avcSeqSent = true
	}

	avccData, err := avc.Annexb2Avcc(annexB)
	if err != nil {
		return fmt.Errorf("rtmp writer: annexb→avcc: %w", err)
	}
	isKey := containsIDRAnnexB(annexB)
	cts := int32(pts) - int32(dts) //nolint:gosec // RTMP CTS is signed 24-bit; we clamp on encode
	naluTag := buildFLVAvcTag(isKey, 1, cts, avccData)
	return w.send(base.RtmpTypeIdVideo, dts, naluTag)
}

// writeAAC sends a single AAC access unit (ADTS-prefixed). Emits the AAC
// sequence header tag once at timestamp 0 before the first raw frame tag.
func (w *RTMPFrameWriter) writeAAC(adts []byte, dts uint32) error {
	if len(adts) < adtsHeaderLength {
		return nil
	}
	if !w.aacSeqSent {
		asc, err := aac.MakeAscWithAdtsHeader(adts[:adtsHeaderLength])
		if err != nil {
			return fmt.Errorf("rtmp writer: extract asc from adts: %w", err)
		}
		seqTag := buildFLVAacTag(0, asc)
		if err := w.send(base.RtmpTypeIdAudio, 0, seqTag); err != nil {
			return err
		}
		w.aacSeqSent = true
	}
	rawAAC := adts[adtsHeaderLength:]
	if len(rawAAC) == 0 {
		return nil
	}
	rawTag := buildFLVAacTag(1, rawAAC)
	return w.send(base.RtmpTypeIdAudio, dts, rawTag)
}

// send wraps payload in an RTMP message header and writes it to the
// session. The message is chunked once and pushed as a single TCP write —
// no per-chunk round-trips.
func (w *RTMPFrameWriter) send(typeID uint8, dts uint32, payload []byte) error {
	header := base.RtmpHeader{
		MsgTypeId:    typeID,
		TimestampAbs: dts,
		MsgLen:       uint32(len(payload)),
		MsgStreamId:  rtmp.Msid1,
	}
	switch typeID {
	case base.RtmpTypeIdVideo:
		header.Csid = rtmp.CsidVideo
	case base.RtmpTypeIdAudio:
		header.Csid = rtmp.CsidAudio
	}
	chunks := rtmp.Message2Chunks(payload, &header)
	return w.session.Write(chunks)
}

// buildFLVAvcTag wraps an AVC payload in the FLV video tag format the
// RTMP wire protocol expects. CompositionTime is encoded as a signed
// 24-bit big-endian integer in bytes 2-4.
func buildFLVAvcTag(isKey bool, packetType byte, cts int32, payload []byte) []byte {
	out := make([]byte, 5+len(payload))
	frameType := byte(2) // inter frame
	if isKey {
		frameType = 1
	}
	out[0] = (frameType << 4) | base.RtmpCodecIdAvc
	out[1] = packetType
	// Signed 24-bit big-endian. negative values clamped to zero — players
	// don't accept negative composition time and we'd rather skip the
	// adjustment than send a malformed tag.
	if cts < 0 {
		cts = 0
	}
	out[2] = byte(cts >> 16)
	out[3] = byte(cts >> 8)
	out[4] = byte(cts)
	copy(out[5:], payload)
	return out
}

// buildFLVAacTag wraps an AAC payload in the FLV audio tag format. The
// SoundFormat byte is fixed at 0xAF (SoundFormat=10 [AAC], SoundRate=3
// [44.1 kHz, ignored by AAC], SoundSize=1 [16-bit, ignored], SoundType=1
// [stereo, ignored]) — all the per-frame audio params come from the ASC
// in the seq header instead.
func buildFLVAacTag(packetType byte, payload []byte) []byte {
	out := make([]byte, 2+len(payload))
	out[0] = 0xAF
	out[1] = packetType
	copy(out[2:], payload)
	return out
}

// extractSpsPpsFromAnnexB walks a single Annex-B-formatted access unit
// and returns the first SPS (NAL type 7) and PPS (NAL type 8) it finds.
// ok=false when either is missing.
func extractSpsPpsFromAnnexB(annexB []byte) (sps, pps []byte, ok bool) {
	_ = avc.IterateNaluAnnexb(annexB, func(nal []byte) {
		if len(nal) == 0 {
			return
		}
		switch nal[0] & 0x1F {
		case 7: // SPS
			if sps == nil {
				sps = append([]byte(nil), nal...)
			}
		case 8: // PPS
			if pps == nil {
				pps = append([]byte(nil), nal...)
			}
		}
	})
	return sps, pps, sps != nil && pps != nil
}

// containsIDRAnnexB returns true when annexB carries any NAL of type 5
// (Coded slice of an IDR picture). Used to set the FLV FrameType correctly.
func containsIDRAnnexB(annexB []byte) bool {
	found := false
	_ = avc.IterateNaluAnnexb(annexB, func(nal []byte) {
		if len(nal) > 0 && (nal[0]&0x1F) == 5 {
			found = true
		}
	})
	return found
}
