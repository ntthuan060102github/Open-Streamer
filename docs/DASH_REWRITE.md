# DASH Publisher Rewrite — Design Doc

**Status:** ✅ Implemented on `refactor/architecture` (2026-05). Live code in
[internal/publisher/dash/](../internal/publisher/dash/). Subsequent fixes on
top of this design (pacing gate, ADTS bundle splitter, video dur via next-
frame peek) live in the same branch — see
[DASH_OUTSTANDING_BUGS.md](./DASH_OUTSTANDING_BUGS.md) for ship status.
Original `internal/publisher/dash_fmp4.go` has been deleted.

**Target branch:** `refactor/architecture`
**Replaces:** `internal/publisher/dash_fmp4.go` (removed)

## 1. Why rewrite

Current `dash_fmp4.go` has accumulated multiple overlapping state machines
that each handle a slice of "timeline correctness":

| State machine | Owns |
|---|---|
| Per-track decode anchors | `originDTSms`, `videoNextDecode`, `audioNextDecode`, `videoFirstDTSmsSet`, `audioFirstDTSmsSet` |
| Drift cap (V) | `videoSkipUntilIDR`, `shouldSkipVideoLocked`, projected-end vs wallclock comparison |
| Drift cap (A) | `shouldSkipAudioLocked`, similar projected-end logic |
| Inter-frame jump guard | `dashSourceSwitchJumpMs`, in-flow PTS jump detection + queue flush |
| Init segments | `videoInit`, `audioInit`, `videoPS` accumulator, `tryInitVideoLocked` retry |
| Sliding window | `onDiskV`, `onDiskA`, `vSegDurs`, `aSegDurs`, `vSegStarts`, `aSegStarts`, per-track trim |
| ABR master | `dashABRMaster` integration, per-shard manifest update |
| Session boundary | `resetTimelineOnSessionBoundary` (Phase 3 addition) |
| V/A pairing | `firstFlushDeadline`, `shouldHoldForPairingLocked` (Phase 3.1 addition) |

Each addition is reasonable in isolation but they interact non-locally.
Recent investigations have found:

1. **Per-track origin trade-off** (commit `2872236`): shared `originDTSms`
   bakes in arrival-order skew. Switched to per-track origin (each track
   seeds at tfdt=0). Improved bac_ninh_raw from 1955 ms → 9 ms skew but
   transcoded streams still showed seconds of residual skew.
2. **Pairing gate** (commit `0b67d7d`): hold first segment flush until
   both tracks ready or timeout. Improved transcoded streams by ~50%
   but cumulative-tfdt divergence remained.
3. **Queue-content divergence**: during the pairing window, the earlier
   track accumulates more frames than the later track. When the gate
   opens, the first segments are different lengths — the gap is baked
   into the timeline forever. (Was about to add another patch for
   this — user said "rewrite instead".)
4. **Sliding-window drift**: trim runs per-track, so V's and A's
   visible windows can drift apart in cumulative tfdt over time.

The pattern: each fix is local, but the system needs a **single
authoritative clock** and **explicit pairing semantics** that the
current code does not provide.

## 2. Design principles

1. **One clock, not two.** A single `tfdt` advancement model: tfdt =
   (wallclock now − AST) × timescale. No per-track origins. No
   drift between V and A.
2. **AST is set ONCE, at successful V/A pairing.** Not on first
   any-segment flush.
3. **Explicit state machine.** Three states: `WaitingForPairing` →
   `Live` → (potentially `SessionBoundary` → `Live` again). Each
   transition has explicit pre/post conditions.
4. **Input agnostic.** AV-path (Normaliser-anchored) and raw-TS path
   (TS chunks) feed the same internal frame queue. Both have their
   PTS interpreted the same way: "media time since session start".
5. **No drift cap, no jump guard, no skip latches.** The Normaliser
   (for AV-path) and DASH packager's own segment-emit-pacing handle
   timeline correctness. Drift cap was a defense against bugs we are
   now fixing upstream.
6. **V and A windows trim together.** Sliding window is per-stream
   (not per-track) so cumulative tfdt cannot diverge.
7. **Test-driven.** Each public surface has a table-driven test
   asserting the contract. Bug-reproducer tests for the production
   incidents.

## 3. Module layout

