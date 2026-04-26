# Open-Streamer — Feature Checklist

Snapshot of what's implemented today, organised by subsystem. For
end-to-end pipeline flow see [APP_FLOW.md](./APP_FLOW.md); for design
rationale see [ARCHITECTURE.md](./ARCHITECTURE.md); for operator-facing
config see [CONFIG.md](./CONFIG.md).

Legend:

| Level | Meaning |
|---|---|
| **Complete** | Implemented and usable for the described scope |
| **Partial** | Works with known limitations or narrow codec/path support |
| **Schema only** | Domain / API / persistence fields exist; not wired into live pipeline |
| **Planned** | Documented intent only |

---

## Core Platform

| Feature | Status | Notes |
|---|---|---|
| Layered configuration (file + env) | Complete | `config.yaml` + `OPEN_STREAMER_*` env vars; only StorageConfig from file, rest from store |
| Dependency injection | Complete | `samber/do/v2`; all services in `cmd/server/main.go` |
| Structured logging | Complete | `slog` with text / json format; level configurable |
| Graceful shutdown | Complete | SIGINT/SIGTERM with 10s timeout; reverse-order teardown |
| Prometheus metrics | Complete | Per-stream uptime, bytes/packets, failovers, restarts, active workers, buffer depth |
| Hardware detection | Complete | `internal/hwdetect` probes /dev for NVIDIA / DRI / Intel — listed in `/config.hw_accels` |
| FFmpeg compatibility probe | Complete | `internal/transcoder.Probe` runs at boot (fail-fast on missing required encoders) + `POST /config/transcoder/probe` for UI test + save-time validation |
| Build version stamping | Complete | `pkg/version` injected at compile via Makefile ldflags / Release workflow |

---

## Storage & API

| Feature | Status | Notes |
|---|---|---|
| Stream repository — JSON | Complete | Default; flat-file under `storage.json_dir` |
| Stream repository — YAML | Complete | Single `open_streamer.yaml` per data dir |
| Recording / Hook / VOD repositories | Complete | Both backends |
| REST API — streams CRUD + start/stop/restart | Complete | `chi/v5` router under `/streams` |
| REST API — `PUT /streams/{code}` hot-reload | Complete | Diff-based; only changed components restart |
| REST API — input switch | Complete | `POST /streams/{code}/inputs/switch` forces active priority |
| REST API — recordings | Complete | CRUD + playlist.m3u8 + timeshift.m3u8 + segment serve + info |
| REST API — hooks CRUD + test (HTTP & Kafka) | Complete | `DeliverTestEvent` routes per hook type |
| REST API — config GET/POST | Complete | `/config` static enums + GlobalConfig; POST hot-applies |
| REST API — config defaults | Complete | `GET /config/defaults` returns implicit values for UI placeholders (incl. encoder routing table per HW) |
| REST API — config YAML editor | Complete | `GET/PUT /config/yaml` round-trips entire system state |
| REST API — VOD mounts | Complete | Browse on-disk recordings outside DVR scope |
| OpenAPI / Swagger | Complete | Spec served at `/swagger/`; `make swagger` regenerates |
| Static delivery — HLS / DASH | Complete | `/{code}/index.m3u8`, `/{code}/index.mpd`, `/{code}/*` |
| Health probes | Complete | `/healthz`, `/readyz` |
| CORS | Complete | Configurable origins/methods/headers/credentials |

---

## Buffer Hub

| Feature | Status | Notes |
|---|---|---|
| In-memory ring buffer per stream | Complete | Fan-out via independent `Subscriber`; write never blocks |
| Raw ingest buffer (`$raw$<code>`) | Complete | Created when transcoder is active |
| Rendition buffers (`$r$<code>$track_N`) | Complete | One per ABR ladder rung |
| Slow-consumer packet drop | Complete | `default:` in fan-out — ingestor never blocked |
| `PlaybackBufferID` resolver | Complete | Picks best rendition for ABR, else logical stream code |
| Capacity tunable | Complete | `buffer.capacity` (default 1024) |

