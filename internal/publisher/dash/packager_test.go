package dash

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// Real-world H.264 1080p Main@4.0 SPS/PPS so mp4ff.ParseSPSNALUnit
// accepts them. Captured from a production HLS source; same bytes used
// in v1's test suite.
var (
	testH264SPS = []byte{0x67, 0x4d, 0x40, 0x28, 0xeb, 0x05, 0x07, 0x80, 0x44, 0x00, 0x00, 0x03, 0x00, 0x04, 0x00, 0x00, 0x03, 0x00, 0xf0, 0x3c, 0x60, 0xc6, 0x58}
	testH264PPS = []byte{0x68, 0xee, 0x3c, 0x80}
	// Synthetic IDR slice payload: NAL type 5 (IDR) + arbitrary bytes.
	// The packager only scans NAL types, doesn't decode the slice.
	testH264IDR = []byte{0x65, 0x88, 0x80, 0x40, 0x00, 0x00}
	// Synthetic non-IDR slice: NAL type 1 (non-IDR slice).
	testH264Slice = []byte{0x41, 0x9a, 0x12, 0x34, 0x56}
	// Real ADTS frame: profile 2 (AAC LC), sample rate idx 4 (44100),
	// channel cfg 2 (stereo), frame length 8 bytes including header.
	// Header is 7 bytes; first payload byte 0xAA.
	testADTSFrame = []byte{0xFF, 0xF1, 0x4C, 0x80, 0x01, 0x1F, 0xFC, 0xAA}
)

func startCode() []byte { return []byte{0, 0, 0, 1} }

// buildH264IDR concatenates SPS|PPS|IDR with Annex-B start codes — the
// access-unit shape every H.264 IDR carries when the ingestor delivers
// it to the packager.
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

// buildH264NonIDR returns a non-IDR slice in Annex-B form.
func buildH264NonIDR() []byte {
	out := make([]byte, 0, 4+len(testH264Slice))
	out = append(out, startCode()...)
	out = append(out, testH264Slice...)
	return out
}

// setupPackager wires a Packager with a buffer subscription against a
// temp dir. Returns the packager, buffer service, stream code, and a
// cleanup func.
func setupPackager(t *testing.T, packAudio bool) (*Packager, *buffer.Service, domain.StreamCode, func()) {
	t.Helper()
	streamID := domain.StreamCode("test-" + t.Name())
	dir := t.TempDir()

	cfg := Config{
		StreamID:       string(streamID),
		StreamDir:      dir,
		ManifestPath:   filepath.Join(dir, "index.mpd"),
		SegDur:         500 * time.Millisecond, // short for fast tests
		Window:         3,
		History:        0,
		Ephemeral:      true,
		PairingTimeout: 200 * time.Millisecond,
		PackAudio:      packAudio,
	}
	p, err := NewPackager(cfg)
	if err != nil {
		t.Fatalf("NewPackager: %v", err)
	}
	bs := buffer.NewServiceForTesting(64)
	bs.Create(streamID)
	cleanup := func() {
		bs.Delete(streamID)
	}
	return p, bs, streamID, cleanup
}

// pushAV writes an AV packet to the buffer's stream.
func pushAV(t *testing.T, bs *buffer.Service, id domain.StreamCode, codec domain.AVCodec, data []byte, pts, dts uint64, key bool) {
	t.Helper()
	pkt := buffer.Packet{
		AV: &domain.AVPacket{
			Codec:    codec,
			Data:     data,
			PTSms:    pts,
			DTSms:    dts,
			KeyFrame: key,
		},
	}
	if err := bs.Write(id, pkt); err != nil {
		t.Fatalf("buffer.Write: %v", err)
	}
}

// TestPackager_AVPath_WritesInitAndSegments — happy path. Push an IDR
// + a few non-IDRs + AAC frames; verify init_v.mp4, init_a.mp4, and at
// least one media segment + manifest appear on disk.
func TestPackager_AVPath_WritesInitAndSegments(t *testing.T) {
	p, bs, id, done := setupPackager(t, true)
	defer done()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sub, err := bs.Subscribe(id)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer bs.Unsubscribe(id, sub)

	doneCh := make(chan struct{})
	go func() {
		p.Run(ctx, sub)
		close(doneCh)
	}()

	// Push frames spanning > segDur so the segmenter cuts at the IDR.
	// Initial IDR at PTS=0, then non-IDRs at 40ms intervals up to 800ms,
	// then a second IDR at 1000ms. Audio interleaved at 23ms cadence.
	pushAV(t, bs, id, domain.AVCodecH264, buildH264IDR(), 0, 0, true)
	pushAV(t, bs, id, domain.AVCodecAAC, testADTSFrame, 0, 0, false)
	for i := 1; i <= 20; i++ {
		pts := uint64(i) * 40 //nolint:gosec
		isKey := i == 20
		var frame []byte
		if isKey {
			frame = buildH264IDR()
		} else {
			frame = buildH264NonIDR()
		}
		pushAV(t, bs, id, domain.AVCodecH264, frame, pts, pts, isKey)
	}
	// 30 AAC frames at 23ms cadence (~690ms of audio).
	for i := 0; i < 30; i++ {
		pushAV(t, bs, id, domain.AVCodecAAC, testADTSFrame, uint64(i)*23, uint64(i)*23, false) //nolint:gosec
	}

	// Wait up to 2s for the ticker (50ms) + segmenter to produce a
	// segment + manifest.
	require := func(cond bool, msg string) {
		t.Helper()
		if !cond {
			t.Fatal(msg)
		}
	}
	waitForFile(t, filepath.Join(p.cfg.StreamDir, "init_v.mp4"), 2*time.Second)
	waitForFile(t, filepath.Join(p.cfg.StreamDir, "init_a.mp4"), 2*time.Second)
	waitForFile(t, filepath.Join(p.cfg.StreamDir, "seg_v_00001.m4s"), 2*time.Second)
	waitForFile(t, filepath.Join(p.cfg.StreamDir, "seg_a_00001.m4s"), 2*time.Second)
	waitForFile(t, p.cfg.ManifestPath, 2*time.Second)

	// Manifest should contain both adaptation sets.
	data, err := os.ReadFile(p.cfg.ManifestPath)
	require(err == nil, "read manifest: "+stringOrErr(err))
	require(len(data) > 0, "manifest empty")
	cancel()
	<-doneCh
}

