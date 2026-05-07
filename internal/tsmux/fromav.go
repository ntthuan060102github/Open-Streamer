package tsmux

import (
	gocodec "github.com/yapingcat/gomedia/go-codec"
	gompeg2 "github.com/yapingcat/gomedia/go-mpeg2"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// FromAV wraps a gomedia TSMuxer and converts AVPackets into 188-byte MPEG-TS packets.
// One instance per output pipeline (segmenter, publisher pump, transcoder stdin feeder, …).
type FromAV struct {
	mux *gompeg2.TSMuxer

	vpid uint16
	apid uint16
	hasV bool
	hasA bool
}

// NewFromAV creates an empty muxer; streams are added on first packet per codec.
func NewFromAV() *FromAV {
	return &FromAV{mux: gompeg2.NewTSMuxer()}
}

// Write converts one AVPacket to TS transport packets via OnPacket callback.
func (f *FromAV) Write(p *domain.AVPacket, onPacket func([]byte)) {
	if p == nil || len(p.Data) == 0 || onPacket == nil {
		return
	}
	f.mux.OnPacket = onPacket
	switch p.Codec {
	case domain.AVCodecUnknown, domain.AVCodecRawTSChunk:
		return // unsupported — RawTSChunk is forwarded as Packet.TS upstream, never reaches the muxer
	case domain.AVCodecH264:
		if !f.hasV {
			f.vpid = f.mux.AddStream(gompeg2.TS_STREAM_H264)
			f.hasV = true
		}
		_ = f.mux.Write(f.vpid, p.Data, p.PTSms, p.DTSms) //nolint:errcheck // mux logs internally
	case domain.AVCodecH265:
		if !f.hasV {
			f.vpid = f.mux.AddStream(gompeg2.TS_STREAM_H265)
			f.hasV = true
		}
		_ = f.mux.Write(f.vpid, p.Data, p.PTSms, p.DTSms) //nolint:errcheck
	case domain.AVCodecAAC:
		if !f.hasA {
			f.apid = f.mux.AddStream(gompeg2.TS_STREAM_AAC)
			f.hasA = true
		}
		dts := p.DTSms
		if dts == 0 {
			dts = p.PTSms
		}
		_ = f.mux.Write(f.apid, p.Data, p.PTSms, dts) //nolint:errcheck
	case domain.AVCodecAC3, domain.AVCodecEAC3,
		domain.AVCodecAV1, domain.AVCodecMPEG2Video:
		// These codecs are recognised at the stats / config layer but we
		// don't currently have a frame extractor that produces AVPackets
		// for them — the existing gompeg2 demuxer drops their PES payload.
		// Re-mux through this path therefore never triggers in practice
		// today; the case exists for exhaustiveness so future contributors
		// adding a custom frame extractor can wire muxing here without
		// chasing a silent default-skip bug.
		return
	case domain.AVCodecMP2, domain.AVCodecMP3:
		// MPEG-1/2 Audio Layer I/II/III — use stream_type 0x04 (MPEG-2
		// audio) as the canonical container for both. The Layer is
		// encoded in each frame header itself, not in the PMT, so a
		// player decoding the resulting TS will pick up Layer III (MP3)
		// or Layer II (MP2) automatically. gomedia's TSMuxer accepts
		// both 0x03 and 0x04; 0x04 is the broader-compatibility choice.
		if !f.hasA {
			f.apid = f.mux.AddStream(gompeg2.TS_STREAM_AUDIO_MPEG2)
			f.hasA = true
		}
		dts := p.DTSms
		if dts == 0 {
			dts = p.PTSms
		}
		_ = f.mux.Write(f.apid, p.Data, p.PTSms, dts) //nolint:errcheck
	default:
		return
	}
}

// KeyFrameH264 reports whether Annex-B H.264 payload contains an IDR slice.
func KeyFrameH264(annexB []byte) bool {
	return gocodec.IsH264IDRFrame(annexB)
}

// KeyFrameH265 reports whether Annex-B H.265 payload contains an IRAP slice.
func KeyFrameH265(annexB []byte) bool {
	return gocodec.IsH265IDRFrame(annexB)
}

// FeedWirePacket forwards raw TS chunks or muxes one AVPacket to TS via mux (lazily allocated).
func FeedWirePacket(ts []byte, av *domain.AVPacket, mux **FromAV, onTS func([]byte)) {
	if len(ts) > 0 {
		onTS(ts)
		return
	}
	if av == nil || len(av.Data) == 0 || onTS == nil {
		return
	}
	if *mux == nil {
		*mux = NewFromAV()
	}
	(*mux).Write(av, onTS)
}

// FindH264ParameterSets scans raw bytes for the first NAL units of type 7
// (SPS) and type 8 (PPS) and returns their bodies (without start codes).
// Detects both 4-byte (00 00 00 01) and 3-byte (00 00 01) Annex-B start
// codes. Returns nil for any NAL not present.
//
// Use case: the publisher's serve_rtmp pipeline needs SPS/PPS to build
// the AVCDecoderConfigurationRecord seq header tag, but receives raw TS
// from the buffer hub (no AV packets). gomedia's TSDemuxer should
// preserve SPS/PPS in OnFrame access-unit bytes per its source, but
// some upstream code paths or stream variations have produced frames
// without parameter sets. Scanning raw TS bytes is a defensive
// fallback that does NOT depend on the demuxer's frame-splitting
// behaviour — at the cost of false positives if SPS-shaped bytes
// appear inside payload data (rare in practice for short scans).
//
// This is a best-effort scan: NAL units split across TS-packet
// boundaries (with adaptation-field stuffing) may be missed. For SPS
// (~30 bytes for typical 1080p H264) this is unlikely since the whole
// NAL fits within a single 184-byte TS payload. PPS is even smaller.
//
// Caller should retain the returned slices (do not reference into the
// input — we copy out so subsequent buffer reuse cannot corrupt the
// cached parameter sets).
func FindH264ParameterSets(raw []byte) (sps, pps []byte) {
	sps = findNALWithType(raw, 7)
	pps = findNALWithType(raw, 8)
	return sps, pps
}

// findNALWithType returns the first NAL with the given nal_unit_type
// (low 5 bits of the NAL header byte) in `raw`, or nil when not found.
// Matches both 4-byte (00 00 00 01) and 3-byte (00 00 01) Annex-B start
// codes and on `(headerByte & 0x1F) == nalType`, so any nal_ref_idc
// (high bits) accepts — production streams use 0x67 (nal_ref_idc=3,
// type=7) for SPS but in principle 0x07 (nal_ref_idc=0) is also valid.
func findNALWithType(raw []byte, nalType byte) []byte {
	for i := 0; i+4 < len(raw); i++ {
		if raw[i] != 0x00 {
			continue
		}
		// 4-byte start code: 00 00 00 01
		if i+4 < len(raw) && raw[i+1] == 0x00 && raw[i+2] == 0x00 && raw[i+3] == 0x01 {
			if raw[i+4]&0x1F == nalType {
				return extractNALBody(raw, i+4)
			}
		}
		// 3-byte start code: 00 00 01
		if i+3 < len(raw) && raw[i+1] == 0x00 && raw[i+2] == 0x01 {
			if raw[i+3]&0x1F == nalType {
				return extractNALBody(raw, i+3)
			}
		}
	}
	return nil
}

// extractNALBody returns the bytes of a single NAL starting at startIdx
// (which points to the NAL header byte itself) up to the next start code
// or end of raw. Caller must ensure startIdx is within bounds.
func extractNALBody(raw []byte, startIdx int) []byte {
	if startIdx >= len(raw) {
		return nil
	}
	// Search forward for the next start code; the NAL ends just before it.
	end := len(raw)
	for i := startIdx + 1; i+2 < len(raw); i++ {
		if raw[i] == 0x00 && raw[i+1] == 0x00 && (raw[i+2] == 0x01 ||
			(i+3 < len(raw) && raw[i+2] == 0x00 && raw[i+3] == 0x01)) {
			end = i
			break
		}
	}
	out := make([]byte, end-startIdx)
	copy(out, raw[startIdx:end])
	return out
}