---

## Ingest

| Feature | Status | Notes |
|---|---|---|
| Pull — HLS / HLS-LL | Complete | `grafov/m3u8`; max-segment-buffer guard; per-input headers/auth |
| Pull — HTTP raw MPEG-TS | Complete | |
| Pull — RTSP | Complete | `gortsplib/v5`; H.264/H.265/AAC; RTCP A/V sync |
| Pull — RTMP | Complete | `q191201771/lal` PullSession; AVCC→Annex-B; ADTS wrap |
| Pull — SRT (caller) | Complete | `datarhei/gosrt` |
| Pull — UDP / MPEG-TS | Complete | Unicast + multicast; auto-strip RTP header |
| Pull — File (`.ts`, `.mp4`, `.flv`) | Complete | Loop mode; paced playback |
| Pull — S3 | Complete | GetObject stream; S3-compatible via `?endpoint=` |
| Pull — `copy://<code>` | Complete | In-process subscribe to another stream's published output (raw or per-rendition) |
| Pull — `mixer://<videoCode>?audio=<audioCode>` | Complete | In-process video+audio mix from two upstream streams |
| Push — RTMP listen | Complete | Shared `:1935` (default); RTMP relay → loopback pull |
| Push — SRT listen | Complete | Shared `:9999`; streamid `live/<code>` dispatch |
| Multi-input registration with priority | Complete | Lower value = higher priority |
| Per-input `Net` config | Partial | `connect_timeout_sec`, `insecure_tls`, `reconnect` consumed; `read_timeout_sec`, `reconnect_delay_sec`, `reconnect_max_delay_sec`, `max_reconnects` declared but not yet wired |
| HLS pull tuning | Complete | Per-stream `connect_timeout_sec`; server-wide `hls_max_segment_buffer` |

---

## Stream Manager (Failover)

| Feature | Status | Notes |
|---|---|---|
| Multi-input failover (Go-level, no FFmpeg restart) | Complete | Old ingestor stops, new one starts; buffer continuity preserved |
| Packet timeout detection | Complete | `manager.input_packet_timeout_sec` (default 30) |
| Background failback probe | Complete | Cooldown 8s probe / 12s switch |
| Bypass-probe recovery | Complete | When ingestor reader auto-reconnects faster than probe cycle, `RecordPacket` clears exhausted state + records recovery switch |
| Switch history (last 20) | Complete | `runtime.switches[]` per stream with reason: `initial`, `error`, `timeout`, `manual`, `failback`, `recovery`, `input_added`, `input_removed`; from/to/at/detail |
| Per-input error history (last 5) | Complete | `runtime.inputs[].errors[]` — degradation reasons + timestamps |
| Live input update (`UpdateInputs`) | Complete | Add/remove/update without pipeline stop; active removal triggers failover with `input_removed` reason |
| Live buffer write-target update | Complete | `UpdateBufferWriteID` — restart active ingestor with new target |
| Manual switch API | Complete | `POST /streams/{code}/inputs/switch { priority }` records `manual` reason |
| Exhausted callback → coordinator | Complete | `setStatus(degraded)`; auto-recover via probe success |

---

## Transcoder

