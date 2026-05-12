package tsnorm_test

import (
	"bytes"
	"testing"

	gompeg2 "github.com/yapingcat/gomedia/go-mpeg2"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/ingestor/tsnorm"
	"github.com/ntt0601zcoder/open-streamer/internal/timeline"
	"github.com/ntt0601zcoder/open-streamer/internal/tsmux"
)

// TestProcess_EmptyChunkReturnsEmpty — defensive zero-input case.
func TestProcess_EmptyChunkReturnsEmpty(t *testing.T) {
	n := tsnorm.New(timeline.DefaultConfig())
	out, err := n.Process(nil)
	if err != nil {
		t.Fatalf("Process(nil) err = %v", err)
	}
	if len(out) != 0 {
		t.Errorf("Process(nil) returned %d bytes, want 0", len(out))
	}
}

// TestProcess_PreservesH264KeyframePayload — feeds one synthetic H.264
// IDR through, verifies the remuxed output demuxes back to the same
// payload (so the roundtrip is lossless for media bytes). PTS/DTS may
// have been wallclock-anchored by the Normaliser, so we don't assert
// exact values — just that the PES survives intact.
func TestProcess_PreservesH264KeyframePayload(t *testing.T) {
	// Build a single-IDR H.264 AVPacket and mux it into TS bytes.
	idrPayload := buildH264IDR()
	srcMuxer := tsmux.NewFromAV()
	var srcBuf bytes.Buffer
	srcMuxer.Write(&domain.AVPacket{
		Codec:    domain.AVCodecH264,
		Data:     idrPayload,
		PTSms:    1000,
		DTSms:    1000,
		KeyFrame: true,
	}, func(b []byte) { srcBuf.Write(b) })
	if srcBuf.Len() == 0 {
		t.Fatal("source muxer produced no TS bytes")
	}

	n := tsnorm.New(timeline.DefaultConfig())
	out, err := n.Process(srcBuf.Bytes())
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("Process returned no bytes for non-empty chunk")
	}

	// Demux the output and ensure the IDR payload survives.
	demux := gompeg2.NewTSDemuxer()
	var gotPayloads [][]byte
	demux.OnFrame = func(cid gompeg2.TS_STREAM_TYPE, frame []byte, _, _ uint64) {
		if cid == gompeg2.TS_STREAM_H264 {
			gotPayloads = append(gotPayloads, bytes.Clone(frame))
		}
	}
	if err := demux.Input(bytes.NewReader(out)); err != nil {
		t.Fatalf("output demux: %v", err)
	}
	if len(gotPayloads) == 0 {
		t.Fatal("output demux yielded no H.264 frames — roundtrip lost the payload")
	}
	if !bytes.Equal(gotPayloads[0], idrPayload) {
		t.Errorf("roundtrip mutated payload\n  got:  %x\n  want: %x", gotPayloads[0], idrPayload)
	}
}

// TestProcess_AnchorsPTSToWallclock — with Enabled=true Normaliser, the
// first H.264 frame's PTS should land NEAR 0 ms (wallclock-since-
// firstSeen), not at the upstream-encoded 100_000 ms. Subsequent
// frames preserve inter-frame deltas.
func TestProcess_AnchorsPTSToWallclock(t *testing.T) {
	cfg := timeline.DefaultConfig()
	n := tsnorm.New(cfg)

	// Two H.264 frames spaced 40 ms apart in source PTS, but starting
	// far in the future (100 s in upstream time). The Normaliser should
	// anchor frame 1 at ~0 ms and frame 2 at ~40 ms in output PTS.
	srcMuxer := tsmux.NewFromAV()
	var srcBuf bytes.Buffer
	for i, srcPTS := range []uint64{100_000, 100_040} {
		isKey := i == 0
		payload := buildH264IDR()
		if !isKey {
			payload = buildH264NonIDR()
		}
		srcMuxer.Write(&domain.AVPacket{
			Codec:    domain.AVCodecH264,
			Data:     payload,
			PTSms:    srcPTS,
			DTSms:    srcPTS,
			KeyFrame: isKey,
		}, func(b []byte) { srcBuf.Write(b) })
	}

	out, err := n.Process(srcBuf.Bytes())
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	demux := gompeg2.NewTSDemuxer()
	var ptsValues []uint64
	demux.OnFrame = func(cid gompeg2.TS_STREAM_TYPE, _ []byte, pts, _ uint64) {
		if cid == gompeg2.TS_STREAM_H264 {
			ptsValues = append(ptsValues, pts)
		}
	}
	if err := demux.Input(bytes.NewReader(out)); err != nil {
		t.Fatalf("output demux: %v", err)
	}
	if len(ptsValues) < 2 {
		t.Fatalf("expected 2 H.264 frames after roundtrip, got %d", len(ptsValues))
	}

	// Frame 0 should be near 0 (wallclock-seeded). Allow some slack
	// because timeline.Normaliser uses time.Now under the hood.
	if ptsValues[0] > 1000 {
		t.Errorf("frame[0].PTS = %d ms, expected near 0 (wallclock-anchored)", ptsValues[0])
	}
	// Inter-frame delta should be ~40 ms (preserved from source).
	delta := int64(ptsValues[1]) - int64(ptsValues[0])
	if delta < 30 || delta > 50 {
		t.Errorf("frame[1]−frame[0] PTS delta = %d ms, expected ~40 ms", delta)
	}
}

