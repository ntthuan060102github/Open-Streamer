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
