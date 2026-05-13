package dash

import (
	"testing"
	"time"
)

// fillVideo populates the queue with N frames at the given inter-frame
// spacing (ms), IDR every gop frames starting at index 0. Returns the
// PTSms of the LAST queued frame.
//
//nolint:unparam // gop is configurable across tests even though current callers all use 25
func fillVideo(q *FrameQueue, n int, spacingMS uint64, gop int) uint64 {
	last := uint64(0)
	for i := 0; i < n; i++ {
		pts := uint64(i) * spacingMS //nolint:gosec
		q.PushVideo(VideoFrame{
			AnnexB: []byte{0x00, 0x00, 0x00, 0x01},
			PTSms:  pts,
			DTSms:  pts,
			IsIDR:  gop > 0 && i%gop == 0,
		})
		last = pts
	}
	return last
}

// fillAudio populates the audio queue with N frames at spacing ms.
func fillAudio(q *FrameQueue, n int, spacingMS uint64) {
	for i := 0; i < n; i++ {
		q.PushAudio(AudioFrame{
			Raw:   []byte{0xAA, 0xBB},
			PTSms: uint64(i) * spacingMS, //nolint:gosec
		})
	}
}

// TestCut_BelowSegDurHolds — segmenter waits when not enough content is queued.
func TestCut_BelowSegDurHolds(t *testing.T) {
	s := NewSegmenter(2*time.Second, 3)
	q := NewFrameQueue()
	// 1 s of video (25 frames @ 40 ms) — below 2 s target.
	fillVideo(q, 25, 40, 25)

	d := s.Cut(time.Now(), q, true, true, 44100)
	if d.Ok {
		t.Fatalf("expected no cut on 1s content (segDur=2s), got %+v", d)
	}
}

// TestCut_AtIDRBoundary — segmenter cuts at the trailing IDR once segDur
// of content is queued.
func TestCut_AtIDRBoundary(t *testing.T) {
	s := NewSegmenter(2*time.Second, 3)
	q := NewFrameQueue()
	// 3 s of video @ 40 ms spacing = 75 frames. GOP=25 (1s) → IDRs at
	// indexes 0, 25, 50. We expect the cut at index 50 (the trailing
	// IDR within segDur window).
	fillVideo(q, 75, 40, 25)
	fillAudio(q, 100, 23) // plenty of audio

	d := s.Cut(time.Now(), q, true, true, 44100)
	if !d.Ok {
		t.Fatalf("expected cut, got Ok=false")
	}
	if !d.IsIDRAligned {
		t.Errorf("expected IDR-aligned cut, got IsIDRAligned=false")
	}
	// The trailing IDR is at index 50 → VideoCount = 51 (drain up to
	// and including that frame).
	if d.VideoCount != 51 {
		t.Errorf("VideoCount = %d, want 51 (drain through IDR at index 50)", d.VideoCount)
	}
	// Audio count: frames with PTSms ≤ video[50].PTSms = 50*40 = 2000.
	// audio PTS = 0, 23, 46, ..., 23*86=1978, 23*87=2001. So 87 frames
	// (indexes 0..86) are ≤ 2000.
	if d.AudioCount != 87 {
		t.Errorf("AudioCount = %d, want 87 (frames ≤ 2000ms)", d.AudioCount)
	}
}

// TestCut_PrefersFirstIDRPastSegDur — segmenter cuts at the FIRST IDR
// whose PTS is at least segDur past the queue's first frame. This
// keeps segment durs ≤ segDur, preventing the overlapping-timeline
// problem where frame-PTS-span exceeded inter-cut wallclock and the
// MPD's (t, d) describes overlapping ranges.
func TestCut_PrefersFirstIDRPastSegDur(t *testing.T) {
	s := NewSegmenter(2*time.Second, 3)
	q := NewFrameQueue()
	// 4 s of video @ 40 ms, GOP=25 → IDRs at 0, 25, 50, 75 (PTS 0, 1000,
	// 2000, 3000). With segDur=2 s, the first IDR whose PTS ≥ 2000ms
	// is index 50 (PTS 2000). Cut drains frames 0..50 = 51 frames.
	fillVideo(q, 100, 40, 25)

	d := s.Cut(time.Now(), q, true, false, 0)
	if !d.Ok || !d.IsIDRAligned {
		t.Fatalf("expected IDR cut, got %+v", d)
	}
	if d.VideoCount != 51 {
		t.Errorf("VideoCount = %d, want 51 (drain through index 50, first IDR past segDur)", d.VideoCount)
	}
}