| Feature | Status | Notes |
|---|---|---|
| FFmpeg subprocess (stdin TS → stdout TS) | Complete | `exec.CommandContext`; killed via context cancel |
| Per-profile encoder pool | Complete | Each `track_N` is independent `profileWorker`; hot start/stop one without affecting others |
| Multi-output mode | Complete | `transcoder.multi_output=true` runs ONE FFmpeg per stream emitting N rendition pipes (single decode, multi encode); ~50% NVDEC + ~40% RAM saved per ABR stream. Hot-toggle restarts running streams |
| Shadow profile workers (multi-output) | Complete | All N ladder rungs appear in `RuntimeStatus.Profiles[]` even though one process drives them — error history accurate per rung |
| ABR profile config | Complete | Resolution, bitrate, codec, preset, profile, level, framerate, GOP, B-frames, refs, SAR, resize_mode |
| Encoder codec routing | Complete | `domain.ResolveVideoEncoder` maps user alias (`""`/`h264`/`h265`/`vp9`/`av1`) + HW backend → FFmpeg encoder name; explicit names (`h264_nvenc`, `h264_qsv`) preserved |
| Preset normalization | Complete | Translates between encoder families (`veryfast` ↔ `p2`, `medium` ↔ `p4`); drops invalid values for backends without `-preset` (VAAPI, VideoToolbox) so cross-family preset choices remain valid |
| Audio encoding | Complete | AAC / MP3 / Opus / AC3 / copy |
| Copy video / copy audio modes | Complete | `video.copy=true` + `audio.copy=true` skips FFmpeg entirely (passthrough) |
| Hardware acceleration | Complete | NVENC, VAAPI, VideoToolbox, QSV; full-GPU pipeline (decode→scale_cuda→encode) when HW matches encoder family |
| Resize modes (pure GPU) | Complete | `pad`, `crop`, `stretch`, `fit` — all stay on GPU (no CPU round-trip via hwdownload) for NVENC; `pad`/`crop` degrade to aspect-preserving fit on GPU |
| Deinterlace | Complete | yadif (CPU) / yadif_cuda (GPU); auto-detect parity or operator-specified tff/bff |
| Watermark | Schema only | Domain fields exist; not yet applied in filter graph |
| Thumbnail | Schema only | Domain fields exist; not yet generated |
| Extra FFmpeg args passthrough | Complete | `extra_args` per stream |
| FFmpeg crash auto-restart | Complete | Per-profile exponential backoff: 2s → 30s cap; retries forever |
| Crash log spam suppression | Complete | After 3 consecutive identical errors, warn drops to debug; events fire only on power-of-2 attempts |
| Per-profile error history (last 5) | Complete | `runtime.transcoder.profiles[].errors[]` — stderr-tail context embedded ("No such filter X") |
| Stderr filtering | Complete | Timestamp resync, packet-corrupt, MMCO chatter → debug; real errors → warn |
| Health detection → coordinator | Complete | After 3 consecutive crashes (sub-30s) fires `onUnhealthy` → status Degraded; sustained run (>30s) fires `onHealthy` → status Active. Hot-restart (Update path) clears flag via `dropHealthState` callback |
| Hot-swap config (`SetConfig`) | Complete | runtime updates `MultiOutput` / `FFmpegPath`; restarts running streams when behaviour-changing field flips |
| `StopProfile` / `StartProfile` | Complete | Granular ladder control; multi-output mode loses granularity (must full-restart) |

---

## Coordinator & Lifecycle

| Feature | Status | Notes |
|---|---|---|
| Start pipeline | Complete | Buffers → manager → publisher → transcoder; raw + rendition buffers per topology |
| Stop pipeline | Complete | Reverse-order teardown; buffer cleanup |
| Bootstrap persisted streams on boot | Complete | Skips disabled / zero-input streams |
| Hot-reload (`Update`) | Complete | Diff engine: 5 categories — inputs, transcoder topology, profiles, protocols/push, DVR |
| Per-profile granular reload | Complete | Add/remove/update one profile without touching others |
| ABR ladder add/remove → `RestartHLSDASH` | Complete | Only HLS+DASH goroutines restart; RTSP/RTMP/SRT viewers preserved |
| ABR profile metadata update | Complete | `UpdateABRMasterMeta` rewrites HLS master playlist in-place (no FFmpeg restart) |
| Topology change → `reloadTranscoderFull` | Complete | Full pipeline rebuild when transcoder nil↔non-nil or mode changes |
| ABR-copy pipeline (`copy://` upstream with ladder) | Complete | N tap goroutines re-publish each upstream rendition; bypasses ingest worker + transcoder |
| ABR-mixer pipeline | Complete | Mirror video ladder + audio fan-out from two upstream streams |
| Stream-level health reconciliation | Complete | `streamDegradation` flags (`inputsExhausted`, `transcoderUnhealthy`) — Degraded if either set, Active when all clear |
| DVR hot-reload | Complete | Toggle on/off; restarts with new mediaBuf when best rendition shifts |
| Narrow service interfaces (`deps.go`) | Complete | `mgrDep`, `tcDep`, `pubDep`, `dvrDep` — spy-based testing |

