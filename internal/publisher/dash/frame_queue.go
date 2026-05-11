// Package dash is the live DASH (fMP4) publisher.
//
// It segments wallclock-anchored AVPackets (or raw MPEG-TS chunks) into
// ISO-BMFF fragments and writes a live MPD with a sliding SegmentTimeline.
// The new package replaces the previous `internal/publisher/dash_fmp4.go`
// monolith — see `docs/DASH_REWRITE.md` for the design rationale.
//
// Public surface is intentionally minimal: a `Packager` type that owns
// its own goroutine, started via `Run(ctx)` and stopped via context
// cancellation. The caller (`publisher.Service`) is responsible for
// subscribing to the buffer hub and feeding packets into the packager.
//
// Internal layout, file-by-file:
//
//   - frame_queue.go — per-track buffer with pairing semantics
//   - segmenter.go   — segment-cut decisions (segDur, IDR alignment)
//   - fmp4_writer.go — fmp4 fragment + init segment serialisation
//   - manifest.go    — MPD XML generation
//   - state.go       — WaitingForPairing → Live ↔ SessionBoundary
//   - packager.go    — public Packager type, run loop
//   - abr.go         — ABR master / shard wiring
//
// Each file is < 300 LOC, single-responsibility, exhaustive table tests.
package dash

// VideoFrame is one access unit handed to the packager. Annex-B bytes
// include the parameter-set prefix for IDR frames; downstream conversion
// to AVCC happens at fragment-write time.
//
// PTSms / DTSms are the Normaliser-anchored or upstream-PCR-derived
// timestamps, in milliseconds since the stream's session start. Both
// must be monotonic per track within a session.
type VideoFrame struct {
	AnnexB []byte
	PTSms  uint64
	DTSms  uint64
	// IsIDR marks H.264 IDR or H.265 IRAP access units. Segment cut
	// decisions snap to IDR boundaries so every emitted fragment starts
	// with a clean keyframe (the DASH startWithSAP="1" guarantee).
	IsIDR bool
}

// AudioFrame is one AAC access unit, ADTS header stripped, raw AAC
// payload. PTSms is the Normaliser-anchored playback time in ms.
type AudioFrame struct {
	Raw   []byte
	PTSms uint64
}

// FrameQueue buffers per-track frames between push (from the buffer hub
// subscriber) and drain (by the segmenter). It is NOT goroutine-safe;
// the packager's run loop holds it on a single goroutine.
//
// The queue owns no clock and makes no segmenting decisions — those
// are the segmenter's job. The queue's primitives are:
//
//   - PushVideo / PushAudio: append a frame.
//   - VideoLen / AudioLen: query queue depth.
//   - FirstVideo / FirstAudio: peek at the oldest frame (without removing).
//   - TruncateBefore: drop frames older than a PTS threshold (used at
//     pairing resolution so both queues start at a shared media moment).
//   - PopVideo / PopAudio: drain the oldest N frames and return them.
//   - VideoSpanMS / AudioSpanMS: ptsMS span between oldest and newest
//     queued frame on the track. Used by the segmenter to decide when
//     there's a full segDur of content available.
type FrameQueue struct {
	video []VideoFrame
	audio []AudioFrame
}

// NewFrameQueue returns an empty queue.
func NewFrameQueue() *FrameQueue {
	return &FrameQueue{}
}

// PushVideo appends a video frame. The queue takes ownership of f.AnnexB
// — callers must not retain a reference.
func (q *FrameQueue) PushVideo(f VideoFrame) {
	q.video = append(q.video, f)
}

// PushAudio appends an audio frame.
func (q *FrameQueue) PushAudio(f AudioFrame) {
	q.audio = append(q.audio, f)
}

// VideoLen returns the number of queued video frames.
func (q *FrameQueue) VideoLen() int { return len(q.video) }

// AudioLen returns the number of queued audio frames.
func (q *FrameQueue) AudioLen() int { return len(q.audio) }

// FirstVideo returns the oldest queued video frame and true, or a
// zero-value VideoFrame and false if the queue is empty.
func (q *FrameQueue) FirstVideo() (VideoFrame, bool) {
	if len(q.video) == 0 {
		return VideoFrame{}, false
	}
	return q.video[0], true
}

