package coordinator

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// mixerDownstream returns a stream coded "mix" whose only input is
// `mixer://<videoCode>,<audioCode>`. Used by ABR-mixer detection tests.
func mixerDownstream(videoCode, audioCode string) *domain.Stream {
	return &domain.Stream{
		Code: "mix",
		Inputs: []domain.Input{
			{Priority: 0, URL: "mixer://" + videoCode + "," + audioCode},
		},
	}
}

// ─── detection ────────────────────────────────────────────────────────────────

func TestDetectABRMixer_RequiresMixerInput(t *testing.T) {
	t.Parallel()
	h := newABRHarness(t, abrUpstream(2))
	s := &domain.Stream{
		Code:   "mix",
		Inputs: []domain.Input{{Priority: 0, URL: "rtmp://origin/x"}},
	}
	_, _, ok := h.coord.detectABRMixer(s)
	assert.False(t, ok)
}

func TestDetectABRMixer_RequiresABRVideoUpstream(t *testing.T) {
	t.Parallel()
	// Video upstream is single-stream → mixer ABR mirror doesn't apply,
	// fall through to normal MixerReader pipeline.
	h := newABRHarness(t,
		&domain.Stream{Code: "vidSingle"},
		&domain.Stream{Code: "audSingle"},
	)
	_, _, ok := h.coord.detectABRMixer(mixerDownstream("vidSingle", "audSingle"))
	assert.False(t, ok)
}

func TestDetectABRMixer_RejectsDownstreamTranscoder(t *testing.T) {
	t.Parallel()
	// Downstream has its own transcoder → use normal MixerReader path
	// (best-rendition tap + encoder-driven ladder), not the mirror path.
	abr := abrUpstream(2)
	h := newABRHarness(t, abr, &domain.Stream{Code: "audio"})
	s := mixerDownstream("up", "audio")
	s.Transcoder = &domain.TranscoderConfig{
		Video: domain.VideoTranscodeConfig{Profiles: []domain.VideoProfile{{Width: 1280, Height: 720}}},
	}
	_, _, ok := h.coord.detectABRMixer(s)
	assert.False(t, ok)
}

func TestDetectABRMixer_HappyPath(t *testing.T) {
	t.Parallel()
	abr := abrUpstream(3)
	audio := &domain.Stream{Code: "radio"}
	h := newABRHarness(t, abr, audio)
	v, a, ok := h.coord.detectABRMixer(mixerDownstream("up", "radio"))
	require.True(t, ok)
	assert.Equal(t, domain.StreamCode("up"), v.Code)
	assert.Equal(t, domain.StreamCode("radio"), a.Code)
}

// ─── pipeline lifecycle ───────────────────────────────────────────────────────

func TestStart_ABRMixer_BypassesIngestorAndTranscoder(t *testing.T) {
	t.Parallel()
	abr := abrUpstream(2)
	audio := &domain.Stream{Code: "radio"}
	h := newABRHarness(t, abr, audio)

	dn := mixerDownstream("up", "radio")
	require.NoError(t, h.coord.Start(context.Background(), dn))
	defer h.coord.Stop(context.Background(), dn.Code)

	assert.False(t, h.mgr.IsRegistered("mix"), "manager must not be registered for ABR mixer")
	h.tc.mu.Lock()
	assert.Empty(t, h.tc.started, "transcoder must not be started for ABR mixer")
	h.tc.mu.Unlock()

	// Publisher must see the synthesized transcoder so it serves ABR.
	h.capPub.mu.Lock()
	require.Len(t, h.capPub.started, 1)
	pubStream := h.capPub.started[0]
	h.capPub.mu.Unlock()
	require.NotNil(t, pubStream.Transcoder, "publisher must receive synthesized transcoder")
	assert.Len(t, pubStream.Transcoder.Video.Profiles, 2)

	// Original downstream pointer must NOT be mutated.
	assert.Nil(t, dn.Transcoder)

	assert.True(t, h.coord.IsRunning("mix"))
}

