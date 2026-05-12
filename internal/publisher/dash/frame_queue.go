// Package dash is the live DASH (fMP4) publisher.
//
// It segments wallclock-anchored AVPackets (or raw MPEG-TS chunks) into
// ISO-BMFF fragments and writes a live MPD with a sliding SegmentTimeline.
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
//
// Bounded retention: Push* enforces maxVideoFrames / maxAudioFrames by
// dropping the oldest frames when the cap is exceeded. Without this,
// pathological run-loop states — e.g. the DASH packager's
// behindPrevSegEnd gate blocking cuts while the per-segment dur drifts
// ahead of wallclock — let the queue grow without bound and pin tens
// of MB of AnnexB / ADTS bytes per stream. Production hit ~692 MB
// across 14 streams (May 12 incident: dash.handleH264 = 83% of heap),
// which the cap converts into a bounded drop instead.
const (
	// maxVideoFrames bounds per-track video retention. At 30 fps a
	// reasonable upper limit on un-popped video is ~30 s — enough to
	// absorb a few segments' worth of pending frames during pacing-gate
	// holds, well short of multi-minute accumulation that causes OOM.
	maxVideoFrames = 900
	// maxAudioFrames bounds per-track audio retention. AAC at 48 kHz
	// runs ~47 frames/s, so 900 frames ≈ 19 s of audio — paired with
	// the video cap, keeps both tracks roughly aligned in retention.
	maxAudioFrames = 900
)

// FrameQueue is the per-Packager FIFO of decoded video + audio access
// units waiting to be cut into segments. Each track has an independent
// capped slice; pushes drop the front when the cap is exceeded so a
// stalled segmenter cannot accumulate unbounded frames (root cause of
// the ~692 MB / 83 % heap leak observed in production on 2026-05-09).
//
// Not safe for concurrent use; the Packager mutex (`p.mu`) serialises
// pushes from the AV/TS ingress and pops from the segmenter run loop.
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
//
// Drops the oldest frame(s) when the queue would exceed maxVideoFrames.
// Returns the number of frames dropped (≥ 0). Production should normally
// see 0 here; non-zero indicates the segmenter has fallen too far behind
// and downstream MPD timeline integrity is already compromised — the
// drop is the lesser evil that prevents OOM.
func (q *FrameQueue) PushVideo(f VideoFrame) int {
	dropped := 0
	if len(q.video) >= maxVideoFrames {
		dropped = len(q.video) - maxVideoFrames + 1
		q.video = dropFrontVideo(q.video, dropped)
	}
	q.video = append(q.video, f)
	return dropped
}

// PushAudio appends an audio frame. Drops the oldest when over the cap;
// see PushVideo for the rationale.
func (q *FrameQueue) PushAudio(f AudioFrame) int {
	dropped := 0
	if len(q.audio) >= maxAudioFrames {
		dropped = len(q.audio) - maxAudioFrames + 1
		q.audio = dropFrontAudio(q.audio, dropped)
	}
	q.audio = append(q.audio, f)
	return dropped
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
		q.video = dropFrontVideo(q.video, vDropped)
	}
	aDropped := 0
	for aDropped < len(q.audio) && q.audio[aDropped].PTSms < thresholdMS {
		aDropped++
	}
	if aDropped > 0 {
		q.audio = dropFrontAudio(q.audio, aDropped)
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
	q.video = dropFrontVideo(q.video, n)
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
	q.audio = dropFrontAudio(q.audio, n)
	return out
}

// dropFrontVideo removes the first n elements of s WITHOUT the
// slice-forward leak: a plain `s[n:]` keeps the underlying array
// alive, so the dropped VideoFrame structs at indices [0, n) — and
// the AnnexB byte slices they reference — stay reachable through the
// backing memory until the next append-induced array growth. Under
// steady-state DASH frame rates this can pin tens of MB of keyframe
// payloads per stream and was the root cause of the May 12 memory
// climb to 95% after the b90eab7 deploy.
//
// We shift the remaining elements to the front, zero the (now
// duplicate) tail slots so their AnnexB references are released, and
// return a slice header of the smaller length. Capacity stays — that
// part is bounded by peak queue depth and doesn't accumulate.
func dropFrontVideo(s []VideoFrame, n int) []VideoFrame {
	if n <= 0 {
		return s
	}
	if n >= len(s) {
		clearVideoSlots(s)
		return s[:0]
	}
	remain := len(s) - n
	copy(s, s[n:])
	clearVideoSlots(s[remain:])
	return s[:remain]
}

func dropFrontAudio(s []AudioFrame, n int) []AudioFrame {
	if n <= 0 {
		return s
	}
	if n >= len(s) {
		clearAudioSlots(s)
		return s[:0]
	}
	remain := len(s) - n
	copy(s, s[n:])
	clearAudioSlots(s[remain:])
	return s[:remain]
}

func clearVideoSlots(s []VideoFrame) {
	for i := range s {
		s[i] = VideoFrame{}
	}
}

func clearAudioSlots(s []AudioFrame) {
	for i := range s {
		s[i] = AudioFrame{}
	}
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

// videoAt returns the video frame at index i (by value, no aliasing
// into the queue's storage) and a presence flag.
func (q *FrameQueue) videoAt(i int) (VideoFrame, bool) {
	if i < 0 || i >= len(q.video) {
		return VideoFrame{}, false
	}
	return q.video[i], true
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
