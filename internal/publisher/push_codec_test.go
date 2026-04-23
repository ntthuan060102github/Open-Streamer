package publisher

import (
	"testing"

	"github.com/q191201771/lal/pkg/base"
	"github.com/stretchr/testify/require"
)

// Composition time field is the whole point of the new adapter — replaces
// lal's AvPacket2RtmpRemuxer which always sends 0 and breaks B-frame
// playback. These tests pin the wire byte layout so future refactors don't
// silently regress motion smoothness.

func TestBuildAVCNaluTag_KeyframeWithCompositionTime(t *testing.T) {
	t.Parallel()
	// IDR slice = 5 bytes of dummy NALU body, ctsMs=40 → composition_time bytes = 00 00 28
	nal := []byte{0x65, 0xAA, 0xBB, 0xCC, 0xDD} // 0x65 = IDR slice (NAL type 5)
	got := buildAVCNaluTag([][]byte{nal}, true, 40)

	require.Equal(t, byte(base.RtmpAvcKeyFrame), got[0], "key frame + AVC")
	require.Equal(t, byte(base.RtmpAvcPacketTypeNalu), got[1], "packet type = NALU")
	require.Equal(t, []byte{0x00, 0x00, 0x28}, got[2:5], "composition_time = 40ms big-endian int24")
	require.Equal(t, []byte{0x00, 0x00, 0x00, 0x05}, got[5:9], "NALU length prefix = 5")
	require.Equal(t, nal, got[9:14], "NALU body verbatim")
}

func TestBuildAVCNaluTag_InterframeWithZeroCTS(t *testing.T) {
	t.Parallel()
	// Non-IDR P-slice; cts=0 → composition_time bytes = 00 00 00
	nal := []byte{0x41, 0x11}
	got := buildAVCNaluTag([][]byte{nal}, false, 0)

	require.Equal(t, byte(base.RtmpAvcInterFrame), got[0])
	require.Equal(t, byte(0x00), got[2])
	require.Equal(t, byte(0x00), got[3])
	require.Equal(t, byte(0x00), got[4])
}

// Multi-NALU access unit (rare in our pipeline but valid in spec) — each
// NALU gets its own length prefix concatenated after the 5-byte tag header.
func TestBuildAVCNaluTag_MultiNALU(t *testing.T) {
	t.Parallel()
	nal1 := []byte{0x41, 0xAA}
	nal2 := []byte{0x41, 0xBB, 0xCC}
	got := buildAVCNaluTag([][]byte{nal1, nal2}, false, 0)

	// header(5) + len1(4) + nal1(2) + len2(4) + nal2(3) = 18
	require.Len(t, got, 18)
	require.Equal(t, []byte{0x00, 0x00, 0x00, 0x02}, got[5:9], "first NALU length")
	require.Equal(t, nal1, got[9:11])
	require.Equal(t, []byte{0x00, 0x00, 0x00, 0x03}, got[11:15], "second NALU length")
	require.Equal(t, nal2, got[15:18])
}

// FeedH264 must:
//   - drop AUD NALUs (NAL type 9) — FLV doesn't carry them
//   - cache SPS/PPS, emit AVC sequence header before the first slice tag
//   - emit metadata once before the first media tag
func TestFeedH264_EmitsMetadataThenSeqHeaderThenSlice(t *testing.T) {
	t.Parallel()

	var msgs []base.RtmpMsg
	a := newPushCodecAdapter(func(m base.RtmpMsg) error {
		msgs = append(msgs, m)
		return nil
	})

	// Construct an Annex-B IDR access unit: SPS + PPS + IDR.
	// SPS: NAL type 7, minimal valid bytes for ParseSps to succeed
	sps := []byte{0x67, 0x42, 0xC0, 0x1E, 0xDA, 0x02, 0x80, 0xBF, 0xE5, 0x00}
	pps := []byte{0x68, 0xCE, 0x06, 0xCB}
	idr := []byte{0x65, 0x88, 0x80, 0x10, 0x00}

	annexB := append([]byte{0, 0, 0, 1}, sps...)
	annexB = append(annexB, 0, 0, 0, 1)
	annexB = append(annexB, pps...)
	annexB = append(annexB, 0, 0, 0, 1)
	annexB = append(annexB, idr...)

	require.NoError(t, a.FeedH264(annexB, 100, 100))

	require.GreaterOrEqual(t, len(msgs), 3, "metadata + seq header + slice tag")
	require.Equal(t, byte(base.RtmpTypeIdMetadata), msgs[0].Header.MsgTypeId, "metadata first")

	// Find the AVC sequence header — it has packet_type=0 in payload[1].
	seqIdx, sliceIdx := -1, -1
	for i, m := range msgs {
		if m.Header.MsgTypeId != base.RtmpTypeIdVideo {
			continue
		}
		if len(m.Payload) >= 2 && m.Payload[1] == base.RtmpAvcPacketTypeSeqHeader {
			seqIdx = i
		} else if len(m.Payload) >= 2 && m.Payload[1] == base.RtmpAvcPacketTypeNalu {
			sliceIdx = i
		}
	}
	require.NotEqual(t, -1, seqIdx, "sequence header emitted")
	require.NotEqual(t, -1, sliceIdx, "slice tag emitted")
	require.Less(t, seqIdx, sliceIdx, "sequence header must precede first slice")
}

