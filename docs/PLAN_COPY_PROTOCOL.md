# Plan — `copy://` Ingest Protocol

Status: **Planned** · Owner: TBD · Tracked in [FEATURES_CHECKLIST.md](./FEATURES_CHECKLIST.md)

Re-stream another already-running stream's published output, with different
protocols / stream keys / push targets / DVR config.

```yaml
streams:
  # Upstream: pulls origin, transcodes to 3-rung ABR ladder.
  - code: source-abr
    inputs:
      - { url: "rtmp://origin/live/key", priority: 0 }
    transcoder:
      video:
        profiles:
          - { width: 1920, height: 1080, bitrate: 4500 }
          - { width: 1280, height: 720,  bitrate: 2500 }
          - { width: 854,  height: 480,  bitrate: 1200 }
    protocols: { hls: true }

  # Downstream: re-publishes the same 3-rung output under a different stream
  # key and pushes to YouTube. No new ingest, no new transcode.
  - code: relay-yt
    inputs:
      - { url: "copy://source-abr", priority: 0 }
    transcoder: null         # required — ladder is inherited
    protocols: { hls: true, rtmp: true }
    push:
      - { url: "rtmp://a.rtmp.youtube.com/live2/<yt_key>" }
```

---

## Mental model

Two perspectives, one feature:

- **Client-facing (API surface)**: `copy://` is just another ingest
  protocol, listed in `inputs[]` exactly like `rtmp://`, `hls://`, etc.
  Users configure it the same way, with the same priority + failover
  semantics surface. Documented in the same place as other ingest URLs.

- **System-internal (pipeline)**: it is **not** a network input. The
  coordinator special-cases the scheme and wires up a re-publish
  pipeline that copies upstream's published output (raw TS or full ABR
  ladder), bypassing the network reader → demuxer path entirely.

Comparable model: like an HLS variant playlist that points at another
origin's segments — same media bytes, new packaging surface.

The "Input is a PacketReader" pattern (RTMP / HLS / SRT / file) only
fits the single-stream case. ABR-copy needs N parallel taps, so the
coordinator handles it as a distinct pipeline mode rather than forcing
it through the single-reader factory.

---

## Motivation & use cases

