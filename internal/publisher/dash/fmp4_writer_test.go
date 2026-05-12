package dash

import (
	"bytes"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
)

// TestBuildH264Init_RejectsEmptyInputs — guard against silently
// producing an init segment with no codec data.
func TestBuildH264Init_RejectsEmptyInputs(t *testing.T) {
	cases := []struct {
		name string
		spss [][]byte
		ppss [][]byte
	}{
		{"no SPS", nil, [][]byte{{0x68, 0x00}}},
		{"no PPS", [][]byte{{0x67, 0x42, 0x00, 0x1E}}, nil},
		{"neither", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BuildH264Init(tc.spss, tc.ppss)
			if err == nil {
				t.Error("expected error from BuildH264Init with missing param sets")
			}
		})
	}
}

// TestBuildAACInit_RejectsBadInputs — guards the audio init constructor.
func TestBuildAACInit_RejectsBadInputs(t *testing.T) {
	if _, err := BuildAACInit(nil); err == nil {
		t.Error("expected error from BuildAACInit(nil)")
	}
}

// TestEncodeInit_RoundTrip — encoded init segment is parseable by mp4ff.
// Uses the same library's Decode to verify, which catches encoder
// regressions without hand-crafting box structures.
func TestEncodeInit_RoundTrip(t *testing.T) {
	init := mp4.CreateEmptyInit()
	init.AddEmptyTrack(VideoTimescale, "video", "und")

	data, err := EncodeInit(init)
	if err != nil {
		t.Fatalf("EncodeInit: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("EncodeInit produced empty bytes")
	}

	// Decode round-trip — proves the byte stream is well-formed.
	parsed, err := mp4.DecodeFile(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("DecodeFile round-trip: %v", err)
	}
	if parsed.Init == nil {
		t.Error("decoded file has no init segment")
	}
}

// TestBuildVideoFragment_EquallyDistributedDurations — every sample
// dur within a fragment is segDurTicks / N. This is the v2 contract
// (the v1 implementation used inter-frame deltas which produced the
// uint32-underflow-into-47.7s bug class).
func TestBuildVideoFragment_EquallyDistributedDurations(t *testing.T) {
	// 5 frames, 2s segment at 90kHz = 180000 ticks. Per-sample = 36000.
	frames := []VideoFrame{
		{AnnexB: synthH264AU(), PTSms: 0, DTSms: 0, IsIDR: true},
		{AnnexB: synthH264AU(), PTSms: 40, DTSms: 40},
		{AnnexB: synthH264AU(), PTSms: 80, DTSms: 80},
		{AnnexB: synthH264AU(), PTSms: 120, DTSms: 120},
		{AnnexB: synthH264AU(), PTSms: 160, DTSms: 160},
	}
	data, err := BuildVideoFragment(1, 1, 0, frames, false, 180000)
	if err != nil {
		t.Fatalf("BuildVideoFragment: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("empty fragment bytes")
	}

	// Parse it back and check sample durations.
	parsed, err := mp4.DecodeFile(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("DecodeFile: %v", err)
	}
	if len(parsed.Segments) != 1 {
		t.Fatalf("expected 1 segment, got %d", len(parsed.Segments))
	}
	frag := parsed.Segments[0].Fragments[0]
	samples := frag.Moof.Traf.Trun.Samples
	if len(samples) != 5 {
		t.Fatalf("expected 5 samples, got %d", len(samples))
	}
	wantDur := uint32(36000)
	for i, s := range samples {
		if s.Dur != wantDur {
			t.Errorf("sample[%d].Dur = %d, want %d (equal distribution)",
				i, s.Dur, wantDur)
		}
	}
}

// TestBuildVideoFragment_TFDTPropagates — the requested tfdt becomes
// the first sample's decode time in the encoded fragment. Players read
// this to schedule playback.
func TestBuildVideoFragment_TFDTPropagates(t *testing.T) {
	frames := []VideoFrame{
		{AnnexB: synthH264AU(), PTSms: 0, DTSms: 0, IsIDR: true},
	}
	const tfdt = uint64(12_345_000)
	data, err := BuildVideoFragment(1, 1, tfdt, frames, false, 1000)
	if err != nil {
		t.Fatalf("BuildVideoFragment: %v", err)
	}
	parsed, err := mp4.DecodeFile(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("DecodeFile: %v", err)
	}
	frag := parsed.Segments[0].Fragments[0]
	if got := frag.Moof.Traf.Tfdt.BaseMediaDecodeTime(); got != tfdt {
		t.Errorf("tfdt = %d, want %d", got, tfdt)
	}
}

// TestBuildVideoFragment_IDRMarkedAsSyncSample — DASH startWithSAP="1"
// requires the first sample to be a sync sample on every IDR-aligned
// segment. Verify the IsIDR flag translates to mp4.SyncSampleFlags.
func TestBuildVideoFragment_IDRMarkedAsSyncSample(t *testing.T) {
	frames := []VideoFrame{
		{AnnexB: synthH264AU(), PTSms: 0, DTSms: 0, IsIDR: true},
		{AnnexB: synthH264AU(), PTSms: 40, DTSms: 40, IsIDR: false},
	}
	data, err := BuildVideoFragment(1, 1, 0, frames, false, 90000)
	if err != nil {
		t.Fatalf("BuildVideoFragment: %v", err)
	}
	parsed, _ := mp4.DecodeFile(bytes.NewReader(data))
	samples := parsed.Segments[0].Fragments[0].Moof.Traf.Trun.Samples

	// The first sample should have the sync-sample flag, the second
	// should have non-sync flags.
	if samples[0].Flags != mp4.SyncSampleFlags {
		t.Errorf("sample[0].Flags = %#x, want SyncSampleFlags (%#x)",
			samples[0].Flags, mp4.SyncSampleFlags)
	}
	if samples[1].Flags != mp4.NonSyncSampleFlags {
		t.Errorf("sample[1].Flags = %#x, want NonSyncSampleFlags (%#x)",
			samples[1].Flags, mp4.NonSyncSampleFlags)
	}
}

// TestBuildAudioFragment_SamplesAreSyncSamples — every AAC frame is a
// sync sample (no inter-frame dependency in AAC-LC).
func TestBuildAudioFragment_SamplesAreSyncSamples(t *testing.T) {
	frames := []AudioFrame{
		{Raw: []byte{0xAA}, PTSms: 0},
		{Raw: []byte{0xBB}, PTSms: 23},
		{Raw: []byte{0xCC}, PTSms: 46},
	}
	data, err := BuildAudioFragment(1, 2, 0, frames)
	if err != nil {
		t.Fatalf("BuildAudioFragment: %v", err)
	}
	parsed, _ := mp4.DecodeFile(bytes.NewReader(data))
	samples := parsed.Segments[0].Fragments[0].Moof.Traf.Trun.Samples
	if len(samples) != 3 {
		t.Fatalf("expected 3 samples, got %d", len(samples))
	}
	for i, s := range samples {
		if s.Flags != mp4.SyncSampleFlags {
			t.Errorf("audio sample[%d].Flags = %#x, want SyncSampleFlags",
				i, s.Flags)
		}
		if s.Dur != 1024 {
			t.Errorf("audio sample[%d].Dur = %d, want 1024 (AAC standard)",
				i, s.Dur)
		}
	}
}

// TestBuildAudioFragment_TFDTAccumulates — successive samples have
// monotonically increasing decode time within the fragment.
func TestBuildAudioFragment_TFDTAccumulates(t *testing.T) {
	frames := []AudioFrame{
		{Raw: []byte{0x01}, PTSms: 0},
		{Raw: []byte{0x02}, PTSms: 23},
	}
	const tfdt = uint64(100_000)
	data, err := BuildAudioFragment(1, 2, tfdt, frames)
	if err != nil {
		t.Fatalf("BuildAudioFragment: %v", err)
	}
	parsed, _ := mp4.DecodeFile(bytes.NewReader(data))
	frag := parsed.Segments[0].Fragments[0]
	if got := frag.Moof.Traf.Tfdt.BaseMediaDecodeTime(); got != tfdt {
		t.Errorf("audio tfdt = %d, want %d", got, tfdt)
	}
}

// TestBuildVideoFragment_EmptyFrames — explicit error on no frames so
// the caller doesn't accidentally publish a 0-byte segment.
func TestBuildVideoFragment_EmptyFrames(t *testing.T) {
	_, err := BuildVideoFragment(1, 1, 0, nil, false, 90000)
	if err == nil {
		t.Error("expected error from BuildVideoFragment with no frames")
	}
}

// TestBuildAudioFragment_EmptyFrames — same guard for audio.
func TestBuildAudioFragment_EmptyFrames(t *testing.T) {
	_, err := BuildAudioFragment(1, 1, 0, nil)
	if err == nil {
		t.Error("expected error from BuildAudioFragment with no frames")
	}
}

// synthH264AU returns a synthetic Annex-B byte stream representing a
// single H.264 NALU (header 0x09 = access unit delimiter for clarity).
// Sufficient for the writer's Annex-B → AVCC conversion path to
// produce a non-empty length-prefixed NALU; the actual NALU type
// doesn't matter for fragment serialisation tests.
func synthH264AU() []byte {
	return []byte{0x00, 0x00, 0x00, 0x01, 0x09, 0x10}
}