```text
internal/publisher/dash/
  packager.go         — public DASHPackager type, run loop, state machine
  frame_queue.go      — frame buffer with V/A pairing semantics
  segmenter.go        — segment-cut decisions (segDur, IDR alignment)
  fmp4_writer.go      — fmp4 fragment + init segment serialisation
  manifest.go         — MPD XML generation
  state.go            — state machine + transition rules
  abr.go              — ABR master / shard wiring (mostly preserved)
  packager_test.go    — table-driven contract tests
```

Each file is < 300 LOC, single responsibility, exhaustive tests.

## 4. Public surface (mostly unchanged)

```go
// (Used by publisher.Service to spawn / stop DASH per stream.)
func NewPackager(...) *Packager
func (p *Packager) Run(ctx context.Context) error
func (p *Packager) Stop()
```

Subscription, ABR wiring, manifest path, config — all preserved as-is
so `publisher/service.go` and `publisher/dash_abr.go` callers don't
change.

## 5. State machine

```text
            ┌─────────────────────┐
            │  WaitingForPairing  │  ← packager just started
            └─────────┬───────────┘
                      │ both V+A first packet OR 3s timeout
                      ▼
            ┌─────────────────────┐
            │        Live         │  ← AST set, segments emitting
            └─────────┬───────────┘
                      │ buffer.Packet.SessionStart = true
                      ▼
            ┌─────────────────────┐
            │  SessionBoundary    │  ← flush in-progress + reset
            └─────────┬───────────┘
                      │ next packet observed
                      ▼
            ┌─────────────────────┐
            │   Live (new session)│  ← AST preserved; segN preserved
            └─────────────────────┘
```

Transition rules:

- **WaitingForPairing → Live** (true pairing): both init segments built
  AND each queue has ≥ 1 frame. AST = `time.Now()`. Both queues
  truncated to drop pre-pairing accumulation. Next segment emitted from
  this moment.
- **WaitingForPairing → Live** (timeout): 3 s elapsed since first init
  appeared, only one track present. AST = `time.Now()`. Proceed
  single-track. (Video-only or audio-only stream.)
- **Live → SessionBoundary**: `pkt.SessionStart == true`. Flush
  in-progress segment (if any). Reset per-track decode anchors but
  KEEP AST, vSegN/aSegN, sliding window, init segments.
- **SessionBoundary → Live**: next packet observed. Treats as live;
  the buffer-hub-level SessionStart cue has already been honoured.

## 6. Single clock model

```text
At Live state, the packager owns ONE wallclock anchor: `astWall`,
set when WaitingForPairing → Live fires.

For any incoming AV packet with PTSms = P:
    media_time_ms = P - sessionStartPTS        (per-track local)
    tfdt_video    = (now - astWall) × 90000    ← wallclock-driven,
    tfdt_audio    = (now - astWall) × audioSR    not PTS-driven

Segment cut decisions use PTS (IDR alignment, frame count) but
tfdt timestamps in the fragments use wallclock-elapsed-since-AST.
```

This eliminates the "videoNextDecode drifts ahead of A over time"
class of bugs entirely — both tracks read the same clock.

Trade-off: per-sample dur within a fragment becomes synthetic
(distributed evenly across frame count) rather than from PTS deltas.
Players don't notice; tfdt is what matters for playback timing.

## 7. Frame queue contract

```go
type frameQueue struct {
    video []videoFrame  // {AVCC bytes, IDR flag, ptsMS}
    audio []audioFrame  // {ADTS-stripped bytes, ptsMS}
    
    // Set at WaitingForPairing→Live; drop earlier frames here.
    sessionStartPTS uint64
}

// Push always succeeds. Tracks per-codec init readiness.
func (q *frameQueue) PushVideo(frame []byte, pts, dts uint64, isIDR bool)
func (q *frameQueue) PushAudio(frame []byte, pts uint64)

// At pairing, truncate so each queue starts at the LATER track's first
// frame's wallclock arrival. Returns true if both tracks have data.
func (q *frameQueue) ResolvePairing(audioReady, videoReady bool) bool

// At segment-cut, drain enough frames for one segment. IDR-aligned
// for video. Sample-count-aligned for audio.
func (q *frameQueue) DrainSegment(segDur time.Duration) (vFrames, aFrames)
```

