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

## Open — separate bugs not in this branch's scope

### Stall handling without explicit boundary

When a raw-TS source stalls > `timeShiftBufferDepth` (24 s default), no
new segments are emitted, the sliding window drains, and the player
buffer underruns. Production servers (Flussonic, Shaka) emit an
explicit DASH `<Period>` boundary or HLS `EXT-X-DISCONTINUITY` on
source stall so the player resets cleanly. Open-Streamer has the
plumbing (`buffer.Packet.SessionStart` flows through to the packager's
`onSessionBoundary`) but **the ingestor doesn't trigger it on raw-TS
stall** — only on full reader reconnect.

Future work: add stall detection in the raw-TS reader (e.g. no packets
for 3 s while context still alive → emit `SetSession` with a "stall
recovery" reason).

### Raw-TS path bypasses the Normaliser

`internal/timeline.Normaliser` covers AV-path only ([worker.go:396](../internal/ingestor/worker.go#L396)
comment: `// nil-safe; only acts on AV path`). Raw-TS chunks
(`AVCodecRawTSChunk`) pass through `writeRawTSChunk` unchanged so any
upstream source drift / jitter / PTS jump propagates straight to the
DASH packager's queue.

Future work (Phase-4 of the
[REFACTOR_PROPOSAL.md](./REFACTOR_PROPOSAL.md) roadmap): demux at
ingest, run frames through the Normaliser, remux back to TS chunks
before the buffer-hub write. The Normaliser already implements the
production-grade pacing (forward-drift drop via `MaxAheadMs`,
backward-drift re-anchor via `MaxBehindMs`, cross-track snap,
session-boundary reset) — just needs wiring for the raw-TS adaptor.

Scope estimate: ~1000–1300 LOC + tests; 1–2 weeks of focused work.

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
