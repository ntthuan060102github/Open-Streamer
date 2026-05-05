package pull

// rtmp_msg_converter.go — translates lal `base.RtmpMsg` values into
// `domain.AVPacket` values, applying the byte-level conversions required
// for downstream consumers (HLS / DASH / MPEG-TS muxer):
//
//   - H.264 video sequence header → Annex-B SPS/PPS prefix (stored)
//   - H.264 NALU                  → AVCC → Annex-B, prepend SPS/PPS on IDR
//   - H.265 video sequence header → Annex-B VPS/SPS/PPS prefix (stored)
//   - H.265 NALU                  → AVCC → Annex-B, prepend prefix on IDR
//   - AAC sequence header         → AscContext (stored for ADTS)
//   - AAC raw frame               → ADTS-prefixed payload
//
// The converter is shared by:
//
//   - the pull reader (rtmp.go), which feeds it `RtmpMsg`s from a remote
//     RTMP source and writes the resulting AVPackets onto a channel; and
//   - the push server (push/rtmp_server.go), which feeds it `RtmpMsg`s
//     from a local encoder and writes AVPackets directly into the buffer
//     hub. Both consumers need the same byte-level conversion guarantees.
//
// Codec config state (avcAnnexbPrefix, hevcAnnexbPrefix, aacCtx) is per-
// instance — instantiate one converter per RTMP stream/session, never
// share across streams. Each instance is single-goroutine; the calling
// session's read loop owns it for its lifetime.

