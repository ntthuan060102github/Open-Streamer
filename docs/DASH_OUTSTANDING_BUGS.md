# DASH publisher — outstanding bugs

Status of issues discovered during the DASH refactor work on branch
`refactor/architecture`. Updated 2026-05-12.

## Fixed (shipped on `refactor/architecture`)

| Issue | Commit |
|---|---|
| MPD `<S t=…>` overlap on bursty raw-TS sources | `fix(dash): timeline-pace gate prevents MPD overlap on bursty sources` — adds `Packager.behindPrevSegEnd` gate in [packager.go](../internal/publisher/dash/packager.go) `tryCut`. Field-verified: bac_ninh_raw / test1 / test5 / test_copy / test_mixer / bac_ninh all moved from alternating ±5 s overlap/gap to uniform sub-second positive gaps. |
| First-IDR-past-segDur (was: trailing IDR) | `refactor(dash): pick first IDR past segDur, not latest in window` — [segmenter.go](../internal/publisher/dash/segmenter.go) `findIDRCutPoint`. Limits segment frame-span to ~segDur+GOP. |
| Audio under-emission ~20 % rate on raw-TS streams | `fix(dash): split bundled ADTS frames in handleAAC` — [packager.go](../internal/publisher/dash/packager.go) `splitADTSBundle` + integration into `handleAAC`. Root cause: gomedia's TSDemuxer delivers 4–8 ADTS frames per AAC PES; `writeAudioSegment` declared each AudioFrame as 1024 samples so a bundled PES collapsed the sample count by the bundling factor. Post-fix audio rate ~100 % on bac_ninh, test_copy, test1, test_mixer. |
| Video dur off-by-one-frame (per-segment 1-frame stutter) | Uncommitted (deployed via `-dirty` build) — [packager.go](../internal/publisher/dash/packager.go) `writeSegments` peeks `videoPTSAt(d.VideoCount)` before `PopVideo` and passes `nextPTSms` into `computeVideoSegDurTicks`. Field-verified: dur values moved from 5.96 → 6.00 s (bac_ninh), 7.88 → 8.00 s (bac_ninh_raw), 4.87 → 5.00 s (test2). |
| Backward-drift re-anchor disabled by default (test3 audio 229 s lag) | `fix(timeline): enable MaxBehindMs by default to catch lagging-track A/V split` — sets `DefaultPTSMaxBehindMs = 3000` in [defaults.go](../internal/domain/defaults.go) and wires it into both `normaliserConfig()` (ingestor) and `abrMixerNormaliserConfig()` (coordinator). Re-anchor jumps forward onto wallclock, no stuck-state pathology. |
| Raw-TS path bypassed the Normaliser | Merged from `refactor/architecture-with-s3-s4`: new [internal/ingestor/tsnorm](../internal/ingestor/tsnorm/tsnorm.go) package demuxes raw-TS chunks, runs each PES through `timeline.Normaliser.Apply`, remuxes back to TS bytes. Wired into `writeRawTSChunk` so UDP / HLS-pull / SRT / file / copy:// / mixer:// sources now get the same wallclock anchoring as the AV path. |
| Stall handling without explicit boundary | Merged from `refactor/architecture-with-s3-s4`: new [stall_watchdog.go](../internal/ingestor/stall_watchdog.go) goroutine spawned alongside `readLoop` ticks every 1 s, emits `SessionStartStallRecovery` via `buf.SetSession` when no write for > 15 s (configurable). Downstream consumers handle through the existing `onSessionBoundary` path — no consumer-side change. |

## In progress (this branch)

### Residual 50 ms tick-granularity gap between segments

Even after the dur fix, segments still show 0–80 ms gaps in MPD
timeline because `tfdt = wallclockTicks(now, AST)` is read at the
run-loop tick (50 ms granularity) and the pacing gate releases between
ticks. Players render this as a brief 1-frame stutter per segment
boundary.

**Two candidate solutions discussed; A recommended for "no freeze"
goal**:

- **A — Sequential tfdt after first segment**: `tfdt(N+1) = prev_end`.
  AST anchors segment 0 to wallclock; subsequent segments are
  cumulative on media time. Zero inter-segment gap (smooth playback)
  but MPD timeline can drift behind wallclock if source < realtime
  (mitigated by daily restart practice).
- **B — Hybrid**: `tfdt = prev_end` normally, jump to wallclock when
  drift > tolerance (e.g. 2 s). Bounded live-edge fidelity but
  introduces occasional explicit gaps that strict players may stall on.

A is the lower-risk pick for the "không treo hình" goal; B trades
smoothness for live-edge precision and adds multi-track desync risk on
mixer sources.

**Not yet shipped — pending user decision.**

## Open — limitations of the tsnorm raw-TS path

