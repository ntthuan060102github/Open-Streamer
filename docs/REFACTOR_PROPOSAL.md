# Refactor Proposal — Stream Session & Timeline Normalisation

**Status**: ALL FIVE PHASES implemented on `refactor/architecture`
(2026-05). Branch remains separate from `main` pending final
post-deploy soak; see [DASH_OUTSTANDING_BUGS.md](./DASH_OUTSTANDING_BUGS.md)
for residual quality-of-service items unrelated to the refactor.

| Phase | Status | Landed as |
|---|---|---|
| 1 — `StreamSession` dual-write across buffer hub | ✅ | `refactor(phase-1): introduce StreamSession dual-write across buffer hub` |
| 2 — Collapse 3 PTS rebasers into `timeline.Normaliser` | ✅ | `refactor(phase-2): collapse 3 PTS rebasers into a single timeline.Normaliser` |
| 3 — Publishers dispatch on `buffer.Packet.SessionStart` | ✅ | `refactor(phase-3): publishers dispatch on buffer.Packet.SessionStart` |
| 4 — Extend Normaliser to cover raw-TS path | ✅ | `internal/ingestor/tsnorm` wraps `timeline.Normaliser` for raw-TS chunks; `writeRawTSChunk` routes UDP / HLS-pull / SRT / file / `copy://` / `mixer://` through it. Plus stall watchdog → SessionStartStallRecovery for the silent-source case. |
| 5 — Cleanup: remove `AVPacket.Discontinuity`, `hlsFailoverGen`, stale comments | ✅ | This commit. AVPacket field deleted; Normaliser no longer writes the flag; re-anchor events stay on `LastDiagnostic()` for telemetry; CLAUDE.md / package docs synced. |

The text below is the original design doc kept for historical reference;
the exact API names in the implementation may have evolved slightly. Always
prefer reading the current code (`internal/timeline/normaliser.go`,
`internal/buffer/`, `internal/ingestor/worker.go`) for the latest contracts.

**Author**: Refactor working session, ca29084 baseline
**Audience**: Maintainers of `internal/{buffer, ingestor, publisher, coordinator}`

This document is the design-only deliverable of **Phase 0**. It does not
change behaviour. It exists so we agree on the target architecture and the
migration path BEFORE touching code.

---

## 1. Why this exists

Across recent debugging sessions a recurring bug class kept surfacing:

| Symptom | Reproducer |
|---|---|
| Long-runtime DASH tfdt drift (player buffer = 10 000+ s after 2-3 days) | natural runtime |
| Source 100 ms V/A skew amplified to 1-2 s in DASH tfdt | bac_ninh + republishers |
| Input switch injects 15 000 s "future" content (single frame stretched to `segDur`) | input source change |
| AAC config change mid-stream → init segment stale → MEDIA_ERR_DECODE | upstream encoder restart |
| test3 RTSP republish: rebaser hard-re-anchor fires every ~2 s, creating discontinuity-per-segment HLS | rebaser/RTSP-serve interaction |

Each was traced to a different module. Fixing one in isolation tended to
introduce regressions in another. After mapping the codebase, the root cause
is **architectural**, not a collection of unrelated bugs: timeline state and
session lifecycle are scattered across 13+ modules with no shared invariant.
See §2 for the inventory.

---

## 2. Current state inventory

### 2.1 Timestamp / baseline / drift state — by module

Every module below maintains its own concept of "the source's clock" and
adjusts incoming PTS/DTS independently. None of them share state; they
communicate (when at all) through ad-hoc flags.