| # | Scenario | Supported in v1? |
| - | -------- | ---------------- |
| 1 | Multi-key publishing — same ABR ladder, distinct stream keys for distribution partners | ✅ |
| 2 | Same source → many push targets without re-encoding | ✅ |
| 3 | Record-only + live-only split (one stream has DVR, other doesn't), same ABR ladder | ✅ |
| 4 | Different ABR ladder from same raw source (re-transcode) | ❌ — needs `copy://X/raw` (future) |
| 5 | Cherry-pick one rung from upstream's ladder | ❌ — needs `copy://X/track_N` (future) |

Use cases 4 and 5 are deliberately out of scope for v1 — see [Out of
scope](#out-of-scope-v1).

---

## Design decisions (locked)

| Question | Decision |
| -------- | -------- |
| What does `copy://X` copy? | Upstream's **published output**: raw buffer if upstream has no transcoder, or all rendition buffers (full ABR ladder) if upstream transcodes. |
| Downstream's own transcoder? | Allowed when upstream is single-stream (re-encodes the raw copy). Disallowed when upstream has ABR (ladder is inherited, local transcoder would be ambiguous). Validated at write time. |
| Downstream's fallback inputs in the same priority list? | Allowed when upstream is single-stream — fallback must also be single-stream (regular network URL or another single-stream `copy://`). Disallowed in v1 when upstream has ABR. |
| What if upstream's ladder shape changes mid-flight? | v1: shape snapshotted at downstream-start. Manual downstream restart required when upstream adds/removes rungs. Reactive cross-stream hot-reload is future work. |
| Cycle detection? | **Required.** Validated at write time AND at coordinator start time. |

---

## Pipeline shape

### Case A — upstream is single-stream (no transcoder)

```text
upstream:                                downstream:
  ingestor → main buffer ─tap──→  (downstream pipeline)
                                  │
                          ┌───────┴────────┐
                          │  transcoder?   │  optional — re-encodes the
                          │  (own config)  │  copied raw stream
                          └───────┬────────┘
                                  ↓
                              publishers
```

The "tap" is a goroutine that subscribes to upstream's main buffer and
re-emits each `domain.AVPacket` (or each `buffer.TSPacket`, depending on
upstream's reader type) into downstream's main buffer. Downstream's
Manager treats this tap exactly like a regular ingest worker for the
purposes of timeout / failover.

### Case B — upstream has ABR transcoder with N rungs

```text
upstream:                                downstream:
  ingestor → raw buffer                  (no ingestor, no transcoder)
            ↓
        N FFmpeg encoders                main buffer (unused)
            ↓
        N rendition buffers ─N taps──→  N matching rendition buffers
                                                ↓
                                            publishers
```

The downstream's `transcoder` field MUST be nil. Coordinator reads
upstream's stream config, derives `[(width, height, bitrate), …]`,
creates matching downstream rendition buffers, and spawns N tap
goroutines. From this point onward downstream's HLS/DASH/RTMP publishers
behave exactly as if a local FFmpeg ladder produced the rungs.

---

## URL scheme

```text
copy://<upstream_stream_code>
```

- Scheme: `copy` (registered in `pkg/protocol`)
- Host part: upstream stream code, e.g. `copy://source-abr`
- No path, no query in v1 (reserved for future qualifiers like `/raw` and
  `/track_N`)

Validation:

- Upstream code must exist in repository at config write time (warning,
  not error — upstream may be created later) and at coordinator start
  time (hard error if missing).
- Self-loop `copy://A` inside stream `A` is rejected immediately.

---

## Cycle detection

Even one self-copy or two-node cycle pegs CPU through the fan-out. Build
a directed graph where `A → B` exists iff `A` has any input
`copy://B`. DFS for cycles.

```text
visit(node, in_progress, finished):
    if node in in_progress:  -> cycle through node
    if node in finished:     -> already verified
    in_progress.add(node)
    for each copy:// edge from node:
        visit(target, in_progress, finished)
    in_progress.remove(node)
    finished.add(node)
```

Run sites:

| Site | When | On cycle |
| ---- | ---- | -------- |
| `domain.ValidateStreamGraph` (new) | `POST /streams`, `PUT /streams/{code}` before persistence — must include the proposed-new state, not just on-disk | Reject 400; error names full cycle path: `cycle: A → B → A` |
| `coordinator.Start` | Bootstrap and explicit start | Refuse to start; emit `stream.error` with cycle payload; status → `degraded` |

Self-loop is short-circuited before DFS — cheaper and clearer error.

---

## Constraints & validation rules

Enforced in `domain.ValidateStream` (or a new sibling that has access to
the repository), called by the API write handlers.

1. **Self-copy rejected** — `copy://A` inside stream `A`.
2. **Cycle rejected** — see above.
3. **Schema mismatch in input list** — if any input is `copy://X` with
   `X` having ABR, the input list MUST contain only that single entry. No
   fallback in v1.
4. **Local transcoder + ABR upstream** — if any active input is
   `copy://X` with `X` having ABR, downstream's `transcoder` field MUST
   be nil. (Reject with explicit error pointing to upstream's ladder.)
5. **Mixed-shape inputs** — if mixing `copy://X` (single) with regular
   network inputs as fallback, all inputs must produce single-stream
   output. Validated by upstream lookup.

Rules 3–5 may relax later as the design matures (e.g. via auto-mode
publisher restart on failover); v1 keeps them strict for predictability.

---

## Lifecycle

### Start

Coordinator detects `copy://` in `inputs` and branches early:

1. Look up upstream from repository.
2. If upstream missing or not running → fail-fast (status `degraded`,
   emit `stream.error`).
3. Snapshot upstream's transcoder shape (`nil` or `N rungs`).
4. Validate downstream config against shape (rules 3–5).
5. Create downstream buffers matching shape.
6. Spawn N tap goroutine(s) (1 if single-stream, N if ABR).
7. Start publishers + DVR + push as usual.

### Failover

- **Single-stream upstream + single-stream fallback in input list**:
  treated like normal failover. The tap is a Manager-tracked input;
  packet-timeout fires after `input_packet_timeout_sec` and the next
  priority input takes over.
- **ABR upstream**: no fallback in v1. If upstream stops, downstream
  goes degraded and waits for upstream to come back. Failback probe
  re-attempts the tap periodically.

### Upstream stops mid-flight

The buffer subscription channel closes → tap goroutine exits → Manager
records timeout → failover (single-stream case) or degraded (ABR case).
Same machinery as RTMP origin going dark.

### Upstream restarts (own failover, transcoder restart)

Subscriber sees a discontinuity but the channel stays open; existing
`#EXT-X-DISCONTINUITY` machinery on downstream's publishers handles it.

### Upstream's ladder shape changes

v1: not handled reactively. The downstream stream must be manually
restarted (or its own `PUT /streams/{code}` re-issued) to re-snapshot.
Out-of-band detection: coordinator's diff engine on upstream COULD emit
an event that downstreams listen to — deferred to a follow-up.

### Stop

Coordinator stops downstream's tap goroutines first (they hold the
upstream subscriber), then tears down buffers / publishers as usual.

---

## Implementation steps

Sized so each step is a small, mergeable commit with green tests.

| # | Change | Files | Tests |
| - | ------ | ----- | ----- |
| 1 | `protocol.KindCopy` + `Detect` for `copy://` scheme | `pkg/protocol/protocol.go`, `_test.go` | scheme detection table |
| 2 | Helper `protocol.CopyTarget(url) (code, error)` — parses `copy://<code>`, validates host non-empty, no path/query | `pkg/protocol/protocol.go`, `_test.go` | parse table including malformed inputs |
| 3 | New domain validation `ValidateStreamGraph(repo, proposed)` — DFS cycle detection | `internal/domain/copy_graph.go`, `_test.go` | self-loop, two-node, three-node, diamond-no-cycle, disjoint subgraphs |
| 4 | New domain validation `ValidateCopyShape(repo, stream)` — rules 3–5 in [Constraints](#constraints--validation-rules) | `internal/domain/copy_shape.go`, `_test.go` | every reject path covered |
| 5 | Hook both validations into `POST /streams`, `PUT /streams/{code}` handlers | `internal/api/handler/streams.go` | API tests: 400 with explicit error |
| 6 | Single-stream tap reader: subscribes to upstream's main buffer, emits `AVPacket`s — implements `ingestor.PacketReader` | `internal/ingestor/pull/copy_reader.go`, `_test.go` | open / read-forwards / close-unsubscribes |
| 7 | Wire `KindCopy` (single-stream case only) into `NewPacketReader` factory; needs `*buffer.Service` + `repo` deps in factory | `internal/ingestor/reader.go` | scheme dispatch |
| 8 | Coordinator branch: detect `copy://` first input, look up upstream shape, route to single-stream pipeline (steps 6–7 wiring) or ABR pipeline (step 9) | `internal/coordinator/coordinator.go`, `_test.go` | spy tests for both paths |
| 9 | ABR copy pipeline: N tap goroutines, no transcoder, downstream rendition buffers shaped from upstream | `internal/coordinator/copy_abr.go`, `_test.go` | shape inheritance, ladder bypass |
| 10 | Defensive cycle check at `coordinator.Start` (catches data persisted before validation existed) | `internal/coordinator/coordinator.go` | bootstrap-cycle test |
| 11 | End-to-end smoke: upstream RTMP + ABR; downstream `copy://upstream` + own HLS; verify all 3 rungs flow | `internal/coordinator/integration_copy_test.go` | mock ingestors, real coordinator |
| 12 | Update `docs/FEATURES_CHECKLIST.md`, `docs/DESIGN.md`, `docs/EVENTS.md` (if new event) | docs | — |

Steps 1–7 deliver single-stream copy (smaller scope, easier first slice).
Steps 8–9 add the ABR re-publish capability — the bigger architectural
change. Steps 3–5 + 10 close the safety gap.

Recommended slicing for review: ship steps 1–7 + 10 + tests as PR #1
(single-stream copy with cycle protection); ship 8–9 + 11 as PR #2 (ABR
re-publish). Each PR is independently shippable and testable.

---

## Test plan

### Unit

- **Protocol**: `copy://X` → `KindCopy`; `copy://` (no host) rejected;
  `copy://X/y` rejected (no path in v1); query string rejected.
- **Cycle detection**: self-loop, two-node, three-node, diamond
  (no cycle), disjoint subgraphs, missing upstream is not a cycle.
- **Shape validation**:
  - Reject local `transcoder` when upstream has ABR.
  - Reject mixed input list when one entry is ABR-copy.
  - Allow single-stream copy + RTMP fallback.
  - Allow ABR-copy as sole input when downstream has no transcoder.
- **Single-stream tap reader**:
  - `Open` returns error when upstream code missing in buffer service.
  - `ReadPackets` forwards every packet emitted by upstream.
  - `Close` releases the subscriber.

### Integration

- Single-stream upstream + downstream copy → HLS plays from downstream.
- ABR upstream (3 rungs) + downstream copy → all 3 rungs visible in
  downstream's master playlist; per-rung segments arrive.
- Stop upstream → downstream's RTMP fallback takes over (single-stream
  case only) within `input_packet_timeout_sec`.
- Restart upstream → failback probe restores the copy tap.

### Manual matrix additions

Add a row to "Manual Test Matrix — Ingest × Publisher" for `copy://` once
implementation lands. Two sub-rows: single-stream-copy and ABR-copy.

---

## Out of scope (v1)

| Feature | Why deferred |
| ------- | ------------ |
| `copy://X/raw` — explicit raw access for re-transcoding | Bigger semantic surface; coupling to upstream's internal raw buffer. Re-evaluate after v1 ships. |
| `copy://X/track_N` — cherry-pick one rung | Requires per-rung addressing; current ladder slugs aren't part of the public surface. |
| Cross-instance copy (`copy://other-host/code`) | Needs an internal RPC; not in scope for the in-process design. |
| Reactive shape sync (upstream adds/removes a rung → downstream auto-restarts) | Requires diff engine to publish "shape changed" events. Add after the snapshot model has been in production. |
| Authorization / stream-key gating across copy boundary | Out of scope until stream-level ACLs exist. |
| Mixed-shape failover (ABR-copy primary + single-stream fallback) | Requires publisher hot-reshape. Consciously deferred. |

---

## Open questions

None — all locked above. New questions surfacing during implementation
get added here with the answer before merging.
