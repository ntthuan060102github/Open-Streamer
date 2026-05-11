package dash

import "time"

// Segmenter decides when and how to cut a DASH media segment.
//
// The decision is intentionally simple compared to the v1 implementation:
//
//   - Cut at the most recent IDR once the video queue has accumulated
//     at least segDur of content. Every emitted fragment starts with a
//     keyframe (DASH startWithSAP="1").
//   - Cut on a wallclock safety-net (segDur × maxFactor since the last
//     cut) when no IDR has arrived — produces a non-IDR-aligned
//     fragment but prevents the live edge from stalling forever on a
//     pathological long-GOP source.
//   - Audio is sample-count-aligned: drain whichever audio frames have
//     PTSms in the video segment's time range.
//
// No drift cap, no inter-frame jump guard, no skip-until-IDR latch — the
// Normaliser anchors timestamps wallclock-accurately upstream, and the
// segmenter trusts what arrives. If something is wrong with the input,
// it's a Normaliser-level bug and gets fixed there, not papered over
// here with a defense layer.
type Segmenter struct {
	// segDur is the target segment duration (e.g. 2s for low-latency,
	// 4s for normal HLS/DASH live).
	segDur time.Duration
	// maxFactor multiplies segDur to set the safety-net wallclock
	// deadline. 3-4× is a good balance: tolerates long-GOP sources
	// (8-12s GOP for a 2s target) without producing many non-IDR cuts.
	maxFactor int

	lastCut time.Time
}

// NewSegmenter returns a Segmenter targeting segDur per segment with
// maxFactor*segDur as the no-IDR safety deadline.
func NewSegmenter(segDur time.Duration, maxFactor int) *Segmenter {
	if maxFactor <= 1 {
		maxFactor = 3
	}
	return &Segmenter{segDur: segDur, maxFactor: maxFactor}
}

// MarkCut records that a segment has just been emitted. The safety-net
// deadline resets from now. Callers invoke this immediately after a
// successful write so the next cut is measured from the new wallclock.
func (s *Segmenter) MarkCut(now time.Time) {
	s.lastCut = now
}

// Reset clears any cut state — used at WaitingForPairing → Live or at
// SessionBoundary to ensure the first post-reset cut is measured from
// the new live timeline.
func (s *Segmenter) Reset() {
	s.lastCut = time.Time{}
}

// CutDecision reports whether the segmenter wants a cut now and what
// frames to drain. ok=false means "keep buffering".
type CutDecision struct {
	// Ok is true when a cut should happen; false when the segmenter
	// wants more frames.
	Ok bool

	// VideoCount is the number of OLDEST video frames to drain from
	// the queue. The last drained frame is the trailing IDR (when
	// available) so the NEXT segment can start at a fresh IDR.
	//
	// On a safety-net cut with no IDR available, VideoCount = the
	// entire video queue at the moment of the cut.
	VideoCount int

	// AudioCount is the number of OLDEST audio frames to drain. Equal
	// to the count whose PTSms ≤ the last drained video frame's PTSms;
	// callers may overshoot by one frame to span the cut moment
	// cleanly. On audio-only or video-only streams the missing track
	// contributes 0.
	AudioCount int

	// IsIDRAligned reports whether the cut lands at an IDR boundary
	// (clean SAP) or a wallclock safety-net (non-SAP). Manifests can
	// emit startWithSAP="1" globally regardless — the safety-net case
	// is rare and players tolerate the occasional non-SAP fragment.
	IsIDRAligned bool
}

// Cut consults the queue and decides whether to emit a segment now.
// Pure function (no clock side-effect — caller MarkCut() to advance).
//
// Rules, in order of preference:
//
//  1. Video queue contains ≥ segDur of PTSms span AND has an IDR after
//     the first segDur worth of content: cut up to and including the
//     LAST IDR within the segDur+ window. IDR-aligned, no audio loss.
//  2. Video queue spans ≥ maxFactor × segDur with no usable IDR AND
//     enough wallclock has elapsed since lastCut: safety-net cut.
//     Drains everything queued; the next segment starts wherever the
//     next IDR appears.
//  3. Audio-only stream (no video init / no video frames): cut on
//     audio span ≥ segDur.
//  4. Otherwise: hold.
func (s *Segmenter) Cut(now time.Time, q *FrameQueue, haveVideo, haveAudio bool) CutDecision {
	segDurMS := uint64(s.segDur.Milliseconds()) //nolint:gosec // segDur > 0 by construction
	maxElapsedMS := segDurMS * uint64(s.maxFactor)

	switch {
	case haveVideo && q.VideoLen() > 0:
		return s.cutVideo(now, q, segDurMS, maxElapsedMS, haveAudio)
	case haveAudio && q.AudioLen() > 0:
		return s.cutAudioOnly(q, segDurMS)
	}
	return CutDecision{}
}