| Module | State owned | Triggers re-anchor / reset |
|---|---|---|
| `internal/ingestor/ptsrebaser/rebaser.go` | Per-track `inputOrigin`, `outputAnchor`, `lastOutputDts`; wallclock anchor; `JumpThresholdMs`; `MaxAheadMs`; cross-track snap | Hard re-anchor on `|drift|>JumpThreshold` OR `expected<lastOutputDts`. Sets `p.Discontinuity=true` on each. **No coordination with consumers.** |
| `internal/ingestor/worker.go` `writeOnePacket` | `firstPacket bool` (per `readLoop` invocation) | Sets `cl.Discontinuity=true` on first packet of each reconnect. **AV path only.** |
| `internal/ingestor/worker.go` `writeRawTSChunk` | (none) | **Drops the flag entirely** — Discontinuity does not propagate to raw-TS consumers |
| `internal/ingestor/pull/mixer.go` | `videoPTSBase`, `audioPTSBase` (independent), re-anchor on `av.Discontinuity` | Reads upstream Discontinuity, re-anchors its own base |
| `internal/coordinator/abr_mixer.go` `ptsRebaser` | `t0`, `firstPTS`, `firstDTS`, `wallOffsetMs`, `lastSrcPTS`, `lastWallMs`; `ptsRebaserPauseGapMs` | Re-anchors on cycle start OR source pause detection. **Independently sets `p.Discontinuity=true`.** |
| `internal/publisher/dash_fmp4.go` | `originDTSms`, `videoNextDecode`, `audioNextDecode`, `videoFirstDTSmsSet`, `audioFirstDTSmsSet`; per-(track, drift) caps via `shouldSkipVideoLocked`/`shouldSkipAudioLocked`; `timestampJumpFromLast`; init segments | `recordOriginDTSLocked` (once), `seedVideo/AudioNextDecodeLocked` (once), drift cap re-anchor on IDR (V only), audio re-anchor every cap firing. Reads `pkt.AV.Discontinuity` **but only resets `tsCarry` byte buffer — not state.** |
| `internal/publisher/hls.go` | `failoverGen` snapshot (from event bus), `knownGen`, `discNext`, `discardUntilIDR`, `segStart` | TS path resets on `failoverGen` change (event bus); AV path resets on `pkt.AV.Discontinuity` (in-band). **Two independent channels for the same signal.** |
| `internal/publisher/push_rtmp.go` | `baseDTS`, `baseDTSSet`; `gotDiscontinuity` atomic | `pkt.AV.Discontinuity` sets `gotDiscontinuity`, run() returns `errDiscontinuity`, outer loop restarts session |
| `internal/publisher/serve_rtsp.go` | `baseDTS` AND `baseAudio` (independent), `lastVideoRTP`/`lastAudioRTP` monotonic clamp, `pace*` shared anchor, `firstVideo` gate | Reads `av.Discontinuity` → resets `firstVideo` + `paceSet` only |
| `internal/publisher/serve_rtmp.go` | Same shape as serve_rtsp.go | Same pattern |
| `internal/publisher/serve_srt.go` | Similar | Reads Discontinuity |
| `internal/dvr/service.go` | `pendingDiscontinuity` per session | Reads `Discontinuity` → flags next segment |
| `internal/transcoder/ffmpeg_args.go` | FFmpeg `-fflags +genpts` | Out of our control — FFmpeg owns its own clock |

**Total: 13 independent state machines** for what should be one concept:
"the timeline of this stream session".

### 2.2 Discontinuity flag overload

The single `AVPacket.Discontinuity bool` field is used to mean **at least
four different things** depending on who set it:

1. "First packet of a fresh source connection" (worker.go `isFirst`) — set
   per reconnect / per source switch.
2. "Per-track PTS jump exceeded threshold and was re-anchored" (ptsrebaser)
   — set on every hard-re-anchor, can fire every ~2 s on bursty sources
   (test3 RTSP case).
3. "ABR mixer paused / source PTS regressed" (abr_mixer) — set on cycle
   start and pause detection.
4. "Mixer upstream reconnected, re-anchoring against new base"
   (pull/mixer.go) — uses the flag both as input and output signal.

Consumers cannot distinguish these. They treat all four identically — but
the appropriate response differs:

- Cases (2) and (4) are mid-stream timing adjustments. Consumers must keep
  segment counters monotonic and continue. **No state reset.**
- Cases (1) and (3) are stream-session boundaries. Consumers must reset
  derived state (origin, seed, init segment if codec config changed).

The session-bug investigation kept producing wrong fixes because we were
reading the same flag with the wrong assumption about which case it meant.

### 2.3 Buffer hub contract — what consumers can actually trust

```go
type Packet struct {
    TS []byte
    AV *domain.AVPacket
}
```

What's guaranteed (today):
- Exactly one of `TS` / `AV` is set
- `pkt.empty()` is false