// AUD (NAL type 9) must be dropped — FLV's video tag carries one access
// unit's slice NALUs only, no delimiter wrappers. If we forwarded AUD,
// receivers would reject the stream as malformed.
func TestFeedH264_DropsAUD(t *testing.T) {
	t.Parallel()
	var msgs []base.RtmpMsg
	a := newPushCodecAdapter(func(m base.RtmpMsg) error {
		msgs = append(msgs, m)
		return nil
	})

	aud := []byte{0x09, 0xF0} // NAL type 9 = AUD
	annexB := append([]byte{0, 0, 0, 1}, aud...)
	require.NoError(t, a.FeedH264(annexB, 0, 0))

	for _, m := range msgs {
		if m.Header.MsgTypeId != base.RtmpTypeIdVideo {
			continue
		}
		require.NotEqual(t, byte(base.RtmpAvcPacketTypeNalu), m.Payload[1],
			"no slice tag emitted from AUD-only frame")
	}
}

// SPS or PPS change mid-stream must trigger seq-header re-emission, so
// receivers stay in sync when the encoder reconfigures (resolution change,
// failover to a source with different codec params).
func TestFeedH264_ResetsSeqHeaderOnSPSChange(t *testing.T) {
	t.Parallel()
	var msgs []base.RtmpMsg
	a := newPushCodecAdapter(func(m base.RtmpMsg) error {
		msgs = append(msgs, m)
		return nil
	})

	sps1 := []byte{0x67, 0x42, 0xC0, 0x1E, 0xDA, 0x02, 0x80, 0xBF, 0xE5, 0x00}
	sps2 := []byte{0x67, 0x42, 0xC0, 0x1F, 0xDA, 0x02, 0x80, 0xBF, 0xE5, 0x00} // differs by 1 byte
	pps := []byte{0x68, 0xCE, 0x06, 0xCB}
	idr := []byte{0x65, 0x88, 0x80, 0x10, 0x00}

	build := func(sps []byte) []byte {
		b := append([]byte{0, 0, 0, 1}, sps...)
		b = append(b, 0, 0, 0, 1)
		b = append(b, pps...)
		b = append(b, 0, 0, 0, 1)
		b = append(b, idr...)
		return b
	}

	require.NoError(t, a.FeedH264(build(sps1), 0, 0))
	firstCount := countSeqHeaders(msgs)
	require.NoError(t, a.FeedH264(build(sps2), 33, 33))
	require.Greater(t, countSeqHeaders(msgs), firstCount,
		"second seq header must emit when SPS changes")
}

// FeedAAC must:
//   - drop frames shorter than the ADTS header
//   - emit the audio sequence header (AudioSpecificConfig) before any raw frame
//   - strip the 7-byte ADTS header from each raw frame
func TestFeedAAC_StripsADTSAndEmitsSeqHeaderFirst(t *testing.T) {
	t.Parallel()
	var msgs []base.RtmpMsg
	a := newPushCodecAdapter(func(m base.RtmpMsg) error {
		msgs = append(msgs, m)
		return nil
	})

	// Minimal ADTS header (sync word + AAC LC, 44.1kHz stereo) + 4 bytes raw payload.
	adts := []byte{
		0xFF, 0xF1, // sync + MPEG-4 + no CRC
		0x50, 0x80, 0x01, 0x7F, 0xFC, // profile=AAC LC, sample rate idx=4 (44.1k), channels=2
		0xDE, 0xAD, 0xBE, 0xEF, // raw AAC payload
	}
	require.NoError(t, a.FeedAAC(adts, 0))

	// First-ever emit: metadata, then audio seq header (packet_type=0), then raw.
	seqIdx, rawIdx := -1, -1
	for i, m := range msgs {
		if m.Header.MsgTypeId != base.RtmpTypeIdAudio {
			continue
		}
		switch m.Payload[1] {
		case base.RtmpAacPacketTypeSeqHeader:
			seqIdx = i
		case base.RtmpAacPacketTypeRaw:
			rawIdx = i
			require.Equal(t, []byte{0xDE, 0xAD, 0xBE, 0xEF}, m.Payload[2:],
				"raw AAC payload after stripping 7-byte ADTS")
			require.Equal(t, byte(0xAF), m.Payload[0], "audio tag header byte")
		}
	}
	require.NotEqual(t, -1, seqIdx, "AAC seq header must emit")
	require.NotEqual(t, -1, rawIdx, "raw AAC frame must emit")
	require.Less(t, seqIdx, rawIdx, "seq header before raw frame")
}

func TestFeedAAC_DropsTooShort(t *testing.T) {
	t.Parallel()
	var msgs []base.RtmpMsg
	a := newPushCodecAdapter(func(m base.RtmpMsg) error {
		msgs = append(msgs, m)
		return nil
	})
	require.NoError(t, a.FeedAAC([]byte{0xFF, 0xF1}, 0))
	require.Empty(t, msgs, "frame shorter than ADTS header dropped silently")
}

// Composition time bigger than int24 max would overflow into the FrameType
// nibble; the cap clamps before that bit corruption can happen.
func TestBuildAVCNaluTag_CompositionTimeCapped(t *testing.T) {
	t.Parallel()
	nal := []byte{0x41, 0x11}
	// Pass max int32 → must clamp to 0x7fffff.
	got := buildAVCNaluTag([][]byte{nal}, false, 0x7fffff)
	require.Equal(t, []byte{0x7f, 0xff, 0xff}, got[2:5], "max int24 cts")
	require.Equal(t, byte(base.RtmpAvcInterFrame), got[0],
		"FrameType nibble untouched by max cts")
}

func countSeqHeaders(msgs []base.RtmpMsg) int {
	n := 0
	for _, m := range msgs {
		if m.Header.MsgTypeId == base.RtmpTypeIdVideo &&
			len(m.Payload) >= 2 &&
			m.Payload[1] == base.RtmpAvcPacketTypeSeqHeader {
			n++
		}
	}
	return n
}
