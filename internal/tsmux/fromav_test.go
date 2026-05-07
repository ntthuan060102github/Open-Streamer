package tsmux

import (
	"testing"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

func TestWriteNilOrEmptyIsNoOp(t *testing.T) {
	f := NewFromAV()
	called := false
	f.Write(nil, func(_ []byte) { called = true })
	f.Write(&domain.AVPacket{}, func(_ []byte) { called = true })
	f.Write(&domain.AVPacket{Codec: domain.AVCodecH264, Data: []byte{0xAA}}, nil)
	if called {
		t.Fatal("emit must not be called for nil/empty inputs")
	}
}

func TestWriteUnknownCodecIsNoOp(t *testing.T) {
	f := NewFromAV()
	called := false
	f.Write(
		&domain.AVPacket{Codec: domain.AVCodecUnknown, Data: []byte{0x01, 0x02}},
		func(_ []byte) { called = true },
	)
	if called {
		t.Fatal("unknown codec must not emit")
	}
}

func TestWriteH264EmitsTSPackets(t *testing.T) {
	f := NewFromAV()
	// Minimal AVCC -> Annex-B doesn't matter for the muxer; gomedia accepts whatever bytes.
	annexB := []byte{
		0x00, 0x00, 0x00, 0x01, 0x67, 0x42, 0x00, 0x1f, // SPS
		0x00, 0x00, 0x00, 0x01, 0x68, 0xce, 0x38, 0x80, // PPS
		0x00, 0x00, 0x00, 0x01, 0x65, 0x88, 0x84, 0x00, // IDR
	}
	var emitted [][]byte
	f.Write(
		&domain.AVPacket{Codec: domain.AVCodecH264, Data: annexB, PTSms: 100, DTSms: 100, KeyFrame: true},
		func(b []byte) { emitted = append(emitted, append([]byte(nil), b...)) },
	)
	if len(emitted) == 0 {
		t.Fatal("expected H.264 muxer to produce TS packets")
	}
	for i, p := range emitted {
		if len(p) != 188 {
			t.Errorf("pkt %d not 188 bytes: %d", i, len(p))
		}
		if p[0] != 0x47 {
			t.Errorf("pkt %d bad sync: %x", i, p[0])
		}
	}
}

func TestWriteAACEmitsTSPackets(t *testing.T) {
	f := NewFromAV()
	// 7-byte ADTS header + dummy AAC payload.
	aac := []byte{0xFF, 0xF1, 0x4C, 0x80, 0x01, 0x1F, 0xFC, 0x21, 0x00, 0x00}
	var n int
	f.Write(
		&domain.AVPacket{Codec: domain.AVCodecAAC, Data: aac, PTSms: 50},
		func(_ []byte) { n++ },
	)
	if n == 0 {
		t.Fatal("expected AAC muxer to emit TS packets")
	}
}

func TestWriteH265EmitsTSPackets(t *testing.T) {
	f := NewFromAV()
	hevc := []byte{
		0x00, 0x00, 0x00, 0x01, 0x40, 0x01, 0x0c, 0x01, // VPS
		0x00, 0x00, 0x00, 0x01, 0x42, 0x01, 0x01, 0x01, // SPS
		0x00, 0x00, 0x00, 0x01, 0x26, 0x01, 0xAB, // IDR slice (NAL type 19)
	}
	var n int
	f.Write(
		&domain.AVPacket{Codec: domain.AVCodecH265, Data: hevc, PTSms: 200, DTSms: 200, KeyFrame: true},
		func(_ []byte) { n++ },
	)
	if n == 0 {
		t.Fatal("expected H.265 muxer to emit TS packets")
	}
}

func TestKeyFrameH264(t *testing.T) {
	idr := []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x88, 0x84, 0x00}
	nonIDR := []byte{0x00, 0x00, 0x00, 0x01, 0x41, 0xAB, 0xCD}

	if !KeyFrameH264(idr) {
		t.Error("IDR slice should be detected as keyframe")
	}
	if KeyFrameH264(nonIDR) {
		t.Error("non-IDR slice should not be a keyframe")
	}
}

func TestKeyFrameH265(t *testing.T) {
	// IDR_W_RADL has nal_unit_type=19. Annex-B start code + nal_unit_header.
	idr := []byte{0x00, 0x00, 0x00, 0x01, 0x26, 0x01, 0xAB}
	nonIDR := []byte{0x00, 0x00, 0x00, 0x01, 0x02, 0x01, 0x00}

	if !KeyFrameH265(idr) {
		t.Error("IDR_W_RADL should be detected as keyframe")
	}
	if KeyFrameH265(nonIDR) {
		t.Error("trailing slice should not be a keyframe")
	}
}