// TestCut_NoIDRWaitingForSafetyNet — long-GOP source has no IDR within
// the segDur window; segmenter holds until either an IDR arrives or
// the safety-net wallclock deadline passes.
func TestCut_NoIDRWaitingForSafetyNet(t *testing.T) {
	s := NewSegmenter(2*time.Second, 3)
	q := NewFrameQueue()
	// 5 s of video @ 40 ms but the ONLY IDR is at index 0 (PTS 0).
	// LastIDR.PTS - first.PTS = 0 → no IDR satisfies segDur window.
	for i := 0; i < 125; i++ {
		pts := uint64(i) * 40 //nolint:gosec
		q.PushVideo(VideoFrame{
			AnnexB: []byte{0x00, 0x00, 0x00, 0x01},
			PTSms:  pts,
			DTSms:  pts,
			IsIDR:  i == 0,
		})
	}

	// lastCut is zero (first cut). Span is 4960 ms (124*40). maxFactor=3,
	// segDur=2s → maxElapsedMS=6000. Span 4960 < 6000 → no cold-start
	// safety-net yet.
	d := s.Cut(time.Now(), q, true, false, 0)
	if d.Ok {
		t.Fatalf("expected hold (span < maxElapsed), got %+v", d)
	}

	// Push more frames until span passes maxElapsed.
	for i := 125; i < 200; i++ {
		pts := uint64(i) * 40 //nolint:gosec
		q.PushVideo(VideoFrame{PTSms: pts, DTSms: pts, IsIDR: false})
	}
	// Now span = 199*40 = 7960 ms > 6000 → cold-start safety-net fires.
	d = s.Cut(time.Now(), q, true, false, 0)
	if !d.Ok {
		t.Fatal("expected safety-net cut after span exceeds maxElapsed")
	}
	if d.IsIDRAligned {
		t.Error("safety-net cut must NOT claim IDR alignment")
	}
	if d.VideoCount != 200 {
		t.Errorf("VideoCount = %d, want 200 (entire queue)", d.VideoCount)
	}
}

// TestCut_SafetyNetByElapsedWallclock — after the first cut, the
// wallclock deadline (maxFactor × segDur since lastCut) takes over.
func TestCut_SafetyNetByElapsedWallclock(t *testing.T) {
	s := NewSegmenter(2*time.Second, 3)
	q := NewFrameQueue()

	t0 := time.Now()
	s.MarkCut(t0)

	// 5 frames of video, no IDR.
	for i := 0; i < 5; i++ {
		pts := uint64(i) * 40 //nolint:gosec
		q.PushVideo(VideoFrame{PTSms: pts, DTSms: pts})
	}

	// Less than 6s elapsed → hold.
	d := s.Cut(t0.Add(3*time.Second), q, true, false, 0)
	if d.Ok {
		t.Fatalf("expected hold (3s < 6s deadline), got %+v", d)
	}

	// Past the 6s deadline → cut everything.
	d = s.Cut(t0.Add(7*time.Second), q, true, false, 0)
	if !d.Ok {
		t.Fatal("expected wallclock safety-net cut at 7s elapsed")
	}
	if d.IsIDRAligned {
		t.Error("safety-net cut must NOT claim IDR alignment")
	}
}

// TestCut_AudioOnlyStream — no video init at all; segmenter cuts on
// audio span ≥ segDur and emits ~segDur worth of frames per cut (NOT
// the entire queue — that produced 28-second segments on bursty
// sources where the queue saturated to maxQueueSpanMs of audio).
func TestCut_AudioOnlyStream(t *testing.T) {
	s := NewSegmenter(2*time.Second, 3)
	q := NewFrameQueue()

	// 50 audio frames @ 23 ms = ~1.15s — below segDur.
	fillAudio(q, 50, 23)
	d := s.Cut(time.Now(), q, false, true, 0)
	if d.Ok {
		t.Fatalf("audio-only below segDur should hold, got %+v", d)
	}

	// 100 frames @ 23ms = ~2.3s — above segDur. Cut takes ~segDur
	// worth (= floor(2000/23)+1 = 87 frames whose PTSms ≤ 0+2000).
	// Remaining frames stay queued for the next cut.
	q2 := NewFrameQueue()
	fillAudio(q2, 100, 23)
	d = s.Cut(time.Now(), q2, false, true, 0)
	if !d.Ok || d.VideoCount != 0 {
		t.Fatalf("audio-only cut: want Ok=true VideoCount=0, got %+v", d)
	}
	if d.AudioCount != 87 {
		t.Errorf("AudioCount = %d, want 87 (~segDur worth, not drain all)", d.AudioCount)
	}

	// Pathological burst: 1000 frames @ 21 ms = ~21 s queued at once
	// (simulates 48 kHz AAC where frame spacing = 1024/48000 ≈ 21.3 ms).
	// Without the segDur cap we'd emit a 21 s segment; with it we emit
	// ~segDur worth and let subsequent cuts drain the rest.
	q3 := NewFrameQueue()
	fillAudio(q3, 1000, 21)
	d = s.Cut(time.Now(), q3, false, true, 0)
	if !d.Ok {
		t.Fatalf("burst audio cut: want Ok=true, got %+v", d)
	}
	// At 21 ms spacing, segDur 2 s ≈ 96 frames. Allow slack for off-by-one.
	if d.AudioCount > 110 {
		t.Errorf("burst AudioCount = %d, want close to segDur worth (~96), not entire queue", d.AudioCount)
	}
}