func TestStart_ABRMixer_CreatesDownstreamRenditionBuffers(t *testing.T) {
	t.Parallel()
	abr := abrUpstream(3)
	audio := &domain.Stream{Code: "radio"}
	h := newABRHarness(t, abr, audio)

	require.NoError(t, h.coord.Start(context.Background(), mixerDownstream("up", "radio")))
	defer h.coord.Stop(context.Background(), "mix")

	for i := 0; i < 3; i++ {
		bid := buffer.RenditionBufferID("mix", buffer.VideoTrackSlug(i))
		_, err := h.buf.Subscribe(bid)
		assert.NoError(t, err, "downstream rendition buffer track_%d must exist", i+1)
	}
}

func TestStop_ABRMixer_DeletesBuffersAndUnregisters(t *testing.T) {
	t.Parallel()
	abr := abrUpstream(2)
	audio := &domain.Stream{Code: "radio"}
	h := newABRHarness(t, abr, audio)

	require.NoError(t, h.coord.Start(context.Background(), mixerDownstream("up", "radio")))
	require.True(t, h.coord.IsRunning("mix"))

	h.coord.Stop(context.Background(), "mix")
	assert.False(t, h.coord.IsRunning("mix"))

	for i := 0; i < 2; i++ {
		bid := buffer.RenditionBufferID("mix", buffer.VideoTrackSlug(i))
		_, err := h.buf.Subscribe(bid)
		assert.Error(t, err, "downstream rendition buffer track_%d must be deleted", i+1)
	}
}

func TestABRMixer_TapStopsOnContextCancel(t *testing.T) {
	t.Parallel()
	abr := abrUpstream(1)
	audio := &domain.Stream{Code: "radio"}
	h := newABRHarness(t, abr, audio)

	upRends := buffer.RenditionsForTranscoder(abr.Code, abr.Transcoder)
	for _, r := range upRends {
		h.buf.Create(r.BufferID)
	}
	h.buf.Create(audio.Code)

	require.NoError(t, h.coord.Start(context.Background(), mixerDownstream("up", "radio")))

	done := make(chan struct{})
	go func() {
		h.coord.Stop(context.Background(), "mix")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return — taps did not honour ctx cancellation")
	}
}

// TestStartABRMixerConcurrentStartIsIdempotent regression-tests the race
// where two callers both pass the unlocked IsRunning check, both reach
// startABRMixer, and the second caller's pub.Start fails with
// "stream already running" while the rollback path then deletes the
// rendition buffers the first start was using — observed in production
// as paired errors:
//
//	"abr mixer publisher: publisher: stream X already running"
//	"HLS ABR subscribe failed: buffer ... not found"
//
// The fix serialises startABRMixer on c.abrMu so concurrent callers see
// a consistent state: first one wins, the rest no-op.
func TestStartABRMixerConcurrentStartIsIdempotent(t *testing.T) {
	t.Parallel()
	abr := abrUpstream(2)
	audio := &domain.Stream{Code: "radio"}
	h := newABRHarness(t, abr, audio)
	defer h.coord.Stop(context.Background(), "mix")

	const callers = 10
	dn := mixerDownstream("up", "radio")
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			errs <- h.coord.Start(context.Background(), dn)
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err, "concurrent Start must be idempotent (no errors)")
	}

	// pub.Start must have been called exactly once across all racers.
	h.capPub.mu.Lock()
	pubStarts := len(h.capPub.started)
	h.capPub.mu.Unlock()
	assert.Equal(t, 1, pubStarts, "pub.Start should run once; got %d", pubStarts)

	// Rendition buffers must still exist (no rollback misfire).
	for i := 0; i < 2; i++ {
		bid := buffer.RenditionBufferID("mix", buffer.VideoTrackSlug(i))
		_, err := h.buf.Subscribe(bid)
		assert.NoError(t, err, "rendition buffer track_%d must survive concurrent Start", i+1)
	}

	assert.True(t, h.coord.IsRunning("mix"))
}

// ─── runtime status ──────────────────────────────────────────────────────────

func TestABRMixerRuntimeStatus_NotRunningReturnsFalse(t *testing.T) {
	t.Parallel()
	h := newABRHarness(t)
	_, ok := h.coord.ABRMixerRuntimeStatus("nonexistent")
	require.False(t, ok)
}