// TestProcess_DisabledConfigPassesThrough — when timeline.Config.Enabled
// is false the Normaliser is a no-op so the roundtrip preserves source
// PTS values exactly. Useful sanity check before exercising the active
// path.
func TestProcess_DisabledConfigPassesThrough(t *testing.T) {
	n := tsnorm.New(timeline.Config{}) // Enabled=false zero value
	srcMuxer := tsmux.NewFromAV()
	var srcBuf bytes.Buffer
	srcMuxer.Write(&domain.AVPacket{
		Codec:    domain.AVCodecH264,
		Data:     buildH264IDR(),
		PTSms:    12_345,
		DTSms:    12_345,
		KeyFrame: true,
	}, func(b []byte) { srcBuf.Write(b) })

	out, err := n.Process(srcBuf.Bytes())
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	demux := gompeg2.NewTSDemuxer()
	var ptsValues []uint64
	demux.OnFrame = func(cid gompeg2.TS_STREAM_TYPE, _ []byte, pts, _ uint64) {
		if cid == gompeg2.TS_STREAM_H264 {
			ptsValues = append(ptsValues, pts)
		}
	}
	if err := demux.Input(bytes.NewReader(out)); err != nil {
		t.Fatalf("demux: %v", err)
	}
	if len(ptsValues) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(ptsValues))
	}
	if ptsValues[0] != 12_345 {
		t.Errorf("disabled Normaliser mutated PTS: got %d, want 12345", ptsValues[0])
	}
}

// TestOnSessionResetsAnchor — after OnSession, the next packet seeds
// against a fresh wallclock origin. We feed two pulses separated by a
// session boundary and check the second pulse's first frame lands near
// 0 ms again rather than continuing the timeline from before.
func TestOnSessionResetsAnchor(t *testing.T) {
	n := tsnorm.New(timeline.DefaultConfig())

	// First pulse: upstream PTS 100_000. Output PTS should be near 0.
	src1 := muxSingleH264(t, 100_000)
	out1, err := n.Process(src1)
	if err != nil {
		t.Fatalf("Process 1: %v", err)
	}
	pts1 := firstH264PTS(t, out1)
	if pts1 > 1000 {
		t.Errorf("pulse 1 first frame PTS = %d, expected near 0", pts1)
	}

	// Cross a session boundary. After OnSession, the next pulse should
	// re-seed even though it shares the same upstream timeline.
	sess := &domain.StreamSession{ID: 42, Reason: domain.SessionStartReconnect}
	n.OnSession(sess)

	// Second pulse: upstream PTS 200_000 (further out than the first).
	// Without the OnSession reset, the Normaliser would treat the gap
	// as a forward jump and either pass through (>JumpThreshold) or
	// drop. With reset, it seeds afresh and emits a PTS near 0 again.
	src2 := muxSingleH264(t, 200_000)
	out2, err := n.Process(src2)
	if err != nil {
		t.Fatalf("Process 2: %v", err)
	}
	pts2 := firstH264PTS(t, out2)
	if pts2 > 1000 {
		t.Errorf("pulse 2 first frame PTS after OnSession reset = %d, expected near 0", pts2)
	}
}