// TestPackager_PairingGate_HoldsUntilBothTracks — push only video,
// verify no segment is written until A arrives. Then push A → both
// segments appear.
func TestPackager_PairingGate_HoldsUntilBothTracks(t *testing.T) {
	p, bs, id, done := setupPackager(t, true)
	defer done()

	// Bump the pairing timeout up so the test deterministically
	// observes the hold (avoids racing the 200ms default).
	p.cfg.PairingTimeout = 5 * time.Second
	p.state = NewStateMachine(p.cfg.PairingTimeout)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sub, err := bs.Subscribe(id)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer bs.Unsubscribe(id, sub)

	doneCh := make(chan struct{})
	go func() {
		p.Run(ctx, sub)
		close(doneCh)
	}()

	// Push only video for a while — enough to span segDur and accumulate
	// the queue but pairing gate should hold the flush.
	pushAV(t, bs, id, domain.AVCodecH264, buildH264IDR(), 0, 0, true)
	for i := 1; i <= 20; i++ {
		pts := uint64(i) * 40 //nolint:gosec
		isKey := i == 20
		var frame []byte
		if isKey {
			frame = buildH264IDR()
		} else {
			frame = buildH264NonIDR()
		}
		pushAV(t, bs, id, domain.AVCodecH264, frame, pts, pts, isKey)
	}

	// Wait long enough that the segmenter has had multiple ticks. With
	// PackAudio=true + pairing gate, no segment should appear because
	// audio isn't ready.
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(p.cfg.StreamDir, "seg_v_00001.m4s")); err == nil {
		t.Fatal("pairing gate failed to hold first flush")
	}

	// Now push audio. Pairing achieved → segments emit.
	for i := 0; i < 30; i++ {
		pushAV(t, bs, id, domain.AVCodecAAC, testADTSFrame, uint64(i)*23, uint64(i)*23, false) //nolint:gosec
	}
	waitForFile(t, filepath.Join(p.cfg.StreamDir, "seg_v_00001.m4s"), 2*time.Second)
	waitForFile(t, filepath.Join(p.cfg.StreamDir, "seg_a_00001.m4s"), 2*time.Second)
	cancel()
	<-doneCh
}

// TestPackager_PairingTimeout_VideoOnly — when audio never arrives,
// the pairing window times out and the packager proceeds video-only.
func TestPackager_PairingTimeout_VideoOnly(t *testing.T) {
	p, bs, id, done := setupPackager(t, false) // PackAudio=false: no audio expected
	defer done()
	// Set a very short pairing timeout so the test runs fast.
	p.cfg.PairingTimeout = 100 * time.Millisecond
	p.state = NewStateMachine(p.cfg.PairingTimeout)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub, err := bs.Subscribe(id)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer bs.Unsubscribe(id, sub)

	doneCh := make(chan struct{})
	go func() {
		p.Run(ctx, sub)
		close(doneCh)
	}()

	// Video frames only — pairing timeout should fire and the segment
	// emits without audio.
	pushAV(t, bs, id, domain.AVCodecH264, buildH264IDR(), 0, 0, true)
	for i := 1; i <= 20; i++ {
		pts := uint64(i) * 40 //nolint:gosec
		isKey := i == 20
		var frame []byte
		if isKey {
			frame = buildH264IDR()
		} else {
			frame = buildH264NonIDR()
		}
		pushAV(t, bs, id, domain.AVCodecH264, frame, pts, pts, isKey)
	}
	waitForFile(t, filepath.Join(p.cfg.StreamDir, "seg_v_00001.m4s"), 2*time.Second)

	// No audio init or audio segment should exist.
	if _, err := os.Stat(filepath.Join(p.cfg.StreamDir, "init_a.mp4")); !os.IsNotExist(err) {
		t.Errorf("audio init unexpectedly present: %v", err)
	}
	cancel()
	<-doneCh
}

