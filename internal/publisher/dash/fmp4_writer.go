package dash

import (
	"bytes"
	"fmt"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/hevc"
	"github.com/Eyevinn/mp4ff/mp4"
)

// VideoTimescale is the fixed video track timescale for all DASH outputs
// produced by this packager (90 kHz, the MPEG-TS convention). Audio
// uses the source sample rate as its timescale, set on init build.
const VideoTimescale uint32 = 90000

// TrackInfo carries codec metadata derived during init segment build.
// Manifest generation reads it for the per-Representation codecs string
// + width/height attributes.
type TrackInfo struct {
	Codec  string // e.g. "avc1.4D4028", "hvc1.1.6.L150.B0", "mp4a.40.2"
	Width  int    // pixel width (0 for audio)
	Height int    // pixel height (0 for audio)
	IsHEVC bool   // distinguishes H.265 from H.264 at fragment build
}

// VideoInit is a built H.264 / H.265 init segment plus metadata.
type VideoInit struct {
	Init    *mp4.InitSegment
	TrackID uint32
	Info    TrackInfo
}

// AudioInit is a built AAC init segment plus metadata.
type AudioInit struct {
	Init       *mp4.InitSegment
	TrackID    uint32
	SampleRate int
	Codec      string
}

// BuildH264Init constructs the video init segment from the cached
// SPS/PPS byte stream. Returns nil + error if the parameter sets can't
// be parsed (caller should keep accumulating and retry on the next IDR).
//
// Inputs: SPS and PPS NALUs WITHOUT start codes (raw payload bytes).
// Multiple SPSes / PPSes are accepted but only the first SPS is used
// for codec string / width / height extraction.
func BuildH264Init(spss, ppss [][]byte) (*VideoInit, error) {
	if len(spss) == 0 || len(ppss) == 0 {
		return nil, fmt.Errorf("h264 init: missing SPS or PPS")
	}
	init := mp4.CreateEmptyInit()
	trak := init.AddEmptyTrack(VideoTimescale, "video", "und")
	if err := trak.SetAVCDescriptor("avc1", spss, ppss, true); err != nil {
		return nil, fmt.Errorf("h264 init: SetAVCDescriptor: %w", err)
	}
	info := TrackInfo{}
	if sps, err := avc.ParseSPSNALUnit(spss[0], false); err == nil && sps != nil {
		info.Codec = avc.CodecString("avc1", sps)
		info.Width = int(sps.Width)
		info.Height = int(sps.Height)
	}
	return &VideoInit{Init: init, TrackID: trak.Tkhd.TrackID, Info: info}, nil
}

// BuildH265Init constructs the video init segment from VPS/SPS/PPS.
// Same contract as BuildH264Init.
func BuildH265Init(vpss, spss, ppss [][]byte) (*VideoInit, error) {
	if len(spss) == 0 || len(ppss) == 0 {
		return nil, fmt.Errorf("h265 init: missing SPS or PPS")
	}
	init := mp4.CreateEmptyInit()
	trak := init.AddEmptyTrack(VideoTimescale, "video", "und")
	if err := trak.SetHEVCDescriptor("hvc1", vpss, spss, ppss, nil, true); err != nil {
		return nil, fmt.Errorf("h265 init: SetHEVCDescriptor: %w", err)
	}
	info := TrackInfo{IsHEVC: true}
	if sps, err := hevc.ParseSPSNALUnit(spss[0]); err == nil && sps != nil {
		w, h := sps.ImageSize()
		info.Codec = hevc.CodecString("hvc1", sps)
		info.Width = int(w)
		info.Height = int(h)
	}
	return &VideoInit{Init: init, TrackID: trak.Tkhd.TrackID, Info: info}, nil
}

// BuildAACInit constructs the audio init segment from the first ADTS
// header. The header determines sample rate (track timescale) and AAC
// profile (only AAC-LC is supported by the codec string baseline; HE-AAC
// would need extra signalling but is rare for live streaming).
func BuildAACInit(hdr *aac.ADTSHeader) (*AudioInit, error) {
	if hdr == nil {
		return nil, fmt.Errorf("aac init: nil ADTS header")
	}
	sr := int(hdr.Frequency())
	if sr <= 0 {
		return nil, fmt.Errorf("aac init: invalid sample rate")
	}
	init := mp4.CreateEmptyInit()
	trak := init.AddEmptyTrack(uint32(sr), "audio", "und") //nolint:gosec // sr is a valid positive int
	if err := trak.SetAACDescriptor(aac.AAClc, sr); err != nil {
		return nil, fmt.Errorf("aac init: SetAACDescriptor: %w", err)
	}
	return &AudioInit{
		Init:       init,
		TrackID:    trak.Tkhd.TrackID,
		SampleRate: sr,
		Codec:      "mp4a.40.2",
	}, nil
}