func TestFeedWirePacketTSPassthrough(t *testing.T) {
	var got []byte
	var mux *FromAV
	FeedWirePacket(
		[]byte{0x47, 0x01, 0x02, 0x03},
		nil,
		&mux,
		func(b []byte) { got = append(got, b...) },
	)
	if mux != nil {
		t.Error("mux must not be allocated for raw TS path")
	}
	if len(got) != 4 || got[0] != 0x47 {
		t.Fatalf("TS not forwarded verbatim: %v", got)
	}
}

func TestFeedWirePacketAVAllocatesMuxLazily(t *testing.T) {
	var n int
	var mux *FromAV

	annexB := []byte{
		0x00, 0x00, 0x00, 0x01, 0x67, 0x42, 0x00, 0x1f,
		0x00, 0x00, 0x00, 0x01, 0x68, 0xce, 0x38, 0x80,
		0x00, 0x00, 0x00, 0x01, 0x65, 0x88, 0x84, 0x00,
	}
	FeedWirePacket(
		nil,
		&domain.AVPacket{Codec: domain.AVCodecH264, Data: annexB, PTSms: 1, DTSms: 1, KeyFrame: true},
		&mux,
		func(_ []byte) { n++ },
	)
	if mux == nil {
		t.Fatal("mux should be allocated on first AV packet")
	}
	if n == 0 {
		t.Fatal("expected emit calls from muxed AV packet")
	}

	saved := mux
	FeedWirePacket(
		nil,
		&domain.AVPacket{Codec: domain.AVCodecH264, Data: annexB, PTSms: 2, DTSms: 2, KeyFrame: true},
		&mux,
		func(_ []byte) {},
	)
	if mux != saved {
		t.Error("mux must be reused, not reallocated")
	}
}

// MP2 audio frames muxed via FromAV must produce TS packets advertising
// stream_type 0x04 in PMT — that is the canonical MPEG-2 audio stream type
// players (ffmpeg, VLC, browser via hls.js) expect for MPEG Audio Layer II.
// Without the AVCodecMP2 case in fromav.go the muxer would silently drop
// audio for DVB radio sources mixed via mixer://.
func TestFromAV_MuxesMP2Audio(t *testing.T) {
	var ts []byte
	var mux *FromAV

	// Synthetic MP2 frame: just enough bytes for the muxer to wrap into a
	// PES packet. Real frames start with 0xFF F? sync but the muxer is
	// codec-agnostic at this layer.
	mp2Frame := []byte{0xFF, 0xFD, 0x40, 0x04, 0xCA, 0xFE, 0xBA, 0xBE, 0xDE, 0xAD}

	FeedWirePacket(
		nil,
		&domain.AVPacket{Codec: domain.AVCodecMP2, Data: mp2Frame, PTSms: 100, DTSms: 100},
		&mux,
		func(b []byte) { ts = append(ts, b...) },
	)

	if mux == nil {
		t.Fatal("mux should be allocated on first MP2 packet")
	}
	if len(ts) == 0 {
		t.Fatal("expected TS bytes emitted for MP2 packet")
	}
	if len(ts)%188 != 0 {
		t.Fatalf("emitted bytes must be 188-aligned, got %d", len(ts))
	}
	if ts[0] != 0x47 {
		t.Fatalf("first byte must be MPEG-TS sync 0x47, got 0x%02X", ts[0])
	}

	// Second MP2 frame must reuse the existing audio PID without reallocating
	// the muxer or re-adding the stream — same property the AAC path has.
	saved := mux
	FeedWirePacket(
		nil,
		&domain.AVPacket{Codec: domain.AVCodecMP2, Data: mp2Frame, PTSms: 124, DTSms: 124},
		&mux,
		func(_ []byte) {},
	)
	if mux != saved {
		t.Error("mux must be reused for subsequent MP2 packets")
	}
}

func TestFeedWirePacketNilInputs(t *testing.T) {
	var mux *FromAV
	called := false
	FeedWirePacket(nil, nil, &mux, func(_ []byte) { called = true })
	FeedWirePacket(nil, &domain.AVPacket{}, &mux, func(_ []byte) { called = true })
	FeedWirePacket(nil, &domain.AVPacket{Codec: domain.AVCodecH264, Data: []byte{1}}, &mux, nil)
	if called {
		t.Fatal("emit must not be called for empty inputs")
	}
	if mux != nil {
		t.Fatal("mux must not be allocated for empty AV packet")
	}
}