// TestProcess_PreservesAACADTSPrefix — file-source (gomedia/go-mp4
// MP4Demuxer) AAC samples arrive ADTS-prefixed. The DASH packager's
// handleAAC requires the ADTS header to build the audio init segment
// (parseADTS on av.Data). If the demux/remux roundtrip strips or
// mangles the ADTS prefix, downstream DASH never builds audioInit and
// the audio AdaptationSet disappears from the MPD entirely —
// reproduced on staging with test2 (mp4 file source) after S4 wired
// tsnorm into writeRawTSChunk.
func TestProcess_PreservesAACADTSPrefix(t *testing.T) {
	// 7-byte ADTS header for AAC LC @ 16 kHz mono, frame_length=8
	// (header 7 + 1 payload byte). Same shape as testADTSFrame in
	// dash/packager_test.go. Mux it into a TS chunk via gomedia, run
	// it through tsnorm, then re-demux the output and verify the
	// emitted frame still starts with the ADTS sync word (0xFFF).
	adtsFrame := []byte{0xFF, 0xF1, 0x4C, 0x80, 0x01, 0x1F, 0xFC, 0xAA}
	srcMuxer := tsmux.NewFromAV()
	var srcBuf bytes.Buffer
	srcMuxer.Write(&domain.AVPacket{
		Codec: domain.AVCodecAAC,
		Data:  adtsFrame,
		PTSms: 0,
		DTSms: 0,
	}, func(b []byte) { srcBuf.Write(b) })
	if srcBuf.Len() == 0 {
		t.Fatal("source muxer produced no TS bytes for AAC")
	}

	n := tsnorm.New(timeline.DefaultConfig())
	out, err := n.Process(srcBuf.Bytes())
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("Process returned no bytes — AAC frame was dropped")
	}

	// Demux the output and check the first AAC frame still starts with
	// the ADTS sync word (0xFFF in the first 12 bits = 0xFF 0xFx).
	demux := gompeg2.NewTSDemuxer()
	var firstAAC []byte
	demux.OnFrame = func(cid gompeg2.TS_STREAM_TYPE, frame []byte, _, _ uint64) {
		if cid == gompeg2.TS_STREAM_AAC && firstAAC == nil {
			firstAAC = bytes.Clone(frame)
		}
	}
	if err := demux.Input(bytes.NewReader(out)); err != nil {
		t.Fatalf("output demux: %v", err)
	}
	if firstAAC == nil {
		t.Fatal("output demux did not yield any AAC frames")
	}
	if len(firstAAC) < 2 {
		t.Fatalf("AAC frame too short: %x", firstAAC)
	}
	// ADTS sync word: 0xFFF in first 12 bits.
	if firstAAC[0] != 0xFF || firstAAC[1]&0xF0 != 0xF0 {
		t.Errorf("AAC frame lost its ADTS prefix\n  got:  %x\n  want: starts with 0xFF 0xFx", firstAAC[:min(len(firstAAC), 10)])
	}
}