func TestABRMixerRuntimeStatus_DegradedBeforeFirstPacket(t *testing.T) {
	t.Parallel()
	abr := abrUpstream(1)
	audio := &domain.Stream{Code: "radio"}
	h := newABRHarness(t, abr, audio)
	require.NoError(t, h.coord.Start(context.Background(), mixerDownstream("up", "radio")))
	defer h.coord.Stop(context.Background(), "mix")

	rt, ok := h.coord.ABRMixerRuntimeStatus("mix")
	require.True(t, ok)
	require.True(t, rt.PipelineActive)
	require.Len(t, rt.Inputs, 1)
	assert.Equal(t, domain.StatusDegraded, rt.Inputs[0].Status,
		"no packets seen yet → degraded")
}

// ─── ptsRebaser ──────────────────────────────────────────────────────────────

// Two unrelated upstream sources (e.g. video PCR ≈ 12 s, audio PCR ≈ 9.8 Ms)
// must collapse onto a near-zero offset after rebase — that is exactly what
// fixes the "black + silent" symptom downstream players show when the gap
// between video.PTS and audio.PTS exceeds their A/V sync window.
func TestPTSRebaser_AlignsUnrelatedSourcesToWallClock(t *testing.T) {
	t.Parallel()
	t0 := time.Now()
	video := ptsRebaser{t0: t0}
	audio := ptsRebaser{t0: t0}

	v := domain.AVPacket{PTSms: 12_000, DTSms: 12_000}
	a := domain.AVPacket{PTSms: 9_800_000_000, DTSms: 9_800_000_000}
	video.apply(&v)
	audio.apply(&a)

	gapMs := int64(v.PTSms) - int64(a.PTSms)
	if gapMs < 0 {
		gapMs = -gapMs
	}
	require.Less(t, gapMs, int64(100),
		"after rebase, video and audio first packets must collapse to ~wall-clock arrival skew, got gap=%dms", gapMs)
}

// Inter-frame deltas inside one source must survive rebase — otherwise the
// downstream HLS segmenter would see PTS collapsing to a single value and
// the player would fail to schedule frames.
func TestPTSRebaser_PreservesInterFrameDeltas(t *testing.T) {
	t.Parallel()
	r := ptsRebaser{t0: time.Now()}
	p1 := domain.AVPacket{PTSms: 100_000, DTSms: 100_000}
	p2 := domain.AVPacket{PTSms: 100_033, DTSms: 100_033}
	p3 := domain.AVPacket{PTSms: 100_066, DTSms: 100_066}
	r.apply(&p1)
	r.apply(&p2)
	r.apply(&p3)

	require.Equal(t, uint64(33), p2.PTSms-p1.PTSms)
	require.Equal(t, uint64(66), p3.PTSms-p1.PTSms)
}

// Realistic B-frame: PTS<DTS (frame is decoded later than displayed). Verify
// the rebaser preserves PTS<DTS ordering after rewrite.
func TestPTSRebaser_PreservesBFramePTSDTSOrder(t *testing.T) {
	t.Parallel()
	r := ptsRebaser{t0: time.Now()}
	iframe := domain.AVPacket{PTSms: 1_000, DTSms: 1_000}
	r.apply(&iframe)
	bframe := domain.AVPacket{PTSms: 1_040, DTSms: 1_080}
	r.apply(&bframe)

	require.Less(t, bframe.PTSms, bframe.DTSms,
		"B-frame PTS<DTS ordering must survive rebase")
}

// Out-of-order packet whose DTS dips below the first-seen DTS must not
// underflow uint64 — ptsRebaserBaseMs provides the headroom.
func TestPTSRebaser_HandlesOutOfOrderBelowAnchor(t *testing.T) {
	t.Parallel()
	r := ptsRebaser{t0: time.Now()}
	first := domain.AVPacket{PTSms: 1_000, DTSms: 1_000}
	r.apply(&first)
	// Reordered packet: DTS 20 ms earlier than the anchor.
	reordered := domain.AVPacket{PTSms: 985, DTSms: 980}
	r.apply(&reordered)

	require.Less(t, reordered.DTSms, uint64(1<<40),
		"DTS must not underflow to a giant uint64; got %d", reordered.DTSms)
}
