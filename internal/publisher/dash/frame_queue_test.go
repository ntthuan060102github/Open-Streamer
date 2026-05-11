package dash

import (
	"testing"
)

// helpers — keep table cases concise.

func vf(pts, dts uint64, idr bool) VideoFrame {
	return VideoFrame{
		AnnexB: []byte{0x00, 0x00, 0x00, 0x01}, // start code; payload doesn't matter for queue tests
		PTSms:  pts,
		DTSms:  dts,
		IsIDR:  idr,
	}
}

func af(pts uint64) AudioFrame {
	return AudioFrame{
		Raw:   []byte{0xAA, 0xBB}, // dummy payload
		PTSms: pts,
	}
}

func TestPushAndLen(t *testing.T) {
	q := NewFrameQueue()

	if q.VideoLen() != 0 || q.AudioLen() != 0 {
		t.Fatalf("new queue should be empty, got V=%d A=%d", q.VideoLen(), q.AudioLen())
	}

	q.PushVideo(vf(0, 0, true))
	q.PushVideo(vf(40, 40, false))
	q.PushAudio(af(0))
	q.PushAudio(af(23))
	q.PushAudio(af(46))

	if q.VideoLen() != 2 {
		t.Errorf("VideoLen = %d, want 2", q.VideoLen())
	}
	if q.AudioLen() != 3 {
		t.Errorf("AudioLen = %d, want 3", q.AudioLen())
	}
}

func TestFirstReturnsOldest(t *testing.T) {
	q := NewFrameQueue()

	_, ok := q.FirstVideo()
	if ok {
		t.Error("FirstVideo on empty queue must return ok=false")
	}
	_, ok = q.FirstAudio()
	if ok {
		t.Error("FirstAudio on empty queue must return ok=false")
	}

	q.PushVideo(vf(100, 100, true))
	q.PushVideo(vf(140, 140, false))
	q.PushAudio(af(200))

	v, ok := q.FirstVideo()
	if !ok || v.PTSms != 100 {
		t.Errorf("FirstVideo = %+v ok=%v, want PTS=100 ok=true", v, ok)
	}
	a, ok := q.FirstAudio()
	if !ok || a.PTSms != 200 {
		t.Errorf("FirstAudio = %+v ok=%v, want PTS=200 ok=true", a, ok)
	}
}

func TestPushOrderingPreserved(t *testing.T) {
	q := NewFrameQueue()

	// Interleave V and A pushes; each track's order must be preserved
	// regardless of cross-track ordering.
	q.PushVideo(vf(0, 0, true))
	q.PushAudio(af(0))
	q.PushVideo(vf(40, 40, false))
	q.PushAudio(af(23))
	q.PushVideo(vf(80, 80, false))

	vs := q.PopVideo(3)
	if len(vs) != 3 {
		t.Fatalf("PopVideo(3) returned %d frames", len(vs))
	}
	wantV := []uint64{0, 40, 80}
	for i, v := range vs {
		if v.PTSms != wantV[i] {
			t.Errorf("video[%d] PTS = %d, want %d", i, v.PTSms, wantV[i])
		}
	}

	as := q.PopAudio(2)
	if len(as) != 2 {
		t.Fatalf("PopAudio(2) returned %d frames", len(as))
	}
	wantA := []uint64{0, 23}
	for i, a := range as {
		if a.PTSms != wantA[i] {
			t.Errorf("audio[%d] PTS = %d, want %d", i, a.PTSms, wantA[i])
		}
	}
}