// FirstAudio returns the oldest queued audio frame and true, or a
// zero-value AudioFrame and false if the queue is empty.
func (q *FrameQueue) FirstAudio() (AudioFrame, bool) {
	if len(q.audio) == 0 {
		return AudioFrame{}, false
	}
	return q.audio[0], true
}

// TruncateBefore drops every frame on both tracks whose PTSms is
// strictly less than thresholdMS. Used at pairing resolution: when V
// arrived first and A finally joined at media-time T, calling
// TruncateBefore(T) drops V's pre-T frames so both queues start at T.
//
// Returns (vDropped, aDropped) — the number of frames removed from each
// track. The packager logs these for observability.
func (q *FrameQueue) TruncateBefore(thresholdMS uint64) (int, int) {
	vDropped := 0
	for vDropped < len(q.video) && q.video[vDropped].PTSms < thresholdMS {
		vDropped++
	}
	if vDropped > 0 {
		q.video = q.video[vDropped:]
	}
	aDropped := 0
	for aDropped < len(q.audio) && q.audio[aDropped].PTSms < thresholdMS {
		aDropped++
	}
	if aDropped > 0 {
		q.audio = q.audio[aDropped:]
	}
	return vDropped, aDropped
}

// PopVideo removes and returns the oldest n video frames. If n exceeds
// the queue length, returns whatever is queued. The returned slice
// shares no underlying array with the queue — callers may retain it.
func (q *FrameQueue) PopVideo(n int) []VideoFrame {
	if n <= 0 || len(q.video) == 0 {
		return nil
	}
	if n > len(q.video) {
		n = len(q.video)
	}
	out := make([]VideoFrame, n)
	copy(out, q.video[:n])
	q.video = q.video[n:]
	return out
}

// PopAudio removes and returns the oldest n audio frames.
func (q *FrameQueue) PopAudio(n int) []AudioFrame {
	if n <= 0 || len(q.audio) == 0 {
		return nil
	}
	if n > len(q.audio) {
		n = len(q.audio)
	}
	out := make([]AudioFrame, n)
	copy(out, q.audio[:n])
	q.audio = q.audio[n:]
	return out
}

// VideoSpanMS returns PTSms of the newest queued video frame minus
// PTSms of the oldest. Zero when fewer than 2 frames are queued.
//
// The span is computed from PTS (not DTS) so a B-frame queue with
// PTS<DTS reordering doesn't underreport — the segmenter cares about
// displayable wallclock span.
func (q *FrameQueue) VideoSpanMS() uint64 {
	if len(q.video) < 2 {
		return 0
	}
	first := q.video[0].PTSms
	last := q.video[len(q.video)-1].PTSms
	if last <= first {
		return 0
	}
	return last - first
}

// AudioSpanMS returns PTSms of newest minus oldest audio frame. Zero
// when fewer than 2 frames are queued.
func (q *FrameQueue) AudioSpanMS() uint64 {
	if len(q.audio) < 2 {
		return 0
	}
	first := q.audio[0].PTSms
	last := q.audio[len(q.audio)-1].PTSms
	if last <= first {
		return 0
	}
	return last - first
}

// LastIDRIndex returns the index of the most recently queued IDR frame,
// or -1 if no IDR is queued. Used by the segmenter to find the
// trailing-edge IDR for a clean segment cut.
func (q *FrameQueue) LastIDRIndex() int {
	for i := len(q.video) - 1; i >= 0; i-- {
		if q.video[i].IsIDR {
			return i
		}
	}
	return -1
}

// videoPTSAt returns the PTSms of the video frame at index i, or
// (0, false) when the index is out of range. Package-internal so the
// segmenter can peek without exposing the underlying slice.
func (q *FrameQueue) videoPTSAt(i int) (uint64, bool) {
	if i < 0 || i >= len(q.video) {
		return 0, false
	}
	return q.video[i].PTSms, true
}

// audioCountAtOrBefore returns the number of OLDEST audio frames whose
// PTSms is ≤ thresholdMS. Used by the segmenter to drain the audio
// frames that share the video segment's time window.
func (q *FrameQueue) audioCountAtOrBefore(thresholdMS uint64) int {
	n := 0
	for n < len(q.audio) && q.audio[n].PTSms <= thresholdMS {
		n++
	}
	return n
}