// cutVideo handles the dual-track or video-only stream. Returns the
// IDR-aligned cut when possible, the safety-net cut when wallclock has
// run out, or {Ok:false} when more content is needed.
func (s *Segmenter) cutVideo(now time.Time, q *FrameQueue, segDurMS, maxElapsedMS uint64, haveAudio bool) CutDecision {
	idx := s.findIDRCutPoint(q, segDurMS)
	if idx >= 0 {
		return s.buildCutDecision(q, idx, haveAudio, true)
	}

	// Safety-net: enough wallclock elapsed since last cut without an
	// IDR landing inside the desired window. Use the caller-provided
	// `now` so tests with a synthetic clock measure deterministically.
	if !s.lastCut.IsZero() {
		elapsed := now.Sub(s.lastCut).Milliseconds()
		if elapsed > 0 && uint64(elapsed) >= maxElapsedMS { //nolint:gosec // bounded by branch above
			return s.buildCutDecision(q, q.VideoLen()-1, haveAudio, false)
		}
	}
	// Cold-start safety-net: lastCut is zero (first cut ever). Allow
	// the queue to grow up to maxElapsedMS of PTS span before forcing
	// a cut; before that, wait for an IDR.
	if s.lastCut.IsZero() && q.VideoSpanMS() >= maxElapsedMS {
		return s.buildCutDecision(q, q.VideoLen()-1, haveAudio, false)
	}
	return CutDecision{}
}

// findIDRCutPoint searches for the trailing IDR within the segDur
// window. Returns the index of the LAST IDR in the video queue whose
// PTSms is at least segDurMS past the first frame's PTSms, or -1 when
// no IDR satisfies that bound.
//
// Picking the LAST IDR (rather than the first) maximises segment
// duration up to segDur — short GOPs that have 2-3 IDRs within segDur
// produce one fragment of segDur, not one fragment per GOP.
func (s *Segmenter) findIDRCutPoint(q *FrameQueue, segDurMS uint64) int {
	first, ok := q.FirstVideo()
	if !ok {
		return -1
	}
	if q.VideoSpanMS() < segDurMS {
		return -1
	}
	last := q.LastIDRIndex()
	if last < 0 {
		return -1
	}
	idrPTS, ok := q.videoPTSAt(last)
	if !ok || idrPTS-first.PTSms < segDurMS {
		return -1
	}
	return last
}

// buildCutDecision packages the cut-up-to-frame-N answer.
func (s *Segmenter) buildCutDecision(q *FrameQueue, lastVideoIdx int, haveAudio bool, idrAligned bool) CutDecision {
	if lastVideoIdx < 0 {
		return CutDecision{}
	}
	vCount := lastVideoIdx + 1
	cutPTSms, _ := q.videoPTSAt(lastVideoIdx)
	aCount := 0
	if haveAudio {
		// Include audio frames whose PTSms is ≤ the cut PTS plus one
		// frame of slack, so the audio segment spans the same wallclock
		// window as the video segment.
		aCount = q.audioCountAtOrBefore(cutPTSms)
	}
	return CutDecision{
		Ok:           true,
		VideoCount:   vCount,
		AudioCount:   aCount,
		IsIDRAligned: idrAligned,
	}
}

// cutAudioOnly handles audio-only streams. Cuts when audio span >= segDur.
func (s *Segmenter) cutAudioOnly(q *FrameQueue, segDurMS uint64) CutDecision {
	if q.AudioSpanMS() < segDurMS {
		return CutDecision{}
	}
	return CutDecision{
		Ok:           true,
		VideoCount:   0,
		AudioCount:   q.AudioLen(),
		IsIDRAligned: true, // audio-only is always cleanly cuttable
	}
}