// TestCut_HybridResidualConverges — the V/A duration coupling math
// (targetA = V_dur × sr / 1024 / 1000) rarely lands on a whole AAC
// frame boundary. Without the residual accumulator, integer truncation
// produces a fixed per-cut deficit that compounds into V/A drift over
// the session (test2 saw 8 ms/cut × 88 cuts = 0.7 s drift before this
// fix). This test simulates 100 sequential cuts at 5 s V dur, 48 kHz
// audio, and asserts cumulative A dur catches up to cumulative V dur
// within one frame.
func TestCut_HybridResidualConverges(t *testing.T) {
	const segDurMs uint64 = 5000
	const sr uint64 = 48000
	const nCuts = 100
	s := NewSegmenter(time.Duration(segDurMs)*time.Millisecond, 3)

	var totalAudioFrames uint64
	for i := 0; i < nCuts; i++ {
		q := NewFrameQueue()
		// 175 frames @ 40 ms = 7 s span; IDR every 50 frames (2 s GOP)
		// → IDRs at indexes 0, 50, 100, 150. The first IDR past
		// segDur (5 s) is at index 125 (PTS 5000) — wait, index 150
		// is at PTS 6000. We expect findIDRCutPoint to pick index
		// 125's IDR. Actually IDRs are only at multiples of 50, so
		// first IDR past 5000 ms is at index 150 (PTS 6000 ms).
		fillVideo(q, 175, 40, 50)
		// Plenty of audio so the cut isn't held.
		fillAudio(q, 500, 21) // 48 kHz frames are ~21.33 ms; use 21
		d := s.Cut(time.Now(), q, true, true, sr)
		if !d.Ok {
			t.Fatalf("cut %d held unexpectedly; AudioLen=%d", i, q.AudioLen())
		}
		totalAudioFrames += uint64(d.AudioCount) //nolint:gosec
	}

	// Each V cut spans frames 0..150 (IDR at index 150, PTS 6000 ms).
	// vCount=151; nextPTSms = videoPTSAt(151) = 151×40 = 6040 ms.
	// Hybrid math: vDurMs = nextPTSms − first.PTSms = 6040.
	// cumulativeV = nCuts × 6040.
	cumulativeVMs := uint64(nCuts) * 6040
	cumulativeAMs := totalAudioFrames * 1024 * 1000 / sr
	deltaMs := int64(cumulativeVMs) - int64(cumulativeAMs)
	if deltaMs > 21 || deltaMs < -21 {
		t.Errorf("cumulative drift after %d cuts = %d ms, want |drift| ≤ 21 ms (one AAC frame at 48 kHz)", nCuts, deltaMs)
	}
}

// TestCut_NoTracksAtAll — both haveVideo and haveAudio false; segmenter
// returns no-op.
func TestCut_NoTracksAtAll(t *testing.T) {
	s := NewSegmenter(2*time.Second, 3)
	q := NewFrameQueue()
	d := s.Cut(time.Now(), q, false, false, 0)
	if d.Ok {
		t.Errorf("no-tracks Cut should return Ok=false, got %+v", d)
	}
}

// TestMarkCut_ResetsSafetyNet — calling MarkCut resets the lastCut
// time, so the next safety-net deadline is measured from the new cut.
func TestMarkCut_ResetsSafetyNet(t *testing.T) {
	s := NewSegmenter(2*time.Second, 3)
	t0 := time.Now()
	s.MarkCut(t0)

	q := NewFrameQueue()
	// Push 5 frames, no IDR.
	for i := 0; i < 5; i++ {
		pts := uint64(i) * 40 //nolint:gosec
		q.PushVideo(VideoFrame{PTSms: pts, DTSms: pts})
	}

	// 5s after MarkCut(t0) — below 6s deadline.
	if d := s.Cut(t0.Add(5*time.Second), q, true, false, 0); d.Ok {
		t.Fatal("5s elapsed should not trip safety-net")
	}

	// Mark a NEW cut at 4s; subsequent check measures from there.
	s.MarkCut(t0.Add(4 * time.Second))
	// At t0+5s, only 1s since the new MarkCut → still below deadline.
	if d := s.Cut(t0.Add(5*time.Second), q, true, false, 0); d.Ok {
		t.Fatal("1s since MarkCut should not trip safety-net")
	}
}

// TestReset_RestoresColdStart — Reset clears lastCut so subsequent
// safety-net logic uses the cold-start branch (span-based, not
// wallclock-based).
func TestReset_RestoresColdStart(t *testing.T) {
	s := NewSegmenter(2*time.Second, 3)
	s.MarkCut(time.Now())
	s.Reset()
	if !s.lastCut.IsZero() {
		t.Errorf("Reset should clear lastCut")
	}
}