// EncodeInit serialises an InitSegment to bytes (init_v.mp4 / init_a.mp4
// file content). Returns the encoded buffer.
func EncodeInit(init *mp4.InitSegment) ([]byte, error) {
	var buf bytes.Buffer
	if err := init.Encode(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// BuildVideoFragment serialises N video frames into one fMP4 media
// fragment (.m4s content).
//
// Parameters:
//   - seqNum: monotonic segment sequence number (1-based; matches the
//     filename suffix and the moof.mfhd.sequence_number).
//   - trackID: from the corresponding init segment.
//   - tfdtTicks: absolute decode time of the first sample in the
//     fragment, in video timescale ticks. Caller computes this from
//     (now - AST) × 90000.
//   - frames: AVCC-encoded access units. Set ForceAVCC to false if the
//     caller has already converted Annex-B → AVCC; otherwise
//     BuildVideoFragment runs the conversion.
//   - isHEVC: distinguishes the conversion path.
//   - segDurTicks: total fragment duration in video timescale ticks.
//     Per-sample dur = segDurTicks / len(frames), distributed evenly.
//
// Returns the serialised .m4s bytes.
func BuildVideoFragment(
	seqNum uint32,
	trackID uint32,
	tfdtTicks uint64,
	frames []VideoFrame,
	isHEVC bool,
	segDurTicks uint64,
) ([]byte, error) {
	if len(frames) == 0 {
		return nil, fmt.Errorf("video fragment: no frames")
	}
	frag, err := mp4.CreateFragment(seqNum, trackID)
	if err != nil {
		return nil, fmt.Errorf("video fragment: CreateFragment: %w", err)
	}

	// Per-sample duration: equally divided. This loses per-frame timing
	// fidelity vs the v1 packager's inter-frame-delta approach, but the
	// player only uses per-sample dur as a hint for renderer scheduling;
	// the across-fragment tfdt is what determines playback timing.
	frameDur := uint32(segDurTicks / uint64(len(frames))) //nolint:gosec // bounded by segDur < ~5s × 90kHz < 2^32
	if frameDur == 0 {
		frameDur = 1 // safety: never emit a 0-duration sample
	}
	cumulative := uint64(0)

	for _, f := range frames {
		var avcc []byte
		if isHEVC {
			avcc = hevcAnnexBToAVCC(f.AnnexB)
		} else {
			avcc = h264AnnexBToAVCC(f.AnnexB)
		}
		if len(avcc) == 0 {
			continue
		}

		flags := mp4.NonSyncSampleFlags
		if f.IsIDR {
			flags = mp4.SyncSampleFlags
		}

		cto := int32((int64(f.PTSms) - int64(f.DTSms)) * 90) //nolint:gosec // ms × 90 fits int32 for any plausible CTO

		frag.AddFullSample(mp4.FullSample{
			Sample: mp4.Sample{
				Flags:                 flags,
				Dur:                   frameDur,
				Size:                  uint32(len(avcc)), //nolint:gosec // bounded by access unit size
				CompositionTimeOffset: cto,
			},
			DecodeTime: tfdtTicks + cumulative,
			Data:       avcc,
		})
		cumulative += uint64(frameDur)
	}

	seg := mp4.NewMediaSegment()
	seg.AddFragment(frag)
	var buf bytes.Buffer
	if err := seg.Encode(&buf); err != nil {
		return nil, fmt.Errorf("video fragment: Encode: %w", err)
	}
	return buf.Bytes(), nil
}

// BuildAudioFragment serialises N AAC frames into one fMP4 media
// fragment. Each AAC frame is 1024 samples (the AAC standard), so
// per-sample dur in the audio timescale is exactly 1024 ticks
// regardless of how the segment was sliced.
//
// Parameters mirror BuildVideoFragment except:
//   - tfdtTicks is in audio timescale ticks (= sample rate Hz).
//   - segDurTicks is ignored; we use the AAC-standard 1024 samples/frame.
func BuildAudioFragment(
	seqNum uint32,
	trackID uint32,
	tfdtTicks uint64,
	frames []AudioFrame,
) ([]byte, error) {
	if len(frames) == 0 {
		return nil, fmt.Errorf("audio fragment: no frames")
	}
	frag, err := mp4.CreateFragment(seqNum, trackID)
	if err != nil {
		return nil, fmt.Errorf("audio fragment: CreateFragment: %w", err)
	}

	const aacSamplesPerFrame uint32 = 1024
	cumulative := uint64(0)

	for _, f := range frames {
		if len(f.Raw) == 0 {
			continue
		}
		frag.AddFullSample(mp4.FullSample{
			Sample: mp4.Sample{
				Flags: mp4.SyncSampleFlags, // every AAC frame is a sync sample
				Dur:   aacSamplesPerFrame,
				Size:  uint32(len(f.Raw)), //nolint:gosec // bounded by AAC frame size
			},
			DecodeTime: tfdtTicks + cumulative,
			Data:       f.Raw,
		})
		cumulative += uint64(aacSamplesPerFrame)
	}

	seg := mp4.NewMediaSegment()
	seg.AddFragment(frag)
	var buf bytes.Buffer
	if err := seg.Encode(&buf); err != nil {
		return nil, fmt.Errorf("audio fragment: Encode: %w", err)
	}
	return buf.Bytes(), nil
}

// ExtractParameterSets returns the SPS and PPS NALU payloads from a
// concatenated Annex-B byte stream. Used to seed BuildH264Init from the
// accumulated SPS/PPS buffer; the buffer is fed from inline parameter
// sets carried on H.264 IDR access units.
func ExtractParameterSets(annexB []byte) (spss, ppss [][]byte) {
	psBuf := annexB4To3(annexB)
	spss = avc.ExtractNalusOfTypeFromByteStream(avc.NALU_SPS, psBuf, false)
	ppss = avc.ExtractNalusOfTypeFromByteStream(avc.NALU_PPS, psBuf, false)
	return spss, ppss
}

// ExtractHEVCParameterSets is the H.265 counterpart of
// ExtractParameterSets, returning VPS / SPS / PPS NALUs.
func ExtractHEVCParameterSets(annexB []byte) (vpss, spss, ppss [][]byte) {
	psBuf := annexB4To3(annexB)
	return hevc.GetParameterSetsFromByteStream(psBuf)
}