## 8. Segmenter contract

```go
// Decides if it's time to cut a segment, based on:
//   - wallclock elapsed since last segment (segDur target)
//   - video queue has ≥ segDur of frames AND ends at IDR
//   - audio queue has ≥ matching frame count
// Returns the FRAMES to flush, not the bytes.
func (s *segmenter) Cut(now time.Time, q *frameQueue) (vFrames, aFrames, ok bool)
```

No drift cap. No jump guard. The pairing + single-clock model removes
the need for these.

## 9. Migration

Replace the existing `dash_fmp4.go` outright. Per the refactor guidance,
the new design is validated by its own contract tests + production
monitoring of invariants (DASH MPD V/A skew, drift vs wallclock,
segment cadence), **not** by parity with the old implementation. The
old implementation is the source of the bugs we are fixing — matching
its behaviour would preserve them.

Step order on `refactor/architecture`:

1. **Step 1: Frame queue + tests.** New `internal/publisher/dash/` package.
   `frame_queue.go` with table tests covering pairing, truncation,
   drain. Standalone, no other module touches.
2. **Step 2: Segmenter + fmp4 writer + tests.** Init segment + fragment
   serialisation. Tests use synthetic frames.
3. **Step 3: Manifest + tests.** MPD generation. ABR master wiring
   (rebuilt in the new module; the existing `dashABRMaster` type is
   replaced by a simpler one inside the new package).
4. **Step 4: State machine + run loop + integration tests.** Tie it
   together. Integration tests with synthetic streams covering each
   scenario (transcoded, raw-TS, mixer, push, ABR).
5. **Step 5: Replace `dash_fmp4.go`.** Single commit deleting the old
   file and switching `publisher/service.go` to the new package.
   At this point `make build` / `make test` exercises only the new
   code.
6. **Step 6: Deploy + probe + monitor.** Deploy to staging. Run
   `probe_all.sh` / `probe_liveedge.sh`. Watch for DASH skew metrics
   over 24-48 hours.

Each step is its own commit, all on `refactor/architecture`. `main`
stays untouched.

## 10. Risk / mitigation

| Risk | Mitigation |
|---|---|
| New module misses an edge case the old one happened to handle | Bug-reproducer tests for every known production scenario (transcoded encoder warmup, mixer V/A race, raw-TS passthrough, session boundary, codec config change). Tests fail → fix before deploy. |
| ABR integration breaks | ABR master rebuilt in the new package; tests assert per-shard manifest update + master playlist consistency. |
| Rollback to `main` if needed | `main` still points at the pre-refactor baseline (commit `7f43fc1`). `git checkout main` is the rollback. We don't preserve old DASH code in the new branch. |
| Player compatibility | Test against dashjs (primary), Shaka. Add screenshot/log capture during smoke test. |
| MPD format drift | Player compatibility test catches format issues. The MPD XML shape is well-defined by the DASH spec, not by the old code. |

## 11. Out of scope (separate work)

- Init segment rebuild on codec change (e.g. AAC sample rate flip). The
  buffer-hub `SetSession` currently passes nil Video/Audio configs;
  v2 reads them when they're populated, otherwise behaves as v1 today.
- HLS or RTSP/RTMP serve rewrite. Each has its own coupling and
  scope; one rewrite at a time.

## 12. Effort estimate

- Step 1 (skeleton + flag): 0.5 day
- Step 2 (queue + tests): 1 day
- Step 3 (segmenter + fmp4 + tests): 1 day
- Step 4 (manifest + ABR + tests): 0.5 day
- Step 5 (state machine + integration tests): 1 day
- Step 6 (deploy + probe): 0.5 day
- Step 7 (delete v1): 0.5 day

Total: **~5 days of focused work**, spread across whatever calendar
time works given user testing cadence.

## 13. Decision needed

User to approve:

- [ ] Module layout (`internal/publisher/dash/` subdir)
- [ ] Single-clock model (tfdt from wallclock, not PTS)
- [ ] Pairing-and-truncate at WaitingForPairing→Live
- [ ] Feature flag rollout (no big-bang switch)
- [ ] No drift cap / jump guard in v2 (rely on Normaliser + segmenter)

Or propose alternative.