What's NOT guaranteed but consumers behave as if it were:
- That `AV.PTSms` / `AV.DTSms` are wallclock-anchored
  - True for RTSP/RTMP pull (ptsrebaser runs)
  - True for RTSP/RTMP push (rebaser also runs)
  - **False** for raw-TS path entirely
  - **Partially true** for abr_mixer / pull/mixer (their own rebaser)
- That `AV.Discontinuity` has a uniform meaning (see §2.2)
- That codec params (SR, channel count, SPS/PPS) are stable across the
  stream lifetime — they are **not** (transcoder restart, mid-stream
  config change in upstream encoder)
- That consecutive packets share a session — no session ID exists

### 2.4 Failover signal — two parallel channels

When the manager performs a failover:

1. **Event bus** publishes `EventInputFailover` →
   `publisher.hlsFailoverGen[streamID]++` → HLS segmenters (TS path only)
   detect change on next packet, flush + emit `EXT-X-DISCONTINUITY`.
2. **In-band**: ingestor's next `firstPacket` carries
   `AV.Discontinuity=true` → AV-path consumers (DASH packager, RTSP/RTMP
   serve, push, DVR) see it.

These two channels are **not synchronised**. The event bus fires when the
manager decides to switch; the in-band flag fires when the new reader
emits its first packet. Order is undefined. HLS AV-path consumers ignore
the event bus signal; TS-path consumers ignore the in-band flag. DASH
ignores the event bus entirely.

### 2.5 Stream lifecycle — implicit, derived

There is no first-class "stream session" concept. Each module guesses:

- DASH packager assumes "stream is healthy for life" once `originDTSms` is
  set. No reset path even on full source switch.
- HLS AV-path guesses session from `AV.Discontinuity`.
- HLS TS-path uses event bus `failoverGen`.
- serve_rtsp.go's `firstVideo` is the closest thing to a "session start"
  gate, but it only gates audio — it doesn't reset the RTP baseline.

No single owner. No invariant.

---

## 3. The bug-class pattern

Every reported bug above is one of:

A. **Same flag, different meaning** — `Discontinuity` triggers a state
   reset that's appropriate for one source but corrupts another.
   (`test3` resets every 2 s.)

B. **Lost signal** — `Discontinuity` is set at ingest but the consumer
   that needs it doesn't receive it. (Raw-TS path strips it; DASH packager
   doesn't reset state on it.)

C. **Two consumers, two answers** — V/A normalisation done independently
   by ingestor + packager + serve_rtsp.go all converge on slightly
   different baselines, producing 1-2 s residual skew that no single
   layer can fix without breaking others.

D. **No session boundary** — codec config change mid-stream isn't a
   "discontinuity" today but should be (init segment must rebuild).

The fix is not "another flag" or "another rebaser layer". The fix is to
**collapse 13 state machines into one and make the contract enforceable**.

---

## 4. Target architecture

### 4.1 Principle

> The buffer hub is the single, authoritative timeline. Every packet
> reaching the buffer carries (a) wallclock-anchored timestamps,
> (b) the stream session it belongs to, and (c) the codec config that
> session promises. Consumers trust these invariants and never roll
> their own normalisation.

### 4.2 Type model

```go
// internal/domain/session.go (NEW)
package domain

type SessionStartReason uint8

const (
    SessionStartFresh        SessionStartReason = iota // brand-new stream
    SessionStartFailover                                // manager switched input
    SessionStartReconnect                               // same input, reconnected after error
    SessionStartConfigChange                            // codec params changed mid-stream
)

// StreamSession identifies one contiguous emission of a stream. It changes
// whenever the upstream timeline is no longer valid for the previous
// session: failover, reconnect, codec config change, manual restart.
//
// Consumers MUST reset their derived timeline state when SessionID
// changes. Within a session, ALL packets share the same SessionID and
// the same (immutable) codec config snapshot.
type StreamSession struct {
    ID        uint64
    StartedAt time.Time
    Reason    SessionStartReason
    Video     *VideoConfig // nil = no video this session
    Audio     *AudioConfig // nil = no audio this session
}

type VideoConfig struct {
    Codec      AVCodec
    Width      int
    Height     int
    FPS        float64 // best-effort; 0 if unknown
    SPS, PPS   []byte  // (or VPS+SPS+PPS for HEVC) — frozen for session
}

type AudioConfig struct {
    Codec       AVCodec
    SampleRate  int
    Channels    int
    Profile     int
}
```