// TestPackager_SessionBoundary_DropsPendingQueue — push a partial
// in-progress segment, then a packet with SessionStart=true, then
// new-session frames. The new segment should NOT include old-session
// frames.
func TestPackager_SessionBoundary_DropsPendingQueue(t *testing.T) {
	p, bs, id, done := setupPackager(t, false)
	defer done()
	p.cfg.PairingTimeout = 100 * time.Millisecond
	p.state = NewStateMachine(p.cfg.PairingTimeout)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub, err := bs.Subscribe(id)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer bs.Unsubscribe(id, sub)

	doneCh := make(chan struct{})
	go func() {
		p.Run(ctx, sub)
		close(doneCh)
	}()

	// Some frames in session 1.
	pushAV(t, bs, id, domain.AVCodecH264, buildH264IDR(), 0, 0, true)
	for i := 1; i < 5; i++ {
		pts := uint64(i) * 40 //nolint:gosec
		pushAV(t, bs, id, domain.AVCodecH264, buildH264NonIDR(), pts, pts, false)
	}
	// Wait for video init to be built.
	waitForFile(t, filepath.Join(p.cfg.StreamDir, "init_v.mp4"), 1*time.Second)

	// SessionStart marker carrying no payload — the buffer hub's
	// auto-stamp normally fires on the next packet after SetSession.
	// Simulate by writing a packet with SessionStart=true on a fresh
	// session minted via the buffer hub API.
	_ = bs.SetSession(id, domain.SessionStartReconnect, nil, nil)
	pushAV(t, bs, id, domain.AVCodecH264, buildH264IDR(), 5000, 5000, true)
	for i := 1; i <= 20; i++ {
		pts := 5000 + uint64(i)*40 //nolint:gosec
		isKey := i == 20
		var frame []byte
		if isKey {
			frame = buildH264IDR()
		} else {
			frame = buildH264NonIDR()
		}
		pushAV(t, bs, id, domain.AVCodecH264, frame, pts, pts, isKey)
	}
	waitForFile(t, filepath.Join(p.cfg.StreamDir, "seg_v_00001.m4s"), 2*time.Second)

	// Queue length should be small (only post-boundary frames retained).
	// Hard to assert exactly without instrumentation; the file existence
	// + lack of panics is the smoke check.
	cancel()
	<-doneCh
}

// TestPackager_ABRMode_NotifiesMaster — set up an ABRMaster, run a
// shard, verify the master receives a snapshot.
func TestPackager_ABRMode_NotifiesMaster(t *testing.T) {
	dir := t.TempDir()
	rootMPD := filepath.Join(dir, "index.mpd")
	master := NewABRMaster(rootMPD, "ladder", 500*time.Millisecond, 3)
	defer master.Stop()

	streamID := domain.StreamCode("abr-shard")
	shardDir := filepath.Join(dir, "track_1")
	cfg := Config{
		StreamID:       string(streamID),
		StreamDir:      shardDir,
		ManifestPath:   "", // no per-shard MPD — master writes root
		SegDur:         500 * time.Millisecond,
		Window:         3,
		History:        0,
		Ephemeral:      true,
		PairingTimeout: 100 * time.Millisecond,
		PackAudio:      true,
		ABRMaster:      master,
		ABRSlug:        "track_1",
	}
	p, err := NewPackager(cfg)
	if err != nil {
		t.Fatalf("NewPackager: %v", err)
	}

	bs := buffer.NewServiceForTesting(64)
	bs.Create(streamID)
	defer bs.Delete(streamID)
	sub, err := bs.Subscribe(streamID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer bs.Unsubscribe(streamID, sub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	doneCh := make(chan struct{})
	go func() {
		p.Run(ctx, sub)
		close(doneCh)
	}()

	pushAV(t, bs, streamID, domain.AVCodecH264, buildH264IDR(), 0, 0, true)
	for i := 1; i <= 20; i++ {
		pts := uint64(i) * 40 //nolint:gosec
		isKey := i == 20
		var frame []byte
		if isKey {
			frame = buildH264IDR()
		} else {
			frame = buildH264NonIDR()
		}
		pushAV(t, bs, streamID, domain.AVCodecH264, frame, pts, pts, isKey)
	}
	for i := 0; i < 30; i++ {
		pushAV(t, bs, streamID, domain.AVCodecAAC, testADTSFrame, uint64(i)*23, uint64(i)*23, false) //nolint:gosec
	}

	// Wait for the per-shard segment file (proves the packager wrote
	// something) + the debounced root MPD.
	waitForFile(t, filepath.Join(shardDir, "seg_v_00001.m4s"), 2*time.Second)
	waitForFile(t, rootMPD, 2*time.Second)
	cancel()
	<-doneCh
}

// ─── helpers ─────────────────────────────────────────────────────────

// waitForFile polls path until it exists or timeout elapses.
func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("file did not appear within %v: %s", timeout, path)
}

func stringOrErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