// FindH264ParameterSets must locate SPS (NAL type 7) + PPS (NAL type 8)
// inside an Annex-B byte stream. Both 4-byte and 3-byte start codes are
// in production use (the muxer used by gomedia outputs 4-byte; some
// embedded encoders / RTSP sources emit 3-byte).
func TestFindH264ParameterSetsBothPresent(t *testing.T) {
	// Realistic 1080p Main@4.0 SPS + a typical Main-profile PPS.
	sps := []byte{0x67, 0x4d, 0x40, 0x28, 0xeb, 0x05, 0x07, 0x80, 0x44, 0x00}
	pps := []byte{0x68, 0xee, 0x3c, 0x80}
	idr := []byte{0x65, 0x88, 0x80, 0x40, 0x00, 0x00, 0x00}

	startCode := []byte{0, 0, 0, 1}
	stream := make([]byte, 0, 64)
	stream = append(stream, startCode...)
	stream = append(stream, sps...)
	stream = append(stream, startCode...)
	stream = append(stream, pps...)
	stream = append(stream, startCode...)
	stream = append(stream, idr...)

	gotSPS, gotPPS := FindH264ParameterSets(stream)
	if gotSPS == nil {
		t.Fatal("SPS not found")
	}
	if gotPPS == nil {
		t.Fatal("PPS not found")
	}
	if gotSPS[0]&0x1F != 7 {
		t.Errorf("first byte of returned SPS is not nal_unit_type 7: 0x%x", gotSPS[0])
	}
	if gotPPS[0]&0x1F != 8 {
		t.Errorf("first byte of returned PPS is not nal_unit_type 8: 0x%x", gotPPS[0])
	}
}

// 3-byte Annex-B start codes (00 00 01) are valid and used by some
// encoders. The scanner must accept them.
func TestFindH264ParameterSets3ByteStartCode(t *testing.T) {
	sps := []byte{0x67, 0x42, 0xc0, 0x1e}
	pps := []byte{0x68, 0xce, 0x38, 0x80}

	startCode := []byte{0, 0, 1}
	stream := append([]byte{}, startCode...)
	stream = append(stream, sps...)
	stream = append(stream, startCode...)
	stream = append(stream, pps...)

	gotSPS, gotPPS := FindH264ParameterSets(stream)
	if gotSPS == nil || gotSPS[0]&0x1F != 7 {
		t.Errorf("SPS missing/wrong type: %v", gotSPS)
	}
	if gotPPS == nil || gotPPS[0]&0x1F != 8 {
		t.Errorf("PPS missing/wrong type: %v", gotPPS)
	}
}

// SPS without a trailing NAL still returns the SPS body up to end of
// buffer. (PPS missing returns nil for PPS — caller handles.)
func TestFindH264ParameterSetsSPSOnlyAtEnd(t *testing.T) {
	sps := []byte{0x67, 0x42, 0xc0, 0x1e, 0xff, 0xee}
	stream := append([]byte{0, 0, 0, 1}, sps...)

	gotSPS, gotPPS := FindH264ParameterSets(stream)
	if gotSPS == nil {
		t.Fatal("SPS not found")
	}
	if len(gotSPS) != len(sps) {
		t.Errorf("SPS body length = %d, want %d", len(gotSPS), len(sps))
	}
	if gotPPS != nil {
		t.Errorf("PPS should be nil when not present, got %v", gotPPS)
	}
}

// Bytes containing no NAL units at all return both nil — the caller's
// "wait until both cached" loop then keeps scanning subsequent chunks.
func TestFindH264ParameterSetsAbsent(t *testing.T) {
	junk := []byte{0xde, 0xad, 0xbe, 0xef, 0x47, 0x40, 0x00, 0x00}
	gotSPS, gotPPS := FindH264ParameterSets(junk)
	if gotSPS != nil || gotPPS != nil {
		t.Errorf("non-NAL bytes must not yield SPS/PPS, got SPS=%v PPS=%v", gotSPS, gotPPS)
	}
}

// The returned slices must be COPIES of the matched bytes — caller may
// retain them long-term while the input buffer is reused.
func TestFindH264ParameterSetsReturnsCopies(t *testing.T) {
	sps := []byte{0x67, 0x42, 0xc0, 0x1e}
	pps := []byte{0x68, 0xce, 0x38, 0x80}
	stream := make([]byte, 0, 4+len(sps)+4+len(pps))
	stream = append(stream, 0, 0, 0, 1)
	stream = append(stream, sps...)
	stream = append(stream, 0, 0, 0, 1)
	stream = append(stream, pps...)

	gotSPS, gotPPS := FindH264ParameterSets(stream)
	// Mutate the input — returned slices must be unaffected.
	for i := range stream {
		stream[i] = 0xAA
	}
	if gotSPS[0] != 0x67 {
		t.Errorf("SPS was a slice into input; expected copy, got first byte 0x%x", gotSPS[0])
	}
	if gotPPS[0] != 0x68 {
		t.Errorf("PPS was a slice into input; expected copy, got first byte 0x%x", gotPPS[0])
	}
}