### 4.3 Buffer hub contract

```go
// internal/buffer/packet.go (REPLACE)
package buffer

type Packet struct {
    // Wire payload — exactly one set.
    TS []byte
    AV *domain.AVPacket

    // Session that owns this packet. Required: every Write must set this.
    SessionID uint64

    // SessionStart is true on the FIRST packet of a session and only
    // that packet. Consumers reset derived state when they observe a
    // true value, then continue as normal.
    SessionStart bool
}

// Buffer-hub-level invariants (enforced by writer, trusted by readers):
//
//  1. Within a single SessionID:
//     a. AV.PTSms / AV.DTSms (if AV is set) are wallclock-anchored —
//        monotonic per track, growing at ≈ realtime rate.
//     b. AV.Discontinuity (if AV is set) means "rebaser re-anchor this
//        single packet only — segmenter boundary cue, NOT session
//        change". DOES NOT require state reset.
//     c. Codec config (SR, channels, SPS/PPS) is fixed for the session.
//        Read from the StreamSession the consumer last received.
//
//  2. Across SessionIDs:
//     a. SessionStart=true on the first packet of the new session.
//     b. SessionID is monotonic per stream (increments only).
//     c. Consumers MUST reset derived state on SessionStart=true.
//     d. The previous session's content may be partly in the consumer's
//        in-flight queue — consumers MUST flush before applying reset.
```

### 4.4 Session table

```go
// internal/buffer/service.go (ADDITIONS)

// Session returns the current StreamSession for the given stream, or
// nil if the stream has no session yet (no packets written).
func (s *Service) Session(id domain.StreamCode) *domain.StreamSession

// SetSession publishes a new session for the stream. The next packet
// Write will carry SessionStart=true and this SessionID. Writes between
// SetSession and the first packet of the new session continue to use
// the previous SessionID (the new session only takes effect at the
// next Write).
func (s *Service) SetSession(id domain.StreamCode, sess domain.StreamSession)
```

The writer (ingestor / transcoder / abr_mixer) calls `SetSession` when:
- It opens a new source (Fresh / Reconnect / Failover — caller knows
  which)
- It detects a codec config change in the input stream

The reader (publisher / DVR / serve\_\*) calls `Session()` on every
`SessionStart=true` packet to get the new config and resets accordingly.

### 4.5 Timeline normaliser

```go
// internal/timeline/normaliser.go (NEW)
package timeline

// Normaliser is the single replacement for ptsrebaser, abr_mixer's
// ptsRebaser, and the ad-hoc baseline math in serve_rtsp.go /
// serve_rtmp.go / push_rtmp.go / dash_fmp4.go.
//
// It is wallclock-anchored, V/A-pair-aware, monotonic per track,
// drift-cap-bounded, and session-aware. The OWNER is the ingestor —
// no other module re-normalises.
type Normaliser struct {
    // ... per-stream state, see Phase 2 spec
}

func (n *Normaliser) Apply(p *Packet, now time.Time) (out []Packet, ok bool)
func (n *Normaliser) OnSession(sess domain.StreamSession)
```

Within a session, `Normaliser.Apply` is a 1:1 transform on PTS/DTS.
Drift cap, V/A pair-and-snap, and monotonic floor are applied here once
and only here. Downstream consumers do zero re-normalisation.

### 4.6 Consumer responsibilities — after refactor

After the refactor, each consumer's job collapses to:

- **DASH packager** ([dash_fmp4.go](../internal/publisher/dash_fmp4.go)):
  Write fMP4 segments. tfdt = packet.DTSms (already anchored). On
  SessionStart=true: flush + rebuild init from new `StreamSession.Audio`
  / `Video` config + reset segment timeline (but keep monotonic segment
  numbers).

- **HLS segmenter** ([hls.go](../internal/publisher/hls.go)):
  Mux to TS. Force-flush on SessionStart=true, emit
  `EXT-X-DISCONTINUITY`. Delete `failoverGen` mechanism (replaced by
  session-aware writes from ingestor).