// TruncateBefore is the load-bearing primitive for pairing resolution.
// Tests cover: drops only pre-threshold frames, preserves the threshold
// boundary, handles empty + all-dropped cases, reports counts honestly.
func TestTruncateBefore(t *testing.T) {
	cases := []struct {
		name      string
		vPTS      []uint64 // initial video frame PTSes
		aPTS      []uint64
		threshold uint64
		wantV     []uint64
		wantA     []uint64
		wantVD    int
		wantAD    int
	}{
		{
			name:      "drop none when threshold below all frames",
			vPTS:      []uint64{100, 140, 180},
			aPTS:      []uint64{100, 123},
			threshold: 50,
			wantV:     []uint64{100, 140, 180},
			wantA:     []uint64{100, 123},
			wantVD:    0, wantAD: 0,
		},
		{
			name:      "drop pre-threshold video, keep audio at threshold",
			vPTS:      []uint64{0, 40, 80, 120, 160},
			aPTS:      []uint64{100, 123, 146},
			threshold: 100,
			wantV:     []uint64{120, 160}, // 100=threshold included; 0/40/80 dropped
			wantA:     []uint64{100, 123, 146},
			wantVD:    3, wantAD: 0,
		},
		{
			name:      "threshold equals first frame on both tracks → drops nothing",
			vPTS:      []uint64{50, 90},
			aPTS:      []uint64{50, 73},
			threshold: 50,
			wantV:     []uint64{50, 90},
			wantA:     []uint64{50, 73},
			wantVD:    0, wantAD: 0,
		},
		{
			name:      "drop all video when threshold beyond newest V",
			vPTS:      []uint64{0, 40, 80},
			aPTS:      []uint64{100, 123, 146},
			threshold: 100,
			wantV:     nil,
			wantA:     []uint64{100, 123, 146},
			wantVD:    3, wantAD: 0,
		},
		{
			name:      "empty queue is no-op",
			vPTS:      nil,
			aPTS:      nil,
			threshold: 1000,
			wantV:     nil,
			wantA:     nil,
			wantVD:    0, wantAD: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := NewFrameQueue()
			for _, p := range tc.vPTS {
				q.PushVideo(vf(p, p, false))
			}
			for _, p := range tc.aPTS {
				q.PushAudio(af(p))
			}

			vd, ad := q.TruncateBefore(tc.threshold)
			if vd != tc.wantVD {
				t.Errorf("video dropped = %d, want %d", vd, tc.wantVD)
			}
			if ad != tc.wantAD {
				t.Errorf("audio dropped = %d, want %d", ad, tc.wantAD)
			}

			gotV := make([]uint64, q.VideoLen())
			for i, f := range q.PopVideo(q.VideoLen()) {
				gotV[i] = f.PTSms
			}
			if !equalU64Slice(gotV, tc.wantV) {
				t.Errorf("remaining video = %v, want %v", gotV, tc.wantV)
			}

			gotA := make([]uint64, q.AudioLen())
			for i, f := range q.PopAudio(q.AudioLen()) {
				gotA[i] = f.PTSms
			}
			if !equalU64Slice(gotA, tc.wantA) {
				t.Errorf("remaining audio = %v, want %v", gotA, tc.wantA)
			}
		})
	}
}

// PopVideo / PopAudio return whatever they had if n > length, and nil
// when n <= 0 or queue empty. Also: the returned slice is a copy so
// callers can retain it without aliasing the queue's storage.
func TestPopVariants(t *testing.T) {
	q := NewFrameQueue()
	q.PushVideo(vf(0, 0, true))
	q.PushVideo(vf(40, 40, false))

	if got := q.PopVideo(0); got != nil {
		t.Errorf("PopVideo(0) = %v, want nil", got)
	}
	if got := q.PopVideo(-1); got != nil {
		t.Errorf("PopVideo(-1) = %v, want nil", got)
	}

	// PopVideo(5) on a 2-frame queue returns all 2.
	got := q.PopVideo(5)
	if len(got) != 2 {
		t.Fatalf("PopVideo(5) on 2-frame queue returned %d frames", len(got))
	}
	if q.VideoLen() != 0 {
		t.Errorf("queue should be drained, got VideoLen=%d", q.VideoLen())
	}

	// Empty queue Pop returns nil.
	if got := q.PopVideo(1); got != nil {
		t.Errorf("PopVideo on empty queue = %v, want nil", got)
	}
	if got := q.PopAudio(1); got != nil {
		t.Errorf("PopAudio on empty queue = %v, want nil", got)
	}
}

// VideoSpanMS / AudioSpanMS report PTS span. The segmenter uses this
// to decide when there's a full segDur of content queued.
func TestSpan(t *testing.T) {
	q := NewFrameQueue()

	// Empty / single-frame → zero span.
	if got := q.VideoSpanMS(); got != 0 {
		t.Errorf("empty VideoSpan = %d, want 0", got)
	}
	q.PushVideo(vf(100, 100, true))
	if got := q.VideoSpanMS(); got != 0 {
		t.Errorf("single-frame VideoSpan = %d, want 0", got)
	}

	// Several frames spanning 200 ms.
	q.PushVideo(vf(140, 140, false))
	q.PushVideo(vf(180, 180, false))
	q.PushVideo(vf(300, 300, false))
	if got := q.VideoSpanMS(); got != 200 {
		t.Errorf("VideoSpan = %d, want 200 (300-100)", got)
	}

	// Backward-jump frame at the end shouldn't produce a negative
	// span — the queue clamps to 0 when last <= first.
	q2 := NewFrameQueue()
	q2.PushVideo(vf(500, 500, false))
	q2.PushVideo(vf(400, 400, false)) // regression
	if got := q2.VideoSpanMS(); got != 0 {
		t.Errorf("regressed VideoSpan = %d, want 0", got)
	}

	// Audio span sanity.
	q.PushAudio(af(50))
	q.PushAudio(af(73))
	q.PushAudio(af(96))
	if got := q.AudioSpanMS(); got != 46 {
		t.Errorf("AudioSpan = %d, want 46 (96-50)", got)
	}
}