---

## Publisher — Delivery

| Feature | Status | Notes |
|---|---|---|
| HLS — single rendition | Complete | Native TS segmenter + media playlist |
| HLS — ABR (master + per-track sub-playlists) | Complete | Auto-active when transcoder ladder present |
| HLS — `#EXT-X-DISCONTINUITY` on failover | Complete | Per-variant generation counter |
| DASH — single representation (fMP4 + dynamic MPD) | Complete | H.264 / H.265 / AAC; MP3 skipped |
| DASH — ABR (root MPD + per-track dirs) | Complete | Audio packaged on best track only |
| RTSP play | Complete | Shared listener (default `:554`); `gortsplib/v5`; `rtsp://host:port/live/<code>` |
| RTMP play | Complete | Shared port with ingest (`:1935`); `rtmp://host:port/live/<code>` |
| SRT play | Complete | Shared listener (`:9999`); `srt://host:port?streamid=live/<code>`; default latency 120ms |
| RTMP push out | Complete | `q191201771/lal` PushSession; `rtmp://` + `rtmps://`; custom codec adapter for proper PTS/DTS composition_time (B-frame friendly) |
| Per-protocol independent context | Complete | Each output (`hls`, `dash`, `rtsp`, `push:<url>`) has its own cancel func |
| `UpdateProtocols(old, new)` | Complete | Only changed protocols stop/start; live viewers preserved |
| Per-push state tracking | Complete | `runtime.publisher.pushes[]` — status (`starting`/`active`/`reconnecting`/`failed`), attempt, connected_at, last 5 errors |

---

## DVR & Timeshift

| Feature | Status | Notes |
|---|---|---|
| Persistent recording (ID = stream code) | Complete | One recording per stream; survives restarts |
| Segment writing (MPEG-TS) | Complete | PTS-based cutting; wall-clock fallback |
| Gap detection + `#EXT-X-DISCONTINUITY` | Complete | Gap timer = 2× segment duration |
| Gap recording in `index.json` | Complete | `DVRGap{From, To, Duration}` |
| Resume after restart | Complete | Playlist parsing rebuilds in-memory segment list |
| `#EXT-X-PROGRAM-DATE-TIME` | Complete | Written before first segment + after every discontinuity |
| Retention by time + size | Complete | Both `retention_sec` (0=forever) and `max_size_gb` (0=unlimited) |
| VOD playlist endpoint | Complete | `GET /recordings/{rid}/playlist.m3u8` |
| Timeshift (absolute / relative) | Complete | `?from=RFC3339&duration=N` or `?offset_sec=N&duration=N` |
| Segment serve | Complete | `GET /recordings/{rid}/{file}` — path traversal sanitised |
| Info endpoint | Complete | `GET /recordings/{rid}/info` — range, gaps, count, total bytes |
| Configurable storage path | Complete | Per-stream `storage_path` overrides `./out/dvr/{streamCode}` default |

---

## Events & Hooks

