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
// Bounded retention: Push* enforces a time-span cap (maxQueueSpanMs)
// plus a frame-count safety net (videoFrameHardCap / audioFrameHardCap)
// by dropping the oldest frames. Without this, pathological run-loop
// states — e.g. the DASH packager's behindPrevSegEnd gate blocking
// cuts while the per-segment dur drifts ahead of wallclock — let the
// queue grow without bound and pin tens
// of MB of AnnexB / ADTS bytes per stream. Production hit ~692 MB
// across 14 streams (May 12 incident: dash.handleH264 = 83% of heap),
// which the cap converts into a bounded drop instead.
const (
	// maxQueueSpanMs is the per-track time-domain cap (newest.PTSms −
	// oldest.PTSms ≤ this value). PRIMARY cap.
	//
	// Reason it's time-based, not frame-count: a frame-count cap of N
	// produces unequal time-spans for V vs A because their frame rates
	// differ (25 fps video → 40 ms/frame; 44.1 kHz AAC → 23 ms/frame).
	// On a bursty source that saturates both caps, video span ≈ 36 s
	// and audio span ≈ 21 s, leaving audio_first 15 s AFTER video_first
	// permanently. The segmenter's audio cut decision uses
	// `audioCountAtOrBefore(cutPTSms_video)` where cutPTSms_video sits
	// near video_first + segDur, so the audio frames that should pair
	// with that video segment are NEVER counted (they've been dropped
	// off the front of the audio queue under cap pressure). Result: 5/9
	// production streams emitted exactly 1–2 audio segments at startup
	// then no more for 11 hours — root cause documented in
	// docs/DASH_INVESTIGATION_2026-05-13.md.
	//
	// 30 s is the smallest value that comfortably exceeds typical
	// segDur×live_window (4 s × 6 = 24 s) plus headroom for the
	// pacing-gate hold and one pre-roll segment.
	maxQueueSpanMs uint64 = 30_000

	// videoFrameHardCap / audioFrameHardCap are SECONDARY caps: an
	// absolute upper bound on slice length, sized well above what
	// maxQueueSpanMs would ever permit. They exist as an OOM safety net
	// for pathological PTS values (regression, overflow) where the
	// time-span computation can't trim the queue.
	//
	// At 60 fps × 60 s = 3600 video frames; at 48 kHz × 70 s = 3 281
	// audio frames. Both round up to round numbers below.
	videoFrameHardCap = 3600
	audioFrameHardCap = 3500
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
// Cap policy:
//   - PRIMARY: time-span. Drop oldest while
//     newest.PTSms − oldest.PTSms > maxQueueSpanMs.
//   - SECONDARY: frame-count hard cap (videoFrameHardCap) — OOM safety
//     net for pathological PTS values.
//
// Returns (droppedOldest, oooDetected). oooDetected=true when the
// pushed frame's PTSms is strictly less than the queue's prior tail,
// indicating either a non-monotonic source OR (commonly for H.264)
// a B-frame whose presentation order is later than its decode order.
// The flag is informational: B-frame OOO is expected and downstream
// segmenter math tolerates it; persistent audio OOO is anomalous.
func (q *FrameQueue) PushVideo(f VideoFrame) (int, bool) {
	dropped := q.trimVideoToSpan(f.PTSms)
	if len(q.video) >= videoFrameHardCap {
		extra := len(q.video) - videoFrameHardCap + 1
		q.video = dropFrontVideo(q.video, extra)
		dropped += extra
	}
	ooo := false
	if n := len(q.video); n > 0 && f.PTSms < q.video[n-1].PTSms {
		ooo = true
	}
	q.video = append(q.video, f)
	return dropped, ooo
}

// PushAudio appends an audio frame. See PushVideo for cap + OOO
// semantics. Audio has no B-frames so an OOO push genuinely indicates
// a non-monotonic source — production should never log this.
func (q *FrameQueue) PushAudio(f AudioFrame) (int, bool) {
	dropped := q.trimAudioToSpan(f.PTSms)
	if len(q.audio) >= audioFrameHardCap {
		extra := len(q.audio) - audioFrameHardCap + 1
		q.audio = dropFrontAudio(q.audio, extra)
		dropped += extra
	}
	ooo := false
	if n := len(q.audio); n > 0 && f.PTSms < q.audio[n-1].PTSms {
		ooo = true
	}
	q.audio = append(q.audio, f)
	return dropped, ooo
}

// trimVideoToSpan drops oldest video frames while the gap between
// newPTSms and the queue's oldest exceeds maxQueueSpanMs. Returns the
// drop count. No-op when the new frame's PTSms is < the oldest's
// (defensive: would otherwise underflow uint64 subtraction).
func (q *FrameQueue) trimVideoToSpan(newPTSms uint64) int {
	dropped := 0
	for len(q.video) > 0 {
		first := q.video[0].PTSms
		if newPTSms < first {
			break // OOO new frame; let append handle it without trimming
		}
		if newPTSms-first <= maxQueueSpanMs {
			break
		}
		q.video = dropFrontVideo(q.video, 1)
		dropped++
	}
	return dropped
}

// trimAudioToSpan is the audio counterpart of trimVideoToSpan.
func (q *FrameQueue) trimAudioToSpan(newPTSms uint64) int {
	dropped := 0
	for len(q.audio) > 0 {
		first := q.audio[0].PTSms
		if newPTSms < first {
			break
		}
		if newPTSms-first <= maxQueueSpanMs {
			break
		}
		q.audio = dropFrontAudio(q.audio, 1)
		dropped++
	}
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

// audioCountAtOrBeforeWithSkip is the diagnostic variant of
// audioCountAtOrBefore: returns the same count plus skippedQualifying =
// number of frames at indices ≥ n whose PTSms ≤ thresholdMS (i.e.,
// frames that would qualify but were excluded because the sequential
// scan stopped at an OOO frame). When skippedQualifying > 0 the queue
// is non-monotonic AND the OOO frame sits exactly at the count boundary
// — a smoking gun for hypothesis 1.
func (q *FrameQueue) audioCountAtOrBeforeWithSkip(thresholdMS uint64) (count, skippedQualifying int) {
	for count < len(q.audio) && q.audio[count].PTSms <= thresholdMS {
		count++
	}
	for i := count; i < len(q.audio); i++ {
		if q.audio[i].PTSms <= thresholdMS {
			skippedQualifying++
		}
	}
	return count, skippedQualifying
}

// AudioTailPTSms returns the newest queued audio frame's PTSms and
// presence flag. Diagnostic helper.
func (q *FrameQueue) AudioTailPTSms() (uint64, bool) {
	if len(q.audio) == 0 {
		return 0, false
	}
	return q.audio[len(q.audio)-1].PTSms, true
}

// VideoTailPTSms returns the newest queued video frame's PTSms.
func (q *FrameQueue) VideoTailPTSms() (uint64, bool) {
	if len(q.video) == 0 {
		return 0, false
	}
	return q.video[len(q.video)-1].PTSms, true
}