The merged `tsnorm` package closes the bypass gap but inherits some
gomedia / MPEG-TS PMT trade-offs that callers should be aware of.

### tsnorm pre-registers H.264 + AAC PIDs — phantom PIDs for non-canonical sources

`tsnorm.New` and `OnSession` eagerly call `muxer.AddStream` for H.264
and AAC so the very first PMT lists both PIDs (avoids the 400 ms
PMT-refresh window where a second-codec PES would land on an
unannounced PID — root cause of the test2 missing-audio incident
addressed in commit `cc93780`). H.265 is registered lazily on first
H.265 frame (commit `2a66a32`) to avoid a phantom HEVC PID breaking
H.264-only sources.

Trade-off: sources that carry only one of the canonical pair (e.g.
audio-only AAC stream, video-only H.264 stream, or an H.265 + AAC
source) still get a phantom PID for the unused codec in their output
PMT. Most players ignore unannounced/empty PIDs; strict demuxers
(ffprobe `-loglevel verbose`, dash.js in dev mode) surface them as
warnings. No correctness impact on Open-Streamer's canonical H.264+AAC
pipeline.

A more robust fix would build the PMT dynamically from observed
codecs (no pre-registration), but that requires either contributing to
gomedia or maintaining a fork. Tracked as future work; current
behaviour is acceptable for production.

### First ~400 ms of audio dropped on file:// + similar lazy-AddStream sources

gomedia's source-side TSMuxer refreshes PMT every 400 ms. When a
file:// source uses lazy AddStream (audio added on first audio frame,
after one or more video frames have already written), the first PMT
emitted by the SOURCE lists only video. tsnorm's demuxer doesn't
learn the audio PID until the source emits a refreshed PMT — so any
audio PES that arrives in that first 400 ms window is dropped on an
unrecognised PID.

For live sources this manifests as a brief silent gap at startup
(imperceptible). For short VOD files it's an audible click. Test
coverage via `TestProcess_LazyAddStream188ByteChunks` verifies the
recovery (audio appears after PMT refresh) but does not assert "no
drop" — explicitly accepted as a known limitation.

### Mixer V/A still split across PTS axes for clock-independent sources

`tsnorm` wallclock-anchors each track independently via the per-track
Normaliser. When a `mixer://` source combines two upstreams running on
independent clocks (e.g. `mixer://bac_ninh,test2` — HLS-pull video +
file VOD audio), the cross-track snap fires at seed time but each
track's later progress is driven by its own input PES rate. If audio
runs sub-realtime (file pace lagging), audio's output PTS structurally
trails video's PTS by the seed-time lag. `MaxBehindMs = 3000` clamps
this to ~3 s of drift in the steady state, but cannot align V and A
within a single HLS segment when source clocks diverge mid-stream.

Documented at the package level in [mixer.go](../internal/ingestor/pull/mixer.go):
> "AV sync is best-effort … lip-sync-critical scenarios are out of
> scope for v1."

For lip-sync-critical use cases, operators should transcode (let
FFmpeg correct V/A timestamps) rather than copy the mixed stream.

### Continuity counter discontinuity on OnSession

`tsnorm.OnSession` rebuilds the muxer from scratch (line 155). Output
TS continuity counters reset to 0 on every PID after a session
boundary. ITU-T H.222.0 strictly requires the `discontinuity_indicator`
in the adaptation field on the first packet after a CC reset; whether
gomedia sets this is not yet verified. Modern demuxers (hls.js,
Shaka, ffmpeg) tolerate the reset and just log a warning; TSDuck and
ffprobe `-loglevel verbose` may flag it. No observed playback impact.

## Open — separate bugs not in this branch's scope

### Large segment durations on test3 + bac_ninh_raw

`test3` (RTSP pull) and `bac_ninh_raw` (HLS pull) occasionally emit
segments with `d` in the 7–17 s range. Triggered when no IDR appears
within `maxFactor × segDur = 6 s` and the cold-start safety-net cut
drains the entire queue (see
[segmenter.go](../internal/publisher/dash/segmenter.go) `cutVideo`).
Player accepts the segment (pacing gate keeps timeline non-overlapping)
but the giant segment forces a 7–17 s player buffer chunk, hurting
startup latency.

Not a correctness bug — quality regression only. Mitigation: cap
safety-net drain to `(now − lastCut) + segDur` worth of frames so the
giant segment splits into multiple realistic ones.

### test2 player issue (not yet reproduced)

User report: test2 (file source) shows "load lâu rồi freeze" in browser
despite clean MPD. Needs reproduction with a specific player (dashjs
version? Shaka?) and browser logs before triage.

---

**Resume order recommendation**: ship Phase 2 (Option A sequential
tfdt) for immediate smoothness improvement, then evaluate user feedback
before Phase 3 (stall detection + boundary) and Phase 4 (Normaliser to
raw-TS).