// TestProcess_InterleavedVAEmitsBothInFirstPMT — verifies that
// pre-registration of all three codecs in tsnorm.New makes the very
// first PMT the muxer emits list every PID. Without pre-registration,
// gomedia's TSMuxer would emit PAT+PMT on the first frame's codec
// only; the second codec's PES (arriving within the 400 ms PMT-refresh
// window) would land on a PID downstream demuxers haven't announced
// and get silently dropped. This was the root cause of test2 mp4
// source losing its entire audio AdaptationSet after S4.
//
// Source built via gomedia directly with BOTH streams pre-registered
// (so the input PMT has both PIDs from the start) — that mimics
// file.go's behaviour AFTER its second-codec AddStream + the 400 ms
// PMT-refresh tick has fired. The test then verifies tsnorm preserves
// both codecs in the output.
func TestProcess_InterleavedVAEmitsBothInFirstPMT(t *testing.T) {
	src := gompeg2.NewTSMuxer()
	var srcBuf bytes.Buffer
	src.OnPacket = func(b []byte) { srcBuf.Write(b) }
	vpid := src.AddStream(gompeg2.TS_STREAM_H264)
	apid := src.AddStream(gompeg2.TS_STREAM_AAC)
	_ = src.Write(vpid, buildH264IDR(), 0, 0)
	_ = src.Write(apid, []byte{0xFF, 0xF1, 0x4C, 0x80, 0x01, 0x1F, 0xFC, 0xAA}, 50, 50)

	n := tsnorm.New(timeline.DefaultConfig())
	out, err := n.Process(srcBuf.Bytes())
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	demux := gompeg2.NewTSDemuxer()
	sawH264 := false
	sawAAC := false
	demux.OnFrame = func(cid gompeg2.TS_STREAM_TYPE, _ []byte, _, _ uint64) {
		switch cid { //nolint:exhaustive // test only checks H264 / AAC presence; other codecs irrelevant.
		case gompeg2.TS_STREAM_H264:
			sawH264 = true
		case gompeg2.TS_STREAM_AAC:
			sawAAC = true
		}
	}
	if err := demux.Input(bytes.NewReader(out)); err != nil {
		t.Fatalf("demux: %v", err)
	}
	if !sawH264 {
		t.Error("output did not yield H264 frame")
	}
	if !sawAAC {
		t.Error("output did not yield AAC frame — the bug: PMT didn't announce AAC PID before audio PES")
	}
}

// ─── helpers ────────────────────────────────────────────────────────

// muxSingleH264 builds a TS chunk containing one IDR with the given
// upstream PTS. Used by tests that need a small known-shape input.
func muxSingleH264(t *testing.T, ptsMS uint64) []byte {
	t.Helper()
	m := tsmux.NewFromAV()
	var buf bytes.Buffer
	m.Write(&domain.AVPacket{
		Codec:    domain.AVCodecH264,
		Data:     buildH264IDR(),
		PTSms:    ptsMS,
		DTSms:    ptsMS,
		KeyFrame: true,
	}, func(b []byte) { buf.Write(b) })
	return buf.Bytes()
}

// firstH264PTS demuxes ts and returns the PTS of the first H.264 frame.
func firstH264PTS(t *testing.T, ts []byte) uint64 {
	t.Helper()
	demux := gompeg2.NewTSDemuxer()
	var got uint64
	var seen bool
	demux.OnFrame = func(cid gompeg2.TS_STREAM_TYPE, _ []byte, pts, _ uint64) {
		if cid == gompeg2.TS_STREAM_H264 && !seen {
			got = pts
			seen = true
		}
	}
	if err := demux.Input(bytes.NewReader(ts)); err != nil {
		t.Fatalf("demux: %v", err)
	}
	if !seen {
		t.Fatal("no H.264 frame in output")
	}
	return got
}

// Real-world H.264 1080p Main@4.0 SPS/PPS borrowed from the dash
// packager tests so the demuxer accepts our synthetic frames as valid
// H.264 access units.
var (
	testH264SPS = []byte{0x67, 0x4d, 0x40, 0x28, 0xeb, 0x05, 0x07, 0x80, 0x44, 0x00, 0x00, 0x03, 0x00, 0x04, 0x00, 0x00, 0x03, 0x00, 0xf0, 0x3c, 0x60, 0xc6, 0x58}
	testH264PPS = []byte{0x68, 0xee, 0x3c, 0x80}
	// Synthetic IDR slice payload: NAL type 5 (IDR) + arbitrary bytes.
	testH264IDR   = []byte{0x65, 0x88, 0x80, 0x40, 0x00, 0x00}
	testH264Slice = []byte{0x41, 0x9a, 0x12, 0x34, 0x56}
)

func startCode() []byte { return []byte{0, 0, 0, 1} }

func buildH264IDR() []byte {
	out := make([]byte, 0, 4*3+len(testH264SPS)+len(testH264PPS)+len(testH264IDR))
	out = append(out, startCode()...)
	out = append(out, testH264SPS...)
	out = append(out, startCode()...)
	out = append(out, testH264PPS...)
	out = append(out, startCode()...)
	out = append(out, testH264IDR...)
	return out
}

func buildH264NonIDR() []byte {
	out := make([]byte, 0, 4+len(testH264Slice))
	out = append(out, startCode()...)
	out = append(out, testH264Slice...)
	return out
}