| Feature | Status | Notes |
|---|---|---|
| In-process event bus | Complete | Typed events, bounded queue (512), worker pool |
| HTTP webhook delivery | Complete | Retries, timeout, optional HMAC `X-OpenStreamer-Signature` |
| Kafka delivery | Complete | Lazy writer per topic; `hooks.kafka_brokers` |
| Per-hook event filter | Complete | `event_types[]` whitelist |
| Per-hook stream filter | Complete | `stream_codes.only[]` / `.except[]` |
| Per-hook metadata injection | Complete | Merged into payload as `metadata.*` |
| Per-hook MaxRetries / TimeoutSec | Complete | Defaults: 3 retries, 10s timeout (from `domain.Default*`) |
| Test endpoint (HTTP + Kafka) | Complete | `POST /hooks/{id}/test` |
| Event documentation | Complete | See [APP_FLOW.md](./APP_FLOW.md#events-reference) |

---

## Runtime Status & Observability

All live state is exposed under `runtime.*` in `GET /streams/{code}` so the UI has one root for everything dynamic. Persisted config stays at the top level — runtime overlay never collides.

| Feature | Status | Notes |
|---|---|---|
| `runtime.status` + `pipeline_active` | Complete | Coordinator-resolved lifecycle: `active` / `degraded` / `stopped` / `idle` |
| `runtime.exhausted` | Complete | True when all inputs are degraded with no failover candidate |
| `runtime.active_input_priority` + `override_input_priority` | Complete | Manager state |
| `runtime.inputs[]` | Complete | Per-input snapshot: status, last_packet_at, bitrate_kbps, errors[] |
| `runtime.switches[]` | Complete | Last 20 active-input switches with reason + detail |
| `runtime.transcoder.profiles[]` | Complete | Per-rung restart_count + errors[]; FFmpeg stderr-tail embedded |
| `runtime.publisher.pushes[]` | Complete | Per-destination status + attempts + errors[]; resets on Active |
| Defensive snapshot copies | Complete | Caller-side mutation cannot leak back into service state |

---

## Configuration Defaults

Single source of truth: [internal/domain/defaults.go](../internal/domain/defaults.go). Exposed via `GET /config/defaults` for frontend placeholder rendering.

| Group | Constants |
|---|---|
| Buffer | `DefaultBufferCapacity=1024` |
| Manager | `DefaultInputPacketTimeoutSec=30` |
| Publisher HLS/DASH | `DefaultLiveSegmentSec=2`, `DefaultLiveWindow=12`, `DefaultLiveHistory=0` |
| DVR | `DefaultDVRSegmentDuration=4`, `DefaultDVRRoot="./out/dvr"` |
| Push | `DefaultPushTimeoutSec=10`, `DefaultPushRetryTimeoutSec=5` |
| Hook | `DefaultHookMaxRetries=3`, `DefaultHookTimeoutSec=10` |
| Video | `DefaultVideoBitrateK=2500`, `DefaultVideoResizeMode=pad` |
| Audio | `DefaultAudioBitrateK=128` |
| Listeners | `DefaultListenHost="0.0.0.0"`, `DefaultRTMPConnectTimeoutSec=10`, `DefaultRTSPConnectTimeoutSec=10`, `DefaultSRTLatencyMS=120` |
| Ingestor | `DefaultHLSPlaylistTimeoutSec=15`, `DefaultHLSSegmentTimeoutSec=60`, `DefaultHLSMaxSegmentBuffer=8` |
| Transcoder | `DefaultFFmpegPath="ffmpeg"` |

---

## Pending / Planned

Tracking what is intentionally NOT done. Each row is a deliberate scope decision.

| Priority | Feature | Status | Notes |
|---|---|---|---|
| Mid | Watermark | Schema only | Needs FFmpeg `overlay`/`drawtext` injection in `buildVideoFilter` + asset upload endpoint |
| Mid | Thumbnail | Schema only | Periodic JPEG snapshot from main buffer; needs ffmpeg `select=eq(pict_type\\,I)` chain |
| Mid | Local-packager error tracking (HLS/DASH) | Not started | Currently slog-only; analogous to push state pattern but per-stream-per-format |
| Low | Per-input net config (read_timeout, reconnect_*, max_reconnects) | Schema only | Declared in domain.InputNetConfig, not consumed; either implement or remove |
| Low | HLS / DASH push out | Not started | Only RTMP/RTMPS push exists |
| Low | WebRTC publish / play | Not started | Pion-based subsystem; large surface (SDP, ICE, DTLS-SRTP) |

### Decided NOT (locked)

| Feature | Reason |
|---|---|
| Auto-recovery scheduler at coordinator level | Replaced by infinite per-module retry with backoff (transcoder retry forever, manager probe forever). No `MaxRestarts`. Pipeline never tears down on crash |
| RTMP ingest server lal migration | Current gomedia-based push server is stable with per-connection `recover()`. Cost-benefit doesn't justify refactor + retest matrix |
| Per-rendition push selection | Push always sends best rendition. Multi-tier publishing → run separate streams |
| Full `gomedia` → `lal` swap | TS infrastructure (`gomedia/go-mpeg2`) has no equivalent in lal. Hybrid stack is intentional |

---

## Testing & Quality

| Feature | Status | Notes |
|---|---|---|
| Unit tests — protocol detection | Complete | |
| Unit tests — buffer ring / fan-out | Complete | |
| Unit tests — manager state machine + bypass-recovery + switch history | Complete | |
| Unit tests — transcoder args, encoder routing, preset normalization, multi-output args | Complete | |
| Unit tests — transcoder health detection (3-fail edge, sustain recovery, multi-profile aggregation) | Complete | |
| Unit tests — coordinator diff engine + degradation reconciliation | Complete | |
| Unit tests — publisher HLS/DASH segmenters, push state | Complete | |
| Unit tests — DVR playlist parsing, gap recording | Complete | |
| Unit tests — error history rings (manager / transcoder / push) | Complete | |
| Unit tests — runtime status snapshots (defensive copy, sort order) | Complete | |
| Unit tests — FFmpeg probe (parsers, integration on PATH, missing binary, non-FFmpeg) | Complete | |
| Unit tests — config defaults endpoint (shape, codec routing table, determinism) | Complete | |
| Integration tests — coordinator.Update routing | Complete | 14 cases, spy implementations of all service interfaces |
| Integration tests — ffmpeg filter chain | Complete | Build-tagged; spawns real ffmpeg with generated `-vf` |
| CI (GitHub Actions) | Complete | `mod-tidy`, `test` (matrix Go 1.25.9 + stable), `lint` (allow-fail), `govulncheck` |
| Pre-commit hook (auto-regen swagger) | Complete | `make hooks-install` symlinks `scripts/git-hooks/pre-commit` |
| golangci-lint | Complete | 0 issues |
| gofumpt formatting | Complete | Enforced via lint |

---

## Operational Notes

- **FFmpeg required for transcoding.** Boot probes `transcoder.ffmpeg_path` (or `$PATH`) — REQUIRED encoders missing → server exits non-zero with a clear error. Optional encoders missing → boot warns but continues.
- **HLS and DASH dirs must differ** when both publishers are active.
- **DVR is per-stream opt-in.** No global enable.
- **Failover timestamp jumps** produce `#EXT-X-DISCONTINUITY` in HLS and are logged at debug level from FFmpeg stderr.
- **`PUT /streams/{code}` is non-disruptive** when the stream is running — only changed components restart.
- **Pipeline never tears down on FFmpeg crash.** Each profile retries forever with backoff. Status flips to `degraded` after 3 consecutive crashes; flips back to `active` after a sustained run (>30s) or hot-restart.
- **Multi-output toggle restarts running streams** — operator confirmation expected via UI before enabling on a busy server (~2-3s downtime per stream).
- **Build version** stamped at compile time (`make build` runs `git describe --tags --always --dirty`); exposed via `GET /config.version`.
- **`build/reinstall.sh <tag>`** downloads + verifies + uninstalls + reinstalls a tagged release on Linux/systemd hosts. Data dir preserved.