// LastIDRIndex is the segmenter's anchor for IDR-aligned cuts.
func TestLastIDRIndex(t *testing.T) {
	q := NewFrameQueue()

	// No frames → -1.
	if got := q.LastIDRIndex(); got != -1 {
		t.Errorf("empty LastIDRIndex = %d, want -1", got)
	}

	// No IDR yet → -1.
	q.PushVideo(vf(0, 0, false))
	q.PushVideo(vf(40, 40, false))
	if got := q.LastIDRIndex(); got != -1 {
		t.Errorf("non-IDR LastIDRIndex = %d, want -1", got)
	}

	// One IDR at index 2.
	q.PushVideo(vf(80, 80, true))
	q.PushVideo(vf(120, 120, false))
	if got := q.LastIDRIndex(); got != 2 {
		t.Errorf("LastIDRIndex = %d, want 2", got)
	}

	// Newer IDR at index 4 — returns the latest.
	q.PushVideo(vf(160, 160, true))
	if got := q.LastIDRIndex(); got != 4 {
		t.Errorf("LastIDRIndex after second IDR = %d, want 4", got)
	}
}

// Production scenario: pairing resolution after transcoder warmup.
// V started accumulating immediately, A only arrived 4 seconds later.
// At pairing, TruncateBefore(A.first.PTS) drops V's pre-A frames so
// both queues align at A's first PTS. Subsequent segment cut uses
// LastIDRIndex on the aligned V queue to ensure clean IDR boundary.
func TestPairingScenario_TruncateAlignsBothQueues(t *testing.T) {
	q := NewFrameQueue()

	// Video accumulated 100 frames at 40ms cadence = 4 seconds.
	// IDRs every 25 frames (1 second GOP).
	for i := 0; i < 100; i++ {
		pts := uint64(i * 40) //nolint:gosec
		q.PushVideo(vf(pts, pts, i%25 == 0))
	}
	// Audio's first frame arrives at PTS=4000 (the 4-second mark).
	q.PushAudio(af(4000))

	// Pairing fires. Truncate V to start at A's first frame PTS.
	aFirst, _ := q.FirstAudio()
	vd, ad := q.TruncateBefore(aFirst.PTSms)
	if vd != 100 {
		t.Errorf("video dropped = %d, want 100 (entire pre-A window)", vd)
	}
	if ad != 0 {
		t.Errorf("audio dropped = %d, want 0", ad)
	}

	// V should be empty now (all frames were before A's first PTS).
	// In practice the next video frame after A.first will arrive and
	// the queue will rebuild. We just verified the truncation rule.
	if q.VideoLen() != 0 {
		t.Errorf("video queue should be empty after truncate, got %d", q.VideoLen())
	}
	if q.AudioLen() != 1 {
		t.Errorf("audio queue should retain its single frame, got %d", q.AudioLen())
	}
}

// Variant of the above: A arrived FIRST (encoder emits AAC immediately,
// V follows after IDR). At pairing, TruncateBefore(V.first.PTS) keeps
// V intact and drops A's pre-V frames. This mirrors the symmetric case.
func TestPairingScenario_AudioArrivedFirst(t *testing.T) {
	q := NewFrameQueue()

	// 50 audio frames at 23ms cadence = ~1.15s of audio.
	for i := 0; i < 50; i++ {
		q.PushAudio(af(uint64(i * 23))) //nolint:gosec
	}
	// V's first frame arrives at PTS=1000.
	q.PushVideo(vf(1000, 1000, true))

	vFirst, _ := q.FirstVideo()
	vd, ad := q.TruncateBefore(vFirst.PTSms)
	if vd != 0 {
		t.Errorf("video dropped = %d, want 0", vd)
	}
	// First A.PTS < 1000: 0, 23, 46, ..., 989 — that's 44 frames at < 1000.
	// 23*43 = 989 < 1000; 23*44 = 1012 >= 1000. So 44 dropped.
	if ad != 44 {
		t.Errorf("audio dropped = %d, want 44", ad)
	}
}

// equalU64Slice compares two []uint64, treating nil and empty as equal.
func equalU64Slice(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