import (
	"github.com/q191201771/lal/pkg/aac"
	"github.com/q191201771/lal/pkg/avc"
	"github.com/q191201771/lal/pkg/base"
	"github.com/q191201771/lal/pkg/hevc"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// RTMPMsgConverter holds the per-stream codec configuration state and
// converts incoming RTMP messages to domain AVPackets.
type RTMPMsgConverter struct {
	// avcAnnexbPrefix is the SPS+PPS bytes in Annex-B form, parsed once
	// from the AVC sequence header. Prepended to every IDR NALU so a
	// fresh decoder can initialise from a single packet.
	avcAnnexbPrefix []byte

	// hevcAnnexbPrefix is the VPS+SPS+PPS bytes in Annex-B form for HEVC,
	// same role as avcAnnexbPrefix.
	hevcAnnexbPrefix []byte

	// aacCtx wraps the AudioSpecificConfig parsed from the AAC sequence
	// header. Used to synthesise the ADTS header on each raw frame.
	aacCtx *aac.AscContext
}

// NewRTMPMsgConverter constructs an empty converter ready to receive
// sequence headers. AVPackets emitted before the first matching codec
// sequence header arrives are dropped (no codec config to attach).
func NewRTMPMsgConverter() *RTMPMsgConverter {
	return &RTMPMsgConverter{}
}

// Convert processes one RtmpMsg and returns the resulting AVPacket(s).
// Sequence-header messages update internal state and emit no packet
// (returns nil). Unsupported codecs (legacy RTMP audio: PCM/G.711/Speex,
// non-AVC/HEVC video) are silently dropped.
func (c *RTMPMsgConverter) Convert(msg base.RtmpMsg) []domain.AVPacket {
	switch msg.Header.MsgTypeId {
	case base.RtmpTypeIdVideo:
		return c.convertVideo(msg)
	case base.RtmpTypeIdAudio:
		return c.convertAudio(msg)
	}
	return nil
}

// convertVideo handles RtmpTypeIdVideo messages.
func (c *RTMPMsgConverter) convertVideo(msg base.RtmpMsg) []domain.AVPacket {
	if len(msg.Payload) < 5 {
		return nil
	}
	codecID := msg.VideoCodecId()

	if msg.IsVideoKeySeqHeader() {
		c.captureVideoSeqHeader(msg, codecID)
		return nil
	}

	avccData := videoMsgAvccPayload(msg)
	if len(avccData) == 0 {
		return nil
	}

	var (
		codec  domain.AVCodec
		prefix []byte
	)
	switch codecID {
	case base.RtmpCodecIdAvc:
		codec = domain.AVCodecH264
		prefix = c.avcAnnexbPrefix
	case base.RtmpCodecIdHevc:
		codec = domain.AVCodecH265
		prefix = c.hevcAnnexbPrefix
	default:
		return nil // unsupported video codec (Sorenson, VP6, …)
	}

	annexB, err := avc.Avcc2Annexb(avccData)
	if err != nil || len(annexB) == 0 {
		return nil
	}

	isKey := msg.IsVideoKeyNalu()
	data := annexB
	if isKey && len(prefix) > 0 {
		data = make([]byte, 0, len(prefix)+len(annexB))
		data = append(data, prefix...)
		data = append(data, annexB...)
	}

	dts := uint64(msg.Header.TimestampAbs)
	pts := dts + uint64(msg.Cts())
	return []domain.AVPacket{{
		Codec:    codec,
		Data:     data,
		PTSms:    pts,
		DTSms:    dts,
		KeyFrame: isKey,
	}}
}

// captureVideoSeqHeader parses the codec configuration record from a
// video sequence-header message and stores the corresponding Annex-B
// prefix (SPS/PPS for AVC, VPS/SPS/PPS for HEVC). Both classic and
// Enhanced RTMP framing are recognised.
func (c *RTMPMsgConverter) captureVideoSeqHeader(msg base.RtmpMsg, codecID uint8) {
	switch codecID {
	case base.RtmpCodecIdAvc:
		// Classic AVC: 5-byte FLV video tag header before the
		// AVCDecoderConfigurationRecord.
		annexB, err := avc.SpsPpsSeqHeader2Annexb(msg.Payload[5:])
		if err == nil {
			c.avcAnnexbPrefix = annexB
		}
	case base.RtmpCodecIdHevc:
		var annexB []byte
		var err error
		if msg.IsEnhanced() {
			annexB, err = hevc.VpsSpsPpsEnhancedSeqHeader2Annexb(msg.Payload)
		} else {
			annexB, err = hevc.VpsSpsPpsSeqHeader2Annexb(msg.Payload[5:])
		}
		if err == nil {
			c.hevcAnnexbPrefix = annexB
		}
	}
}

// videoMsgAvccPayload returns the AVCC-formatted NALU bytes from a video
// message, accounting for the difference between classic and Enhanced
// RTMP framing. Returns nil when the message is not a NALU carrier.
func videoMsgAvccPayload(msg base.RtmpMsg) []byte {
	if msg.IsEnhanced() {
		idx := msg.GetEnchanedHevcNaluIndex()
		if idx == 0 || idx >= len(msg.Payload) {
			return nil
		}
		return msg.Payload[idx:]
	}
	if len(msg.Payload) <= 5 {
		return nil
	}
	return msg.Payload[5:]
}

// convertAudio handles RtmpTypeIdAudio messages. Only AAC is surfaced;
// legacy RTMP audio formats (PCM, MP3, G.711, Speex, Nellymoser) are
// dropped because the rest of the pipeline assumes AAC at the buffer
// hub for AV inputs (raw-TS sources go through a different path).
func (c *RTMPMsgConverter) convertAudio(msg base.RtmpMsg) []domain.AVPacket {
	if len(msg.Payload) < 2 {
		return nil
	}
	if msg.AudioCodecId() != base.RtmpSoundFormatAac {
		return nil
	}
	if msg.IsAacSeqHeader() {
		ctx, err := aac.NewAscContext(msg.Payload[2:])
		if err == nil {
			c.aacCtx = ctx
		}
		return nil
	}
	if c.aacCtx == nil {
		return nil // no codec config seen yet — drop until seq header arrives
	}
	rawAAC := msg.Payload[2:]
	if len(rawAAC) == 0 {
		return nil
	}
	adtsHeader := c.aacCtx.PackAdtsHeader(len(rawAAC))
	out := make([]byte, 0, len(adtsHeader)+len(rawAAC))
	out = append(out, adtsHeader...)
	out = append(out, rawAAC...)

	dts := uint64(msg.Header.TimestampAbs)
	return []domain.AVPacket{{
		Codec: domain.AVCodecAAC,
		Data:  out,
		PTSms: dts,
		DTSms: dts,
	}}
}