- **RTSP / RTMP serve / push** (`serve_*.go`, `push_rtmp.go`): Use
  packet.DTSms directly for RTP/RTMP timestamps (subtract a session-
  local base set on `SessionStart=true`). Delete independent
  `baseDTS` / `baseAudio` baselines. Delete `firstVideo` gate (replaced
  by codec-config check from `StreamSession.Video.SPS` non-empty).

- **DVR** ([dvr/service.go](../internal/dvr/service.go)): Already
  reads `Discontinuity`; switch to reading `SessionStart` for segment
  break decisions. Existing logic is mostly aligned.

- **abr_mixer** ([coordinator/abr_mixer.go](../internal/coordinator/abr_mixer.go)):
  Delete `ptsRebaser`. The Normaliser handles the timeline. Coordinator
  publishes Session on cycle start.

---

## 5. Migration phases

Each phase is one PR. Each must be deployable in isolation, with rollback
safety. Phase 2 is the only high-risk phase.

### Phase 0 — Design (this document)

- **Deliverable**: this document
- **Risk**: none
- **Approval gate**: user agrees on §4 target before any code lands

### Phase 1 — Foundation, dual-write, no behaviour change

- Add `domain.StreamSession`, `domain.VideoConfig`, `domain.AudioConfig`
  types
- Add `SessionID`, `SessionStart` fields to `buffer.Packet`
- Add `buffer.Service.SetSession` / `Session()` API + sessions table
- Ingestor's `writeOnePacket` / `writeRawTSChunk` set `SessionID` (always
  the current session) and `SessionStart` (on `isFirst` only)
- abr_mixer + pull/mixer publish session on their own re-anchor events
- **All consumers ignore the new fields** — read them but take no action
- All existing tests pass unchanged
- New tests: session lifecycle (Create / Write / Subscribe / SetSession /
  observe SessionStart on next Write)
- **Risk**: low. No runtime behaviour change. Easy revert (just delete
  the new fields).

### Phase 2 — Timeline normaliser (the hard one)

- Build `internal/timeline/Normaliser` package
- Define Normaliser semantics in tests FIRST (table-driven):
  - V/A pairing (defer seed until both tracks have first packet, or
    timeout)
  - Wallclock anchor
  - Monotonic floor per track
  - Drift cap (symmetric — both ahead AND behind)
  - Cross-session reset
- Wire Normaliser into the ingestor pipeline behind a feature flag
  (config knob `INGESTOR_TIMELINE_NORMALISER=true|false`)
- **Run dual-path**: existing ptsrebaser stays in place. Normaliser
  reads the same input independently. Log when outputs differ.
- Capture differences on prod for 1 week. Adjust Normaliser until
  parity, then drift in the directions we want (e.g., drift cap fires
  less often; V/A skew goes to ≈ 0 instead of preserve-natural-pre-roll).
- Switch over: feature flag default `true`. Delete ptsrebaser code,
  abr_mixer's ptsRebaser, pull/mixer's rebaser.
- **Risk**: HIGH. Days of dual-path validation needed. This is the only
  phase that ABSOLUTELY requires production observation.

### Phase 3 — Session enforcement (consumers reset on SessionStart)

- DASH packager: rebuild init segment + reset seed + flush queue on
  `SessionStart`
- HLS: replace `failoverGen` channel with session boundaries
- serve_rtsp / serve_rtmp / push_rtmp: delete independent baselines,
  use session-local base set on `SessionStart`
- DVR: switch from `Discontinuity` to `SessionStart` for segment break
- abr_mixer: delete its ptsRebaser, rely on Normaliser
- **Risk**: medium. Each consumer's reset path is testable in isolation.

### Phase 4 — Test infrastructure

Integration tests for:
- Source switch mid-stream (HLS pull → RTMP pull failover)
- AAC config change mid-stream (32 kHz → 48 kHz)
- 24h compressed-wallclock simulation (drift behaviour)
- Failover storm (5 switches in 60 s)
- HLS pull reconnect storm
- Codec mismatch: V1=H264 then V2=H265

These are the tests that prevent re-introduction. Each maps to a
specific reported bug.

- **Risk**: none. Tests don't change behaviour.

### Phase 5 — Cleanup ✅

Done. Specifically:

- `AVPacket.Discontinuity` field deleted from `internal/domain/avpacket.go`.
  Normaliser no longer writes it; the lone telemetry consumer
  (tests + `LastDiagnostic().HardReanchored`) reads from the
  Normaliser's diagnostic instead.
- `hlsFailoverGen` event-bus channel removed (HLS dispatches on
  `pkt.SessionStart` for both AV and raw-TS paths via the unified
  `handleIncoming` → `onSessionBoundary` route).
- Stale package comments updated: `pull/mixer.go`, `serve_rtsp.go`,
  `push_rtmp.go`, `coordinator/abr_mixer.go`, `buffer/packet.go`,
  `domain/defaults.go`, `timeline/normaliser.go` (package doc),
  `internal/publisher/hls.go`.
- CLAUDE.md synced — Normaliser scope now describes raw-TS coverage
  via tsnorm; the Discontinuity flag is documented as deleted.

**Risk realised**: low. Tests migrated from `p.Discontinuity` to
`n.LastDiagnostic().HardReanchored` without behavioural change.

---

## 6. Rollback strategy

- **Phase 1**: pure additive. Revert = delete fields. Safe.
- **Phase 2**: feature-flag-gated. Revert = flip flag back to old path.
- **Phase 3**: per-consumer rollout. Each consumer's PR is small and
  isolated; revert one without touching others.
- **Phase 4**: tests are independent. Add only those that pass; defer
  others.
- **Phase 5**: cleanup happens only after Phase 2-3 have been stable
  in production for ≥ 1 week.

---

## 7. Effort estimate

Sessions = continuous focused working sessions, not calendar days.

| Phase | Sessions | Calendar | Key gate |
|---|---:|---|---|
| 0 — Design | 1 | this | user approves §4 |
| 1 — Foundation | 1-2 | ≤ 2 days | merge + deploy verify |
| 2 — Normaliser | 2-3 + 1 week observation | ≤ 2 weeks | feature flag flip |
| 3 — Sessions | 1-2 per consumer × 5 consumers = 5-10 | ≤ 1 week | consumer-by-consumer rollout |
| 4 — Tests | 1-2 | ≤ 3 days | CI gate |
| 5 — Cleanup | 1 | ≤ 1 day | post-stability |

**Total**: ~3-4 weeks calendar if done sequentially, less if Phase 3
consumers parallelise.

---

## 8. Open questions for user

These need decisions before Phase 1 starts:

1. **Session ID source of truth**: who owns the counter? Buffer hub
   service (cross-stream uniqueness)? Per-stream (simpler, but harder
   to debug cross-stream events)? **Recommendation**: per-stream
   `uint64`, monotonically increasing.

2. **Codec config change handling**: when ingestor detects SR change,
   do we (a) emit a new session and let consumers rebuild init, or
   (b) reject the change and reconnect? **Recommendation**: (a) — it's
   cheaper, the player handles it via fMP4 init segment re-fetch and
   HLS `#EXT-X-DISCONTINUITY`.

3. **AVPacket.Discontinuity deprecation timing**: keep for Phase 1-3,
   remove in Phase 5? Or remove in Phase 3 once all consumers switched?
   **Recommendation**: keep through Phase 3, remove in Phase 5.

4. **Existing tests**: Phase 2 dual-path comparison — what's the
   acceptance criterion for "Normaliser output ≈ ptsrebaser output"?
   Bit-exact? Within N ms tolerance? **Recommendation**: PTS deltas
   within ±1 ms; Discontinuity placement may differ (Normaliser is
   stricter about session boundaries).

5. **Phase 2 feature flag default**: tested-but-off, or tested-and-on?
   **Recommendation**: tested-but-off for one week, then on.

---

## 9. What this proposal does NOT cover

- ABR ladder topology changes (renditions added/removed mid-stream).
- Source URL templating, authentication, signed URLs.
- Multi-region replication.
- Recording / VOD playback.
- API surface changes (REST, gRPC, WebSocket).

These are orthogonal. They keep their current code paths.

---

## 10. Decision needed

Reply with:

- **Approve §4 target architecture as-is** → proceed to Phase 1.
- **Approve with changes** → list specific revisions needed.
- **Reject / different direction** → describe the alternative.

The answers to §8 questions are also welcome but not blocking — they can
be revisited at each phase boundary.
